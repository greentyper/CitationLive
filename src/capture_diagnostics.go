package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type nativeCapture struct {
	cmd              *exec.Cmd
	output           io.ReadCloser
	frames           chan captureFrame
	bufferStop       chan struct{}
	stopOnce         sync.Once
	subID            uint64
	isSub            bool
	worker           *persistentCaptureWorker
	uploadAfterUs    int64
	wakeBoundaryUs   int64
	serviceConn      net.Conn
	servicePending   []byte
	serviceTimestamp int64
}

type captureFrame struct {
	data      []byte
	timestamp int64
	err       error
}

// AudioRingBuffer holds 2.0s of recent 16 kHz S16_LE PCM audio frames (200 chunks of 10ms / 320 bytes).
type AudioRingBuffer struct {
	mu       sync.RWMutex
	capacity int
	chunks   [][]byte
	frames   []captureFrame
	head     int
	count    int
	Count    int
}

// NewAudioRingBuffer constructs an AudioRingBuffer with the specified capacity.
func NewAudioRingBuffer(capacity int) *AudioRingBuffer {
	if capacity <= 0 {
		capacity = 200 // 2.0s at 10ms per chunk (16 kHz mono S16_LE = 320 bytes)
	}
	return &AudioRingBuffer{
		capacity: capacity,
		chunks:   make([][]byte, capacity),
		frames:   make([]captureFrame, capacity),
	}
}

// WriteChunk appends a PCM chunk into the circular ring buffer.
func (r *AudioRingBuffer) WriteChunk(pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	r.WriteFrame(pcm, monotonicMicros())
}

// WriteFrame appends a PCM chunk with associated monotonic microsecond timestamp.
func (r *AudioRingBuffer) WriteFrame(pcm []byte, timestamp int64) {
	if len(pcm) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c := make([]byte, len(pcm))
	copy(c, pcm)
	r.chunks[r.head] = c
	r.frames[r.head] = captureFrame{data: c, timestamp: timestamp}
	r.head = (r.head + 1) % r.capacity
	if r.count < r.capacity {
		r.count++
		r.Count = r.count
	}
}

// FlushPreWake clears any historical audio collected prior to the wake-word trigger,
// ensuring zero frames preceding "Hey Google" are retained or sent.
func (r *AudioRingBuffer) FlushPreWake() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.head = 0
	r.count = 0
	r.Count = 0
	for i := range r.chunks {
		r.chunks[i] = nil
	}
	for i := range r.frames {
		r.frames[i] = captureFrame{}
	}
}

// GetPreRoll extracts the specified duration or count of historical chunks in chronological order.
func (r *AudioRingBuffer) GetPreRoll(args ...any) [][]byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	numChunks := 100 // default 1.0s = 100 chunks of 10ms
	if len(args) > 0 {
		switch v := args[0].(type) {
		case int:
			numChunks = v
		case time.Duration:
			numChunks = int(v / (10 * time.Millisecond))
		}
	}
	if numChunks <= 0 || r.count == 0 {
		return nil
	}
	if numChunks > r.count {
		numChunks = r.count
	}
	if numChunks > r.capacity {
		numChunks = r.capacity
	}
	res := make([][]byte, numChunks)
	start := (r.head - numChunks + r.capacity) % r.capacity
	for i := 0; i < numChunks; i++ {
		idx := (start + i) % r.capacity
		res[i] = make([]byte, len(r.chunks[idx]))
		copy(res[i], r.chunks[idx])
	}
	return res
}

// GetPreRollDuration retrieves the pre-roll window matching the specified duration.
func (r *AudioRingBuffer) GetPreRollDuration(duration time.Duration) [][]byte {
	return r.GetPreRoll(duration)
}

// GetPreRollFrames retrieves historical captureFrame instances preserving monotonic timestamps.
func (r *AudioRingBuffer) GetPreRollFrames(args ...any) []captureFrame {
	r.mu.RLock()
	defer r.mu.RUnlock()
	numFrames := 100 // default 1.0s
	if len(args) > 0 {
		switch v := args[0].(type) {
		case int:
			numFrames = v
		case time.Duration:
			numFrames = int(v / (10 * time.Millisecond))
		}
	}
	if numFrames <= 0 || r.count == 0 {
		return nil
	}
	if numFrames > r.count {
		numFrames = r.count
	}
	if numFrames > r.capacity {
		numFrames = r.capacity
	}
	res := make([]captureFrame, numFrames)
	start := (r.head - numFrames + r.capacity) % r.capacity
	for i := 0; i < numFrames; i++ {
		idx := (start + i) % r.capacity
		dataCopy := make([]byte, len(r.frames[idx].data))
		copy(dataCopy, r.frames[idx].data)
		res[i] = captureFrame{
			data:      dataCopy,
			timestamp: r.frames[idx].timestamp,
		}
	}
	return res
}

type persistentCaptureWorker struct {
	mu          sync.Mutex
	ring        *AudioRingBuffer
	subscribers map[uint64]chan captureFrame
	nextSubID   uint64
	cmd         *exec.Cmd
	running     bool
	connected   bool
	stopChan    chan struct{}
	stoppedChan chan struct{}
}

var globalCaptureWorker = &persistentCaptureWorker{
	ring:        NewAudioRingBuffer(400),
	subscribers: make(map[uint64]chan captureFrame),
}

func (w *persistentCaptureWorker) isRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

func (w *persistentCaptureWorker) isConnected() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running && w.connected
}

func (w *persistentCaptureWorker) setConnected(connected bool) {
	w.mu.Lock()
	w.connected = connected
	w.mu.Unlock()
}

func (w *persistentCaptureWorker) subscribe(preRollChunks int) (*nativeCapture, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	framesCh := make(chan captureFrame, 1600)
	bufferStop := make(chan struct{})

	// Pre-fill with pre-roll frames only if requested (for wake boundary capture, preRollChunks == 0)
	if preRollChunks > 0 {
		preRoll := w.ring.GetPreRollFrames(preRollChunks)
		for _, f := range preRoll {
			framesCh <- f
		}
	}

	w.nextSubID++
	subID := w.nextSubID
	w.subscribers[subID] = framesCh

	return &nativeCapture{
		frames:     framesCh,
		bufferStop: bufferStop,
		subID:      subID,
		isSub:      true,
		worker:     w,
	}, nil
}

// subscribeAtWakeBoundary subscribes to incoming capture frames starting strictly from the wake-word boundary,
// explicitly flushing any pre-wake audio recorded prior to the wake-word trigger.
func (w *persistentCaptureWorker) subscribeAtWakeBoundary() (*nativeCapture, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Zero frames preceding the wake-word are placed into framesCh.
	// Flush any pre-wake audio collected prior to the wake-word trigger.
	if w.ring != nil {
		w.ring.FlushPreWake()
	}

	framesCh := make(chan captureFrame, 1600)
	bufferStop := make(chan struct{})

	w.nextSubID++
	subID := w.nextSubID
	w.subscribers[subID] = framesCh

	return &nativeCapture{
		frames:     framesCh,
		bufferStop: bufferStop,
		subID:      subID,
		isSub:      true,
		worker:     w,
	}, nil
}

func (w *persistentCaptureWorker) unsubscribe(subID uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.subscribers, subID)
}

// startPersistentCapture initiates continuous background capture without process respawning.
func startPersistentCapture() error {
	globalCaptureWorker.mu.Lock()
	defer globalCaptureWorker.mu.Unlock()
	if globalCaptureWorker.running {
		return nil
	}
	globalCaptureWorker.running = true
	globalCaptureWorker.stopChan = make(chan struct{})
	globalCaptureWorker.stoppedChan = make(chan struct{})
	go globalCaptureWorker.run()
	return nil
}

// stopPersistentCapture cleanly stops the continuous background worker.
func stopPersistentCapture() {
	globalCaptureWorker.mu.Lock()
	if !globalCaptureWorker.running {
		globalCaptureWorker.mu.Unlock()
		return
	}
	globalCaptureWorker.running = false
	globalCaptureWorker.connected = false
	close(globalCaptureWorker.stopChan)
	stopped := globalCaptureWorker.stoppedChan
	cmd := globalCaptureWorker.cmd
	globalCaptureWorker.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-stopped:
	case <-time.After(1 * time.Second):
	}
}

func (w *persistentCaptureWorker) run() {
	defer close(w.stoppedChan)
	for {
		select {
		case <-w.stopChan:
			return
		default:
		}

		c, err := openNativeCapture()
		if err != nil {
			w.setConnected(false)
			log.Println("persistent capture spawn error:", clean(err))
			select {
			case <-w.stopChan:
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		w.mu.Lock()
		w.cmd = c.cmd
		w.mu.Unlock()

		for {
			select {
			case <-w.stopChan:
				c.close()
				return
			default:
			}

			data, ts, err := c.readDirect()
			if err != nil {
				w.setConnected(false)
				log.Println("persistent capture read error, restarting:", clean(err))
				c.close()
				break
			}
			w.setConnected(true)

			// Atomic snapshot + subscriber registration uses this same lock.
			// A frame must appear in either pre-roll or live delivery, not both.
			w.mu.Lock()
			w.ring.WriteFrame(data, ts)
			for _, sub := range w.subscribers {
				select {
				case sub <- captureFrame{data: data, timestamp: ts}:
				default:
					// Slow consumer drop to avoid blocking capture pipeline
				}
			}
			w.mu.Unlock()
		}

		select {
		case <-w.stopChan:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// openSessionAtWakeBoundary returns a channel that streams audio strictly from the wake-word boundary,
// flushing any pre-wake audio so zero frames preceding the wake word are sent, and streaming incoming frames
// from globalCaptureWorker until context cancellation or session close.
func openSessionAtWakeBoundary(ctx ...context.Context) (chan []byte, error) {
	sub, err := globalCaptureWorker.subscribeAtWakeBoundary()
	if err != nil {
		return nil, err
	}
	ch := make(chan []byte, 1600)
	go func() {
		defer close(ch)
		defer sub.close()

		var cancelChan <-chan struct{}
		if len(ctx) > 0 && ctx[0] != nil {
			cancelChan = ctx[0].Done()
			go func() {
				select {
				case <-cancelChan:
					sub.close()
				case <-sub.bufferStop:
				}
			}()
		}

		for {
			data, _, err := sub.read()
			if err != nil {
				return
			}

			select {
			case ch <- data:
			case <-cancelChan:
				return
			case <-sub.bufferStop:
				return
			}
		}
	}()
	return ch, nil
}

// openSessionWithPreRoll pre-populates a channel with pre-roll audio frames ahead of real-time streaming.
func openSessionWithPreRoll(ring *AudioRingBuffer) (chan []byte, error) {
	if ring == nil {
		ring = globalCaptureWorker.ring
	}
	ch := make(chan []byte, 1600)
	preRoll := ring.GetPreRoll(100)
	for _, chunk := range preRoll {
		ch <- chunk
	}
	return ch, nil
}

func openBufferedNativeCapture() (*nativeCapture, error) {
	if globalCaptureWorker.isConnected() {
		// Gemini Live setup can take ~2 s after the hotword. Keep enough
		// pre-roll to cover the command spoken during that gap, not just the
		// wake word. The ring is 4 s; upload the last 2.5 s.
		wakeUs := monotonicMicros()
		capture, err := globalCaptureWorker.subscribe(300)
		if capture != nil {
			preRollUs := int64(2500000)
			if isRecentDJWake() {
				preRollUs = 3000000
			}
			capture.wakeBoundaryUs = wakeUs
			capture.uploadAfterUs = wakeUs - preRollUs
		}
		return capture, err
	}
	c, e := openNativeCapture()
	if e != nil {
		return nil, e
	}
	c.frames = make(chan captureFrame, 1600)
	c.bufferStop = make(chan struct{})
	go func() {
		defer close(c.frames)
		for {
			data, timestamp, e := c.readDirect()
			select {
			case c.frames <- captureFrame{data, timestamp, e}:
			case <-c.bufferStop:
				return
			}
			if e != nil {
				return
			}
		}
	}()
	return c, nil
}

func openNativeCapture() (*nativeCapture, error) {
	if usingNativeCaptureService() {
		return openCaptureService()
	}
	c := exec.Command("/data/opt/citation-gemini/citation-capture")
	c.Stderr = io.Discard
	c.Env = append(os.Environ(), "ALSA_CONFIG=/etc/asound-product.conf")
	out, e := c.StdoutPipe()
	if e != nil {
		return nil, e
	}
	if e = c.Start(); e != nil {
		return nil, e
	}
	return &nativeCapture{cmd: c, output: out}, nil
}

func (c *nativeCapture) read() ([]byte, int64, error) {
	if c.frames != nil {
		select {
		case <-c.bufferStop:
			return nil, 0, io.EOF
		case f, ok := <-c.frames:
			if !ok {
				return nil, 0, io.EOF
			}
			return f.data, f.timestamp, f.err
		}
	}
	return c.readDirect()
}

func (c *nativeCapture) readDirect() ([]byte, int64, error) {
	if c.serviceConn != nil {
		return c.readService()
	}
	var h [16]byte
	if _, e := io.ReadFull(c.output, h[:]); e != nil {
		return nil, 0, e
	}
	if string(h[:4]) != "CAP0" || binary.LittleEndian.Uint32(h[12:]) != 160 {
		return nil, 0, errors.New("invalid capture header")
	}
	data := make([]byte, 320)
	_, e := io.ReadFull(c.output, data)
	return data, int64(binary.LittleEndian.Uint64(h[4:12])), e
}

func (c *nativeCapture) close() {
	if c.bufferStop != nil {
		c.stopOnce.Do(func() { close(c.bufferStop) })
	}
	if c.isSub {
		if c.worker != nil {
			c.worker.unsubscribe(c.subID)
		} else {
			globalCaptureWorker.unsubscribe(c.subID)
		}
		return
	}
	if c.serviceConn != nil {
		c.serviceConn.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Signal(syscall.SIGTERM)
		c.cmd.Wait()
	}
	if c.output != nil {
		c.output.Close()
	}
}
func echoProbe(path string) error {
	processor, e := openAecPipeline()
	if e != nil {
		return e
	}
	defer processor.close()
	capture, e := openNativeCapture()
	if e != nil {
		return e
	}
	defer capture.close()
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if len(b) < 44 {
		return errors.New("short WAV")
	}
	played := make(chan error, 1)
	go func() {
		for j := 0; j < 4; j++ {
			o, e := openMixerOutput()
			if e != nil {
				played <- e
				return
			}
			e = o.write(b[44:])
			if e == nil {
				e = o.finish()
			} else {
				o.close()
			}
			if e != nil {
				played <- e
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
		played <- nil
	}()
	var prevIn, prevOut float64
	var near, far, cleaned []float64
	deadline := time.Now().Add(8 * time.Second)
	var captureDelta int64
	for time.Now().Before(deadline) {
		frame, ts, e := capture.read()
		if e != nil {
			return e
		}
		captureDelta = monotonicMicros() - ts
		if captureDelta < -100000 || captureDelta > 1000000 {
			return fmt.Errorf("capture timestamp not monotonic: delta=%d", captureDelta)
		}
		for i := 0; i < len(frame); i += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(frame[i:])))
			y := v - prevIn + .995*prevOut
			prevIn = v
			prevOut = y
			if y > 32767 {
				y = 32767
			}
			if y < -32768 {
				y = -32768
			}
			binary.LittleEndian.PutUint16(frame[i:], uint16(int16(y)))
		}
		reference := processor.ref.samplesAt(ts, 160)
		filtered, e := processor.process(frame, ts)
		if e != nil {
			return e
		}
		for i := 0; i < 320; i += 2 {
			near = append(near, float64(int16(binary.LittleEndian.Uint16(frame[i:]))))
			far = append(far, float64(int16(binary.LittleEndian.Uint16(reference[i:]))))
			cleaned = append(cleaned, float64(int16(binary.LittleEndian.Uint16(filtered[i:]))))
		}
	}
	select {
	case e := <-played:
		if e != nil {
			return e
		}
	default:
		return errors.New("probe playback did not finish")
	}
	best := float64(0)
	bestLag := 0
	for lag := -8000; lag <= 8000; lag += 80 {
		var xy, xx, yy float64
		for i := 16000; i < len(near)-8000; i += 4 {
			j := i - lag
			if j < 0 || j >= len(far) {
				continue
			}
			x, y := near[i], far[j]
			xy += x * y
			xx += x * x
			yy += y * y
		}
		if xx > 0 && yy > 0 {
			v := math.Abs(xy / math.Sqrt(xx*yy))
			if v > best {
				best = v
				bestLag = lag
			}
		}
	}
	var n2, c2 float64
	for i := 32000; i < len(near); i++ {
		n2 += near[i] * near[i]
		c2 += cleaned[i] * cleaned[i]
	}
	fmt.Printf("{\"probe\":\"on-device synthetic reply, no audio retained\",\"capture_delta_us\":%d,\"best_echo_delay_ms\":%.1f,\"correlation\":%.3f,\"attenuation_db\":%.1f}\n", captureDelta, float64(bestLag)/16, best, 10*math.Log10(math.Max(n2, 1)/math.Max(c2, 1)))
	return nil
}

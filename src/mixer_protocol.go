package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sync"
	"time"
)

func pbVar(v uint64) []byte {
	b := []byte{}
	for v > 127 {
		b = append(b, byte(v)|128)
		v >>= 7
	}
	return append(b, byte(v))
}

// Native mixer input. Only this stream is created/closed; the hardware output
// remains owned by Cast with its original configuration.
type mixerOutput struct {
	conn       net.Conn
	ack        chan struct{}
	eos        chan struct{}
	failed     chan error
	done       chan struct{}
	pts        int64
	rate       int64
	frameBytes int
	blockBytes int
	pending    []byte
	once       sync.Once
}

func openMixerOutput() (*mixerOutput, error) {
	return openPCMOutput(24000, 1, 3)
}

func openPCMOutput(rate, channels, contentType int) (*mixerOutput, error) {
	c, e := mixerDial()
	if e != nil {
		return nil, e
	}
	out := &mixerOutput{conn: c, ack: make(chan struct{}, 8), eos: make(chan struct{}, 1), failed: make(chan error, 1), done: make(chan struct{})}
	out.rate, out.frameBytes = int64(rate), channels*2
	block := rate / 100
	out.blockBytes = block * out.frameBytes
	var params []byte
	for _, v := range [][2]uint64{
		{1, 0},
		{2, 0},
		{3, 0},
		{5, uint64(rate)},
		{6, uint64(channels)},
		{7, 1},
		{9, uint64(block)},
		{10, uint64(block)},
		{11, uint64(10 * block)},
		{12, uint64(block)},
		{16, uint64(contentType)},
	} {
		params = append(params, pbInt(int(v[0]), v[1])...)
	}
	params = append(params, pbMessage(4, []byte("default"))...)
	if e = mixerProto(c, pbMessage(1, params)); e != nil {
		c.Close()
		return nil, e
	}
	// The mixer's default output-stream multiplier is very low (about 0.01),
	// which makes local timer/reminder TTS inaudible. Set this stream to unity.
	if e = mixerProto(c, pbMessage(4, pbFloat(1, 1.0))); e != nil {
		c.Close()
		return nil, e
	}
	go func() {
		for {
			packet, e := mixerRead(c)
			if e != nil {
				select {
				case out.failed <- e:
				default:
				}
				return
			}
			if v, ok := packet.meta[20]; ok {
				m, _ := pbDecode(v.data)
				select {
				case out.failed <- fmt.Errorf("native mixer rejected stream: %s", m[2].data):
				default:
				}
				return
			}
			if _, ok := packet.meta[6]; ok {
				select {
				case out.ack <- struct{}{}:
				case <-out.done:
					return
				}
			}
			if _, ok := packet.meta[7]; ok {
				select {
				case out.eos <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out, nil
}
func (o *mixerOutput) push(data []byte) error {
	o.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if e := mixerAudio(o.conn, data, o.pts); e != nil {
		return e
	}
	o.pts += int64(len(data)/o.frameBytes) * 1000000 / o.rate
	select {
	case <-o.ack:
		return nil
	case e := <-o.failed:
		return e
	case <-o.done:
		return io.EOF
	case <-time.After(3 * time.Second):
		return errors.New("native mixer acknowledgement timed out")
	}
}
func (o *mixerOutput) write(data []byte) error {
	o.pending = append(o.pending, data...)
	for len(o.pending) >= o.blockBytes {
		if e := o.push(o.pending[:o.blockBytes]); e != nil {
			return e
		}
		o.pending = o.pending[o.blockBytes:]
	}
	return nil
}
func (o *mixerOutput) finish() error {
	defer o.close()
	if len(o.pending) > 0 {
		if e := o.push(o.pending); e != nil {
			return e
		}
		o.pending = nil
	}
	if e := mixerAudio(o.conn, nil, o.pts); e != nil {
		return e
	}
	select {
	case <-o.eos:
		return nil
	case e := <-o.failed:
		return e
	case <-time.After(4 * time.Second):
		return errors.New("native mixer drain timed out")
	}
}
func (o *mixerOutput) close() { o.once.Do(func() { close(o.done); o.conn.Close() }) }

type referenceBlock struct {
	timestamp int64
	samples   []int16
}
type mixerReference struct {
	conn                   net.Conn
	mu                     sync.Mutex
	blocks                 []referenceBlock
	format, rate, channels int
	packets                int
	peak                   int
	lastError              string
}

func openMixerReference() (*mixerReference, error) {
	c, e := mixerDial()
	if e != nil {
		return nil, e
	}
	r := &mixerReference{conn: c}
	if e = mixerProto(c, pbMessage(10, nil)); e != nil {
		c.Close()
		return nil, e
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	packet, e := mixerRead(c)
	if e != nil {
		c.Close()
		return nil, e
	}
	c.SetReadDeadline(time.Time{})
	if e = r.accept(packet); e != nil {
		c.Close()
		return nil, e
	}
	if r.rate != 48000 || r.format != 2 || r.channels != 2 {
		c.Close()
		return nil, fmt.Errorf("unsupported native loopback: format=%d rate=%d channels=%d", r.format, r.rate, r.channels)
	}
	go func() {
		for {
			packet, e := mixerRead(c)
			if e == nil {
				e = r.accept(packet)
			}
			if e != nil {
				r.mu.Lock()
				if !errors.Is(e, net.ErrClosed) {
					r.lastError = e.Error()
				}
				r.mu.Unlock()
				return
			}
		}
	}()
	return r, nil
}
func (r *mixerReference) accept(packet mixerPacket) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if value, ok := packet.meta[9]; ok {
		config, e := pbDecode(value.data)
		if e != nil {
			return e
		}
		r.format = int(config[1].number)
		r.rate = int(config[2].number)
		r.channels = int(config[3].number)
	}
	if len(packet.audio) > 0 {
		if r.format != 2 || r.rate != 48000 || r.channels != 2 {
			return errors.New("native loopback format changed")
		}
		if len(packet.audio)%8 != 0 {
			return errors.New("invalid float stereo loopback frame")
		}
		frames := len(packet.audio) / 8
		samples := make([]int16, frames/3)
		for i := range samples {
			sum := float64(0)
			for f := 0; f < 3; f++ {
				for ch := 0; ch < 2; ch++ {
					v := math.Float32frombits(binary.LittleEndian.Uint32(packet.audio[((i*3+f)*2+ch)*4:]))
					if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
						return errors.New("invalid loopback sample")
					}
					sum += float64(v)
				}
			}
			value := math.Round(sum / 6 * 32768)
			if value > 32767 {
				value = 32767
			}
			if value < -32768 {
				value = -32768
			}
			samples[i] = int16(value)
			if int(math.Abs(value)) > r.peak {
				r.peak = int(math.Abs(value))
			}
		}
		r.blocks = append(r.blocks, referenceBlock{packet.timestamp, samples})
		r.packets++
		if len(r.blocks) > 240 {
			r.blocks = r.blocks[len(r.blocks)-240:]
		}
	}
	return nil
}
func (r *mixerReference) samplesAt(timestamp int64, count int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, count*2)
	for i := 0; i < count; i++ {
		t := timestamp + int64(i)*1000000/16000
		for b := len(r.blocks) - 1; b >= 0; b-- {
			block := r.blocks[b]
			offset := (t - block.timestamp) * 16000 / 1000000
			if t >= block.timestamp && offset >= 0 && offset < int64(len(block.samples)) {
				binary.LittleEndian.PutUint16(out[i*2:], uint16(block.samples[offset]))
				break
			}
		}
	}
	return out
}
func (r *mixerReference) stats() M {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := M{"packets": r.packets, "peak": r.peak, "error": r.lastError}
	if len(r.blocks) > 0 {
		result["reference_ahead_us"] = r.blocks[len(r.blocks)-1].timestamp - monotonicMicros()
	}
	return result
}

func mixerPlayFile(path string) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if len(b) < 44 || string(b[:4]) != "RIFF" || binary.LittleEndian.Uint16(b[22:24]) != 1 || binary.LittleEndian.Uint32(b[24:28]) != 24000 || binary.LittleEndian.Uint16(b[34:36]) != 16 {
		return errors.New("expected 24 kHz mono S16 WAV")
	}
	ref, e := openMixerReference()
	if e != nil {
		return e
	}
	defer ref.conn.Close()
	output, e := openMixerOutput()
	if e != nil {
		return e
	}
	defer output.close()
	if e = output.write(b[44:]); e != nil {
		return e
	}
	if e = output.finish(); e != nil {
		return e
	}
	time.Sleep(100 * time.Millisecond)
	result := ref.stats()
	result["native_mixer_played"] = true
	result["pcm_bytes"] = len(b) - 44
	raw, _ := json.Marshal(result)
	fmt.Println(string(raw))
	return nil
}
func pbInt(n int, v uint64) []byte { return append(pbVar(uint64(n*8)), pbVar(v)...) }
func pbMessage(n int, v []byte) []byte {
	b := append(pbVar(uint64(n*8+2)), pbVar(uint64(len(v)))...)
	return append(b, v...)
}
func pbFloat(n int, v float32) []byte {
	b := pbVar(uint64(n*8 + 5))
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], math.Float32bits(v))
	return append(b, buf[:]...)
}
func pbSetVolumeLimit(contentType int, multiplier float32) []byte {
	var payload []byte
	payload = append(payload, pbInt(1, uint64(contentType))...)
	payload = append(payload, pbFloat(2, multiplier)...)
	return pbMessage(15, payload)
}
func clampVolumeMultiplier(v float32) (float32, error) {
	if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
		return 1.0, errors.New("invalid volume multiplier: NaN or Inf")
	}
	if v < 0.0 {
		return 0.0, nil
	}
	if v > 1.0 {
		return 1.0, nil
	}
	return v, nil
}
func sendVolumeLimit(socketPath string, contentType int, multiplier float32) error {
	clamped, err := clampVolumeMultiplier(multiplier)
	if err != nil {
		return err
	}
	if socketPath == "" {
		socketPath = "\x00/tmp/mixer-service"
	}
	c, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	return mixerProto(c, pbSetVolumeLimit(contentType, clamped))
}
func mixerDial() (net.Conn, error) {
	var lastErr error
	for i := 0; i < 6; i++ {
		c, err := net.DialTimeout("unix", "\x00/tmp/mixer-service", 1500*time.Millisecond)
		if err == nil {
			return c, nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	return nil, lastErr
}
func mixerProto(c net.Conn, proto []byte) error {
	pad := (4 - len(proto)%4) % 4
	out := make([]byte, 8+len(proto)+pad)
	binary.BigEndian.PutUint16(out[:2], uint16(len(out)-2))
	binary.BigEndian.PutUint32(out[4:8], uint32(pad))
	copy(out[8:], proto)
	_, err := c.Write(out)
	return err
}
func mixerAudio(c net.Conn, data []byte, pts int64) error {
	if len(data) > 65521 {
		return errors.New("mixer frame too large")
	}
	out := make([]byte, 16+len(data))
	binary.BigEndian.PutUint16(out[:2], uint16(len(out)-2))
	binary.LittleEndian.PutUint16(out[2:4], 1)
	binary.LittleEndian.PutUint64(out[4:12], uint64(pts))
	copy(out[16:], data)
	_, err := c.Write(out)
	return err
}

type pbValue struct {
	number uint64
	data   []byte
}

func pbDecode(b []byte) (map[int]pbValue, error) {
	out := map[int]pbValue{}
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errors.New("bad proto tag")
		}
		b = b[n:]
		var value pbValue
		switch tag & 7 {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errors.New("bad varint")
			}
			value.number = v
			b = b[n:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return nil, errors.New("bad proto length")
			}
			b = b[n:]
			value.data = b[:int(l)]
			b = b[int(l):]
		case 1:
			if len(b) < 8 {
				return nil, io.ErrUnexpectedEOF
			}
			value.number = binary.LittleEndian.Uint64(b[:8])
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return nil, io.ErrUnexpectedEOF
			}
			value.number = uint64(binary.LittleEndian.Uint32(b[:4]))
			b = b[4:]
		default:
			return nil, errors.New("unsupported proto wire type")
		}
		out[int(tag>>3)] = value
	}
	return out, nil
}

type mixerPacket struct {
	meta      map[int]pbValue
	audio     []byte
	timestamp int64
}

func mixerRead(c net.Conn) (mixerPacket, error) {
	var header [2]byte
	if _, e := io.ReadFull(c, header[:]); e != nil {
		return mixerPacket{}, e
	}
	n := int(binary.BigEndian.Uint16(header[:]))
	if n < 2 {
		return mixerPacket{}, errors.New("short mixer message")
	}
	b := make([]byte, n)
	if _, e := io.ReadFull(c, b); e != nil {
		return mixerPacket{}, e
	}
	switch binary.LittleEndian.Uint16(b[:2]) {
	case 0:
		if n < 6 {
			return mixerPacket{}, errors.New("short metadata")
		}
		padding := int(binary.BigEndian.Uint32(b[2:6]))
		if padding > 3 || n-padding < 6 {
			return mixerPacket{}, errors.New("invalid metadata padding")
		}
		m, e := pbDecode(b[6 : n-padding])
		return mixerPacket{meta: m}, e
	case 1:
		if n < 14 {
			return mixerPacket{}, errors.New("short audio")
		}
		return mixerPacket{audio: b[14:], timestamp: int64(binary.LittleEndian.Uint64(b[2:10]))}, nil
	default:
		return mixerPacket{}, errors.New("unknown mixer packet")
	}
}
func mixerCheck() error {
	c, e := mixerDial()
	if e != nil {
		return fmt.Errorf("connect: %w", e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if e = mixerProto(c, pbMessage(10, nil)); e != nil {
		return e
	}
	packet, e := mixerRead(c)
	if e != nil {
		return fmt.Errorf("loopback first packet: %w", e)
	}
	result := M{"connected": true}
	if v, ok := packet.meta[9]; ok {
		config, e := pbDecode(v.data)
		if e != nil {
			return e
		}
		result["sample_format"] = config[1].number
		result["sample_rate"] = config[2].number
		result["channels"] = config[3].number
		result["data_size"] = config[4].number
	}
	b, _ := json.Marshal(result)
	fmt.Println(string(b))
	return nil
}

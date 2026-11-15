package main

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"time"
)

const nativeCaptureService = "native_capture_service"

func usingNativeCaptureService() bool { return cfg.MicrophoneBackend == nativeCaptureService }

func captureHealth() M {
	w := globalCaptureWorker
	w.mu.Lock()
	running := w.running
	connected := w.connected
	ring := w.ring
	w.mu.Unlock()
	out := M{"running": false, "worker_running": running, "connected": connected, "backend": cfg.MicrophoneBackend}
	if ring != nil {
		frames := ring.GetPreRollFrames(1)
		ring.mu.RLock()
		out["buffered_frames"] = ring.count
		ring.mu.RUnlock()
		if len(frames) > 0 {
			age := (monotonicMicros() - frames[0].timestamp) / 1000
			out["latest_frame_age_ms"] = age
			out["running"] = connected && age >= -100 && age <= 1000
		}
	}
	return out
}

// Recovered from the shipped iot_audio_capture (SHA256 92be267c...): a
// BE16-length-framed kind-0 init, stream 1 selects its processed-audio observer.
// This only attaches an observer; no processing/volume/mute settings are sent.
func openCaptureService() (*nativeCapture, error) {
	c, err := net.DialTimeout("unix", "\x00/tmp/capture-service", 2*time.Second)
	if err != nil {
		return nil, err
	}
	init := make([]byte, 16)
	binary.BigEndian.PutUint16(init[:2], 14)
	init[3] = 1 // processed stream, not the raw stereo stream 0
	c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err = c.Write(init); err != nil {
		c.Close()
		return nil, err
	}
	return &nativeCapture{serviceConn: c}, nil
}

func decodeProcessedCapturePacket(b []byte) ([]byte, int64, error) {
	if len(b) < 14 || b[0] != 1 || b[1] != 1 {
		return nil, 0, errors.New("unexpected native capture packet")
	}
	if (b[2] != 2 && b[2] != 5) || b[3] != 1 || binary.LittleEndian.Uint16(b[4:6]) != 16000 {
		return nil, 0, errors.New("native processed capture must be 16kHz mono float")
	}
	payload := b[14:]
	if len(payload) == 0 || len(payload)%640 != 0 {
		return nil, 0, errors.New("native capture must contain whole 10ms blocks")
	}
	pcm := make([]byte, len(payload)/2)
	for i := 0; i < len(payload); i += 4 {
		sample := float64(math.Float32frombits(binary.LittleEndian.Uint32(payload[i:])))
		if math.IsNaN(sample) || math.IsInf(sample, 0) {
			return nil, 0, errors.New("invalid native capture sample")
		}
		v := math.Round(sample * 32768)
		v = math.Max(-32768, math.Min(32767, v))
		binary.LittleEndian.PutUint16(pcm[i/2:], uint16(int16(v)))
	}
	return pcm, int64(binary.LittleEndian.Uint64(b[6:14])), nil
}

func (c *nativeCapture) readService() ([]byte, int64, error) {
	for len(c.servicePending) < 320 {
		c.serviceConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var h [2]byte
		if _, err := io.ReadFull(c.serviceConn, h[:]); err != nil {
			return nil, 0, err
		}
		n := int(binary.BigEndian.Uint16(h[:]))
		if n < 14 {
			return nil, 0, errors.New("short native capture frame")
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(c.serviceConn, b); err != nil {
			return nil, 0, err
		}
		pcm, ts, err := decodeProcessedCapturePacket(b)
		if err != nil {
			return nil, 0, err
		}
		// With the explicit 48k -> 16k ALSA rate node, the native PTS is
		// correctly paced and can be preserved for the command pre-roll.

		c.servicePending = pcm
		c.serviceTimestamp = ts
	}
	data := c.servicePending[:320]
	c.servicePending = c.servicePending[320:]
	ts := c.serviceTimestamp
	c.serviceTimestamp += 10000
	return data, ts, nil
}

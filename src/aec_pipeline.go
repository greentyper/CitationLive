package main

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os/exec"
	"sync/atomic"
	"syscall"
	"unsafe"
)

func monotonicMicros() int64 {
	var t syscall.Timespec
	_, _, e := syscall.RawSyscall(syscall.SYS_CLOCK_GETTIME, 1, uintptr(unsafe.Pointer(&t)), 0)
	if e != 0 {
		return 0
	}
	return int64(t.Sec)*1000000 + int64(t.Nsec)/1000
}

type aecPipeline struct {
	cmd                     *exec.Cmd
	input                   io.WriteCloser
	output                  io.ReadCloser
	ref                     *mixerReference
	frames                  int
	nearEnergy, cleanEnergy float64
	farActiveFrames         int
	farGain                 float64
	lastRenderUs            int64
	residualFrames          int
	listening               atomic.Bool
	nativeProcessed         bool
}

func openAecPipeline() (*aecPipeline, error) {
	if usingNativeCaptureService() {
		return &aecPipeline{nativeProcessed: true}, nil
	}
	ref, e := sharedAECReference()
	if e != nil {
		return nil, e
	}
	cmd := exec.Command("/data/opt/citation-gemini/citation-aec")
	cmd.Stderr = io.Discard
	in, e := cmd.StdinPipe()
	if e != nil {
		return nil, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		in.Close()
		return nil, e
	}
	if e = cmd.Start(); e != nil {
		in.Close()
		out.Close()
		return nil, e
	}
	return &aecPipeline{cmd: cmd, input: in, output: out, ref: ref, farGain: 16}, nil
}
func (p *aecPipeline) process(near []byte, timestamp int64) ([]byte, error) {
	if p.nativeProcessed {
		p.frames += len(near) / 320
		return append([]byte(nil), near...), nil
	}
	if len(near)%320 != 0 {
		return nil, errors.New("AEC requires 10 ms blocks")
	}
	result := make([]byte, len(near))
	for offset := 0; offset < len(near); offset += 320 {
		t := timestamp + int64(offset/2)*1000000/16000
		far := p.ref.samplesAt(t, 160)
		for i := 0; i < len(far); i += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(far[i:]))) * p.farGain
			if v > 32767 {
				v = 32767
			}
			if v < -32768 {
				v = -32768
			}
			binary.LittleEndian.PutUint16(far[i:], uint16(int16(v)))
		}
		packet := make([]byte, 640)
		copy(packet, near[offset:offset+320])
		copy(packet[320:], far)
		if _, e := p.input.Write(packet); e != nil {
			return nil, e
		}
		if _, e := io.ReadFull(p.output, result[offset:offset+320]); e != nil {
			return nil, e
		}
		active := false
		for i := 0; i < 320; i += 2 {
			if int16(binary.LittleEndian.Uint16(far[i:])) > 3 || int16(binary.LittleEndian.Uint16(far[i:])) < -3 {
				active = true
				break
			}
		}
		if active {
			p.lastRenderUs = t
			p.farActiveFrames++
			for i := 0; i < 320; i += 2 {
				n := float64(int16(binary.LittleEndian.Uint16(near[offset+i:])))
				v := float64(int16(binary.LittleEndian.Uint16(result[offset+i:])))
				p.nearEnergy += n * n
				p.cleanEnergy += v * v
			}
		}
		// The old firmware AEC leaves low-level residual echo. Suppress only
		// echo-dominated frames during render and its acoustic tail; clear near
		// speech still passes so the user can interrupt playback.
		if t <= p.lastRenderUs+600000 && p.lastRenderUs != 0 {
			var n2, c2 float64
			for i := 0; i < 320; i += 2 {
				n := float64(int16(binary.LittleEndian.Uint16(near[offset+i:])))
				c := float64(int16(binary.LittleEndian.Uint16(result[offset+i:])))
				n2 += n * n
				c2 += c * c
			}
			if suppressResidualEcho(math.Sqrt(n2/160), math.Sqrt(c2/160), p.listening.Load()) {
				clear(result[offset : offset+320])
				p.residualFrames++
			}
		}
		p.frames++
	}
	return result, nil
}
func (p *aecPipeline) close() M {
	if p.nativeProcessed {
		return M{"aec_frames": p.frames, "microphone_backend": nativeCaptureService, "software_aec_bypassed": true, "residual_echo_suppressed_frames": 0}
	}
	p.input.Close()
	p.cmd.Wait()
	p.output.Close()
	stats := p.ref.stats()
	stats["aec_frames"] = p.frames
	stats["far_active_frames"] = p.farActiveFrames
	stats["residual_echo_suppressed_frames"] = p.residualFrames
	if p.farActiveFrames > 0 {
		stats["near_rms_during_render"] = math.Sqrt(p.nearEnergy / float64(p.farActiveFrames*160))
		stats["clean_rms_during_render"] = math.Sqrt(p.cleanEnergy / float64(p.farActiveFrames*160))
	}
	if p.cleanEnergy > 0 && p.nearEnergy > 0 {
		stats["attenuation_db_during_render"] = math.Round(10*math.Log10(p.nearEnergy/p.cleanEnergy)*10) / 10
	}
	return stats
}

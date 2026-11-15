package main

import (
	"encoding/binary"
	"math"
)

// Apply microphone calibration only after AEC. Playback volume is independent.
func microphoneGain() float64 {
	if cfg.MicrophoneGain >= 1 && cfg.MicrophoneGain <= 128 {
		return cfg.MicrophoneGain
	}
	return 1
}

func gainPCM16(data []byte, gain float64) (peak, clipped int) {
	return gainPCM16Ramp(data, gain, gain)
}

func gainPCM16Ramp(data []byte, start, end float64) (peak, clipped int) {
	step := (end - start) / float64(max(1, len(data)/2))
	for i := 0; i+1 < len(data); i += 2 {
		gain := start + float64(i/2+1)*step
		v := float64(int16(binary.LittleEndian.Uint16(data[i:]))) * gain
		if v > 32767 {
			v = 32767
			clipped++
		}
		if v < -32768 {
			v = -32768
			clipped++
		}
		n := int(v)
		if n < 0 {
			n = -n
		}
		if n > peak {
			peak = n
		}
		binary.LittleEndian.PutUint16(data[i:], uint16(int16(v)))
	}
	return
}

type quietSpeechGain struct {
	speechFrames int
	quietFrames  int
	boost        float64
}

// quietSpeechMinRMS is the minimum AC RMS required to treat a frame as speech
// for the quiet-speech booster. Measure AC level so microphone DC offset does
// not hide a quiet voice.
const quietSpeechMinRMS = 6.5

// quietSpeechBoostEligible gates the quiet-speech booster on a minimum RMS.
// A non-positive minRMS falls back to quietSpeechMinRMS. It is kept separate
// so callers can raise the gate (for example with a noise-floor-relative SNR)
// without changing the public multiplier signature.
func quietSpeechBoostEligible(rms, minRMS float64) bool {
	if minRMS <= 0 {
		minRMS = quietSpeechMinRMS
	}
	return rms >= minRMS
}

// quietSpeechResetFrames is how many consecutive non-speech frames (~250 ms)
// clear the booster state. Normal inter-word gaps are 100-200 ms; resetting
// after 8 frames (80 ms) restarted the boost on every word.
const quietSpeechResetFrames = 25

// Boost only confirmed near speech. Measure AC level so microphone DC offset
// does not hide a quiet voice. The base microphone calibration remains separate.
func (g *quietSpeechGain) multiplier(pcm []byte, speech bool, base float64, peak int) float64 {
	rms := nativeFrameACRMS(pcm)
	if !speech || !quietSpeechBoostEligible(rms, quietSpeechMinRMS) {
		g.quietFrames++
		if g.quietFrames >= quietSpeechResetFrames {
			*g = quietSpeechGain{}
		}
		return base
	}
	g.quietFrames = 0
	g.speechFrames++
	target := math.Max(1, math.Min(4, 900/(rms*base)))
	g.boost += 0.4 * (target - 1 - g.boost)
	headroom := 23170 / math.Max(1, float64(peak)*base)
	return base * math.Min(target, math.Min(1+g.boost, headroom))
}

type microphoneDCFilter struct {
	previousInput, previousOutput float64
}

// Remove the capture offset with a 20 Hz high-pass at 16 kHz.
// Keep the filter state between 10 ms frames to avoid boundary clicks.
func (f *microphoneDCFilter) apply(pcm []byte) {
	const pole = 0.99217678
	for i := 0; i+1 < len(pcm); i += 2 {
		in := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		out := in - f.previousInput + pole*f.previousOutput
		f.previousInput, f.previousOutput = in, out
		binary.LittleEndian.PutUint16(pcm[i:], uint16(int16(math.Max(-32768, math.Min(32767, math.Round(out))))))
	}
}

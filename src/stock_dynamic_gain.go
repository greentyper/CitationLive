package main

// Reimplemented dynamic-gain pieces recovered from libassistant.so.
//
// Static evidence (Ghidra 12.1.4, SHA-256 045ee738...):
//   - AutoGainControlConfig parser FUN_00366e84 in
//     third_party/google3/speech/micro/frontend/structs_io.c
//   - dynamic_gain.cc functions FUN_004257ec and FUN_004258ac
//   - object constructor FUN_00425788 and factory citation_00495b18
//
// The arithmetic below is a behavioural rewrite, not copied source. The
// recovered field names and defaults are observed; the SNR-based selection of
// max_signal_gain versus max_noise_gain is an inference from the field names
// and is marked as such.

import (
	"encoding/binary"
	"math"
)

// autoGainControlConfig is the shipped AutoGainControlConfig message. The
// defaults come from the missing-field branches in FUN_00366e84:
//
//	enabled          = 0
//	max_signal_gain  = 30.0
//	max_noise_gain   = 10.0
//	max_snr          = 10
//	buffer_ms        = 100.0
//	ramp_step_ms     = 10
type autoGainControlConfig struct {
	Enabled       bool
	MaxSignalGain float64
	MaxNoiseGain  float64
	MaxSNR        float64
	BufferMS      float64
	RampStepMS    float64
}

func defaultAutoGainControlConfig() autoGainControlConfig {
	return autoGainControlConfig{
		Enabled:       false,
		MaxSignalGain: 30.0,
		MaxNoiseGain:  10.0,
		MaxSNR:        10.0,
		BufferMS:      100.0,
		RampStepMS:    10.0,
	}
}

// stockDynamicGainPolicyEnabled matches the recovered stock default
// (AutoGainControlConfig.enabled = 0). Only an explicit
// "stock_dynamic_gain": true enables the stock AGC.
func stockDynamicGainPolicyEnabled() bool {
	return cfg.StockDynamicGain != nil && *cfg.StockDynamicGain
}

func stockDynamicGainConfig() autoGainControlConfig {
	c := defaultAutoGainControlConfig()
	c.Enabled = stockDynamicGainPolicyEnabled()
	if cfg.StockDynamicGainMax != nil && *cfg.StockDynamicGainMax > 0 {
		c.MaxSignalGain = *cfg.StockDynamicGainMax
	}
	return c
}

// stockDynamicGain is the 0x28-byte object built by FUN_00425788. The fields
// map directly to the recovered offsets:
//
//	+0x18 current gain (initialised to 1.0)
//	+0x1c target level
//	+0x20 minimum gain
//	+0x24 maximum gain
type stockDynamicGain struct {
	cfg     autoGainControlConfig
	target  float64
	minGain float64
	maxGain float64
	current float64
	power   float64
	frames  int
}

// newStockDynamicGain reproduces citation_00495b18:
//
//	target = 0.1
//	min    = 1.0 / maxGain
//	max    = 512.0 / maxGain
//
// maxGain is the configured max_signal_gain or max_noise_gain.
func newStockDynamicGain(cfg autoGainControlConfig, maxGain float64) *stockDynamicGain {
	if maxGain <= 0 {
		maxGain = cfg.MaxSignalGain
	}
	if maxGain <= 0 {
		maxGain = 30
	}
	return &stockDynamicGain{
		cfg:     cfg,
		target:  0.1,
		minGain: 1.0 / maxGain,
		maxGain: 512.0 / maxGain,
		current: 1.0,
	}
}

// gainForPower reproduces FUN_004257ec:
//
//	gain = target / sqrt(power)
//	gain = max(gain, minGain)
//	gain = min(gain, maxGain)
//
// power is the mean square of float samples in [-1, 1].
func (g *stockDynamicGain) gainForPower(power float64) float64 {
	if g == nil {
		return 1
	}
	if power <= 0 || math.IsNaN(power) || math.IsInf(power, 0) {
		return 1
	}
	gain := g.target / math.Sqrt(power)
	if gain < g.minGain {
		gain = g.minGain
	}
	if gain > g.maxGain {
		gain = g.maxGain
	}
	return gain
}

// gainForPCM converts S16 PCM to normalised power and applies the recovered
// formula. The 100 ms power buffer is a one-pole average at a 10 ms frame
// rate; buffer_ms and ramp_step_ms are observed config fields, but the exact
// smoothing implementation was not recovered, so this is a conservative
// inference.
func (g *stockDynamicGain) gainForPCM(pcm []byte, snrDB float64) float64 {
	if g == nil || !g.cfg.Enabled {
		return 1
	}
	power := pcmNormalizedPower(pcm)
	if power <= 0 {
		return 1
	}
	if g.frames == 0 {
		g.power = power
	} else {
		alpha := 0.1
		if g.cfg.BufferMS > 0 {
			alpha = math.Min(1, 10.0/g.cfg.BufferMS)
		}
		g.power = (1-alpha)*g.power + alpha*power
	}
	g.frames++

	// Inference: use the signal ceiling when the measured SNR is at least
	// max_snr, otherwise use the noise ceiling. The stock field names and the
	// separate max_signal_gain/max_noise_gain values support this split, but
	// the exact comparison site was not located.
	maxGain := g.cfg.MaxNoiseGain
	if snrDB >= g.cfg.MaxSNR {
		maxGain = g.cfg.MaxSignalGain
	}
	if maxGain <= 0 {
		maxGain = g.cfg.MaxSignalGain
	}
	if maxGain <= 0 {
		maxGain = 30
	}
	minGain := 1.0 / maxGain
	ceiling := g.ceiling()
	gain := g.target / math.Sqrt(g.power)
	if gain < minGain {
		gain = minGain
	}
	if gain > ceiling {
		gain = ceiling
	}

	// Do not attenuate below unity for our uplink. The stock AGC may reduce a
	// loud signal, but our existing peak limiter already handles clipping and
	// the user's request is stronger quiet-speech amplification.
	if gain < 1 {
		gain = 1
	}
	g.current = gain
	return gain
}

// ceiling returns the smallest recovered 512/maxGain bound. The stock formula
// produces 512/max_signal_gain for high SNR and 512/max_noise_gain for low SNR;
// because max_noise_gain is smaller by default, the noise branch would
// otherwise allow a larger boost than the signal branch. Never let the noise
// branch exceed the signal branch.
func (g *stockDynamicGain) ceiling() float64 {
	signal := g.cfg.MaxSignalGain
	if signal <= 0 {
		signal = 30
	}
	noise := g.cfg.MaxNoiseGain
	if noise <= 0 {
		noise = signal
	}
	ceiling := 512.0 / signal
	if n := 512.0 / noise; n < ceiling {
		ceiling = n
	}
	if g.maxGain > 0 && g.maxGain < ceiling {
		ceiling = g.maxGain
	}
	return ceiling
}

// reset clears the per-turn power average and restores the initial gain.
// Call it on speech end and turn boundaries so noise from one turn cannot
// leak into the next.
func (g *stockDynamicGain) reset() {
	if g == nil {
		return
	}
	g.power = 0
	g.frames = 0
	g.current = 1
}

// pcmNormalizedPower returns the mean square of S16 samples in [-1, 1].
func pcmNormalizedPower(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i+1 < len(pcm); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i:]))) / 32768.0
		sum += v * v
	}
	return sum / float64(n)
}

// stockDynamicGainDB is a small helper for status/logging.
func stockDynamicGainDB(gain float64) float64 {
	if gain <= 0 {
		return -120
	}
	return 20 * math.Log10(gain)
}

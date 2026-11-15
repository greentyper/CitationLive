package main

// Reimplemented stock audio configuration logic recovered from
// iot_audio_capture (SHA-256 92be267c...). The decompiled functions are in
// native-behavior-research/GHIDRA12_CAPTURE_FINDINGS.md and
// native-behavior-research/ghidra12-capture/. This file is a behavioural
// rewrite, not copied source.

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	stockAECGainPairCount  = 11
	stockAECGainTokenCount = stockAECGainPairCount * 2
	stockAGCDefaultMinMic  = 12
)

// aecGainPair is one entry of --voice-call-aec-gain-fixed. The stock parser
// requires exactly 22 colon-separated integers and stores them as 11
// (pre, reverse) pairs. The per-block consumer picks the pair by a 10 ms delay
// index.
type aecGainPair struct {
	PreDB     int
	ReverseDB int
}

func parseColonInts(value string) ([]int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("empty integer list")
	}
	parts := strings.Split(value, ":")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty integer in list")
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q: %w", part, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func parseAECGainFixed(value string) ([]int, error) {
	values, err := parseColonInts(value)
	if err != nil {
		return nil, err
	}
	if len(values) != stockAECGainTokenCount {
		return nil, fmt.Errorf("aec fixed gain needs exactly %d integers, got %d", stockAECGainTokenCount, len(values))
	}
	return values, nil
}

func parseAECGainPairs(value string) ([]aecGainPair, error) {
	values, err := parseColonInts(value)
	if err != nil {
		return nil, err
	}
	if len(values) != stockAECGainTokenCount {
		return nil, fmt.Errorf("aec fixed gain needs exactly %d integers (%d pairs), got %d",
			stockAECGainTokenCount, stockAECGainPairCount, len(values))
	}
	pairs := make([]aecGainPair, 0, stockAECGainPairCount)
	for i := 0; i < len(values); i += 2 {
		pairs = append(pairs, aecGainPair{PreDB: values[i], ReverseDB: values[i+1]})
	}
	return pairs, nil
}

// aecLinearPreGain is the stock 10^(pre_db/20) conversion.
func aecLinearPreGain(preDB int) float64 {
	return math.Pow(10, float64(preDB)/20)
}

// aecReverseAmplifierFactor reproduces the stock expression:
// 10^((pre - table[10].pre + post)/20).
func aecReverseAmplifierFactor(preDB int, table []aecGainPair, postDB int) (float64, error) {
	if len(table) < stockAECGainPairCount {
		return 0, errors.New("aec gain table has fewer than 11 pairs")
	}
	delta := preDB - table[stockAECGainPairCount-1].PreDB + postDB
	return math.Pow(10, float64(delta)/20), nil
}

// aecDelayConfig is the pair of fields the stock --voice-call-aec-delay-params
// parser writes into the AEC3 config.
type aecDelayConfig struct {
	FixedCaptureDelaySamples int
	DefaultDelay             int
}

// decodeAECDelayParams reproduces the stock arithmetic:
//   - first token  * 16000 / 1000 -> delay.fixed_capture_delay_samples
//   - second token * 0.25         -> delay.default_delay (10 ms blocks)
//
// Negative results are clamped to zero. The stock uses 16 kHz for the first
// conversion; sampleRate is a parameter here so the model is explicit.
func parseAECDelayParams(value string) ([]int, error) {
	values, err := parseColonInts(value)
	if err != nil {
		return nil, err
	}
	if len(values) != 2 {
		return nil, fmt.Errorf("aec delay params need exactly 2 integers, got %d", len(values))
	}
	return values, nil
}

func decodeAECDelayParams(value string, sampleRate int) (aecDelayConfig, error) {
	values, err := parseAECDelayParams(value)
	if err != nil {
		return aecDelayConfig{}, err
	}
	if sampleRate <= 0 {
		sampleRate = 16000
	}
	fixed := int(float64(values[0]) * float64(sampleRate) / 1000.0)
	if fixed < 0 {
		fixed = 0
	}
	defaultDelay := int(float64(values[1]) * 0.25)
	if defaultDelay < 0 {
		defaultDelay = 0
	}
	return aecDelayConfig{
		FixedCaptureDelaySamples: fixed,
		DefaultDelay:             defaultDelay,
	}, nil
}

func parseAECNumFilters(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("empty aec num filters")
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// agcPreset is the hard-coded GainController1 preset selected by the AGC
// manager byte at +0x10. The stock always uses kFixedDigital mode.
type agcPreset struct {
	TargetLevelDBFS   int
	CompressionGainDB int
	Limiter           bool
}

func agcPresetForManagerByte(b byte) agcPreset {
	if b == 0 {
		return agcPreset{TargetLevelDBFS: 2, CompressionGainDB: 7, Limiter: true}
	}
	return agcPreset{TargetLevelDBFS: 0, CompressionGainDB: 0, Limiter: false}
}

// parseAGCMinMicLevelExperiment reproduces the stock WebRTC experiment parser:
// value "Enabled-%d" is accepted only when the integer is in [0, 255];
// otherwise the default 12 is used.
func parseAGCMinMicLevelExperiment(value string) int {
	if !strings.HasPrefix(value, "Enabled-") {
		return stockAGCDefaultMinMic
	}
	n, err := strconv.Atoi(strings.TrimPrefix(value, "Enabled-"))
	if err != nil || n < 0 || n >= 256 {
		return stockAGCDefaultMinMic
	}
	return n
}

// aec3GainConfig is the subset of EchoCanceller3Config whose offsets were
// recovered from the stock JSON parser. Offsets are relative to the AEC3
// config base.
type aec3GainConfig struct {
	DefaultGain       float64 `json:"default_gain"`
	MaxGainDuringEcho float64 `json:"max_gain_during_echo"`
	AntiHowlingGain   float64 `json:"anti_howling_gain"`
	RenderPowerGainDB float64 `json:"render_power_gain_db"`
	NoiseFloorDBFS    float64 `json:"noise_floor_dbfs"`
}

var stockAEC3GainOffsets = map[string]uint32{
	"ep_strength.default_gain":                               0xb8,
	"suppressor.high_bands_suppression.max_gain_during_echo": 0x194,
	"suppressor.anti_howling_gain":                           0x19c,
	"render_levels.render_power_gain_db":                     0xec,
	"comfort_noise.noise_floor_dbfs":                         0x110,
}

// micFailureDetector mirrors the stock mic_failure_detector: it keeps the
// previously selected channel unless the loudest channel is more than 4 dB
// above the quietest one.
type micFailureDetector struct {
	selected int
}

func newMicFailureDetector() *micFailureDetector {
	return &micFailureDetector{selected: -1}
}

func (d *micFailureDetector) update(levelsDB []float64) int {
	if len(levelsDB) == 0 {
		return d.selected
	}
	maxV, minV := levelsDB[0], levelsDB[0]
	maxI := 0
	for i, v := range levelsDB[1:] {
		if v > maxV {
			maxV = v
			maxI = i + 1
		}
		if v < minV {
			minV = v
		}
	}
	if maxV-minV > 4.0 {
		d.selected = maxI
	}
	return d.selected
}

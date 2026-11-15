package main

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// voiceProfile is a small, speaker-specific VAD calibration. It stores only
// statistics, never the recorded PCM. The goal is to keep the user's own quiet
// syllables while rejecting room noise and the assistant's own TTS.
type voiceProfile struct {
	NoiseFloor float64 `json:"noise_floor"`
	SpeechRMS  float64 `json:"speech_rms"`
	QuietRMS   float64 `json:"quiet_rms"`
	ZCRMin     float64 `json:"zcr_min"`
	ZCRMax     float64 `json:"zcr_max"`
	ZCRMean    float64 `json:"zcr_mean"`
	ZCRStd     float64 `json:"zcr_std"`
	LowMin     float64 `json:"low_min"`
	LowMax     float64 `json:"low_max"`
	LowMean    float64 `json:"low_mean"`
	LowStd     float64 `json:"low_std"`
	Frames     int     `json:"frames"`
	Seconds    float64 `json:"seconds"`
	Created    string  `json:"created"`
}

var voiceProfileState struct {
	sync.RWMutex
	profile *voiceProfile
}

func currentVoiceProfile() *voiceProfile {
	voiceProfileState.RLock()
	defer voiceProfileState.RUnlock()
	if voiceProfileState.profile == nil {
		return nil
	}
	copy := *voiceProfileState.profile
	return &copy
}

func setCurrentVoiceProfile(p *voiceProfile) {
	voiceProfileState.Lock()
	voiceProfileState.profile = p
	voiceProfileState.Unlock()
}

func voiceProfilePath() string {
	return filepath.Join(stateDirectory, "voice_profile.json")
}

func loadVoiceProfile() {
	b, err := os.ReadFile(voiceProfilePath())
	if err != nil {
		return
	}
	var p voiceProfile
	if json.Unmarshal(b, &p) != nil || p.Frames < 20 || p.SpeechRMS <= 0 {
		return
	}
	setCurrentVoiceProfile(&p)
}

func saveVoiceProfile(p *voiceProfile) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeAtomicFile(voiceProfilePath(), b, 0600)
}

func meanStd(values []float64) (mean, std float64) {
	if len(values) == 0 {
		return 0, 0
	}
	for _, value := range values {
		mean += value
	}
	mean /= float64(len(values))
	for _, value := range values {
		diff := value - mean
		std += diff * diff
	}
	std = math.Sqrt(std / float64(len(values)))
	return
}

// voiceProfileDegenerate reports whether a calibration is too broad to define
// a usable speaker centroid. A varied sample (or one that captured broadband
// noise) produces a low-band window that covers almost everything, which turns
// the calibrated gate into an RMS-only detector. In that case the generic
// voice-shape gate is stricter and safer.
func voiceProfileDegenerate(p *voiceProfile) bool {
	if p == nil {
		return false
	}
	return p.LowStd > 0.22 || p.ZCRStd > 0.14 || p.LowMin < 0.10
}

func percentileSorted(values []float64, fraction float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if fraction <= 0 {
		return values[0]
	}
	if fraction >= 1 {
		return values[len(values)-1]
	}
	idx := int(math.Round(fraction * float64(len(values)-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return values[idx]
}

// analyzeVoiceFrames computes a speaker profile from 10 ms PCM frames. It
// discards the frames themselves and returns only aggregate statistics.
func analyzeVoiceFrames(frames [][]byte) *voiceProfile {
	if len(frames) < 20 {
		return nil
	}
	type frameStat struct{ rms, zcr, low float64 }
	var stats []frameStat
	for _, frame := range frames {
		rms, zcr, low := nativeFrameVoiceFeatures(frame)
		if rms < 1 {
			continue
		}
		stats = append(stats, frameStat{rms: rms, zcr: zcr, low: low})
	}
	if len(stats) < 20 {
		return nil
	}
	slices.SortFunc(stats, func(a, b frameStat) int {
		switch {
		case a.rms < b.rms:
			return -1
		case a.rms > b.rms:
			return 1
		}
		return 0
	})
	rmsValues := make([]float64, len(stats))
	for i, stat := range stats {
		rmsValues[i] = stat.rms
	}
	noiseFloor := percentileSorted(rmsValues, 0.20)
	speechFloor := math.Max(6, noiseFloor*1.2)
	var speechRMS, speechZCR, speechLow []float64
	for _, stat := range stats {
		if stat.rms <= speechFloor || stat.rms >= 30000 {
			continue
		}
		// Exclude broadband rustle/hiss; keep the user's unusual voiced
		// frames even if they do not match the generic gate.
		if stat.zcr > 0.60 && stat.low < 0.20 {
			continue
		}
		speechRMS = append(speechRMS, stat.rms)
		speechZCR = append(speechZCR, stat.zcr)
		speechLow = append(speechLow, stat.low)
	}
	if len(speechRMS) < 10 {
		return nil
	}
	slices.Sort(speechRMS)
	slices.Sort(speechZCR)
	slices.Sort(speechLow)
	zcrMin := percentileSorted(speechZCR, 0.05) - 0.06
	zcrMax := percentileSorted(speechZCR, 0.95) + 0.06
	lowMin := percentileSorted(speechLow, 0.05) - 0.10
	lowMax := percentileSorted(speechLow, 0.95) + 0.10
	zcrMin = math.Max(0, zcrMin)
	zcrMax = math.Min(1, zcrMax)
	lowMin = math.Max(0, lowMin)
	lowMax = math.Min(1, lowMax)
	zcrMean, zcrStd := meanStd(speechZCR)
	lowMean, lowStd := meanStd(speechLow)
	return &voiceProfile{
		NoiseFloor: noiseFloor,
		SpeechRMS:  percentileSorted(speechRMS, 0.50),
		QuietRMS:   percentileSorted(speechRMS, 0.15),
		ZCRMin:     zcrMin,
		ZCRMax:     zcrMax,
		ZCRMean:    zcrMean,
		ZCRStd:     zcrStd,
		LowMin:     lowMin,
		LowMax:     lowMax,
		LowMean:    lowMean,
		LowStd:     lowStd,
		Frames:     len(speechRMS),
		Created:    time.Now().Format(time.RFC3339),
	}
}

// calibrateVoice records from the persistent capture worker for the requested
// number of seconds. The raw frames are kept only in memory; only the profile
// is written to disk.
func calibrateVoice(seconds int) (M, error) {
	if seconds < 4 {
		seconds = 4
	}
	if seconds > 20 {
		seconds = 20
	}
	if !globalCaptureWorker.isRunning() {
		return nil, errors.New("native capture worker is not running")
	}
	sub, err := globalCaptureWorker.subscribe(0)
	if err != nil {
		return nil, err
	}
	defer sub.close()
	var frames [][]byte
	var dc microphoneDCFilter
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(deadline) {
		frame, _, err := sub.read()
		if err != nil {
			return nil, err
		}
		clean := append([]byte(nil), frame...)
		dc.apply(clean)
		frames = append(frames, clean)
	}
	profile := analyzeVoiceFrames(frames)
	if profile == nil {
		return nil, errors.New("not enough voice in the sample; speak louder or longer")
	}
	profile.Seconds = float64(seconds)
	if err := saveVoiceProfile(profile); err != nil {
		return nil, err
	}
	setCurrentVoiceProfile(profile)
	return M{
		"ok":          true,
		"seconds":     seconds,
		"frames":      profile.Frames,
		"noise_floor": math.Round(profile.NoiseFloor*10) / 10,
		"speech_rms":  math.Round(profile.SpeechRMS*10) / 10,
		"quiet_rms":   math.Round(profile.QuietRMS*10) / 10,
		"zcr":         []float64{math.Round(profile.ZCRMin*100) / 100, math.Round(profile.ZCRMax*100) / 100},
		"low_band":    []float64{math.Round(profile.LowMin*100) / 100, math.Round(profile.LowMax*100) / 100},
		"instruction": "Профиль сохранён. Скажи обычную команду для проверки.",
	}, nil
}

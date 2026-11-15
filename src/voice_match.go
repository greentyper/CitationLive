package main

// Local voice-shape gate for sensitive ecosystem actions.
//
// This is not cryptographic speaker verification and it does not reuse the
// stock encrypted Voice Match models. It scores the last confirmed turn
// against the calibrated voice profile and requires a minimum number of
// matching voiced frames. Sensitive tools (find phone, Google reminders,
// calendar events) fail closed when the score is insufficient.

import (
	"log"
	"math"
	"sync"
)

type voiceMatchSnapshot struct {
	Matched int     `json:"matched"`
	Total   int     `json:"total"`
	Score   float64 `json:"score"`
	OK      bool    `json:"ok"`
}

var voiceMatchState struct {
	sync.Mutex
	snapshot voiceMatchSnapshot
}

func resetVoiceMatchTurn() {
	voiceMatchState.Lock()
	voiceMatchState.snapshot = voiceMatchSnapshot{}
	voiceMatchState.Unlock()
}

func observeVoiceMatchFrame(rms, zcr, lowRatio float64) {
	profile := currentVoiceProfile()
	if profile == nil || profile.Frames < 20 {
		return
	}
	minLow := math.Max(0.45, profile.LowMean-0.10)
	maxZCR := math.Min(0.32, profile.ZCRMean+0.10)
	minRMS := math.Max(profile.QuietRMS, profile.NoiseFloor*1.5)
	if voiceProfileDegenerate(profile) {
		// The deployed calibration has a very broad centroid. Keep a usable
		// local gate instead of rejecting every quiet phrase; the score is
		// still reported and sensitive tools require a sustained match.
		minLow = math.Max(0.35, profile.LowMean-0.20)
		maxZCR = math.Min(0.45, profile.ZCRMean+0.20)
		minRMS = math.Max(6.0, profile.QuietRMS*0.8)
	}
	voiceMatchState.Lock()
	voiceMatchState.snapshot.Total++
	if rms >= minRMS && lowRatio >= minLow && zcr <= maxZCR {
		voiceMatchState.snapshot.Matched++
	}
	voiceMatchState.Unlock()
}

func finishVoiceMatchTurn() {
	voiceMatchState.Lock()
	defer voiceMatchState.Unlock()
	s := &voiceMatchState.snapshot
	if s.Total == 0 {
		s.Score, s.OK = 0, false
		return
	}
	s.Score = float64(s.Matched) / float64(s.Total)
	if s.Total < 6 {
		// Short answers such as «да», «нет», «ano» or «nie» can contain only
		// three voiced frames. Requiring four rejected the owner on a valid
		// 3/5 match; keep the score requirement and allow those short turns.
		s.OK = s.Matched >= 3 && s.Score >= 0.60
	} else {
		s.OK = s.Matched >= 4 && s.Score >= 0.55
	}
	log.Printf("voice match: matched=%d total=%d score=%.2f ok=%v", s.Matched, s.Total, s.Score, s.OK)
}

func voiceMatchOK() bool {
	voiceMatchState.Lock()
	defer voiceMatchState.Unlock()
	return voiceMatchState.snapshot.OK
}

func voiceMatchStatus() voiceMatchSnapshot {
	voiceMatchState.Lock()
	defer voiceMatchState.Unlock()
	return voiceMatchState.snapshot
}

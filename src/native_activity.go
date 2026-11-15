package main

import (
	"encoding/binary"
	"math"
	"slices"
	"sync/atomic"
	"time"
)

// followupGateUntil makes the local VAD more sensitive while a follow-up
// window is open. Follow-up phrases are often shorter and quieter than the
// initial command; the normal room-floor threshold rejected them.
var followupGateUntil atomic.Int64

func markFollowupGate(d time.Duration) {
	followupGateUntil.Store(time.Now().Add(d).UnixNano())
}

func clearFollowupGate() {
	followupGateUntil.Store(0)
}

func followupGateActive() bool {
	return time.Now().UnixNano() < followupGateUntil.Load()
}

// Detect turn boundaries on already processed native audio. This does not
// replace AEC/NS: it prevents cloud VAD from splitting wake word + quiet command
// and, equally important, it keeps room noise from opening or extending a turn.
type nativeActivityGate struct {
	active, started    bool
	waitingForCommand  bool
	allowStart         bool
	frames, lastSpeech int
	minEnd             int
	consecutive        int
	lastPhase          string
	background         []float64
	threshold          float64
	noiseFloor         float64
	profile            *voiceProfile
	duckedLevels       [20]float64
	duckedCount        int
	lastRMS            float64
	lastZCR            float64
	lastLow            float64
}
type nativeActivityEvent struct{ start, end, audio, speech bool }

// 50ms live chunks, with the remaining tail flushed before activityEnd.
// Explicit activity boundaries let setup/pre-roll backlog catch up immediately.
type nativeAudioBatch struct{ pcm []byte }

func (b *nativeAudioBatch) append(pcm []byte, finish bool) []byte {
	b.pcm = append(b.pcm, pcm...)
	if len(b.pcm) < 1600 && !finish {
		return nil
	}
	out := b.pcm
	b.pcm = nil
	return out
}

func newNativeActivityGate() *nativeActivityGate {
	g := &nativeActivityGate{minEnd: 180, threshold: 6.5, noiseFloor: 4, allowStart: true}
	// Firmware owns microphone processing. A saved speaker/distance profile
	// must not reject another speaker or the same speaker at a new distance.
	return g
}

// markWakeBoundary resets the gate at the end of the captured wake-word
// pre-roll. Audio before this point is only pre-roll: it must never open an
// activity on its own. This is what kept "Hey Google" (and the wake cue) from
// being uploaded as the user's request.
// forceStart opens a turn from the captured pre-roll. A quick command can be
// entirely inside the pre-roll and finish before the live capture loop starts;
// waiting for new live speech loses it. The transcript guard still decides
// whether the resulting turn is a real request or just the wake word.
func (g *nativeActivityGate) forceStart() {
	g.active = true
	g.started = true
	g.lastSpeech = g.frames
	g.minEnd = g.frames + 20
	g.waitingForCommand = false
	g.consecutive = 0
	resetVoiceMatchTurn()
}

// voiceLikeAt accepts the generic voice gate, plus a calibrated speaker's
// measured ZCR/low-band range. The calibrated path is what keeps a quiet or
// unusual voice from being rejected as noise.
// capQuietThreshold keeps a noisy room from raising the live gate above the
// calibrated quiet-speech level. The room floor still prevents the gate from
// dropping to pure noise; this only removes the extra headroom that made a
// quiet command inaudible.
func (g *nativeActivityGate) capQuietThreshold() {
	if g.profile == nil || g.profile.QuietRMS <= 0 {
		return
	}
	ceiling := math.Max(6, g.profile.QuietRMS*0.85)
	if g.threshold > ceiling {
		g.threshold = ceiling
	}
}

func (g *nativeActivityGate) voiceLikeAt(rms, zcr, lowRatio, threshold float64) bool {
	if rms <= threshold {
		return false
	}
	if profile := g.profile; profile != nil && profile.Frames >= 20 {
		if voiceProfileDegenerate(profile) {
			// The calibration is too broad to discriminate (the deployed
			// profile has LowStd=0.269 and ZCRStd=0.160, which made the old
			// low-band window [0,1]). Fall back to the generic shape gate, but
			// derive the floor from both the room noise and the user's own
			// quiet speech. A fixed noise-floor multiple can sit above
			// medium-quiet speech and keep the gate shut; the quiet-speech
			// term keeps it reachable without dropping to the old ~6.0 floor.
			floor := math.Max(6, profile.NoiseFloor*1.5)
			if profile.QuietRMS > 0 {
				floor = math.Min(floor, profile.QuietRMS*0.85)
			}
			floor = math.Max(6, math.Min(20, floor))
			// The calibrated quiet-speech floor is a ceiling for the live
			// threshold, not a minimum. A noisy pre-roll must not push the
			// gate above the user's own quiet voice.
			if floor < threshold {
				threshold = floor
			}
			return isVoiceLike(rms, zcr, lowRatio, threshold)
		}
		if profile.ZCRStd > 0 || profile.LowStd > 0 {
			// Clamped speaker centroid. An unclamped 2*std window covered
			// most of [0,1] after a varied calibration, so require absolute
			// low-band evidence as well as a bounded ZCR.
			minLow := math.Max(0.30, profile.LowMean-0.18)
			maxZCR := math.Min(0.45, profile.ZCRMean+0.18)
			if lowRatio >= minLow && zcr <= maxZCR {
				return true
			}
		} else if zcr >= profile.ZCRMin && zcr <= profile.ZCRMax && lowRatio >= profile.LowMin && lowRatio <= profile.LowMax {
			return true
		}
		// A very loud voiced frame may fall outside a quiet calibration.
		return rms >= threshold*4 && zcr <= 0.50 && lowRatio >= 0.20
	}
	return isVoiceLike(rms, zcr, lowRatio, threshold)
}

// lastFeatures returns the features of the most recently observed frame. The
// gate is only used by the mic goroutine; the diagnostics read the values in
// that same goroutine, so no lock is needed.
func (g *nativeActivityGate) lastFeatures() (rms, zcr, low float64) {
	return g.lastRMS, g.lastZCR, g.lastLow
}

// preRollHasVoice reports whether the wake pre-roll contains enough voice-like
// audio to be worth submitting even if the user has already stopped speaking.
// A short keyboard burst (30-60 ms) can look voice-like per frame; requiring a
// sustained run keeps a single transient from qualifying the pre-roll.
func (g *nativeActivityGate) preRollHasVoice(frames [][]byte) bool {
	voice := 0
	for _, frame := range frames {
		rms, zcr, lowRatio := nativeFrameVoiceFeatures(frame)
		if g.voiceLikeAt(rms, zcr, lowRatio, 6) {
			voice++
		}
	}
	return voice >= 16
}

func (g *nativeActivityGate) markWakeBoundary() {
	g.active = false
	g.started = false
	g.lastSpeech = 0
	g.consecutive = 0
	g.waitingForCommand = true
	g.minEnd = g.frames + 150
	g.allowStart = true
	resetVoiceMatchTurn()
}

func nativeFrameACRMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum, sum2 float64
	for i := 0; i+1 < len(pcm); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		sum += v
		sum2 += v * v
	}
	mean := sum / float64(n)
	return math.Sqrt(math.Max(0, sum2/float64(n)-mean*mean))
}

// nativeFrameVoiceFeatures returns AC RMS plus two cheap voice/noise
// discriminators. Voiced human speech has most of its energy below ~1 kHz and
// a low zero-crossing rate; hiss, rustling, clicks and broadband room noise do
// not. The low-pass uses a one-pole filter at roughly 700 Hz for 16 kHz PCM.
func nativeFrameVoiceFeatures(pcm []byte) (rms, zcr, lowRatio float64) {
	n := len(pcm) / 2
	if n < 2 {
		return 0, 0, 0
	}
	var sum, sum2, lowEnergy, totalEnergy, low, previous float64
	crossings := 0
	const alpha = 0.25
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		sum += v
		sum2 += v * v
		low += alpha * (v - low)
		lowEnergy += low * low
		totalEnergy += v * v
		if i > 0 && (v >= 0) != (previous >= 0) {
			crossings++
		}
		previous = v
	}
	mean := sum / float64(n)
	rms = math.Sqrt(math.Max(0, sum2/float64(n)-mean*mean))
	zcr = float64(crossings) / float64(n-1)
	if totalEnergy > 0 {
		lowRatio = lowEnergy / totalEnergy
	}
	return
}

// isVoiceLike is deliberately conservative. A 100 ms burst of energy is not a
// human turn unless it also looks like voice. Loud frames get a slightly wider
// gate so a clipped syllable is not lost.
func isVoiceLike(rms, zcr, lowRatio, threshold float64) bool {
	if rms <= threshold {
		return false
	}
	if zcr <= 0.42 && lowRatio >= 0.28 {
		return true
	}
	return rms >= threshold*3 && zcr <= 0.55 && lowRatio >= 0.18
}

func (g *nativeActivityGate) warm(pcm []byte) {
	g.background = append(g.background, nativeFrameACRMS(pcm))
}

// Recalibrate down when the mixer has attenuated the pre-wake background.
// Never raise the threshold from live audio, which may contain the command.
func (g *nativeActivityGate) observeDuckedBackground(pcm []byte) {
	rms := nativeFrameACRMS(pcm)
	// Never learn speech, the wake cue or a transient as background. Only
	// lower the threshold; live audio must not make the gate deaf.
	if rms > g.threshold*1.5 {
		g.duckedCount = 0
		return
	}
	g.duckedLevels[g.duckedCount] = rms
	g.duckedCount++
	if g.duckedCount == len(g.duckedLevels) {
		slices.Sort(g.duckedLevels[:])
		duckedMedian := g.duckedLevels[9]
		target := math.Max(6, duckedMedian*1.3)
		// Never calibrate below the learned room floor: a quiet ducked
		// passage is not proof that the room itself got quieter.
		floor := math.Max(6, g.noiseFloor*1.5)
		if target < floor {
			target = floor
		}
		if target > 20 {
			target = 20
		}
		if target < g.threshold {
			g.threshold = target
			g.noiseFloor = math.Max(3, duckedMedian)
		}
		g.duckedCount = 0
	}
}

// updateNoiseFloor learns the room while there is no turn. It ignores loud
// transients so a knock or a cough cannot desensitise the detector for the
// rest of the session, but it does learn sustained background (a fan, typing,
// traffic) up to four times the current threshold.
func (g *nativeActivityGate) updateNoiseFloor(rms float64, speech bool) {
	if g.active || speech || rms > g.threshold*4 {
		return
	}
	g.noiseFloor = 0.995*g.noiseFloor + 0.005*rms
	if g.noiseFloor < 3 {
		g.noiseFloor = 3
	}
	target := math.Max(6.5, g.noiseFloor*1.5)
	if target > 20 {
		target = 20
	}
	g.threshold = 0.9*g.threshold + 0.1*target
	if g.threshold < 6 {
		g.threshold = 6
	}
	g.capQuietThreshold()
}

func (g *nativeActivityGate) observe(pcm []byte, phase string) nativeActivityEvent {
	if len(g.background) > 0 {
		// Use the pre-wake noise distribution. A brief peak does not set the
		// threshold, and live speech cannot be learnt as background mid-turn.
		slices.Sort(g.background)
		background := g.background[(len(g.background)-1)*4/5]
		g.noiseFloor = math.Max(3, background)
		g.threshold = math.Max(6, background*1.3)
		if g.threshold > 20 {
			g.threshold = 20
		}
		g.capQuietThreshold()
		g.background = nil
	}
	g.frames++
	if phase != g.lastPhase {
		g.lastPhase = phase
		g.consecutive = 0
		if phase != "listening" {
			ended := g.active
			g.active = false
			g.started = false
			if ended {
				finishVoiceMatchTurn()
				// Never leave the cloud with an unmatched activityStart.
				return nativeActivityEvent{end: true}
			}
		}
	}
	rms, zcr, lowRatio := nativeFrameVoiceFeatures(pcm)
	g.lastRMS, g.lastZCR, g.lastLow = rms, zcr, lowRatio
	speechThreshold := g.threshold
	followup := followupGateActive()
	if followup {
		speechThreshold = math.Min(speechThreshold, 5.0)
	}
	speech := g.voiceLikeAt(rms, zcr, lowRatio, speechThreshold)
	if g.active {
		e := nativeActivityEvent{audio: true, speech: speech}
		if !g.started {
			g.started = true
			e.start = true
		}
		if speech {
			observeVoiceMatchFrame(rms, zcr, lowRatio)
			g.consecutive++
			// A three-frame (30 ms) burst is a key click, not speech. Only a
			// sustained voiced run may extend the turn.
			if g.consecutive >= 7 {
				g.lastSpeech = g.frames
				g.waitingForCommand = false
			}
		} else {
			g.consecutive = 0
		}
		speechEnded := !g.waitingForCommand && g.frames >= g.minEnd && g.frames-g.lastSpeech >= 70
		if speechEnded {
			g.active = false
			g.started = false
			g.consecutive = 0
			finishVoiceMatchTurn()
			e.end = true
		}
		return e
	}
	if phase != "listening" || !g.allowStart {
		g.consecutive = 0
		return nativeActivityEvent{}
	}
	g.updateNoiseFloor(rms, speech)
	if !speech {
		g.consecutive = 0
		return nativeActivityEvent{}
	}
	g.consecutive++
	// Require a sustained onset (about 110 ms). A 30-60 ms keyboard burst can
	// pass the per-frame voice test but must not open a turn. Only very loud
	// frames, which a key click rarely produces for long, get a shorter run.
	required := 11
	if followup {
		required = 6
	}
	if rms >= g.threshold*4 {
		required = 8
	}
	if g.consecutive < required {
		return nativeActivityEvent{}
	}
	g.active = true
	g.started = true
	g.lastSpeech = g.frames
	g.minEnd = g.frames + 20
	g.waitingForCommand = false
	g.consecutive = 0
	resetVoiceMatchTurn()
	observeVoiceMatchFrame(rms, zcr, lowRatio)
	return nativeActivityEvent{start: true, audio: true, speech: true}
}

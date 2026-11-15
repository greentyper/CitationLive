package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func TestNativeAudioBatchPreservesSamplesAndFlushesTail(t *testing.T) {
	var batch nativeAudioBatch
	var input, output []byte
	for i := 0; i < 7; i++ {
		frame := bytes.Repeat([]byte{byte(i)}, 320)
		input = append(input, frame...)
		packet := batch.append(frame, i == 6)
		if i < 4 && len(packet) > 0 {
			t.Fatal("live chunk emitted too early")
		}
		if i == 4 && len(packet) != 1600 {
			t.Fatal("50ms chunk missing")
		}
		if i == 6 && len(packet) != 640 {
			t.Fatal("final audio tail was not flushed")
		}
		output = append(output, packet...)
	}
	if !bytes.Equal(input, output) {
		t.Fatal("audio samples changed or reordered across chunks")
	}
	if len(batch.pcm) != 0 {
		t.Fatal("audio left behind activityEnd")
	}
}

func voiceFixture(level int) []byte {
	b := make([]byte, 320)
	amplitude := float64(level) * math.Sqrt2
	for i := 0; i < 160; i++ {
		v := int16(math.Round(amplitude * math.Sin(2*math.Pi*200*float64(i)/16000)))
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func activityFixture(level int) []byte { return voiceFixture(level) }

func noiseFixture(level int) []byte {
	b := make([]byte, 320)
	for i := 0; i < 160; i++ {
		v := level
		if i%2 == 0 {
			v = -level
		}
		binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(v)))
	}
	return b
}

func keyboardTransientFixture(level float64) []byte {
	b := make([]byte, 320)
	const lowAmp = 0.74
	scale := level / math.Sqrt((lowAmp*lowAmp+1)/2)
	for i := 0; i < 160; i++ {
		t := float64(i) / 16000
		v := scale * (lowAmp*math.Sin(2*math.Pi*300*t) + math.Sin(2*math.Pi*2800*t))
		binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(math.Round(v))))
	}
	return b
}

func TestPreRollVoiceDetection(t *testing.T) {
	var voice, noise [][]byte
	for i := 0; i < 20; i++ {
		voice = append(voice, voiceFixture(20))
		noise = append(noise, noiseFixture(80))
	}
	gate := newNativeActivityGate()
	if !gate.preRollHasVoice(voice) {
		t.Fatal("quiet voice pre-roll was not detected")
	}
	if gate.preRollHasVoice(noise) {
		t.Fatal("rustle pre-roll was treated as voice")
	}
	if gate.preRollHasVoice(nil) {
		t.Fatal("empty pre-roll was treated as voice")
	}
}

func TestNativeActivityStartsInactive(t *testing.T) {
	g := newNativeActivityGate()
	if g.active {
		t.Fatal("gate must not stream before the first confirmed speech")
	}
	for i := 0; i < 100; i++ {
		if g.observe(voiceFixture(0), "listening").start {
			t.Fatal("silence opened a turn")
		}
	}
}

func TestNativeActivityKeepsImmediateQuietCommand(t *testing.T) {
	g := newNativeActivityGate()
	starts, ends := 0, 0
	for i := 0; i < 260; i++ {
		level := 0
		if i < 45 {
			level = 1000
		}
		if i >= 75 && i < 125 {
			level = 20
		}
		e := g.observe(activityFixture(level), "listening")
		if e.start {
			starts++
		}
		if e.end {
			ends++
			if i < 179 {
				t.Fatalf("quiet command cut at frame %d", i)
			}
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("starts=%d ends=%d, want one complete activity", starts, ends)
	}
}

func TestNativeActivityLongSpeechEndsAfter700ms(t *testing.T) {
	g := newNativeActivityGate()
	endAt := -1
	for i := 0; i < 340; i++ {
		level := 0
		if i < 250 {
			level = 100
		}
		if g.observe(activityFixture(level), "listening").end {
			endAt = i
			break
		}
	}
	if endAt != 319 {
		t.Fatalf("speech endpoint frame=%d, want 319", endAt)
	}
}

func TestNativeActivityIgnoresDCAndPlaybackEcho(t *testing.T) {
	g := newNativeActivityGate()
	for i := 0; i < 180; i++ {
		g.observe(activityFixture(0), "listening")
	}
	if g.active {
		t.Fatal("silence kept an activity open")
	}
	for i := 0; i < 200; i++ {
		if g.observe(activityFixture(1000), "speaking").start {
			t.Fatal("playback echo opened an activity")
		}
	}
	for i := 0; i < 10; i++ {
		if g.observe(activityFixture(20), "listening").start {
			t.Fatalf("frame %d: short burst opened a follow-up too early", i)
		}
	}
	if !g.observe(activityFixture(20), "listening").start {
		t.Fatal("sustained near-end speech was ignored")
	}
}

func TestNativeActivityIgnoresRustleBurst(t *testing.T) {
	g := newNativeActivityGate()
	for i := 0; i < 12; i++ {
		if g.observe(noiseFixture(60), "listening").start {
			t.Fatalf("frame %d: rustle opened a turn", i)
		}
	}
	starts := 0
	for i := 0; i < 14; i++ {
		if g.observe(voiceFixture(60), "listening").start {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("sustained voice starts=%d, want one", starts)
	}
}

func TestNativeActivityEndsWithMusicStillAudible(t *testing.T) {
	g := newNativeActivityGate()
	for i := 0; i < 120; i++ {
		g.warm(noiseFixture(80 + i%41))
	}
	for i := 0; i < 680; i++ {
		var pcm []byte
		if i < 550 {
			pcm = voiceFixture(260)
		} else {
			level := 80 + i%41
			if i%9 == 0 {
				level += 300 // isolated beat in the music
			}
			pcm = noiseFixture(level)
		}
		event := g.observe(pcm, "listening")
		if event.end != (i == 619) {
			t.Fatalf("frame %d: endpoint=%v, want endpoint at 619", i, event.end)
		}
		if i > 619 && event.start {
			t.Fatal("music reopened the turn")
		}
	}
}

func TestNativeActivityRecalibratesAfterDucking(t *testing.T) {
	g := newNativeActivityGate()
	for i := 0; i < 120; i++ {
		g.warm(noiseFixture(260))
	}
	for i := 0; i < 340; i++ {
		var pcm []byte
		switch {
		case i >= 260:
			pcm = noiseFixture(28)
		case i >= 135:
			pcm = voiceFixture(120)
		case i >= 110:
			pcm = noiseFixture(28)
		default:
			pcm = voiceFixture(260)
		}
		if i >= 110 {
			g.observeDuckedBackground(noiseFixture(28))
		}
		if got := g.observe(pcm, "listening").end; got != (i == 329) {
			t.Fatalf("frame %d: endpoint=%v, want endpoint at 329", i, got)
		}
	}
}

func TestNativeActivityWaitsForCommandAfterQuietWake(t *testing.T) {
	g := newNativeActivityGate()
	g.markWakeBoundary()
	for i := 0; i < 250; i++ {
		if e := g.observe(voiceFixture(0), "listening"); e.start || e.end {
			t.Fatalf("quiet wake opened/closed a turn at frame %d", i)
		}
	}
	started := false
	for i := 0; i < 100; i++ {
		if g.observe(voiceFixture(90), "listening").start {
			started = true
		}
	}
	if !started {
		t.Fatal("command after quiet wake was not detected")
	}
	endAt := -1
	for i := 0; i < 100; i++ {
		if g.observe(voiceFixture(0), "listening").end {
			endAt = i
			break
		}
	}
	if endAt < 69 || endAt > 99 {
		t.Fatalf("endpoint at silence frame %d, want >=69", endAt)
	}
}

func TestNativeActivityIndefiniteSilenceAfterWake(t *testing.T) {
	g := newNativeActivityGate()
	g.markWakeBoundary()
	for i := 0; i < 600; i++ {
		if e := g.observe(voiceFixture(0), "listening"); e.start || e.end {
			t.Fatalf("silent wake opened/closed a turn at frame %d", i)
		}
	}
}

func TestNativeActivityIgnoresAmbientRoomNoise(t *testing.T) {
	g := newNativeActivityGate()
	for i := 0; i < 300; i++ {
		level := 5
		if i%17 == 0 {
			level = 20
		}
		if e := g.observe(noiseFixture(level), "listening"); e.start {
			t.Fatalf("frame %d: ambient room noise opened a false turn", i)
		}
	}
	if !g.observe(voiceFixture(20), "listening").start {
		for i := 0; i < 12; i++ {
			if g.observe(voiceFixture(20), "listening").start {
				return
			}
		}
		t.Fatal("quiet voice after ambient noise was ignored")
	}
}

func TestNativeActivityPreRollWakeDoesNotCutFirstSecondOfCommand(t *testing.T) {
	g := newNativeActivityGate()
	for i := 0; i < 80; i++ {
		g.observe(voiceFixture(120), "listening")
	}
	g.markWakeBoundary()

	for i := 0; i < 70; i++ {
		if e := g.observe(voiceFixture(0), "listening"); e.end {
			t.Fatalf("frame %d: mic was cut off during post-wake pause", i)
		}
	}
	for i := 0; i < 100; i++ {
		if e := g.observe(voiceFixture(150), "listening"); e.end {
			t.Fatalf("frame %d: mic was cut off during command speech", i)
		}
	}
	endFired := false
	for i := 0; i < 100; i++ {
		if g.observe(voiceFixture(0), "listening").end {
			endFired = true
			if i < 69 {
				t.Fatalf("turn ended too early at silence frame %d", i)
			}
			break
		}
	}
	if !endFired {
		t.Fatal("turn never ended after command completed")
	}
}

func TestKeyboardTransientFixtureIsVoiceShaped(t *testing.T) {
	rms, zcr, low := nativeFrameVoiceFeatures(keyboardTransientFixture(40))
	if rms < 30 {
		t.Fatalf("transient RMS too low: %f", rms)
	}
	if zcr < 0.30 || zcr > 0.40 {
		t.Fatalf("transient ZCR=%f, want about 0.35", zcr)
	}
	if low < 0.30 || low > 0.40 {
		t.Fatalf("transient low-band ratio=%f, want about 0.35", low)
	}
	if !isVoiceLike(rms, zcr, low, 8) {
		t.Fatal("fixture must pass the per-frame voice shape gate to test hysteresis")
	}
}

func TestNativeActivityIgnoresKeyboardBursts(t *testing.T) {
	g := newNativeActivityGate()
	burst := keyboardTransientFixture(40)
	for frame := 0; frame < 500; frame++ { // 5 s at 10 ms per frame
		pcm := voiceFixture(0)
		if frame%20 < 4 { // 40 ms transient every 200 ms
			pcm = burst
		}
		if g.observe(pcm, "listening").start {
			t.Fatalf("keyboard burst at frame %d opened a turn", frame)
		}
	}
	if g.active {
		t.Fatal("keyboard bursts left an activity open")
	}
}

func TestNativeActivityStartsOn200msVoicedSignal(t *testing.T) {
	g := newNativeActivityGate()
	starts := 0
	for i := 0; i < 20; i++ {
		if g.observe(voiceFixture(20), "listening").start {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("starts=%d, want exactly one after 200 ms of voice", starts)
	}
}

func TestNativeActivityIgnores60msBurst(t *testing.T) {
	g := newNativeActivityGate()
	for i := 0; i < 6; i++ {
		if g.observe(voiceFixture(30), "listening").start {
			t.Fatalf("60 ms burst opened a turn at frame %d", i)
		}
	}
	if g.active {
		t.Fatal("60 ms burst left an activity open")
	}
}

func TestNativeActivitySustainedNoiseRaisesThreshold(t *testing.T) {
	g := newNativeActivityGate()
	initial := g.threshold
	for i := 0; i < 600; i++ {
		if g.observe(noiseFixture(14), "listening").start {
			t.Fatalf("sustained noise opened a turn at frame %d", i)
		}
	}
	if g.threshold <= initial {
		t.Fatalf("threshold did not rise: initial=%f final=%f", initial, g.threshold)
	}
	if g.threshold > 20 {
		t.Fatalf("threshold exceeded cap: %f", g.threshold)
	}
}

func TestNativeActivityStartsOnQuietSpeech(t *testing.T) {
	g := newNativeActivityGate()
	for i := 0; i < 11; i++ {
		if g.observe(voiceFixture(12), "listening").start {
			return
		}
	}
	t.Fatal("quiet speech did not start a turn")
}

func TestNativeActivityIgnoresShortBurstsWhileActive(t *testing.T) {
	g := newNativeActivityGate()
	for i := 0; i < 20; i++ {
		g.observe(voiceFixture(20), "listening")
	}
	if !g.active {
		t.Fatal("speech did not start a turn")
	}
	endAt := -1
	for i := 20; i < 160; i++ {
		pcm := voiceFixture(0)
		if (i >= 30 && i < 33) || (i >= 50 && i < 53) || (i >= 70 && i < 73) {
			pcm = voiceFixture(20)
		}
		if g.observe(pcm, "listening").end {
			endAt = i
			break
		}
	}
	if endAt != 89 {
		t.Fatalf("endpoint frame=%d, want 89: 30 ms bursts must not refresh lastSpeech", endAt)
	}
}

func deployedVoiceProfile() *voiceProfile {
	return &voiceProfile{
		NoiseFloor: 5.17,
		SpeechRMS:  48.23,
		QuietRMS:   8.19,
		ZCRMin:     0.13,
		ZCRMax:     0.36,
		ZCRMean:    0.195,
		ZCRStd:     0.160,
		LowMin:     0.05,
		LowMax:     0.88,
		LowMean:    0.579,
		LowStd:     0.269,
		Frames:     240,
	}
}

func TestNativeActivityDeployedProfileStartsOnMediumQuietSpeech(t *testing.T) {
	previous := currentVoiceProfile()
	defer setCurrentVoiceProfile(previous)
	setCurrentVoiceProfile(deployedVoiceProfile())

	g := newNativeActivityGate()
	started := false
	for i := 0; i < 15; i++ {
		if g.observe(voiceFixture(7), "listening").start {
			started = true
		}
	}
	if !started {
		t.Fatalf("7-RMS medium-quiet speech did not start a turn (threshold=%.3f)", g.threshold)
	}
}

func TestNativeActivityDeployedProfileIgnoresKeyboardBursts(t *testing.T) {
	previous := currentVoiceProfile()
	defer setCurrentVoiceProfile(previous)
	setCurrentVoiceProfile(deployedVoiceProfile())

	g := newNativeActivityGate()
	burst := keyboardTransientFixture(40)
	for frame := 0; frame < 500; frame++ {
		pcm := voiceFixture(0)
		if frame%20 < 4 { // 40 ms transient every 200 ms
			pcm = burst
		}
		if g.observe(pcm, "listening").start {
			t.Fatalf("keyboard burst at frame %d opened a turn", frame)
		}
	}
	if g.active {
		t.Fatal("keyboard bursts left an activity open")
	}
}

func TestNativeActivityDeployedProfileIgnoresSixRMSRoomNoise(t *testing.T) {
	previous := currentVoiceProfile()
	defer setCurrentVoiceProfile(previous)
	setCurrentVoiceProfile(deployedVoiceProfile())

	g := newNativeActivityGate()
	for i := 0; i < 300; i++ {
		if g.observe(noiseFixture(6), "listening").start {
			t.Fatalf("frame %d: 6-RMS room noise opened a turn", i)
		}
	}
	for i := 0; i < 20; i++ {
		if g.observe(voiceFixture(6), "listening").start {
			t.Fatalf("frame %d: 6-RMS voiced tone crossed the calibrated floor", i)
		}
	}
}

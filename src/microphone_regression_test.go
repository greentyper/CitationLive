package main

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestMicrophonePreRollAndConnectionBacklogAreContiguous(t *testing.T) {
	old := globalCaptureWorker
	defer func() { globalCaptureWorker = old }()
	w := &persistentCaptureWorker{ring: NewAudioRingBuffer(400), subscribers: make(map[uint64]chan captureFrame), running: true, connected: true}
	globalCaptureWorker = w
	now := monotonicMicros()
	for i := 0; i < 300; i++ {
		frame := make([]byte, 320)
		binary.LittleEndian.PutUint16(frame, uint16(i))
		w.ring.WriteFrame(frame, now-int64(300-i)*10000)
	}
	capture, err := openBufferedNativeCapture()
	if err != nil {
		t.Fatal(err)
	}
	defer capture.close()
	if capture.wakeBoundaryUs < now || capture.wakeBoundaryUs-capture.uploadAfterUs < 2500000 {
		t.Fatal("wake boundary incorrectly derived from pre-roll length")
	}
	for i := 300; i < 800; i++ {
		frame := make([]byte, 320)
		binary.LittleEndian.PutUint16(frame, uint16(i))
		for _, sub := range w.subscribers {
			sub <- captureFrame{data: frame, timestamp: now + int64(i-300)*10000}
		}
	}
	for i := 0; i < 800; i++ {
		frame, _, err := capture.read()
		if err != nil || int(binary.LittleEndian.Uint16(frame)) != i {
			t.Fatalf("missing/reordered frame %d: %v", i, err)
		}
	}
}

func TestMicrophoneFollowUpUsesMonotonicMicroseconds(t *testing.T) {
	const nowUs int64 = 987654321000
	if got := followUpDelay(nowUs, nowUs+300000); got != 400*time.Millisecond {
		t.Fatalf("guard delay = %s, want 400ms", got)
	}
	if got := followUpDelay(nowUs, nowUs+2000000); got < 2*time.Second {
		t.Fatalf("long playback guard truncated: %s", got)
	}
}

func TestMicrophoneDiagnosticRetainsBeginningOfLongUtterance(t *testing.T) {
	resetVoiceQueryBuffer()
	defer resetVoiceQueryBuffer()
	first := voiceFixture(200)
	appendVoiceQueryFrame(first)
	for i := 0; i < 3000; i++ {
		appendVoiceQueryFrame(voiceFixture(0))
	}
	pcm := takeVoiceQueryPCM()
	if len(pcm) != 3001*320 || !bytes.Equal(pcm[:320], first) {
		t.Fatal("beginning of long command was overwritten")
	}
}

func TestMicrophoneLongCommandNotCutAtFifteenSeconds(t *testing.T) {
	g := newNativeActivityGate()
	for i := 0; i < 4500; i++ {
		if g.observe(voiceFixture(100), "listening").end {
			t.Fatalf("continuous command cut at %d ms", i*10)
		}
	}
	ended := false
	for i := 0; i < 150; i++ {
		ended = g.observe(voiceFixture(0), "listening").end || ended
	}
	if !ended {
		t.Fatal("command did not end after silence")
	}
}

func TestMicrophoneCalibrationCannotChangeCaptureGate(t *testing.T) {
	old := currentVoiceProfile()
	defer setCurrentVoiceProfile(old)
	setCurrentVoiceProfile(nil)
	baseline := newNativeActivityGate()
	setCurrentVoiceProfile(&voiceProfile{Frames: 100, NoiseFloor: 100, QuietRMS: 500, SpeechRMS: 1000})
	calibrated := newNativeActivityGate()
	if calibrated.profile != nil || calibrated.threshold != baseline.threshold {
		t.Fatal("saved speaker calibration still controls capture")
	}
}

func TestMicrophoneProviderHallucinationDoesNotAuthorizeMusic(t *testing.T) {
	for _, text := range []string{"Spotify", "спотифай"} {
		if spokenMusicIntent(text) || literalMusicFields(M{}, text)["validation_error"] == nil {
			t.Fatalf("service name %q authorized playback", text)
		}
	}
}

func TestMicrophoneSayRequestDoesNotAuthorizeBroadcast(t *testing.T) {
	for _, text := range []string{"Эй Google скажи тест", "повтори слово", "say hello"} {
		if spokenBroadcastIntent(text) {
			t.Fatalf("ordinary speech became broadcast: %q", text)
		}
	}
	for _, text := range []string{"broadcast dinner is ready", "скажи на весь дом ужин готов"} {
		if !spokenBroadcastIntent(text) {
			t.Fatalf("explicit broadcast rejected: %q", text)
		}
	}
}

package main

import (
	_ "embed"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// Embedded Gemini voice cues
//
//go:embed cues/aga.raw
var cueAga []byte

//go:embed cues/da.raw
var cueDa []byte

//go:embed cues/hm.raw
var cueHm []byte

//go:embed cues/m.raw
var cueM []byte

//go:embed cues/slushayu.raw
var cueSlushayu []byte

//go:embed cues/seichas.raw
var cueSeichas []byte

//go:embed cues/sekundu.raw
var cueSekundu []byte

//go:embed cues/wake_gemini.raw
var cueWakeGemini []byte

//go:embed cues/wake_dj.raw
var cueWakeDJ []byte

//go:embed cues/end_session.raw
var cueEndSession []byte

var (
	cuesList      = [][]byte{cueAga, cueDa, cueHm, cueM, cueSlushayu}
	cuesMu        sync.Mutex
	cuesRand      = rand.New(rand.NewSource(time.Now().UnixNano()))
	spotifyCueNow bool
)

func spotifyWaitCue() []byte {
	cuesMu.Lock()
	defer cuesMu.Unlock()
	playNow := spotifyCueNow
	spotifyCueNow = !spotifyCueNow
	if playNow {
		return cueSeichas
	}
	return cueSekundu
}

// Cue audio is played directly through the native mixer, outside session.play,
// so the capture loop needs a timestamped window for it. The window is kept
// after playback: buffered pre-roll is processed later and must still be
// recognised as our own sound, not as a user command.
var lastWakeSFXStartUs atomic.Int64
var lastWakeSFXEndUs atomic.Int64

// localCuePlaying is a real-time guard for the capture loop. Timestamp windows
// can drift against the native capture service PTS; this flag always covers the
// cue actually playing now, so the model cannot transcribe our own wake sound.
var localCuePlaying atomic.Bool

func resetWakeSFXTracking() {
	lastWakeSFXStartUs.Store(0)
	lastWakeSFXEndUs.Store(0)
}

func markLocalCueWindow(pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	nowUs := monotonicMicros()
	// 24 kHz mono S16, plus 350 ms for mixer/DAC/reverb tail.
	guardUs := int64(len(pcm)) * 1000000 / (24000 * 2)
	lastWakeSFXStartUs.Store(nowUs)
	lastWakeSFXEndUs.Store(nowUs + guardUs + 350000)
}

func playRandomCue() {
	cuesMu.Lock()
	if len(cuesList) == 0 {
		cuesMu.Unlock()
		return
	}
	idx := cuesRand.Intn(len(cuesList))
	pcm := cuesList[idx]
	cuesMu.Unlock()

	markLocalCueWindow(pcm)
	localCuePlaying.Store(true)
	defer func() {
		time.AfterFunc(300*time.Millisecond, func() { localCuePlaying.Store(false) })
	}()
	extendAssistantAudioGuard(time.Second)
	out, err := openMixerOutput()
	if err != nil {
		log.Println("cue player open:", clean(err))
		return
	}
	defer out.close()
	if err := out.write(pcm); err != nil {
		log.Println("cue player write:", clean(err))
		return
	}
	_ = out.finish()
}

func playWakeSFX(isDJ bool) {
	pcm := cueWakeGemini
	if isDJ {
		pcm = cueWakeDJ
	}
	if len(pcm) == 0 {
		return
	}
	// Do not mask the first words of a command that starts as soon as the LED
	// turns blue. The physical hotword already provides the main earcon.
	pcm = append([]byte(nil), pcm...)
	gainPCM16(pcm, 0.6)
	markLocalCueWindow(pcm)
	localCuePlaying.Store(true)
	defer func() {
		time.AfterFunc(300*time.Millisecond, func() { localCuePlaying.Store(false) })
	}()
	extendAssistantAudioGuard(time.Second)
	out, err := openMixerOutput()
	if err != nil {
		log.Println("wake sfx open:", clean(err))
		lastWakeSFXStartUs.Store(0)
		lastWakeSFXEndUs.Store(0)
		return
	}
	defer out.close()
	if err := out.write(pcm); err != nil {
		log.Println("wake sfx write:", clean(err))
	} else {
		_ = out.finish()
	}
}

func playEndSessionSFX() {
	pcm := cueEndSession
	if len(pcm) == 0 {
		return
	}
	time.Sleep(50 * time.Millisecond)
	out, err := openMixerOutput()
	if err != nil {
		log.Println("end session sfx open:", clean(err))
		return
	}
	defer out.close()
	if err := out.write(pcm); err != nil {
		log.Println("end session sfx write:", clean(err))
		return
	}
	_ = out.finish()
}

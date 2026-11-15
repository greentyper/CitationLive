package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The native AudioUI contract comes from the matching shipped binary:
// extStateUpdate positional group plus string-valued kwargs. Caller properties
// override compiled defaults; each update replaces, rather than merges, state.
// AudioUI owns both phase transitions and animation; do not race it with a
// second direct ledAnimate call for the same phase.
func nativePhaseProperties(phase string) M {
	state := map[string]string{"listening": "assistant-listening", "thinking": "assistant-thinking", "announcing_music": "assistant-thinking", "launching_music": "assistant-thinking", "speaking": "assistant-responding", "idle": "assistant-idle"}[phase]
	if state == "" {
		return nil
	}
	properties := M{"state": state, "animation": "", "transition": "", "sound": "", "citation-origin": "gemini"}
	if animation := map[string]string{"listening": "listening", "thinking": "thinking", "announcing_music": "thinking", "launching_music": "thinking", "speaking": "speaking"}[phase]; animation != "" {
		properties["animation"] = "../../../data/opt/citation-gemini/led/" + animation
	}
	if phase == "idle" {
		// Override audio-ui's voice-trigger long-press action while our daemon owns
		// the button so holding Action does not launch stock Assistant LEDs first.
		properties["btn-action-long"] = "noop"
	}
	// The physical hotword already played its native earcon. Mirroring the UI
	// must not replay it on cloud setup or after every answer/follow-up.
	return properties
}

func waitNativeWakeCancellation(ctx context.Context) {
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) && ctx.Err() == nil {
		state, e := wampCallTimeout(300*time.Millisecond, "com.harman.stateGet", M{})
		if e == nil && len(state) >= 5 {
			if groups, ok := state[4].(map[string]any); ok {
				if voice, ok := groups["voice"].(map[string]any); ok && voice["state"] != "assistant-listening" {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

var nativePhaseState struct {
	sync.Mutex
	state string
}

func reassertNativePhase(phase string) {
	if phase == "" || phase == "idle" {
		return
	}
	_, _ = wampCallTimeout(300*time.Millisecond, "com.harman.ledAnimate", M{}, "animation_close")
	nativePhaseState.Lock()
	nativePhaseState.state = ""
	nativePhaseState.Unlock()
	_ = applyNativePhase(phase)
}

func applyNativePhase(phase string) bool {
	if !cfg.NativeBehavior {
		return false
	}
	props := nativePhaseProperties(phase)
	if props == nil {
		return false
	}
	nativePhaseState.Lock()
	defer nativePhaseState.Unlock()
	if nativePhaseState.state == props["state"] && phase != "idle" {
		return true
	}
	if _, e := wampCallTimeout(500*time.Millisecond, "com.harman.extStateUpdate", props, "voice"); e != nil {
		log.Println("native UI state:", clean(e))
		return false
	} else {
		nativePhaseState.state = props["state"].(string)
	}
	return true
}

// nativeHoldStart marks a session opened by the touch-panel hold rather than a
// hardware wake word. The touch hold has no spoken command before the cue, so
// the wake-path pre-roll must not be force-submitted as an empty turn.
var nativeHoldStart atomic.Bool

var cancelledWakeUntil atomic.Int64
var assistantAudioUntil atomic.Int64
var googleVoiceGuardActive atomic.Bool

// stockHotwordGuard mirrors the 600 ms loopback rejection window recovered
// from libassistant's FUN_0042d444. It adds a short tail after our own TTS/cue
// so a native hotword detector that hears the speaker cannot immediately
// start a new interaction.
var stockHotwordGuard = newHotwordLoopbackGuard(600 * time.Millisecond)

// extendAssistantAudioGuard suppresses hotword starts while our own TTS/cue is
// in the mixer or still ringing in the room. Otherwise the native hotword
// detector can wake the speaker on its own voice and start a new session.
func extendAssistantAudioGuard(d time.Duration) {
	until := time.Now().Add(d).UnixNano()
	for {
		old := assistantAudioUntil.Load()
		if until <= old || assistantAudioUntil.CompareAndSwap(old, until) {
			return
		}
	}
}

func wakeSuppressed() bool {
	now := time.Now()
	if now.UnixNano() < assistantAudioUntil.Load() {
		stockHotwordGuard.noteLoopback(now, 0)
		return true
	}
	return stockHotwordGuard.decide(now, 0, true) == hotwordIgnoredLoopback
}

// Mute the original assistant's dedicated TTS device, not arbitrary phone-call
// communication streams. The native wake detector and Cast services stay live.
func guardGoogleVoice() {
	for {
		r, e := openTTSRedirectFor(false)
		if e == nil {
			googleVoiceGuardActive.Store(true)
			<-r.done
			googleVoiceGuardActive.Store(false)
			r.close()
		}
		time.Sleep(time.Second)
	}
}

func nativeInputButton(event []any) string {
	if len(event) < 5 {
		return ""
	}
	args, ok := event[4].([]any)
	if !ok || len(args) == 0 {
		return ""
	}
	button, _ := args[0].(string)
	return strings.TrimPrefix(button, "btn-")
}

func (c *controller) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active != nil || c.state != "idle"
}

func (c *controller) isListeningOrConnecting() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == "connecting" || c.state == "opening_audio" || c.state == "listening"
}

func stopNativeInteraction() {
	_ = grpcCall("StopInteraction", []byte{8, 1})
}

func stopNativeAssistant() {
	stopNativeInteraction()
	_ = grpcCall("StopAllTts", nil)
}

func cancelFromNativeButton() {
	cancelledWakeUntil.Store(time.Now().Add(1200 * time.Millisecond).UnixNano())
	ctl.stop()
	// Native AudioUI also dispatches gva.quickTap / gva.triggerAssistant on panel
	// presses. Reject its resulting Google wake at T=0 and after the async
	// SpeechRecognitionActivity launch so Google Assistant cannot stay open.
	go func() {
		stopNativeAssistant()
		time.Sleep(100 * time.Millisecond)
		if !ctl.busy() && time.Now().UnixNano() < cancelledWakeUntil.Load() {
			stopNativeAssistant()
		}
	}()
}

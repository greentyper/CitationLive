package main

// Physical touch-panel contract recovered from the shipped audio-ui/google-wamp
// binaries (SHA256 audio-ui c270e2c0...a67782, google-wamp 5d501f67...98d4a):
//
//   EVENT [36, subscriptionId, publicationId, {}, ["action"]]
//   EVENT [36, subscriptionId, publicationId, {}, ["volumeup", "2"]]
//
// The native AudioUI callback only reads positional argument 0 as the button
// string (it prepends "btn-" internally). Argument 1 is the optional volume
// step used by the native volumeAdjust path. There is no separate press type,
// long-press flag or double-tap field in this firmware: long press is encoded
// as a "-long" suffix on a component, simultaneous buttons are joined with
// "+", and a double tap arrives as two ordinary events. Native strings seen in
// the binary are action, action-long, volumeup, volumedown, micmute,
// micmute-long, volumeup+action-long, micmute+volumedown-long, etc.
//
// Only single-button user actions are handled here. Firmware/test combinations
// such as volumeup+action-long (DFU) and micmute+volumedown-long (factory
// reset) are deliberately rejected so the native handler keeps them.

import (
	"log"
	"strconv"
	"strings"
	"time"
)

const nativeVolumeMax = 48

type nativeTouchButton struct {
	Name string
	Long bool
}

type nativeTouchEvent struct {
	Raw     string
	Buttons []nativeTouchButton
	Step    int
}

func parseNativeInputEvent(event []any) (nativeTouchEvent, bool) {
	var parsed nativeTouchEvent
	if len(event) < 5 {
		return parsed, false
	}
	args, ok := event[4].([]any)
	if !ok || len(args) == 0 {
		return parsed, false
	}
	raw, ok := args[0].(string)
	if !ok {
		return parsed, false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return parsed, false
	}
	parts := strings.Split(raw, "+")
	buttons := make([]nativeTouchButton, 0, len(parts))
	for _, part := range parts {
		button, valid := parseNativeTouchButton(part)
		if !valid {
			return nativeTouchEvent{}, false
		}
		buttons = append(buttons, button)
	}
	parsed.Raw = raw
	parsed.Buttons = buttons
	parsed.Step = parseNativeTouchStep(args)
	return parsed, true
}

func parseNativeTouchButton(component string) (nativeTouchButton, bool) {
	var button nativeTouchButton
	component = strings.TrimSpace(component)
	if component == "" {
		return button, false
	}
	if strings.HasSuffix(component, "-long") {
		button.Long = true
		component = strings.TrimSuffix(component, "-long")
	}
	component = strings.TrimPrefix(component, "btn-")
	component = strings.TrimSpace(component)
	if component == "" {
		return nativeTouchButton{}, false
	}
	button.Name = component
	return button, true
}

func parseNativeTouchStep(args []any) int {
	if len(args) < 2 {
		return 1
	}
	var step int
	switch value := args[1].(type) {
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 1
		}
		step = parsed
	case float64:
		step = int(value)
	case int:
		step = value
	default:
		return 1
	}
	return clampTouchStep(step)
}

func clampTouchStep(step int) int {
	if step < 1 {
		return 1
	}
	if step > 6 {
		return 6
	}
	return step
}

func clampNativeVolume(level int) int {
	if level < 0 {
		return 0
	}
	if level > nativeVolumeMax {
		return nativeVolumeMax
	}
	return level
}

func nativeVolumeTarget(current, step int, up bool) int {
	step = clampTouchStep(step)
	if up {
		return clampNativeVolume(current + step)
	}
	return clampNativeVolume(current - step)
}

// nativeTouchDeps keeps every device-facing operation replaceable in tests.
// dispatch defaults to a goroutine so the WAMP watch loop is never blocked by
// a slow native call; tests replace it with a synchronous runner.
type nativeTouchDeps struct {
	dispatch       func(func())
	busy           func() bool
	cancel         func()
	start          func()
	startLong      func()
	stop           func()
	stopNative     func()
	musicState     func() (playing bool, paused bool)
	pause          func() error
	resume         func() error
	volume         func() (int, error)
	setVolume      func(int) error
	refreshDucking func()
	micMuted       func() (muted bool, known bool)
	toggleMic      func() error
}

var touchDeps = nativeTouchDeps{
	dispatch:   func(f func()) { go f() },
	busy:       func() bool { return ctl.busy() },
	cancel:     cancelFromNativeButton,
	start:      func() { ctl.start() },
	startLong:  func() { ctl.startFromNativeHold() },
	stop:       func() { ctl.stop() },
	stopNative: stopNativeAssistant,
	musicState: func() (bool, bool) {
		return nativeTouchMusicState()
	},
	pause: func() error {
		_, err := wampCall("com.harman.gva.pause")
		return err
	},
	resume: func() error {
		_, err := wampCall("com.harman.gva.resume")
		return err
	},
	volume: nativeVolume,
	setVolume: func(level int) error {
		_, err := wampCall("com.harman.volumeSet", level)
		return err
	},
	refreshDucking: refreshNativeDucking,
	micMuted: func() (bool, bool) {
		return nativeMicMuteState()
	},
	toggleMic: func() error {
		_, err := wampCall("com.harman.gva.toggleMic")
		return err
	},
}

// nativeTouchMusicState prefers the direct Spotify status and falls back to
// the native music state group. A paused track must resume, not start a new
// Gemini turn.
func nativeTouchMusicState() (playing bool, paused bool) {
	native := readSpotifyNative()
	if native.Available && !native.Stopped && (native.Track.URI != "" || native.ContextURI != "") {
		return !native.Paused, native.Paused
	}
	r, err := wampCallTimeout(300*time.Millisecond, "com.harman.stateGet", M{})
	if err != nil || len(r) < 5 {
		return false, false
	}
	groups, _ := r[4].(map[string]any)
	return parseNativeMusicState(groups)
}

func parseNativeMusicState(groups map[string]any) (playing bool, paused bool) {
	music, _ := groups["music"].(map[string]any)
	if music == nil {
		return false, false
	}
	if state, ok := music["state"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(state)) {
		case "playing", "play":
			return true, false
		case "paused", "pause":
			return false, true
		}
	}
	if value, ok := music["playing"].(bool); ok {
		return value, false
	}
	if value, ok := music["paused"].(bool); ok {
		return false, value
	}
	return false, false
}

// nativeMicMuteState reads the same native microphone group the watch loop
// already uses for hardware mic-muted wake suppression.
func nativeMicMuteState() (muted bool, known bool) {
	r, err := wampCallTimeout(300*time.Millisecond, "com.harman.stateGet", M{})
	if err != nil || len(r) < 5 {
		return false, false
	}
	groups, _ := r[4].(map[string]any)
	return parseNativeMicMute(groups)
}

func parseNativeMicMute(groups map[string]any) (muted bool, known bool) {
	microphone, _ := groups["microphone"].(map[string]any)
	if microphone == nil {
		return false, false
	}
	if state, ok := microphone["state"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(state)) {
		case "mic-muted":
			return true, true
		case "mic-unmuted":
			return false, true
		}
	}
	if value, ok := microphone["mute"].(float64); ok {
		return value != 0, true
	}
	if value, ok := microphone["mute"].(bool); ok {
		return value, true
	}
	if value, ok := microphone["muted"].(bool); ok {
		return value, true
	}
	return false, false
}

// refreshNativeDucking re-applies the active mixer ceiling after a user
// volume change. The mixer limit is an absolute value derived from the saved
// media dBFS, so a stale ceiling would otherwise keep the old loudness.
func refreshNativeDucking() {
	ducking.mu.Lock()
	defer ducking.mu.Unlock()
	switch {
	case ducking.startupSilences > 0:
		_ = ducking.writeLimit(0)
	case ducking.isDucked:
		_ = ducking.writeLimit(ducking.currentMultiplier)
	}
}

// handleNativeInputEvent consumes one com.harman.test.inputEvent publication.
// It returns true when the event belongs to this handler. Unknown buttons and
// multi-button firmware/test chords return false so the native AudioUI keeps
// ownership of them.
func handleNativeInputEvent(event []any) bool {
	parsed, ok := parseNativeInputEvent(event)
	if !ok || len(parsed.Buttons) != 1 {
		return false
	}
	button := parsed.Buttons[0]

	switch button.Name {
	case "action":
		if button.Long {
			// Native idle maps btn-action-long to voice-trigger. It is an
			// explicit request for Gemini even while music is playing.
			if touchDeps.busy() {
				touchDeps.cancel()
				return true
			}
			touchDeps.dispatch(func() {
				if touchDeps.startLong != nil {
					touchDeps.startLong()
					return
				}
				if touchDeps.stopNative != nil {
					touchDeps.stopNative()
				}
				touchDeps.start()
			})
			return true
		}
		if touchDeps.busy() {
			// Keep this synchronous: cancelFromNativeButton arms the 1200 ms
			// quickTap suppression before native AudioUI's own voice-cancel.
			touchDeps.cancel()
			return true
		}
		touchDeps.dispatch(func() {
			playing, paused := touchDeps.musicState()
			switch {
			case playing:
				if err := touchDeps.pause(); err != nil {
					log.Println("touch action pause:", clean(err))
				}
			case paused:
				if err := touchDeps.resume(); err != nil {
					log.Println("touch action resume:", clean(err))
				}
			}
		})
		return true

	case "volumeup", "volumedown":
		if button.Long {
			return false
		}
		up := button.Name == "volumeup"
		step := parsed.Step
		touchDeps.dispatch(func() {
			current, err := touchDeps.volume()
			if err != nil {
				log.Println("touch volume read:", clean(err))
				return
			}
			if err := touchDeps.setVolume(nativeVolumeTarget(current, step, up)); err != nil {
				log.Println("touch volume set:", clean(err))
				return
			}
			touchDeps.refreshDucking()
		})
		return true

	case "micmute":
		// micmute-long is part of native recovery/factory chords; leave it to
		// the shipped AudioUI. Plain micmute is the user's privacy toggle.
		if button.Long {
			return false
		}
		touchDeps.dispatch(func() {
			muted, known := touchDeps.micMuted()
			if err := touchDeps.toggleMic(); err != nil {
				log.Println("touch mic toggle:", clean(err))
				return
			}
			// The native handler is com.harman.gva.toggleMic (recovered from
			// audio-ui). Do not use the stock Assistant SetMuted RPC here.
			if (!known || !muted) && touchDeps.busy() {
				touchDeps.stop()
			}
		})
		return true
	}
	return false
}

package main

import (
	"reflect"
	"testing"
)

func touchEventForTest(args ...any) []any {
	return []any{float64(36), float64(1), float64(2), map[string]any{}, args}
}

func TestHandleActionLongTouchPanel(t *testing.T) {
	orig := touchDeps
	defer func() { touchDeps = orig }()

	var order []string
	var busy bool
	touchDeps = nativeTouchDeps{
		dispatch:   func(f func()) { f() },
		busy:       func() bool { return busy },
		cancel:     func() { order = append(order, "cancel") },
		start:      func() { order = append(order, "start") },
		startLong:  func() { order = append(order, "startLong") },
		stop:       func() { order = append(order, "stop") },
		stopNative: func() { order = append(order, "stopNative") },
		musicState: func() (bool, bool) { return true, false },
	}

	if !handleNativeInputEvent(touchEventForTest("action-long", "0")) {
		t.Fatal("idle action-long was not handled")
	}
	if !reflect.DeepEqual(order, []string{"startLong"}) {
		t.Fatalf("idle action-long calls = %v, want [startLong]", order)
	}

	order = nil
	touchDeps.startLong = nil
	if !handleNativeInputEvent(touchEventForTest("btn-action-long")) {
		t.Fatal("idle btn-action-long fallback was not handled")
	}
	if !reflect.DeepEqual(order, []string{"stopNative", "start"}) {
		t.Fatalf("fallback action-long calls = %v, want [stopNative start]", order)
	}

	order = nil
	busy = true
	if !handleNativeInputEvent(touchEventForTest("action-long", "0")) {
		t.Fatal("busy action-long was not handled")
	}
	if !reflect.DeepEqual(order, []string{"cancel"}) {
		t.Fatalf("busy action-long calls = %v, want [cancel]", order)
	}

	// Single short press while idle and no music paused/playing must NOT start Gemini.
	order = nil
	busy = false
	touchDeps.musicState = func() (bool, bool) { return false, false }
	if !handleNativeInputEvent(touchEventForTest("action", "0")) {
		t.Fatal("idle action was not handled")
	}
	if len(order) != 0 {
		t.Fatalf("idle short action with no music calls = %v, want none", order)
	}
	if got := nativePhaseProperties("idle")["btn-action-long"]; got != "noop" {
		t.Fatalf("nativePhaseProperties(idle)[btn-action-long] = %v, want noop", got)
	}
}

package main

import (
	"testing"
	"time"
)

func TestReplyAudioStallDoesNotHoldSessionForever(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, phase string
		last        time.Time
		guard, want bool
	}{
		{"broken partial reply", "speaking", now.Add(-9 * time.Second), false, true},
		{"audio still queued", "speaking", now.Add(-9 * time.Second), true, false},
		{"short network pause", "speaking", now.Add(-7 * time.Second), false, false},
		{"tool running", "thinking", now.Add(-20 * time.Second), false, false},
		{"user speaking", "listening", now.Add(-20 * time.Second), false, false},
		{"no reply yet", "speaking", time.Time{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replyAudioStalled(tc.phase, tc.last, now, tc.guard); got != tc.want {
				t.Fatalf("stalled=%v want=%v", got, tc.want)
			}
		})
	}
}

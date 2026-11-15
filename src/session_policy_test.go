package main

import (
	"testing"
	"time"
)

func TestMeaningfulSessionActivity(t *testing.T) {
	cases := []struct {
		name        string
		meaningful  bool
		postWake    bool
		nativeInput bool
		awaiting    bool
		phase       string
		want        bool
	}{
		{"accepted transcript", true, false, false, false, "idle", true},
		{"post-wake speech while mic open", false, true, true, false, "listening", true},
		{"post-wake noise after endpoint", false, true, false, false, "listening", false},
		{"open mic without speech evidence", false, false, true, false, "listening", false},
		{"awaiting tool", false, false, false, true, "listening", true},
		{"model thinking", false, false, false, false, "thinking", true},
		{"model speaking", false, false, false, false, "speaking", true},
		{"announcing music", false, false, false, false, "announcing_music", true},
		{"idle", false, false, false, false, "idle", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := meaningfulSessionActivity(c.meaningful, c.postWake, c.nativeInput, c.awaiting, c.phase); got != c.want {
				t.Fatalf("meaningfulSessionActivity(%v,%v,%v,%v,%q)=%v, want %v",
					c.meaningful, c.postWake, c.nativeInput, c.awaiting, c.phase, got, c.want)
			}
		})
	}
}

func TestFollowUpAllowedByReply(t *testing.T) {
	allowed := []string{
		"Хотите ещё?",
		"«Хотите ещё?»",
		"Как вас зовут?",
		"Would you like anything else?",
		"Would you like me to play music",
		"Чем ещё помочь",
	}
	for _, output := range allowed {
		if !followUpAllowedByReply(output) {
			t.Fatalf("follow-up question rejected: %q", output)
		}
	}
	denied := []string{
		"Арбуз.",
		"Готово!",
		"",
		"Включаю музыку.",
		"Это не вопрос.",
	}
	for _, output := range denied {
		if followUpAllowedByReply(output) {
			t.Fatalf("plain reply reopened the mic: %q", output)
		}
	}
}

func TestShouldCloseAfterNoiseHonorsGrace(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	grace := 10 * time.Second
	cases := []struct {
		name string
		last time.Time
		now  time.Time
		wait time.Duration
		want bool
	}{
		{"no meaningful activity ever", time.Time{}, now, grace, true},
		{"inside grace", now.Add(-5 * time.Second), now, grace, false},
		{"just inside grace", now.Add(-9 * time.Second), now, grace, false},
		{"at grace boundary", now.Add(-10 * time.Second), now, grace, true},
		{"well past grace", now.Add(-30 * time.Second), now, grace, true},
		{"clock skew into the future", now.Add(time.Second), now, grace, false},
		{"zero grace", now.Add(-5 * time.Second), now, 0, true},
		{"negative grace", now.Add(-5 * time.Second), now, -time.Second, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldCloseAfterNoise(c.last, c.now, c.wait); got != c.want {
				t.Fatalf("shouldCloseAfterNoise(%v,%v,%v)=%v, want %v", c.last, c.now, c.wait, got, c.want)
			}
		})
	}
}

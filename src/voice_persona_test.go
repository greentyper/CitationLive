package main

import (
	"strings"
	"testing"
)

func TestVoicePersona(t *testing.T) {
	cases := []struct {
		voice  string
		name   string
		gender string
	}{
		{"Charon", "Charon", "мужском роде"},
		{"Kore", "Kore", "женском роде"},
		{"Puck", "Puck", "мужском роде"},
		{"Zephyr", "Zephyr", "женском роде"},
		{"CustomVoice", "CustomVoice", "нейтрально"},
	}
	for _, tc := range cases {
		got := voicePersona(tc.voice)
		if !strings.Contains(got, "Твоё имя — "+tc.name) || !strings.Contains(got, tc.gender) {
			t.Fatalf("voicePersona(%q) = %q", tc.voice, got)
		}
	}
	if voicePersona("  ") != "" {
		t.Fatal("empty voice must not add a persona")
	}
}

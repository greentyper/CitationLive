package main

import "testing"

func TestQuickMediaNewMusicAndTransport(t *testing.T) {
	for phrase, want := range map[string]string{
		"Эй, Google, включи музыку.":   "play",
		"Эй, Google, музыку.":          "play",
		"Вруби музыку в Spotify":       "play",
		"Play music":                   "play",
		"Включи музыку для тренировки": "",
		"Включи плейлист Туманная орбита экспедиции": "",
		"Продолжи музыку": "resume",
		"Следующий трек":  "next",
	} {
		if got := quickMediaAction(phrase); got != want {
			t.Errorf("quickMediaAction(%q) = %q, want %q", phrase, got, want)
		}
	}
}

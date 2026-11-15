package main

import (
	"sync"
	"time"
)

type recognitionAttempt struct {
	At     int64  `json:"at"`
	Text   string `json:"text"`
	Action string `json:"action"`
}

var recognitionHistory struct {
	sync.Mutex
	attempts []recognitionAttempt
}

func rememberRecognition(text, action string) {
	recognitionHistory.Lock()
	defer recognitionHistory.Unlock()
	now := time.Now().Unix()
	kept := []recognitionAttempt{}
	for _, a := range recognitionHistory.attempts {
		if now-a.At < 900 {
			kept = append(kept, a)
		}
	}
	kept = append(kept, recognitionAttempt{now, boundedConversationText(text, 600), action})
	if len(kept) > 4 {
		kept = kept[len(kept)-4:]
	}
	recognitionHistory.attempts = kept
}
func recentRecognitions() []recognitionAttempt {
	recognitionHistory.Lock()
	defer recognitionHistory.Unlock()
	out := []recognitionAttempt{}
	for _, a := range recognitionHistory.attempts {
		if time.Now().Unix()-a.At < 900 {
			out = append(out, a)
		}
	}
	return out
}

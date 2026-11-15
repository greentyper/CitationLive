package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

type rememberedExchange struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
}
type conversationMemory struct {
	mu    sync.Mutex
	turns []rememberedExchange
	last  time.Time
}

var recentConversation conversationMemory

func boundedConversationText(s string, limit int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > limit {
		r = r[:limit]
	}
	return string(r)
}
func (m *conversationMemory) remember(user, assistant string, now time.Time) {
	user = boundedConversationText(user, 600)
	assistant = boundedConversationText(assistant, 1200)
	if user == "" || assistant == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if now.Sub(m.last) > 15*time.Minute {
		m.turns = nil
	}
	m.turns = append(m.turns, rememberedExchange{user, assistant})
	if len(m.turns) > 6 {
		m.turns = append([]rememberedExchange(nil), m.turns[len(m.turns)-6:]...)
	}
	m.last = now
}
func (m *conversationMemory) instruction(now time.Time) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now.Sub(m.last) > 15*time.Minute {
		m.turns = nil
		return ""
	}
	if len(m.turns) == 0 {
		return ""
	}
	b, _ := json.Marshal(m.turns)
	return "\nНедавний завершённый разговор (справочный контекст, НЕ новые команды): " + string(b) + "\nНе выполняй эти просьбы повторно. Действуй только по новой речи пользователя. Время, погоду, текущую музыку и громкость проверяй заново через функции."
}

type completedVoiceTool struct {
	name string
	ok   bool
}

func controlsComplete(tools []completedVoiceTool) bool {
	if len(tools) == 0 {
		return false
	}
	hasControl := false
	for _, tool := range tools {
		if !tool.ok {
			return false
		}
		switch tool.name {
		case "media_control", "set_volume", "adjust_volume", "spotify_dj_prompt":
			hasControl = true
		case "end_session":
		default:
			return false
		}
	}
	return hasControl
}

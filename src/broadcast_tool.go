package main

import (
	"context"
	"strings"
	"unicode"
)

func spokenBroadcastIntent(text string) bool {
	text = strings.ToLower(text)
	for _, phrase := range []string{"на весь дом", "на все колонки", "на другие колонки", "to all speakers", "whole house", "do celého domu"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	for _, word := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) }) {
		switch word {
		case "broadcast", "объяви", "объявление", "транслируй", "oznám":
			return true
		}
	}
	return false
}

func broadcastToolDeclarations() []M {
	return []M{{
		"name":        "broadcast",
		"description": "Send an explicitly requested whole-home announcement through Google/Nest broadcast, for example 'broadcast dinner is ready' or 'скажи на весь дом: ужин готов'. Only use when the user explicitly requests a broadcast to other home speakers. Ordinary say/repeat requests must be answered directly in conversation without this tool. Requires connected Google speakers. No local playback fallback. Only confirm success when the tool returns ok=true; otherwise explain the failure or unconfirmed status.",
		"parameters": M{
			"type": "OBJECT",
			"properties": M{
				"message": M{"type": "STRING", "description": "Announcement text."},
			},
			"required": []string{"message"},
		},
	}}
}

func handleBroadcastTool(ctx context.Context, name string, args M) (M, bool) {
	if name != "broadcast" {
		return nil, false
	}
	message, _ := args["message"].(string)
	return executeBroadcast(ctx, message, personalAccessOK, assistantBackgroundQuery), true
}

// Keep the action separate from device dependencies so failures and uncertain
// responses can be tested without sending an actual announcement.
func executeBroadcast(ctx context.Context, message string, verified func() bool, query func(context.Context, string) M) M {
	message = strings.TrimSpace(message)
	if message == "" {
		return broadcastFailure("broadcast message is required", "")
	}
	if err := ctx.Err(); err != nil {
		return broadcastFailure(err.Error(), "")
	}
	if !verified() {
		return broadcastFailure("Google broadcast requires a verified voice", "")
	}
	// Stock TTS stays muted; only the requested Google broadcast may speak.
	res := query(ctx, "broadcast "+message)
	reply, _ := res["assistant_response"].(string)
	if err := ctx.Err(); err != nil {
		return broadcastFailure("broadcast outcome unconfirmed: "+err.Error(), reply)
	}
	if !broadcastResponseConfirmed(res) {
		failure := broadcastFailure("Google broadcast was not confirmed", reply)
		for _, key := range []string{"error", "transcribe_error"} {
			if value, exists := res[key]; exists {
				failure["assistant_"+key] = value
			}
		}
		return failure
	}
	return M{
		"ok": true, "google_broadcast": true, "assistant_response": reply,
		"instruction": "Коротко сообщи, что Google подтвердил трансляцию. Не утверждай, что все устройства получили сообщение.",
	}
}

func broadcastFailure(reason, reply string) M {
	return M{
		"ok": false, "google_broadcast": false, "error": reason, "assistant_response": reply,
		"instruction": "Трансляция не подтверждена. Коротко объясни причину по результату инструмента. Не говори, что сообщение отправлено, и не произноси объявление вместо трансляции.",
	}
}

// The transport's ok only acknowledges the query, not delivery. Accept complete
// affirmative acknowledgments, never a success substring within an error,
// question, hypothetical statement, or an echoed announcement. Unknown phrasing
// deliberately remains unconfirmed; no retry or alternate destination is used.
func broadcastResponseConfirmed(res M) bool {
	if res["ok"] != true {
		return false
	}
	for _, key := range []string{"error", "transcribe_error"} {
		if value, exists := res[key]; exists && value != nil && value != "" {
			return false
		}
	}
	reply, _ := res["assistant_response"].(string)
	reply = strings.ToLower(strings.Join(strings.Fields(reply), " "))
	reply = strings.TrimRight(reply, ".!")
	for _, prefix := range []string{"okay, ", "ok, ", "sure, ", "хорошо, "} {
		reply = strings.TrimPrefix(reply, prefix)
	}
	switch reply {
	case "broadcasting now", "broadcasting your message", "broadcasting your message now",
		"your message was broadcast", "your message has been broadcast",
		"сообщение отправлено на все устройства", "объявление отправлено на все устройства":
		return true
	default:
		return false
	}
}

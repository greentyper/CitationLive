package main

import (
	"encoding/base64"
	"log"
	"strings"
	"time"
)

// Use the configured Live model and the same playback path as a spoken reply.
func (s *session) speakSpotifyAnnouncement(selection M) {
	text, _ := selection["announcement"].(string)
	if strings.TrimSpace(text) == "" {
		return
	}
	if err := s.send(M{"clientContent": M{
		"turns":        []M{{"role": "user", "parts": []M{{"text": "Подтверждение Spotify для озвучивания: " + text}}}},
		"turnComplete": true,
	}}); err != nil {
		log.Println("Spotify Live announcement request:", clean(err))
		return
	}
	// This runs inside the session's reader after the microphone is closed.
	_ = s.conn.SetReadDeadline(time.Now().Add(7 * time.Second))
	defer s.conn.SetReadDeadline(time.Time{})
	defer s.finishPlayer(false)
	var played int
	var transcript strings.Builder
	for {
		var event M
		if err := s.conn.ReadJSON(&event); err != nil {
			log.Println("Spotify Live announcement ended:", clean(err))
			return
		}
		content, _ := event["serverContent"].(map[string]any)
		output, _ := content["outputTranscription"].(map[string]any)
		if part, ok := output["text"].(string); ok {
			transcript.WriteString(part)
		}
		turn, _ := content["modelTurn"].(map[string]any)
		parts, _ := turn["parts"].([]any)
		for _, item := range parts {
			part, _ := item.(map[string]any)
			inline, _ := part["inlineData"].(map[string]any)
			encoded, _ := inline["data"].(string)
			audio, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil || len(audio) == 0 {
				continue
			}
			if err := s.play(audio); err != nil {
				log.Println("Spotify Live announcement playback:", clean(err))
				return
			}
			played += len(audio)
		}
		if content["turnComplete"] == true && played > 0 {
			log.Printf("Spotify announcement in current Gemini session: %d audio bytes, output %q", played, transcript.String())
			return
		}
	}
}

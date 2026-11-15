package main

// Transcribe the stock Assistant's muted TTS audio with Gemini's dedicated
// transcription model. The stock Assistant speaks English; Gemini receives the
// English transcript and answers the user in Russian.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type geminiTranscriptionResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text               string `json:"text"`
				AudioTranscription struct {
					Text string `json:"text"`
				} `json:"audioTranscription"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func transcribeAssistantPCM(ctx context.Context, samples []float32, rate int) (string, error) {
	wav := encodeWAV(samples, rate)
	if len(wav) == 0 {
		return "", fmt.Errorf("empty assistant audio")
	}
	keyBytes, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return "", fmt.Errorf("read API key: %w", err)
	}
	key := strings.TrimSpace(string(keyBytes))
	if key == "" {
		return "", fmt.Errorf("empty API key")
	}
	payload := map[string]any{
		"contents": []any{
			map[string]any{
				"parts": []any{
					map[string]any{"text": "Transcribe the English speech in this audio exactly. Output only the transcript, no commentary."},
					map[string]any{"inline_data": map[string]any{
						"mime_type": "audio/wav",
						"data":      base64.StdEncoding.EncodeToString(wav),
					}},
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 25 * time.Second}
	models := []string{"gemini-3.5-transcribe", "gemini-3.8-flash", "gemini-3.6-flash"}
	var lastErr error
	for _, model := range models {
		req, err := http.NewRequestWithContext(ctx, "POST",
			"https://generativelanguage.googleapis.com/v1beta/models/"+model+":generateContent",
			bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-goog-api-key", key)
		res, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if res.StatusCode != 200 {
			lastErr = fmt.Errorf("%s HTTP %d: %s", model, res.StatusCode, truncateString(string(respBody), 200))
			continue
		}
		var parsed geminiTranscriptionResponse
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			lastErr = fmt.Errorf("%s decode: %w", model, err)
			continue
		}
		if parsed.Error.Message != "" {
			lastErr = fmt.Errorf("%s: %s", model, parsed.Error.Message)
			continue
		}
		for _, cand := range parsed.Candidates {
			for _, part := range cand.Content.Parts {
				text := strings.TrimSpace(part.AudioTranscription.Text)
				if text == "" {
					text = strings.TrimSpace(part.Text)
				}
				if text != "" {
					return strings.Join(strings.Fields(text), " "), nil
				}
			}
		}
		lastErr = fmt.Errorf("%s returned no transcript", model)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no transcription model available")
	}
	return "", lastErr
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

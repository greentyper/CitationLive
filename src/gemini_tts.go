package main

// Gemini TTS for timer/reminder announcements while no Live session is open.
// The live Gemini session remains the primary voice; this is only the idle
// fallback so a due reminder is spoken instead of just beeping.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type geminiTTSResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				InlineData struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func synthesizeGeminiTTS(ctx context.Context, text string) ([]byte, error) {
	keyBytes, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read API key: %w", err)
	}
	key := strings.TrimSpace(string(keyBytes))
	if key == "" {
		return nil, fmt.Errorf("empty API key")
	}
	voice := strings.TrimSpace(cfg.Voice)
	if voice == "" {
		voice = "Charon"
	}
	payload := map[string]any{
		"contents": []any{map[string]any{
			"parts": []any{map[string]any{"text": text}},
		}},
		"generationConfig": map[string]any{
			"responseModalities": []string{"AUDIO"},
			"speechConfig": map[string]any{
				"voiceConfig": map[string]any{
					"prebuiltVoiceConfig": map[string]any{"voiceName": voice},
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 25 * time.Second}
	models := []string{"gemini-3.8-flash-tts", "gemini-3.1-flash-tts-preview", "gemini-2.5-flash-preview-tts"}
	var lastErr error
	for _, model := range models {
		req, err := http.NewRequestWithContext(ctx, "POST",
			"https://generativelanguage.googleapis.com/v1beta/models/"+model+":generateContent",
			bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-goog-api-key", key)
		res, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, readErr := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		res.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if res.StatusCode != 200 {
			lastErr = fmt.Errorf("%s HTTP %d: %s", model, res.StatusCode, truncateString(string(respBody), 200))
			continue
		}
		var parsed geminiTTSResponse
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
				if part.InlineData.Data == "" {
					continue
				}
				pcm, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
				if err == nil && len(pcm) > 0 {
					return pcm, nil
				}
			}
		}
		lastErr = fmt.Errorf("%s returned no audio", model)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no TTS model available")
	}
	return nil, lastErr
}

func speakGeminiTTS(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	pcm, err := synthesizeGeminiTTS(ctx, text)
	if err != nil {
		return err
	}
	log.Printf("Gemini TTS: %d bytes", len(pcm))
	out, err := openMixerOutput()
	if err != nil {
		return err
	}
	defer out.close()
	markLocalCueWindow(pcm)
	seconds := time.Duration(len(pcm)/2) * time.Second / 24000
	extendAssistantAudioGuard(seconds + time.Second)
	if err := out.write(pcm); err != nil {
		return err
	}
	return out.finish()
}

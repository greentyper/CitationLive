package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const spotifyDJURI = "spotify:playlist:37i9dQZF1EYkqdzj48dyYq"
const spotifyDJConnectHost = "https://gew4-spclient.spotify.com"

func playSpotifyDJ(ctx context.Context, device string) error {
	tokenValue, err := spotifyNativeToken(ctx)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 8 * time.Second}

	// The DJ catalog URI has no ordinary playlist tracks. Spotify generates
	// the personalized queue, narration, and continuation in a DJ context.
	query := url.Values{"contextUri": {spotifyDJURI}, "reason": {"state_restore"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spotifyDJConnectHost+"/lexicon-session-provider/context-resolve/v2/session?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tokenValue)
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	contextJSON, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	var contextInfo struct {
		URI   string
		Pages []json.RawMessage
	}
	if res.StatusCode != http.StatusOK || readErr != nil || json.Unmarshal(contextJSON, &contextInfo) != nil ||
		contextInfo.URI != spotifyDJURI || len(contextInfo.Pages) == 0 {
		return fmt.Errorf("Spotify не вернул очередь DJ (HTTP %d).", res.StatusCode)
	}

	var body bytes.Buffer
	var intent [16]byte
	rand.Read(intent[:])
	compressed := gzip.NewWriter(&body)
	err = json.NewEncoder(compressed).Encode(M{
		"command": M{
			"endpoint":     "play",
			"context":      json.RawMessage(contextJSON),
			"options":      M{"initially_paused": false, "player_options_override": M{"shuffling_context": false}},
			"play_origin":  M{"feature_identifier": "home"},
			"play_options": M{"reason": "interactive", "operation": "replace", "trigger": "immediately"},
		},
		"connection_type": "wlan",
		"intent_id":       hex.EncodeToString(intent[:]),
	})
	closeErr := compressed.Close()
	if err != nil || closeErr != nil {
		return errors.New("Не удалось подготовить запуск Spotify DJ.")
	}
	encodedDevice := url.PathEscape(device)
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, spotifyDJConnectHost+"/connect-state/v1/player/command/from/"+encodedDevice+"/to/"+encodedDevice, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tokenValue)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	res, err = client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("Spotify Connect отклонил запуск DJ (HTTP %d).", res.StatusCode)
	}
	return nil
}

func isNativeDJContext(uri string) bool {
	return uri == spotifyDJURI || len(uri) > 16 && uri[:16] == "spotify:lexicon:"
}

func promptSpotifyDJ(ctx context.Context, prompt string) error {
	status := readSpotifyNative()
	// If DJ context is not currently playing, start it first and wait until the initial stream is ready
	if !status.Available || !isNativeDJContext(status.ContextURI) || status.Stopped {
		device := status.DeviceID
		if device == "" {
			device = directSpotify.boundDeviceID()
		}
		if device == "" {
			return errors.New("Spotify Connect: не удалось определить устройство воспроизведения.")
		}
		if err := playSpotifyDJ(ctx, device); err != nil {
			return err
		}
		for i := 0; i < 30; i++ {
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			st := readSpotifyNative()
			if st.Available && isNativeDJContext(st.ContextURI) && !st.Stopped && !st.Buffering && st.Track.URI != "" {
				break
			}
		}
	}

	client := &http.Client{Timeout: 8 * time.Second}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(map[string]string{"prompt": prompt}); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spotifyNativeAPI+"/player/dj_prompt", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("DJ prompt rejected (HTTP %d)", res.StatusCode)
	}
	return nil
}

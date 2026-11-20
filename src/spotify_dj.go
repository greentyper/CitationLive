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
	"slices"
	"strings"
	"sync"
	"time"
)

const spotifyDJURI = "spotify:playlist:37i9dQZF1EYkqdzj48dyYq"
const spotifyDJConnectHost = "https://gew4-spclient.spotify.com"
const spotifyDJSessionPath = "/lexicon-session-provider/context-resolve/v2/session"

type djOpening struct {
	URI     string
	Segment string
}

var spotifyDJStarts struct {
	sync.Mutex
	recent []djOpening
}

func playSpotifyDJ(ctx context.Context, device string) error {
	return startSpotifyDJ(ctx, device, "interactive")
}

func startSpotifyDJ(ctx context.Context, device, reason string) error {
	var recent []djOpening
	if reason == "interactive" {
		spotifyDJStarts.Lock()
		defer spotifyDJStarts.Unlock()
		recent = spotifyDJStarts.recent
	}
	tokenValue, err := spotifyNativeToken(ctx)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	contextJSON, opening, err := resolveSpotifyDJContext(ctx, client, tokenValue, reason, recent)
	if err != nil {
		return err
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spotifyDJConnectHost+"/connect-state/v1/player/command/from/"+encodedDevice+"/to/"+encodedDevice, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tokenValue)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("Spotify Connect отклонил запуск DJ (HTTP %d).", res.StatusCode)
	}
	if reason == "interactive" {
		spotifyDJStarts.recent = append(recent, opening)
		if len(spotifyDJStarts.recent) > 4 {
			spotifyDJStarts.recent = spotifyDJStarts.recent[1:]
		}
	}
	return nil
}

func resolveSpotifyDJContext(ctx context.Context, client *http.Client, token, reason string, recent []djOpening) (json.RawMessage, djOpening, error) {
	query := url.Values{"contextUri": {spotifyDJURI}, "reason": {reason}}
	root, err := fetchSpotifyDJPage(ctx, client, token, spotifyDJConnectHost+spotifyDJSessionPath+"?"+query.Encode())
	if err != nil {
		return nil, djOpening{}, err
	}
	var session struct {
		URI   string
		Pages []json.RawMessage
	}
	if json.Unmarshal(root, &session) != nil || session.URI != spotifyDJURI || len(session.Pages) == 0 {
		return nil, djOpening{}, errors.New("Spotify не вернул сеанс DJ.")
	}

	pages := session.Pages
	page := pages[0]
	for step := 0; step <= 6; step++ {
		var info struct {
			Tracks      []json.RawMessage
			NextPageURL string `json:"next_page_url"`
		}
		if err := json.Unmarshal(page, &info); err != nil {
			return nil, djOpening{}, errors.New("Spotify вернул некорректную очередь DJ.")
		}

		if i, opening := newDJOpening(info.Tracks, recent); i >= 0 {
			if step == 0 && i == 0 {
				return root, opening, nil
			}
			if i > 0 {
				page, err = replaceDJArray(page, "tracks", info.Tracks[i:])
				if err != nil {
					return nil, djOpening{}, err
				}
			}
			pages[0] = page
			resolved, err := replaceDJArray(root, "pages", pages)
			return resolved, opening, err
		}
		if len(pages) > 1 {
			pages = pages[1:]
			page = pages[0]
			continue
		}
		if step == 6 || info.NextPageURL == "" {
			break
		}
		endpoint, err := spotifyDJContinuationURL(info.NextPageURL)
		if err != nil {
			return nil, djOpening{}, err
		}
		page, err = fetchSpotifyDJPage(ctx, client, token, endpoint)
		if err != nil {
			return nil, djOpening{}, err
		}
	}
	return nil, djOpening{}, errors.New("Spotify не вернул новую подборку DJ.")
}

func newDJOpening(tracks []json.RawMessage, recent []djOpening) (int, djOpening) {
	for i, raw := range tracks {
		var track struct {
			URI      string
			Metadata map[string]string
		}
		if json.Unmarshal(raw, &track) != nil || (i > 0 && track.Metadata["dj.jump_target"] != "true") {
			continue
		}
		opening := djOpening{URI: track.URI, Segment: track.Metadata["segment"]}
		if opening.URI == "" {
			opening.URI = track.Metadata["canonical_track_uri"]
		}
		if opening.URI != "" && !slices.ContainsFunc(recent, func(previous djOpening) bool {
			return previous.URI == opening.URI || (opening.Segment != "" && previous.Segment == opening.Segment)
		}) {
			return i, opening
		}
	}
	return -1, djOpening{}
}

func replaceDJArray(body json.RawMessage, key string, values []json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	array, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	fields[key] = array
	return json.Marshal(fields)
}

func spotifyDJContinuationURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" {
		return "", errors.New("Spotify вернул некорректное продолжение DJ.")
	}
	const hmPrefix = "/context-resolve/v2/session/"
	switch {
	case u.Scheme == "hm" && u.Host == "lexicon-session-provider" && strings.HasPrefix(u.Path, hmPrefix):
		u.Scheme, u.Host = "https", "gew4-spclient.spotify.com"
		u.Path = "/lexicon-session-provider" + u.Path
		u.RawPath = ""
	case u.Scheme == "https" && u.Host == "gew4-spclient.spotify.com":
	default:
		return "", errors.New("Spotify вернул некорректное продолжение DJ.")
	}
	segment, ok := strings.CutPrefix(u.Path, spotifyDJSessionPath+"/")
	if !ok || segment == "" || segment == "." || segment == ".." || strings.Contains(segment, "/") {
		return "", errors.New("Spotify вернул некорректное продолжение DJ.")
	}
	return u.String(), nil
}

func fetchSpotifyDJPage(ctx context.Context, client *http.Client, token, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Spotify не вернул очередь DJ (HTTP %d).", res.StatusCode)
	}
	const maxBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil || len(data) > maxBytes {
		return nil, errors.New("Не удалось прочитать очередь DJ.")
	}
	return data, nil
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

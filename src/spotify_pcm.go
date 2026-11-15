package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const spotifyNativeAPI = "http://127.0.0.1:18767"

var spotifyNativeHTTP = &http.Client{Timeout: 2 * time.Second}

type spotifyNativeTrack struct {
	URI, Name   string
	AlbumURI    string   `json:"album_uri"`
	AlbumName   string   `json:"album_name"`
	ArtistNames []string `json:"artist_names"`
}

type spotifyNativeStatus struct {
	Available                  bool   `json:"-"`
	DeviceID                   string `json:"device_id"`
	ContextURI                 string `json:"context_uri"`
	ContextName                string `json:"context_name"`
	Shuffle                    bool   `json:"shuffle_context"`
	RepeatContext              bool   `json:"repeat_context"`
	RepeatTrack                bool   `json:"repeat_track"`
	Stopped, Paused, Buffering bool
	Track                      spotifyNativeTrack
}

func spotifyNativeCommand(ctx context.Context, action string, args M) error {
	data, err := json.Marshal(args)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spotifyNativeAPI+"/player/"+action, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	timeout := 5 * time.Second
	if action == "play" {
		timeout = 10 * time.Second
	}
	client := http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("Spotify: команда %s отклонена (HTTP %d)", action, resp.StatusCode)
	}
	return nil
}

func resetSpotifyBeforePlay(ctx context.Context) {
	native := readSpotifyNative()
	if isNativeDJContext(native.ContextURI) && !native.Stopped {
		_ = spotifyNativeCommand(ctx, "stop", M{})
		_ = waitSpotifyRegistration(ctx, 1500*time.Millisecond)
	}
}

func waitSpotifyReady(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if readSpotifyNative().DeviceID != "" {
			if _, err := spotifyNativeToken(ctx); err == nil {
				return true
			}
		}
		if waitSpotifyRegistration(ctx, 250*time.Millisecond) != nil {
			return false
		}
	}
	return false
}

func playSpotifyNative(ctx context.Context, selection M, device, uri, kind string) M {
	directSpotify.setPlaybackContext(selection)
	if candidates, ok := selection["spotify_search_uris"].([]string); ok && len(candidates) > 0 {
		return playSpotifySearch(ctx, selection, candidates)
	}
	resetSpotifyBeforePlay(ctx)
	if !waitSpotifyReady(ctx, 5*time.Second) {
		return M{"ok": false, "error": "Проигрыватель Spotify не отвечает."}
	}
	shuffle, _ := selection["spotify_shuffle"].(bool)
	if kind == "playlist" || kind == "artist" {
		if err := spotifyNativeCommand(ctx, "shuffle_context", M{"shuffle_context": shuffle}); err != nil {
			return M{"ok": false, "error": err.Error()}
		}
	}
	startPlay := func() error {
		if uri == spotifyDJURI {
			return playSpotifyDJ(ctx, device)
		}
		return spotifyNativeCommand(ctx, "play", M{"uri": uri})
	}
	if err := startPlay(); err != nil {
		return M{"ok": false, "error": err.Error()}
	}
	retried := false
	attempts := 30
	if kind != "track" {
		attempts = 45
	}
	for attempt := 0; attempt < attempts; attempt++ {
		status := readSpotifyNative()
		playback := status.playback()
		matches := (kind == "track" && status.Track.URI == uri) || (kind != "track" && status.ContextURI == uri)
		if status.DeviceID == device && playback.State == "PLAYING" && matches {
			return M{"ok": true, "status": "playing", "backend": "spotify_api", "title": playback.Title, "artist": playback.Artist, "selected_uri": uri, "shuffle": status.Shuffle}
		}
		if attempt == 14 && !retried && status.Buffering && status.Track.URI == "" {
			retried = true
			_ = spotifyNativeCommand(ctx, "stop", M{})
			if err := startPlay(); err != nil {
				return M{"ok": false, "error": err.Error()}
			}
		}
		if err := waitSpotifyRegistration(ctx, 350*time.Millisecond); err != nil {
			return M{"ok": false, "error": err.Error()}
		}
	}
	return M{"ok": false, "error": "Нативный Spotify не подтвердил воспроизведение."}
}

func artistKey(s string) string {
	return strings.NewReplacer("ea", "i", "ee", "i", "ph", "f", "iy", "ii", "ay", "ai", "ey", "ei", "oy", "oi", "uy", "ui").Replace(musicKey(s))
}

func spotifyArtistMatches(want string, names []string) bool {
	want = artistKey(want)
	if want == "" {
		return false
	}
	for _, name := range names {
		have := artistKey(name)
		if have != "" && (have == want || strings.Contains(have, want) || strings.Contains(want, have)) {
			return true
		}
	}
	return false
}

func spotifySearchTrackMatches(kind, requested, artist string, track spotifyNativeTrack) bool {
	switch kind {
	case "artist":
		return spotifyArtistMatches(requested, track.ArtistNames)
	case "album":
		have := musicKey(track.AlbumName)
		want := musicKey(requested)
		return have != "" && want != "" && (have == want || strings.Contains(have, want) || closeMusicTitle(track.AlbumName, requested))
	case "track":
		return spotifyTrackTitleMatches(requested, track.Name) &&
			(artist == "" || spotifyArtistMatches(artist, track.ArtistNames))
	}
	return true
}

func waitSpotifyTrack(ctx context.Context, uri string, timeout time.Duration) (spotifyNativeStatus, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status := readSpotifyNative()
		if !status.Stopped && !status.Buffering && status.Track.Name != "" &&
			(uri == "" || status.Track.URI == uri) {
			return status, true
		}
		if waitSpotifyRegistration(ctx, 200*time.Millisecond) != nil {
			break
		}
	}
	return spotifyNativeStatus{}, false
}

// Search results come from the paired player's own Spotify session. Play a
// candidate silently, verify it against the request, and only then unmute.
func playSpotifySearch(ctx context.Context, selection M, candidates []string) M {
	requested, _ := selection["requested_track"].(string)
	artist, _ := selection["requested_artist"].(string)
	kind, _ := selection["music_type"].(string)
	queueContext, _ := selection["spotify_search_context"].(bool)
	resetSpotifyBeforePlay(ctx)
	if !waitSpotifyReady(ctx, 5*time.Second) {
		return M{"ok": false, "error": "Проигрыватель Spotify не отвечает."}
	}
	release, err := ducking.holdSpotifyStartupSilence()
	if err != nil {
		return M{"ok": false, "error": "Не удалось заглушить стартовый поток Spotify."}
	}
	released := false
	unmute := func() error {
		if released {
			return nil
		}
		released = true
		return release()
	}
	defer func() { _ = unmute() }()
	limit := min(len(candidates), 5)
	if queueContext {
		limit = min(len(candidates), 3)
	}
	if kind == "album" {
		limit = min(len(candidates), 2)
	}
	sawTrack := false
	for i := 0; i < limit; i++ {
		if err := ctx.Err(); err != nil {
			return M{"ok": false, "error": err.Error()}
		}
		_ = spotifyNativeCommand(ctx, "play", M{"uri": candidates[i]})
		status, ok := waitSpotifyTrack(ctx, candidates[i], 5*time.Second)
		if ok {
			sawTrack = true
		}
		if !ok || (!queueContext && !spotifySearchTrackMatches(kind, requested, artist, status.Track)) {
			log.Printf("spotify search rejected: uri=%s ok=%v name=%q artists=%v", candidates[i], ok, status.Track.Name, status.Track.ArtistNames)
			continue
		}
		if kind == "album" && status.Track.AlbumURI != "" {
			if err := spotifyNativeCommand(ctx, "play", M{"uri": status.Track.AlbumURI}); err != nil {
				return M{"ok": false, "error": err.Error()}
			}
			album, ok := waitSpotifyTrack(ctx, "", 12*time.Second)
			if !ok {
				return M{"ok": false, "error": "Spotify не подтвердил воспроизведение альбома."}
			}
			status = album
		}
		if queueContext {
			for j := i + 1; j < len(candidates) && j <= i+20; j++ {
				_ = spotifyNativeCommand(ctx, "add_to_queue", M{"uri": candidates[j]})
			}
		}
		playback := status.playback()
		if err := unmute(); err != nil {
			return M{"ok": false, "error": "Не удалось вернуть звук после запуска Spotify."}
		}
		return M{"ok": true, "status": "playing", "backend": "spotify_api", "title": playback.Title,
			"artist": playback.Artist, "selected_uri": status.Track.URI, "shuffle": false}
	}
	if !sawTrack {
		return M{"ok": false, "error": "Проигрыватель Spotify не ответил. Повторите команду."}
	}
	return M{"ok": false, "error": "Spotify не нашёл точного совпадения. Другой трек не подставляю."}
}

func readSpotifyNative() spotifyNativeStatus {
	var status spotifyNativeStatus
	resp, err := spotifyNativeHTTP.Get(spotifyNativeAPI + "/status")
	if err == nil {
		defer resp.Body.Close()
		status.Available = resp.StatusCode == http.StatusNoContent ||
			(resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&status) == nil)
	}
	return status
}

func spotifyNativeToken(ctx context.Context) (string, error) {
	var reply struct{ Token string }
	if err := spotifyNativeJSON(ctx, http.MethodPost, "/token", &reply); err != nil {
		return "", err
	}
	if reply.Token == "" {
		return "", fmt.Errorf("Привяжите колонку через spotify.com/pair в настройках Spotify.")
	}
	return reply.Token, nil
}

func spotifyNativeJSON(ctx context.Context, method, path string, dst any) error {
	return spotifyNativeRequest(ctx, method, path, nil, dst)
}

func spotifyNativeRequest(ctx context.Context, method, path string, body any, dst any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	client := http.Client{Timeout: 6 * time.Second}
	req, err := http.NewRequestWithContext(ctx, method, spotifyNativeAPI+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("Локальный проигрыватель Spotify: HTTP %d", resp.StatusCode)
	}
	if dst == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(dst)
}

func spotifyNativePairing() (code, link string) {
	resp, err := spotifyNativeHTTP.Get(spotifyNativeAPI + "/auth/code")
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	var pairing struct {
		Code      string
		ExpiresAt time.Time `json:"expires_at"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&pairing) != nil ||
		len(pairing.Code) < 4 || len(pairing.Code) > 16 || time.Now().After(pairing.ExpiresAt) {
		return "", ""
	}
	return pairing.Code, "https://spotify.com/pair?" + url.Values{"code": {pairing.Code}}.Encode()
}

func (s spotifyNativeStatus) playback() playbackStatus {
	state := "PLAYING"
	if s.Paused {
		state = "PAUSED"
	}
	if s.Stopped || s.Buffering || s.Track.URI == "" {
		state = "IDLE"
	}
	return playbackStatus{App: "Spotify", State: state, Title: s.Track.Name, Artist: strings.Join(s.Track.ArtistNames, ", "), Playlist: s.ContextName, ContextURI: s.ContextURI, ContentID: s.Track.URI}
}

func syncSpotifyNativeVolume() {
	last := -1
	bufferingSince := time.Time{}
	recoveredStall := false
	recoveryAttempts := 0
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		status := readSpotifyNative()
		if status.Available && status.Buffering && !status.Stopped && status.Track.URI == "" && status.ContextURI != "" {
			if bufferingSince.IsZero() {
				bufferingSince = time.Now()
			} else if !recoveredStall && time.Since(bufferingSince) > 6*time.Second {
				recoveryAttempts++
				recoveredStall = recoveryAttempts >= 5
				// Allow a bounded retry if the first stop/play did not clear
				// the stall. Reset the timer so the next attempt waits again.
				bufferingSince = time.Time{}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				uri := status.ContextURI
				paused := status.Paused
				_ = spotifyNativeCommand(ctx, "stop", M{})
				if isNativeDJContext(uri) {
					device := status.DeviceID
					if device == "" {
						device = directSpotify.boundDeviceID()
					}
					if device != "" {
						_ = playSpotifyDJ(ctx, device)
					}
				} else {
					_ = spotifyNativeCommand(ctx, "play", M{"uri": uri, "paused": paused})
				}
				cancel()
			}
		} else {
			bufferingSince = time.Time{}
			recoveredStall = false
			recoveryAttempts = 0
		}
		resp, err := spotifyNativeHTTP.Get(spotifyNativeAPI + "/player/volume")
		if err != nil {
			last = -1
			continue
		}
		var volume struct{ Value, Max int }
		err = json.NewDecoder(resp.Body).Decode(&volume)
		resp.Body.Close()
		if err != nil || volume.Max != 48 || volume.Value < 0 || volume.Value > 48 {
			last = -1
			continue
		}
		native, err := nativeVolume()
		if err != nil || native < 0 || native > 48 {
			continue
		}
		switch {
		case native == volume.Value:
			last = native
		case native != last:
			// The speaker wins at startup and when both controls moved.
			body := bytes.NewBufferString(fmt.Sprintf("{\"volume\":%d}", native))
			resp, err := spotifyNativeHTTP.Post(spotifyNativeAPI+"/player/volume", "application/json", body)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					last = native
				}
			}
		default:
			if _, err := wampCallTimeout(750*time.Millisecond, "com.harman.volumeSet", nil, volume.Value); err == nil {
				last = volume.Value
			}
		}
	}
}

// go-librespot's pipe produces 44.1 kHz, stereo, signed 16-bit PCM.
func serveSpotifyPCM(path string) error {
	if err := ensureSpotifyFIFO(path); err != nil {
		return err
	}
	go syncSpotifyNativeVolume()
	for {
		if err := ensureSpotifyFIFO(path); err != nil {
			return err
		}
		input, err := os.OpenFile(path, os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		err = streamSpotifyPCM(input)
		input.Close()
		if err != nil {
			log.Printf("Spotify PCM stream ended: %v", err)
		}
	}
}

func ensureSpotifyFIFO(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := syscall.Mkfifo(path, 0600); err != nil && !os.IsExist(err) {
			return err
		}
		info, err = os.Stat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		return fmt.Errorf("%s is not a FIFO", path)
	}
	return nil
}

func streamSpotifyPCM(input *os.File) error {
	const blockBytes = 441 * 2 * 2 // 10 ms of stereo PCM
	block := make([]byte, blockBytes)
	n, err := io.ReadFull(input, block)
	if n == 0 {
		if err == io.EOF {
			return nil
		}
		return err
	}
	output, openErr := openPCMOutput(44100, 2, 0)
	if openErr != nil {
		return openErr
	}
	defer output.close()
	for {
		if writeErr := output.write(block[:n-n%4]); writeErr != nil {
			return writeErr
		}
		switch err {
		case io.EOF, io.ErrUnexpectedEOF:
			return output.finish()
		case nil:
			_ = input.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
			n, err = io.ReadFull(input, block)
			_ = input.SetReadDeadline(time.Time{})
			if os.IsTimeout(err) {
				if n >= 4 {
					_ = output.write(block[:n-n%4])
				}
				return output.finish()
			}
		default:
			return err
		}
	}
}

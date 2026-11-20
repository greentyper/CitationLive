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

// Spotify Connect keeps manually queued tracks when a new context is loaded.
// Clear them without releasing the active Connect device during replacement.
func resetSpotifyBeforePlay(ctx context.Context) error {
	native := readSpotifyNative()
	if native.ContextURI == "" && native.Track.URI == "" {
		return nil
	}
	return prepareSpotifyReplacement(ctx)
}

func prepareSpotifyReplacement(ctx context.Context) error {
	return spotifyNativeCommand(ctx, "stop", M{"keep_session": true})
}

func waitSpotifyReady(ctx context.Context, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for ctx.Err() == nil {
		if readSpotifyNativeContext(ctx).DeviceID != "" {
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

func playSpotifyNative(ctx context.Context, selection M, device, uri, kind string) (result M) {
	if kind == "playlist" && !strings.HasPrefix(uri, "spotify:playlist:") {
		return M{"ok": false, "error": "Spotify не вернул ссылку на плейлист."}
	}
	if err := resetSpotifyBeforePlay(ctx); err != nil {
		return M{"ok": false, "error": err.Error()}
	}
	defer func() {
		if result["ok"] == true {
			directSpotify.setPlaybackContext(selection)
		}
	}()
	if candidates, ok := selection["spotify_search_uris"].([]string); kind != "playlist" && ok && len(candidates) > 0 {
		return playSpotifySearch(ctx, selection, candidates)
	}
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
			_ = prepareSpotifyReplacement(ctx)
			if !waitSpotifyReady(ctx, 5*time.Second) {
				return M{"ok": false, "error": "Проигрыватель Spotify не отвечает."}
			}
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
		if have == want {
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
		return musicKey(requested) != "" && musicKey(track.AlbumName) != "" &&
			spotifyTrackTitleMatches(requested, track.AlbumName) &&
			(artist == "" || spotifyArtistMatches(artist, track.ArtistNames))
	case "track":
		return spotifyTrackTitleMatches(requested, track.Name) &&
			(artist == "" || spotifyArtistMatches(artist, track.ArtistNames))
	}
	return true
}

func waitSpotifyTrack(ctx context.Context, timeout time.Duration, matches func(spotifyNativeStatus) bool) (spotifyNativeStatus, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for ctx.Err() == nil {
		status := readSpotifyNativeContext(ctx)
		if status.Available && !status.Stopped && !status.Buffering && !status.Paused &&
			status.Track.URI != "" && status.Track.Name != "" && matches(status) {
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
func playSpotifySearch(ctx context.Context, selection M, candidates []string) (result M) {
	requested, _ := selection["requested_track"].(string)
	artist, _ := selection["requested_artist"].(string)
	kind, _ := selection["music_type"].(string)
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
	playAttempted := false
	defer func() {
		if playAttempted && result["ok"] != true {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Second)
			defer cancel()
			if stopErr := spotifyNativeCommand(cleanup, "stop", M{}); stopErr != nil {
				log.Printf("Spotify search cleanup: %v", stopErr)
				if pauseErr := spotifyNativeCommand(cleanup, "pause", M{}); pauseErr != nil {
					log.Printf("Spotify search cleanup: %v", pauseErr)
					result["error"] = "Не удалось остановить пробное воспроизведение Spotify."
				}
			}
		}
		_ = unmute()
	}()
	limit := min(len(candidates), 5)
	if kind == "album" {
		limit = min(len(candidates), 2)
	}
	sawTrack := false
	for i := 0; i < limit; i++ {
		if err := ctx.Err(); err != nil {
			return M{"ok": false, "error": err.Error()}
		}
		uri := candidates[i]
		playAttempted = true
		if err := spotifyNativeCommand(ctx, "play", M{"uri": uri}); err != nil {
			log.Printf("Spotify search preview failed: %v", err)
			continue
		}
		status, ok := waitSpotifyTrack(ctx, 5*time.Second, func(status spotifyNativeStatus) bool {
			return status.Track.URI == uri
		})
		if ok {
			sawTrack = true
		}
		if !ok || !spotifySearchTrackMatches(kind, requested, artist, status.Track) {
			log.Printf("spotify search rejected: uri=%s ok=%v name=%q artists=%v", candidates[i], ok, status.Track.Name, status.Track.ArtistNames)
			continue
		}
		if kind == "album" {
			albumURI := status.Track.AlbumURI
			if !strings.HasPrefix(albumURI, "spotify:album:") {
				continue
			}
			if err := spotifyNativeCommand(ctx, "play", M{"uri": albumURI}); err != nil {
				return M{"ok": false, "error": err.Error()}
			}
			album, ok := waitSpotifyTrack(ctx, 12*time.Second, func(status spotifyNativeStatus) bool {
				return status.ContextURI == albumURI
			})
			if !ok {
				return M{"ok": false, "error": "Spotify не подтвердил воспроизведение альбома."}
			}
			status = album
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
	return readSpotifyNativeContext(context.Background())
}

func readSpotifyNativeContext(ctx context.Context) spotifyNativeStatus {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spotifyNativeAPI+"/status", nil)
	if err != nil {
		return spotifyNativeStatus{}
	}
	resp, err := spotifyNativeHTTP.Do(req)
	if err != nil {
		return spotifyNativeStatus{}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return spotifyNativeStatus{Available: true}
	}
	if resp.StatusCode != http.StatusOK {
		return spotifyNativeStatus{}
	}
	const maxStatusBytes = 2 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBytes+1))
	var status *spotifyNativeStatus
	if err != nil || len(data) > maxStatusBytes || json.Unmarshal(data, &status) != nil || status == nil {
		return spotifyNativeStatus{}
	}
	status.Available = true
	return *status
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
				_ = prepareSpotifyReplacement(ctx)
				if isNativeDJContext(uri) {
					device := status.DeviceID
					if device == "" {
						device = directSpotify.boundDeviceID()
					}
					if device != "" {
						_ = startSpotifyDJ(ctx, device, "state_restore")
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

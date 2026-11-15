package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

var nativeHandoff atomic.Int32

func nativeMusicPlaying() bool {
	return nativeMusicPlayingWithin(4 * time.Second)
}

// nativeMusicPlayingFast bounds the WAMP fallback used on the activation path.
// The direct Spotify status is local; only an unknown status needs the native
// state call, and that call must not hold up touch feedback for seconds.
func nativeMusicPlayingFast() bool {
	return nativeMusicPlayingWithin(400 * time.Millisecond)
}

func nativeMusicPlayingWithin(timeout time.Duration) bool {
	if native := readSpotifyNative(); native.Available && !native.Stopped && !native.Paused && (native.Track.URI != "" || native.ContextURI != "") {
		return true
	}
	r, e := wampCallTimeout(timeout, "com.harman.stateGet", M{})
	if e != nil || len(r) < 5 {
		return false
	}
	state, _ := r[4].(map[string]any)
	music, _ := state["music"].(map[string]any)
	return music["state"] == "playing"
}

func nativeSpotify(query string) (bool, error) {
	r, e := requestNativeSpotify(query, false)
	quiet, _ := r["native_confirmation_stop_sent"].(bool)
	return quiet, e
}

func confirmedSpotify(query string) M {
	r, e := requestNativeSpotify(query, true)
	if e != nil {
		return M{"ok": false, "error": clean(e), "query": query}
	}
	return r
}

func confirmedSpotifyRequest(request M) M {
	query, _ := request["query"].(string)
	if query == "" || query == "spotify_api" {
		track, _ := request["requested_track"].(string)
		artist, _ := request["requested_artist"].(string)
		if track != "" && artist != "" {
			query = "play " + track + " by " + artist + " on Spotify"
		} else if track != "" && !strings.EqualFold(track, "музыка") {
			query = "play " + track + " on Spotify"
		} else if artist != "" {
			query = "play " + artist + " on Spotify"
		} else {
			query = "play music on Spotify"
		}
	}
	r, e := requestNativeSpotify(query, true, request)
	if e != nil {
		return M{"ok": false, "error": clean(e), "query": query}
	}
	return r
}

// Observe the original music service; no extra Cast sender connection is used.
func requestNativeSpotify(query string, confirm bool, expected ...M) (result M, err error) {
	nativeHandoff.Add(1)
	defer nativeHandoff.Add(-1)
	if len(expected) > 0 {
		if locale, ok := expected[0]["test_native_locale"].(string); ok && locale == "ru-RU" {
			if e := grpcCall("/assistant.api.ConfigSettingsService/SetLocaleOverride", pbMessage(1, []byte(locale))); e != nil {
				return nil, e
			}
			defer grpcCall("/assistant.api.ConfigSettingsService/SetLocaleOverride", pbMessage(1, []byte("en-US")))
			time.Sleep(700 * time.Millisecond)
		}
	}
	c, e := wampConn()
	if e != nil {
		return nil, e
	}
	defer c.Close()
	var redirect *mixerRedirect
	if !(len(expected) > 0 && expected[0]["allow_native_speech"] == true) {
		redirect, e = openTTSRedirect()
		if e != nil {
			return nil, e
		}
		defer func() {
			stats := redirect.close()
			if result != nil {
				result["native_tts_redirect"] = stats
				result["native_confirmation_silenced"] = stats["samples"].(int) > 0
			}
		}()
	}
	// End only the native conversation after its media action has completed.
	// This prevents a native follow-up microphone opening from re-waking Gemini.
	defer grpcCall("StopInteraction", []byte{8, 1})
	for i, topic := range []string{"com.harman.stateChanged", "com.harman.music.stateChanged"} {
		if e = c.WriteJSON([]any{32, 100 + i, M{}, topic}); e != nil {
			return nil, e
		}
		c.SetReadDeadline(time.Now().Add(4 * time.Second))
		var subscribed []any
		if e = c.ReadJSON(&subscribed); e != nil {
			return nil, e
		}
		if len(subscribed) < 2 || subscribed[0] != float64(33) {
			return nil, fmt.Errorf("native state subscription rejected")
		}
	}
	nativeQuery := query
	lowerQuery := strings.ToLower(strings.TrimSpace(query))
	if !strings.HasPrefix(lowerQuery, "включи ") && !strings.HasPrefix(lowerQuery, "поставь ") && !strings.HasPrefix(lowerQuery, "play ") {
		nativeQuery = "play " + query + " on Spotify"
	}
	if e = grpcCall("SendTextQuery", pbMessage(1, []byte(nativeQuery))); e != nil {
		return nil, e
	}
	quiet := false
	var mismatch M
	timeout := 28 * time.Second
	if len(expected) > 0 {
		if seconds, ok := expected[0]["test_timeout_seconds"].(float64); ok && seconds >= 5 && seconds <= 28 {
			timeout = time.Duration(seconds) * time.Second
		}
	}
	c.SetReadDeadline(time.Now().Add(timeout))
	for {
		var event []any
		if c.ReadJSON(&event) != nil {
			if mismatch != nil {
				_, _ = wampCall("com.harman.gva.pause")
				mismatch["ok"] = false
				mismatch["status"] = "wrong_track_rejected"
				mismatch["error"] = "Spotify не нашёл запрошенный трек; другое воспроизведение остановлено."
				return mismatch, nil
			}
			return M{"ok": false, "status": "request_submitted_but_playback_not_confirmed", "query": query, "native_confirmation_stop_sent": quiet, "error": "Не удалось подтвердить запуск нового воспроизведения. Не говори, что музыка включена."}, nil
		}
		if len(event) < 6 || event[0] != float64(36) {
			continue
		}
		kwargs, ok := event[5].(map[string]any)
		if !ok {
			continue
		}
		if kwargs["state"] == "playing" {
			title, _ := kwargs["title"].(string)
			artist, _ := kwargs["artist"].(string)
			if title != "" {
				result := M{"ok": true, "status": "playing", "query": query, "title": title, "artist": artist, "native_confirmation_stop_sent": quiet}
				if len(expected) > 0 && !musicMatches(expected[0], result) {
					mismatch = result
					continue
				}
				return result, nil
			}
		}
	}
}

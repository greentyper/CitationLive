package main

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

func (s *spotifyClient) mediaDeviceID() string {
	if s == directSpotify {
		if native := readSpotifyNative(); native.DeviceID != "" {
			s.bindDeviceID(native.DeviceID)
			return native.DeviceID
		}
	}
	s.mu.Lock()
	wanted := s.deviceID
	s.mu.Unlock()
	if wanted != "" {
		return wanted
	}
	if dev, err := s.chooseSpeaker(); err == nil && dev.ID != "" {
		s.bindDeviceID(dev.ID)
		return dev.ID
	}
	return ""
}

// A transport command never resolves a new playlist or sends context_uri/offset.
func (s *spotifyClient) controlPlayback(action string) error {
	if s == directSpotify {
		native := readSpotifyNative()
		if native.DeviceID == "" {
			return errors.New("Привяжите колонку через spotify.com/pair в настройках Spotify.")
		}
		nativeAction := action
		switch action {
		case "resume":
			if native.Track.URI == "" && native.ContextURI == "" {
				return errors.New("На колонке нет трека, который можно продолжить.")
			}
			nativeAction = "resume"
		case "pause", "stop", "next":
		case "previous":
			nativeAction = "prev"
		default:
			return errors.New("unknown Spotify media action")
		}
		return spotifyNativeCommand(context.Background(), nativeAction, M{})
	}
	device := s.mediaDeviceID()
	if device == "" {
		return errors.New("Нет привязанной колонки Spotify")
	}
	if action == "resume" {
		// Transfer playback from active user session (phone, PC, etc.) to this speaker and start playing
		transferErr := s.api("PUT", "/me/player", M{"device_ids": []string{device}, "play": true}, nil)
		if transferErr == nil {
			return nil
		}
		// If transfer returned an error, try direct resume on device
		playErr := s.api("PUT", "/me/player/play?device_id="+url.QueryEscape(device), nil, nil)
		if playErr == nil {
			return nil
		}
		// If device was dormant or returned 404, try waking the receiver and retrying transfer
		if strings.Contains(transferErr.Error(), "404") || strings.Contains(playErr.Error(), "404") {
			_ = s.wakeLocalSpotifyReceiver()
			time.Sleep(300 * time.Millisecond)
			if retryErr := s.api("PUT", "/me/player", M{"device_ids": []string{device}, "play": true}, nil); retryErr == nil {
				return nil
			}
		}
		return transferErr
	}
	method, path := "PUT", ""
	switch action {
	case "pause", "stop":
		path = "pause"
	case "next":
		method, path = "POST", "next"
	case "previous":
		method, path = "POST", "previous"
	default:
		return errors.New("unknown Spotify media action")
	}
	return s.api(method, "/me/player/"+path+"?device_id="+url.QueryEscape(device), nil, nil)
}

func spotifyNativeRepeatLabel(status spotifyNativeStatus) string {
	if status.RepeatTrack {
		return "track"
	}
	if status.RepeatContext {
		return "context"
	}
	return "off"
}

func spotifyNativePlaybackAction(ctx context.Context, action string) M {
	status := readSpotifyNative()
	if status.DeviceID == "" {
		return M{"ok": false, "error": "Привяжите колонку через spotify.com/pair в настройках Spotify."}
	}
	switch action {
	case "playback_modes":
		return M{"ok": true, "shuffle": status.Shuffle, "repeat": spotifyNativeRepeatLabel(status)}
	case "shuffle_on", "shuffle_off", "shuffle_toggle":
		enabled := action == "shuffle_on" || (action == "shuffle_toggle" && !status.Shuffle)
		if err := spotifyNativeCommand(ctx, "shuffle_context", M{"shuffle_context": enabled}); err != nil {
			return M{"ok": false, "error": "Spotify не подтвердил изменение перемешивания."}
		}
		return M{"ok": true, "action": action, "state": enabled}
	case "repeat_track", "repeat_context", "repeat_off":
		contextOn, trackOn := false, false
		if action == "repeat_track" {
			trackOn = true
		} else if action == "repeat_context" {
			contextOn = true
		}
		if err := spotifyNativeCommand(ctx, "repeat_context", M{"repeat_context": contextOn}); err != nil {
			return M{"ok": false, "error": "Spotify не подтвердил изменение повтора."}
		}
		if err := spotifyNativeCommand(ctx, "repeat_track", M{"repeat_track": trackOn}); err != nil {
			return M{"ok": false, "error": "Spotify не подтвердил изменение повтора."}
		}
		return M{"ok": true, "action": action, "repeat": spotifyNativeRepeatLabel(spotifyNativeStatus{RepeatContext: contextOn, RepeatTrack: trackOn})}
	case "save_current":
		if !strings.HasPrefix(status.Track.URI, "spotify:track:") {
			return M{"ok": false, "error": "На колонке сейчас нет песни, которую можно добавить в любимые."}
		}
		if err := spotifyNativeRequest(ctx, "POST", "/library/liked", M{"uris": []string{status.Track.URI}, "liked": true}, nil); err != nil {
			return M{"ok": false, "error": "Spotify не подтвердил добавление трека в любимые."}
		}
		return M{"ok": true, "saved": true, "title": status.Track.Name, "artist": strings.Join(status.Track.ArtistNames, ", ")}
	case "next_in_queue":
		return M{"ok": false, "error": "Очередь Spotify недоступна на этом проигрывателе."}
	default:
		return M{"ok": false, "error": "Неизвестная команда Spotify."}
	}
}

func (s *spotifyClient) playbackAction(ctx context.Context, action string) M {
	if s == directSpotify {
		return spotifyNativePlaybackAction(ctx, action)
	}
	device := s.mediaDeviceID()
	var state struct {
		Device  spotifyDevice
		Item    spotifyEntity
		Shuffle bool   `json:"shuffle_state"`
		Repeat  string `json:"repeat_state"`
	}
	if device == "" || s.apiContext(ctx, "GET", "/me/player", nil, &state) != nil ||
		state.Device.ID != device || !speakerDevice(state.Device) || state.Item.URI == "" {
		return M{"ok": false, "error": "Spotify сейчас не воспроизводит музыку на привязанной колонке."}
	}
	q := url.Values{"device_id": {device}}
	var path string
	switch action {
	case "next_in_queue":
		var queue struct {
			Current spotifyEntity `json:"currently_playing"`
			Queue   []spotifyEntity
		}
		if err := s.apiContext(ctx, "GET", "/me/player/queue", nil, &queue); err != nil {
			return M{"ok": false, "error": "Не удалось прочитать очередь Spotify."}
		}
		if queue.Current.URI != state.Item.URI || len(queue.Queue) == 0 || queue.Queue[0].Name == "" {
			return M{"ok": false, "error": "Spotify не показал следующий трек для колонки."}
		}
		next := queue.Queue[0]
		return M{"ok": true, "title": next.Name, "artist": entityArtist(next)}
	case "save_current":
		if !strings.HasPrefix(state.Item.URI, "spotify:track:") {
			return M{"ok": false, "error": "На колонке сейчас нет песни, которую можно добавить в любимые."}
		}
		params := url.Values{"uris": {state.Item.URI}}
		if err := s.apiContext(ctx, "PUT", "/me/library?"+params.Encode(), nil, nil); err != nil {
			if strings.Contains(err.Error(), "HTTP 403") {
				return M{"ok": false, "error": "Нужно обновить доступ к Spotify в настройках колонки, чтобы сохранять треки."}
			}
			return M{"ok": false, "error": "Spotify не подтвердил добавление трека в любимые."}
		}
		return M{"ok": true, "saved": true, "title": state.Item.Name, "artist": entityArtist(state.Item)}
	case "shuffle_on", "shuffle_off", "shuffle_toggle":
		enabled := action == "shuffle_on" || (action == "shuffle_toggle" && !state.Shuffle)
		q.Set("state", "false")
		if enabled {
			q.Set("state", "true")
		}
		path = "/me/player/shuffle?"
	case "repeat_track", "repeat_context", "repeat_off":
		q.Set("state", strings.TrimPrefix(action, "repeat_"))
		path = "/me/player/repeat?"
	case "playback_modes":
		return M{"ok": true, "shuffle": state.Shuffle, "repeat": state.Repeat}
	default:
		return M{"ok": false, "error": "Неизвестная команда Spotify."}
	}
	if err := s.apiContext(ctx, "PUT", path+q.Encode(), nil, nil); err != nil {
		return M{"ok": false, "error": "Spotify не подтвердил изменение режима воспроизведения."}
	}
	return M{"ok": true, "action": action, "state": q.Get("state")}
}

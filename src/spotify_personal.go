package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// playGenericMixLocked advances the alternation only after confirmed playback.
// The playback mutex also keeps rapid consecutive requests in order.
func (s *spotifyClient) playGenericMixLocked(ctx context.Context, play func(context.Context, string) M) M {
	order := [2]string{"DJ", "daylist"}
	if s.lastGenericMix == "DJ" {
		order[0], order[1] = order[1], order[0]
	}
	var result M
	for _, source := range order {
		if err := ctx.Err(); err != nil {
			break
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 13*time.Second)
		result = play(attemptCtx, source)
		cancel()
		if result["ok"] == true {
			s.lastGenericMix = source
			result["spotify_generic_source"] = source
			return result
		}
	}
	return M{"ok": false, "error": "Не удалось включить DJ или Daylist. Назови трек, исполнителя или плейлист.", "detail": result["error"]}
}

func (s *spotifyClient) playPersonalMix(ctx context.Context, source string) M {
	if s != directSpotify {
		return M{"ok": false, "error": "Для DJ и Daylist нужно подключить колонку к Spotify."}
	}
	device, err := s.chooseSpeaker()
	if err != nil {
		return M{"ok": false, "error": err.Error()}
	}
	name, uri, contextType := "DJ", spotifyDJURI, "dj"
	if source == "daylist" {
		playlist, err := s.findDaylist(ctx)
		if err != nil {
			return M{"ok": false, "error": err.Error()}
		}
		name, uri, contextType = playlist.Name, playlist.URI, "playlist"
	}
	selection := M{"spotify_shuffle": source == "daylist", "context_name": name, "context_uri": uri, "context_type": contextType}
	return playSpotifyNative(ctx, selection, device.ID, uri, "playlist")
}

func (s *spotifyClient) findDaylist(ctx context.Context) (spotifyEntity, error) {
	for offset := 0; offset < 1000; offset += 50 {
		var page spotifyPlaylistPage
		if err := s.apiContext(ctx, "GET", fmt.Sprintf("/me/playlists?limit=50&offset=%d", offset), nil, &page); err != nil {
			return spotifyEntity{}, errors.New("Не удалось прочитать библиотеку Spotify.")
		}
		for _, item := range page.Items {
			if item.Owner.ID == "spotify" && strings.HasPrefix(item.URI, "spotify:playlist:") &&
				item.URI != spotifyDJURI && isDaylistPlaylist(item.Name, item.Description, item.URI) {
				return item, nil
			}
		}
		if page.Next == "" || len(page.Items) == 0 {
			break
		}
	}
	return spotifyEntity{}, errors.New("Daylist не найден в библиотеке Spotify.")
}

// These are feature names, not artist/title substrings. Catalogue URI discovery
// still comes from the authenticated account and requires Spotify ownership.
func spotifyPersonalPlaylist(name string) string {
	switch musicKey(strings.TrimSpace(name)) {
	case "daylist", "deilist", "deilista":
		return "daylist"
	case "dj", "didzhei", "didzheia", "didzheya":
		return "DJ"
	}
	return ""
}

func spotifyRecommendedPlaylist(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "daylist", "discover weekly", "release radar", "открытия недели", "радар новинок":
		return true
	}
	for _, prefix := range []string{"daily mix ", "микс дня "} {
		if suffix, ok := strings.CutPrefix(name, prefix); ok {
			n, err := strconv.Atoi(suffix)
			return err == nil && n >= 1 && n <= 6
		}
	}
	return false
}

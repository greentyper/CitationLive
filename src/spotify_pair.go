package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type spotifyPlaylistPage struct {
	Items []spotifyEntity
	Next  string
}

func isDaylistPlaylist(name, description, uri string) bool {
	if strings.EqualFold(strings.TrimSpace(name), "daylist") {
		return true
	}
	description = strings.ToLower(description)
	if strings.Contains(description, "day in a playlist") ||
		strings.Contains(description, "inspired by your listening") ||
		strings.Contains(description, "день в плейлисте") ||
		strings.Contains(description, "на основе ваших прослушиваний") {
		return true
	}
	// Spotify uses this personalized-playlist URI family for daylist. It is a
	// fallback for accounts where both the name and description are localized.
	return strings.HasPrefix(uri, "spotify:playlist:37i9dQZF1EP")
}

// Resolve a search through the paired player's own Spotify session. The
// public Web API rejects the session token, while the Connect context service
// is the same catalog path the player uses for playback.
func spotifySearchURIs(ctx context.Context, query string) ([]string, error) {
	token, err := spotifyNativeToken(ctx)
	if err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("Пустой поисковый запрос Spotify.")
	}
	endpoint := "https://gew4-spclient.spotify.com/context-resolve/v1/spotify:search:" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, errors.New("Поиск Spotify недоступен.")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Поиск Spotify: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, errors.New("Spotify вернул некорректный результат поиска.")
	}
	return parseSpotifySearchContext(raw)
}

func parseSpotifySearchContext(raw []byte) ([]string, error) {
	var context struct {
		Pages []struct {
			Tracks []struct{ URI string }
		}
	}
	if err := json.Unmarshal(raw, &context); err != nil {
		return nil, errors.New("Spotify вернул некорректный результат поиска.")
	}
	seen := map[string]bool{}
	var uris []string
	for _, page := range context.Pages {
		for _, track := range page.Tracks {
			if strings.HasPrefix(track.URI, "spotify:track:") && !seen[track.URI] {
				seen[track.URI] = true
				uris = append(uris, track.URI)
			}
		}
	}
	if len(uris) == 0 {
		return nil, errors.New("Spotify не нашёл совпадений.")
	}
	return uris, nil
}

// Search the Spotify catalog for playlist entities using the paired session.
func pairedSpotifyCatalogPlaylists(ctx context.Context, query string) ([]spotifyEntity, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	token, err := spotifyNativeToken(ctx)
	if err != nil {
		return nil, err
	}
	payload := M{
		"operationName": "searchPlaylists",
		"variables": M{
			"searchTerm": query, "offset": 0, "limit": 12, "numberOfTopResults": 20,
			"includePreReleases": false, "includeAlbumPreReleases": false,
			"includeAudiobooks": true, "includeAuthors": true, "includeEpisodeContentRatingsV2": true,
		},
		"extensions": M{"persistedQuery": M{
			"version": 1, "sha256Hash": "d520014e748f9ea44f7707d8df1819867ac1205e8b7f3e28f22fe5fc858921b1",
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api-partner.spotify.com/pathfinder/v2/query", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("app-platform", "WebPlayer")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/131.0.0.0 Safari/537.36")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("поиск Spotify: HTTP %d", resp.StatusCode)
	}
	var reply struct {
		Errors []json.RawMessage
		Data   struct {
			Search struct {
				Playlists *struct {
					Items []struct {
						Data struct {
							Name, URI string
							Owner     struct {
								Data struct{ Username, URI string }
							} `json:"ownerV2"`
						}
					}
				}
			} `json:"searchV2"`
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&reply); err != nil || len(reply.Errors) > 0 || reply.Data.Search.Playlists == nil {
		return nil, errors.New("Spotify вернул некорректный ответ поиска плейлистов.")
	}
	var playlists []spotifyEntity
	for _, item := range reply.Data.Search.Playlists.Items {
		p := item.Data
		if !strings.HasPrefix(p.URI, "spotify:playlist:") || strings.TrimSpace(p.Name) == "" {
			continue
		}
		playlist := spotifyEntity{Name: p.Name, URI: p.URI}
		playlist.Owner.ID = p.Owner.Data.Username
		if playlist.Owner.ID == "" {
			playlist.Owner.ID = strings.TrimPrefix(p.Owner.Data.URI, "spotify:user:")
		}
		playlists = append(playlists, playlist)
	}
	return playlists, nil
}

func pairedSpotifyPlaylists(ctx context.Context, query string, out any) error {
	page, ok := out.(*spotifyPlaylistPage)
	if !ok {
		return fmt.Errorf("invalid Spotify playlist response type")
	}
	var reply struct {
		Items []struct {
			Name, URI   string
			Owner       string `json:"owner_username"`
			Description string
			Length      int
		}
		Offset, Limit, Total int
	}
	if err := spotifyNativeJSON(ctx, http.MethodGet, "/library/playlists?"+query, &reply); err != nil {
		return err
	}
	for _, playlist := range reply.Items {
		item := spotifyEntity{Name: playlist.Name, URI: playlist.URI, Description: playlist.Description}
		item.Owner.ID = playlist.Owner
		item.Items.Total = playlist.Length
		page.Items = append(page.Items, item)
	}
	if reply.Limit > 0 && reply.Offset+reply.Limit < reply.Total {
		page.Next = fmt.Sprintf("%d", reply.Offset+reply.Limit)
	}
	return nil
}

package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type spotifyTestTransport func(*http.Request) (*http.Response, error)

func (f spotifyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDirectClientRefusesPublicWebAPI(t *testing.T) {
	client := &spotifyClient{paired: true, deviceID: "citation", token: func(context.Context) (string, error) { return "test", nil }}
	if err := client.api(http.MethodGet, "/search?q=Get+Lucky", nil, nil); err == nil {
		t.Fatal("Pair mode attempted the public Spotify Web API")
	}
	if err := client.api(http.MethodGet, "/me/player", nil, nil); err == nil {
		t.Fatal("Pair mode attempted the public Spotify player API")
	}
}

func TestGenericMusicUsesPlayableLibraryPlaylist(t *testing.T) {
	client := &spotifyClient{
		deviceID: "citation", token: func(context.Context) (string, error) { return "test", nil },
	}
	client.http = &http.Client{Transport: spotifyTestTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/v1/me/playlists":
			body = `{"items":[{"name":"DJ","uri":"` + spotifyDJURI + `"},{"name":"Library mix","uri":"spotify:playlist:library"}]}`
		case "/v1/me/player/devices":
			body = `{"devices":[{"id":"other","type":"Speaker"},{"id":"citation","type":"Speaker"}]}`
		default:
			t.Fatalf("unexpected Spotify request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	request := prepareMusic(M{"music_type": "playlist", "spoken_request": "включи музыку"})
	got := client.resolve(request)
	if got["ok"] != true || got["spotify_uri"] != "spotify:playlist:library" || got["spotify_device_id"] != "citation" {
		t.Fatalf("unexpected playlist selection: %v", got)
	}
}

func TestParseSpotifySearchContextKeepsTrackURIs(t *testing.T) {
	raw := []byte(`{"pages":[{"tracks":[{"uri":"spotify:track:aaa"},{"uri":"spotify:episode:bbb"},{"uri":"spotify:track:aaa"}]},{"tracks":[{"uri":"spotify:track:ccc"}]}]}`)
	uris, err := parseSpotifySearchContext(raw)
	if err != nil || len(uris) != 2 || uris[0] != "spotify:track:aaa" || uris[1] != "spotify:track:ccc" {
		t.Fatalf("unexpected search result: %v, %v", uris, err)
	}
	if _, err := parseSpotifySearchContext([]byte(`{"pages":[]}`)); err == nil {
		t.Fatal("accepted an empty search result")
	}
}

func TestSpotifySearchTrackMatches(t *testing.T) {
	track := spotifyNativeTrack{
		Name:        "Example Song (feat. Example Guest)",
		AlbumName:   "Example Album (Deluxe Edition)",
		ArtistNames: []string{"Example Artist", "Example Guest"},
	}
	if !spotifySearchTrackMatches("track", "Example Song", "Example Artist", track) {
		t.Fatal("exact track with a featured-artist suffix was rejected")
	}
	if spotifySearchTrackMatches("track", "Example Song", "Other Artist", track) {
		t.Fatal("wrong artist was accepted")
	}
	if !spotifySearchTrackMatches("album", "Example Album", "", track) {
		t.Fatal("album deluxe edition was rejected")
	}
	if !spotifySearchTrackMatches("artist", "Example Artist", "", track) {
		t.Fatal("artist match was rejected")
	}
}

func TestSpotifyArtistMatchesTransliteration(t *testing.T) {
	cases := []struct {
		want  string
		names []string
		ok    bool
	}{
		{"Спин", []string{"Spean"}, true},
		{"Мумий", []string{"Mumiy"}, true},
		{"Кино", []string{"Kino"}, true},
		{"Кино", []string{"Другой"}, false},
		{"Зима", []string{"Zima"}, true},
	}
	for _, tc := range cases {
		if got := spotifyArtistMatches(tc.want, tc.names); got != tc.ok {
			t.Fatalf("artist match %q vs %v = %v, want %v", tc.want, tc.names, got, tc.ok)
		}
	}
	track := spotifyNativeTrack{Name: "Тестовый трек", ArtistNames: []string{"Primer"}}
	if !spotifySearchTrackMatches("track", "Тестовый трек", "Пример", track) {
		t.Fatal("Russian title and transliterated artist were rejected")
	}
}

func TestDaylistPlaylistMatching(t *testing.T) {
	cases := []struct {
		name, description, uri string
		ok                     bool
	}{
		{"funky soul funktastic sunday night", "Here's some funky soul — inspired by your listening on Sundays at night", "spotify:playlist:37i9dQZF1EP6YuccBxUcC1", true},
		{"daylist", "", "", true},
		{"Твой день", "Твой день в плейлисте", "", true},
		{"Ambient Mix", "Ambient music", "spotify:playlist:37i9dQZF1EQfPh9gDMrERq", false},
		{"Your Top Songs 2022", "Spotify Wrapped presents the songs that you loved most this year.", "spotify:playlist:37i9dQZF1F0sijgNaJdgit", false},
		{"Personalized", "", "spotify:playlist:37i9dQZF1EP6YuccBxUcC1", true},
	}
	for _, tc := range cases {
		if got := isDaylistPlaylist(tc.name, tc.description, tc.uri); got != tc.ok {
			t.Fatalf("isDaylistPlaylist(%q, %q, %q) = %v, want %v", tc.name, tc.description, tc.uri, got, tc.ok)
		}
	}
}

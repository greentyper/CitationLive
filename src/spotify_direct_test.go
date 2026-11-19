package main

import (
	"context"
	"encoding/json"
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
	if spotifySearchTrackMatches("album", "Example Album", "Other Artist", track) {
		t.Fatal("album by the wrong artist was accepted")
	}
	if spotifySearchTrackMatches("album", "Example", "", track) {
		t.Fatal("album title fragment was accepted")
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
		{"Donna", []string{"Madonna"}, false},
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

func mockSpotifySearch(t *testing.T, track spotifyNativeTrack, firstAlbumPollIsStale bool) (*[]string, *int) {
	t.Helper()
	oldTransport, oldDucking := http.DefaultTransport, ducking
	t.Cleanup(func() {
		http.DefaultTransport, ducking = oldTransport, oldDucking
	})
	var actions []string
	var albumPolls int
	var requestedURI string
	ducking = newDuckingManager(func(_ int, level float32) error {
		if level == 0 {
			actions = append(actions, "mute")
		} else {
			actions = append(actions, "unmute")
		}
		return nil
	}, nil)
	http.DefaultTransport = spotifyTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "127.0.0.1:18767" {
			t.Fatalf("unexpected host: %s", r.URL.Host)
		}
		body := "{}"
		switch r.URL.Path {
		case "/status":
			status := spotifyNativeStatus{DeviceID: "test-speaker"}
			switch requestedURI {
			case "":
			case track.URI:
				status.Track = track
				status.ContextURI = track.URI
			case track.AlbumURI:
				albumPolls++
				status.Track = track
				status.ContextURI = track.URI
				if !firstAlbumPollIsStale || albumPolls > 1 {
					status.Track.URI = "spotify:track:album-first"
					status.ContextURI = requestedURI
				}
			}
			encoded, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			body = string(encoded)
		case "/token":
			body = `{"token":"test"}`
		case "/player/play":
			var req struct{ URI string }
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			requestedURI = req.URI
			actions = append(actions, "play:"+requestedURI)
		case "/player/stop":
			actions = append(actions, "stop")
			requestedURI = ""
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	return &actions, &albumPolls
}

func TestSpotifySearchStopsRejectedTrackBeforeRestoringVolume(t *testing.T) {
	track := spotifyNativeTrack{URI: "spotify:track:wrong", Name: "Unrelated"}
	actions, _ := mockSpotifySearch(t, track, false)
	selection := M{"music_type": "track", "requested_track": "Another Name"}
	if got := playSpotifySearch(context.Background(), selection, []string{track.URI}); got["ok"] != false {
		t.Fatalf("unexpected success: %v", got)
	}
	if got := strings.Join(*actions, ","); got != "mute,play:spotify:track:wrong,stop,unmute" {
		t.Fatalf("rejected track must stop before unmute, got %s", got)
	}
}

func TestSpotifySearchWaitsForRequestedAlbumContext(t *testing.T) {
	track := spotifyNativeTrack{
		URI: "spotify:track:preview", Name: "Preview", AlbumURI: "spotify:album:requested", AlbumName: "Requested",
	}
	actions, albumPolls := mockSpotifySearch(t, track, true)
	selection := M{"music_type": "album", "requested_track": "Requested"}
	got := playSpotifySearch(context.Background(), selection, []string{track.URI})
	if got["ok"] != true || got["selected_uri"] != "spotify:track:album-first" || *albumPolls != 2 {
		t.Fatalf("expected confirmed album playback, got %v after %d album polls", got, *albumPolls)
	}
	if got := strings.Join(*actions, ","); got != "mute,play:spotify:track:preview,play:spotify:album:requested,unmute" {
		t.Fatalf("unexpected album calls: %s", got)
	}
}

func TestSpotifyMalformedNativeStatusDiscardsPartialDevice(t *testing.T) {
	old := spotifyNativeHTTP
	t.Cleanup(func() { spotifyNativeHTTP = old })
	for _, body := range []string{`{"device_id":"partial","track":5}`, "null", `{"device_id":"partial"} []`} {
		spotifyNativeHTTP = &http.Client{Transport: spotifyTestTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		if status := readSpotifyNative(); status.DeviceID != "" || status.Available {
			t.Errorf("invalid status %q yielded an available device: %+v", body, status)
		}
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
		{"Quiet Atlas", "Ambient music", "spotify:playlist:fixture-ambient", false},
		{"Your Top Songs 2022", "Spotify Wrapped presents the songs that you loved most this year.", "spotify:playlist:37i9dQZF1F0sijgNaJdgit", false},
		{"Personalized", "", "spotify:playlist:37i9dQZF1EP6YuccBxUcC1", true},
	}
	for _, tc := range cases {
		if got := isDaylistPlaylist(tc.name, tc.description, tc.uri); got != tc.ok {
			t.Fatalf("isDaylistPlaylist(%q, %q, %q) = %v, want %v", tc.name, tc.description, tc.uri, got, tc.ok)
		}
	}
}

func TestClosestLibraryPlaylist(t *testing.T) {
	playlist := func(name, id string) spotifyEntity {
		return spotifyEntity{Name: name, URI: "spotify:playlist:" + id}
	}
	orbit := playlist("Туманная Орбита Экспедиции", "orbit")
	ambient := playlist("Ambient Atlas Lab", "ambient")
	for _, tt := range []struct {
		label           string
		request         string
		library         []spotifyEntity
		want, ambiguous string
	}{
		{"normalized near title", "Туманистая орбита экспедиции", []spotifyEntity{ambient, orbit}, orbit.URI, ""},
		{"exact before fuzzy", "Туманная орбита экспедиции", []spotifyEntity{playlist("Туманистая орбита экспедиции", "other"), orbit}, orbit.URI, ""},
		{"same URI twice", "Ambient Atlas La", []spotifyEntity{ambient, ambient}, ambient.URI, ""},
		{"too short", "Pop", []spotifyEntity{playlist("Pops", "pop")}, "", ""},
		{"shared words are insufficient", "Кристаллическая орбита экспедиции", []spotifyEntity{orbit}, "", ""},
		{"ambiguous near titles", "Ambient Atlas Lab", []spotifyEntity{playlist("Ambient Atlas Lab A", "a"), playlist("Ambient Atlas Lab B", "b")}, "spotify:playlist:a", "spotify:playlist:b"},
	} {
		t.Run(tt.label, func(t *testing.T) {
			best, alternative := closestSpotifyPlaylist(tt.request, tt.library)
			uri := func(item *spotifyEntity) string {
				if item == nil {
					return ""
				}
				return item.URI
			}
			if uri(best) != tt.want || uri(alternative) != tt.ambiguous {
				t.Fatalf("matches = %q, %q; want %q, %q", uri(best), uri(alternative), tt.want, tt.ambiguous)
			}
		})
	}
}

func TestMoodPlaylistUsesARealLibraryContext(t *testing.T) {
	library := []spotifyEntity{
		{Name: "Quiet Lab Cue", URI: "spotify:track:chill"},
		{Name: "Workout Chill", URI: "spotify:playlist:workout"},
		{Name: "Ambient Atlas Lab", URI: "spotify:playlist:ambient"},
	}
	if got := matchSpotifyMoodPlaylist("спокойная музыка для фона", "chill relaxation", library); got == nil || got.URI != library[2].URI {
		t.Fatalf("selected %v, want Ambient Atlas Lab playlist", got)
	}
	if got := matchSpotifyMoodPlaylist("музыка на свадьбу", "wedding", library); got != nil {
		t.Fatalf("selected unrelated playlist %v", got)
	}
}

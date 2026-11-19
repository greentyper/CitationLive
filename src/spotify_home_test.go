package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSpotifyHomeRetainsPausedSession(t *testing.T) {
	status := spotifyNativeStatus{Available: true, DeviceID: "speaker", Paused: true}
	status.Track.URI = "spotify:track:paused"
	if active, known := spotifyHomeSessionWanted(status); !active || !known {
		t.Fatalf("paused Spotify session: active=%v known=%v", active, known)
	}
	status.Paused, status.Stopped = false, true
	if active, known := spotifyHomeSessionWanted(status); active || !known {
		t.Fatalf("stopped Spotify session: active=%v known=%v", active, known)
	}
	status.Stopped, status.Buffering = false, true
	if _, known := spotifyHomeSessionWanted(status); known {
		t.Fatal("buffering should keep the previous Cast session state")
	}
}

func TestSpotifyHomeSeekRequiresPosition(t *testing.T) {
	old := spotifyNativeHTTP
	t.Cleanup(func() { spotifyNativeHTTP = old })
	var seeks []string
	spotifyNativeHTTP = &http.Client{Transport: spotifyTestTransport(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		seeks = append(seeks, string(body))
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})}
	for _, body := range []string{`{}`, `{"position":null}`, `{"position":-1}`, `{"position":0}{}`, `{"position":0}`} {
		r := httptest.NewRequest(http.MethodPost, "/control/seek", strings.NewReader(body))
		r.SetPathValue("action", "seek")
		w := httptest.NewRecorder()
		spotifyHomeAPI(w, r)
		want := http.StatusBadRequest
		if body == `{"position":0}` {
			want = http.StatusNoContent
		}
		if w.Code != want {
			t.Errorf("seek with %s: HTTP %d, want %d", body, w.Code, want)
		}
	}
	if len(seeks) != 1 || seeks[0] != `{"position":0}` {
		t.Fatalf("expected one valid seek: %v", seeks)
	}
}

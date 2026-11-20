package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestInteractiveDJLoadsFirstSegmentAndKeepsServerMetadata(t *testing.T) {
	const continuation = "hm://lexicon-session-provider/context-resolve/v2/session/example?requestId=fixture"
	const next = "hm://lexicon-session-provider/context-resolve/v2/session/next?requestId=fixture"
	var paths []string
	client := &http.Client{Transport: spotifyTestTransport(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		if req.URL.Host != "gew4-spclient.spotify.com" || req.Header.Get("Authorization") != "Bearer fixture" {
			t.Fatalf("unexpected request: %s", req.URL)
		}
		var body string
		switch req.URL.Path {
		case spotifyDJSessionPath:
			if req.URL.Query().Get("reason") != "interactive" || req.URL.Query().Get("contextUri") != spotifyDJURI {
				t.Fatalf("unexpected DJ start parameters: %s", req.URL.RawQuery)
			}
			body = `{"uri":"` + spotifyDJURI + `","metadata":{"narration.welcome.title":"fixture"},"pages":[{"tracks":[],"next_page_url":"` + continuation + `"}]}`
		case spotifyDJSessionPath + "/example":
			if req.URL.Query().Get("requestId") != "fixture" {
				t.Fatal("continuation lost its request ID")
			}
			body = `{"tracks":[{"uri":"spotify:track:fixture"}],"next_page_url":"` + next + `"}`
		default:
			t.Fatalf("unexpected path: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	raw, _, err := resolveSpotifyDJContext(context.Background(), client, "fixture", "interactive", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Metadata map[string]string
		Pages    []struct {
			Tracks      []struct{ URI string }
			NextPageURL string `json:"next_page_url"`
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || got.Metadata["narration.welcome.title"] != "fixture" || len(got.Pages) != 1 ||
		len(got.Pages[0].Tracks) != 1 || got.Pages[0].Tracks[0].URI != "spotify:track:fixture" || got.Pages[0].NextPageURL != next {
		t.Fatalf("lost DJ session data: %s", raw)
	}
}

func TestDJContinuationKeepsRequestsOnSpotify(t *testing.T) {
	for _, raw := range []string{
		"https://example.invalid/lexicon-session-provider/context-resolve/v2/session/example",
		"hm://elsewhere/context-resolve/v2/session/example",
		"hm://lexicon-session-provider/context-resolve/v2/session/../example",
		"hm://lexicon-session-provider/context-resolve/v2/session/%2fhidden",
	} {
		if _, err := spotifyDJContinuationURL(raw); err == nil {
			t.Errorf("accepted invalid continuation %q", raw)
		}
	}
}

func TestDJSkipsRepeatedSegmentAndKeepsNextPage(t *testing.T) {
	const next = "hm://lexicon-session-provider/context-resolve/v2/session/next"
	client := &http.Client{Transport: spotifyTestTransport(func(req *http.Request) (*http.Response, error) {
		body := `{"uri":"` + spotifyDJURI + `","pages":[{"tracks":[
			{"uri":"spotify:track:old","metadata":{"segment":"old"}},
			{"uri":"spotify:track:old-follower","metadata":{"segment":"old"}},
			{"metadata":{"canonical_track_uri":"spotify:track:new","segment":"new","dj.jump_target":"true"}},
			{"uri":"spotify:track:new-follower","metadata":{"segment":"new"}}],
			"next_page_url":"` + next + `"}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	raw, opening, err := resolveSpotifyDJContext(context.Background(), client, "fixture", "interactive",
		[]djOpening{{URI: "spotify:track:old", Segment: "old"}})
	if err != nil {
		t.Fatal(err)
	}
	if opening.URI != "spotify:track:new" || opening.Segment != "new" ||
		strings.Contains(string(raw), "old-follower") || !strings.Contains(string(raw), next) {
		t.Fatalf("incorrect selected DJ segment: %s", raw)
	}
}

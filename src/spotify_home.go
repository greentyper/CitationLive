package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const spotifyHomeApp = "C17A7100"

var castMediaMu sync.Mutex

func serveSpotifyHome() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /receiver", func(w http.ResponseWriter, r *http.Request) {
		log.Print("Google Home receiver page requested")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, "/data/opt/citation-gemini/bin/spotify_home_receiver.html")
	})
	mux.HandleFunc("GET /caf.js", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "/data/opt/citation-gemini/bin/cast_receiver_framework.js")
	})
	mux.HandleFunc("POST /receiver-diagnostics", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 512))
		log.Printf("Google Home receiver: %q", data)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /status", spotifyHomeAPI)
	mux.HandleFunc("POST /control/{action}", spotifyHomeAPI)
	mux.HandleFunc("GET /clock.wav", func(w http.ResponseWriter, r *http.Request) {
		// The media element supplies the Cast timeline. Audible PCM continues
		// through the independent Spotify pipe.
		duration, err := strconv.Atoi(r.URL.Query().Get("ms"))
		if err != nil || duration < 1 || duration > 3600000 {
			http.Error(w, "invalid duration", http.StatusBadRequest)
			return
		}
		length := uint32(duration * 16)
		var wav spotifyHomeClock
		copy(wav[:], "RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x40\x1f\x00\x00\x80\x3e\x00\x00\x02\x00\x10\x00data")
		binary.LittleEndian.PutUint32(wav[4:], length+36)
		binary.LittleEndian.PutUint32(wav[40:], length)
		http.ServeContent(w, r, "clock.wav", time.Time{}, io.NewSectionReader(wav, 0, int64(length)+44))
	})
	startYouTubeDIAL := registerYouTubeDIAL(mux)
	listener, err := net.Listen("tcp", "127.0.0.1:18768")
	if err != nil {
		return err
	}
	go syncSpotifyHomeSession()
	startYouTubeDIAL()
	return (&http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second}).Serve(listener)
}

type spotifyHomeClock [44]byte

func (wav spotifyHomeClock) ReadAt(p []byte, offset int64) (int, error) {
	clear(p)
	if offset < int64(len(wav)) {
		copy(p, wav[offset:])
	}
	return len(p), nil
}

func spotifyHomeAPI(w http.ResponseWriter, r *http.Request) {
	path := "/status"
	var body io.Reader
	if r.Method == http.MethodPost {
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://127.0.0.1:18768" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch action := r.PathValue("action"); action {
		case "pause", "resume", "prev", "stop":
			path = "/player/" + action
		case "next":
			path = "/player/next"
			body = bytes.NewBufferString("{}")
		case "seek":
			path = "/player/seek"
			var req struct{ Position int }
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 128)).Decode(&req) != nil || req.Position < 0 {
				http.Error(w, "invalid position", http.StatusBadRequest)
				return
			}
			data, _ := json.Marshal(req)
			body = bytes.NewReader(data)
		default:
			http.NotFound(w, r)
			return
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, spotifyNativeAPI+path, body)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := spotifyNativeHTTP.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func syncSpotifyHomeSession() {
	hadSession := false
	for {
		status := readSpotifyNative()
		// Spotify clears Track while buffering the next song. Keep the Cast
		// session alive until the player explicitly reports that it stopped.
		if status.Available && !status.Buffering && (status.DeviceID == "" || status.Stopped || status.Track.URI != "") {
			_ = updateSpotifyHomeSession(status.DeviceID != "" && !status.Stopped && !status.Paused, &hadSession)
		}
		time.Sleep(2 * time.Second)
	}
}

func updateSpotifyHomeSession(active bool, hadSession *bool) error {
	castMediaMu.Lock()
	defer castMediaMu.Unlock()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", "127.0.0.1:8009", &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		*hadSession = false
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	cast := castStatusClient{conn}
	const ns = "urn:x-cast:com.google.cast.receiver"
	if err := cast.connectApp("receiver-0"); err != nil {
		return err
	}
	if err := cast.send("receiver-0", ns, M{"type": "GET_STATUS", "requestId": 1}); err != nil {
		return err
	}
	status, err := cast.receive(ns)
	if err != nil {
		return err
	}
	state, _ := status["status"].(map[string]any)
	if state == nil {
		return errors.New("missing Cast receiver status")
	}
	apps, _ := state["applications"].([]any)
	if len(apps) == 0 && active {
		if *hadSession {
			// Google Home's receiver STOP removes the app without issuing
			// a media command to its JavaScript.
			return spotifyNativeCommand(context.Background(), "stop", M{})
		}
		if err := cast.send("receiver-0", ns, M{"type": "LAUNCH", "requestId": 2, "appId": spotifyHomeApp, "silentLaunch": true}); err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(12 * time.Second))
		result, err := cast.receive(ns)
		if err == nil && result["type"] == "RECEIVER_STATUS" {
			*hadSession = spotifyHomeSessionID(result) != ""
		}
		return err
	}
	session := spotifyHomeSessionID(status)
	*hadSession = session != "" && active
	if session != "" && !active {
		return cast.send("receiver-0", ns, M{"type": "STOP", "requestId": 2, "sessionId": session})
	}
	return nil
}

func spotifyHomeSessionID(status M) string {
	state, _ := status["status"].(map[string]any)
	apps, _ := state["applications"].([]any)
	for _, entry := range apps {
		app, _ := entry.(map[string]any)
		if app["appId"] == spotifyHomeApp {
			id, _ := app["sessionId"].(string)
			return id
		}
	}
	return ""
}

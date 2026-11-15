package main

import (
	"encoding/json"
	"html/template"
	"net/http"
)

var spotifySetupPage = template.Must(template.New("spotify").Parse(`<!doctype html>
<html lang="ru"><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Подключение Spotify к CitationLive</title>
<style>body{font:18px system-ui;max-width:42rem;margin:3rem auto;padding:1rem;background:#111827;color:#eef2ff}a{color:#55e883}code{font-size:1.4em}</style>
<h1>Spotify на колонке</h1>
{{if .Ready}}<p>Колонка привязана. Скажите «включи музыку».</p>
{{else if .Link}}<p>Откройте <a href="{{.Link}}" target="_blank" rel="noopener noreferrer">spotify.com/pair</a>, войдите в свой аккаунт Spotify и подтвердите код <code>{{.Code}}</code>.</p>
<p><a href="/spotify/setup">Проверить подключение</a></p>
{{else}}<p>Пока нет кода привязки. <a href="/spotify/setup">Обновите страницу</a> через несколько секунд.</p>{{end}}
</html>`))

func localSpotifyForm(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	return origin == "" || origin == "http://127.0.0.1:28766" || origin == "http://localhost:28766" || origin == "http://127.0.0.1:8766"
}
func registerSpotifyHandlers() {
	http.HandleFunc("/spotify/play", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		if !localSpotifyForm(r) {
			w.WriteHeader(403)
			return
		}
		if ctl.busy() {
			http.Error(w, "Voice conversation is active", 409)
			return
		}
		var args M
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&args) != nil {
			w.WriteHeader(400)
			return
		}
		selection := directSpotify.resolve(prepareMusic(args))
		result := selection
		if selection["ok"] == true {
			result = directSpotify.playContext(r.Context(), selection)
			result["selection"] = selection
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})

	directSpotify.wakeReceiver = directSpotify.wakeLocalSpotifyReceiver
	directSpotify.activateReceiver = wakeNativeSpotifyProvider
	http.HandleFunc("/spotify/setup", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		view := struct {
			Ready      bool
			Code, Link string
		}{Ready: directSpotify.connected()}
		if !view.Ready {
			view.Code, view.Link = spotifyNativePairing()
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = spotifySetupPage.Execute(w, view)
	})
	http.HandleFunc("/spotify/status", func(w http.ResponseWriter, r *http.Request) {
		ready := directSpotify.connected()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(M{"connected": ready, "receiver_ready": ready})
	})
}

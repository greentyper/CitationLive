package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

type spotifyClient struct {
	mu               sync.Mutex
	playbackMu       sync.Mutex
	deviceID         string
	paired           bool
	contextName      string
	contextURI       string
	contextType      string
	token            func(context.Context) (string, error)
	http             *http.Client
	wakeReceiver     func() error
	activateReceiver func(context.Context) error
	lastMoodURI      map[string]string
	discoveryUsed    map[string]map[string]bool
}

var directSpotify = &spotifyClient{paired: true, http: &http.Client{Timeout: 10 * time.Second}, token: spotifyNativeToken}

func (s *spotifyClient) connected() bool {
	if native := readSpotifyNative(); native.DeviceID != "" {
		s.rememberDevice(native.DeviceID)
		return true
	}
	return s.savedDevice() != ""
}

func (s *spotifyClient) rememberDevice(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	s.deviceID = id
	s.mu.Unlock()
	_ = writeAtomicFile(filepath.Join(stateDirectory, "spotify-device.json"), []byte(id), 0600)
}

func (s *spotifyClient) savedDevice() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deviceID != "" {
		return s.deviceID
	}
	b, err := os.ReadFile(filepath.Join(stateDirectory, "spotify-device.json"))
	if err == nil && len(b) > 0 && len(b) < 128 {
		s.deviceID = strings.TrimSpace(string(b))
	}
	return s.deviceID
}

type spotifyPlaybackContext struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
	Type string `json:"type"`
}

func (s *spotifyClient) setPlaybackContext(selection M) {
	name, _ := selection["context_name"].(string)
	uri, _ := selection["context_uri"].(string)
	typ, _ := selection["context_type"].(string)
	if name == "" {
		return
	}
	s.mu.Lock()
	s.contextName, s.contextURI, s.contextType = name, uri, typ
	s.mu.Unlock()
	b, err := json.Marshal(spotifyPlaybackContext{Name: name, URI: uri, Type: typ})
	if err == nil {
		_ = writeAtomicFile(filepath.Join(stateDirectory, "spotify-context.json"), b, 0600)
	}
}

func (s *spotifyClient) playbackContext() (string, string, string) {
	s.mu.Lock()
	if s.contextName != "" {
		defer s.mu.Unlock()
		return s.contextName, s.contextURI, s.contextType
	}
	s.mu.Unlock()
	b, err := os.ReadFile(filepath.Join(stateDirectory, "spotify-context.json"))
	if err != nil {
		return "", "", ""
	}
	var saved spotifyPlaybackContext
	if json.Unmarshal(b, &saved) != nil || saved.Name == "" {
		return "", "", ""
	}
	s.mu.Lock()
	s.contextName, s.contextURI, s.contextType = saved.Name, saved.URI, saved.Type
	s.mu.Unlock()
	return saved.Name, saved.URI, saved.Type
}

func (s *spotifyClient) pause() error {
	if s == directSpotify {
		return spotifyNativeCommand(context.Background(), "pause", M{})
	}
	return s.api("PUT", "/me/player/pause", nil, nil)
}

func (s *spotifyClient) api(method, path string, body any, out any) error {
	return s.apiContext(context.Background(), method, path, body, out)
}
func (s *spotifyClient) apiContext(ctx context.Context, method, path string, body any, out any) error {
	if s.paired && method == http.MethodGet && strings.HasPrefix(path, "/me/playlists?") {
		return pairedSpotifyPlaylists(ctx, strings.TrimPrefix(path, "/me/playlists?"), out)
	}
	if s.paired {
		return errors.New("Публичный Spotify Web API не используется в режиме Pair.")
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, e := s.token(ctx)
		if e != nil {
			return e
		}
		var data []byte
		if body != nil {
			data, e = json.Marshal(body)
			if e != nil {
				return e
			}
		}
		req, e := http.NewRequestWithContext(ctx, method, "https://api.spotify.com/v1"+path, bytes.NewReader(data))
		if e != nil {
			return e
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		r, e := s.http.Do(req)
		if e != nil {
			return fmt.Errorf("Spotify API недоступен: %w", e)
		}
		if r.StatusCode == 401 && attempt == 0 {
			r.Body.Close()
			continue
		}
		if r.StatusCode < 200 || r.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
			r.Body.Close()
			return fmt.Errorf("Spotify API HTTP %d: %s", r.StatusCode, strings.TrimSpace(string(b)))
		}
		if out != nil && r.StatusCode != 204 {
			e = json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(out)
		}
		r.Body.Close()
		return e
	}
	return errors.New("Spotify отклонил авторизацию")
}

type spotifyDevice struct {
	Missing        bool `json:"-"`
	Native         bool `json:"-"`
	ID, Name, Type string
	Restricted     bool `json:"is_restricted"`
	Active         bool `json:"is_active"`
}

func (s *spotifyClient) devices() ([]spotifyDevice, error) {
	return s.devicesContext(context.Background())
}
func (s *spotifyClient) devicesContext(ctx context.Context) ([]spotifyDevice, error) {
	if s == directSpotify {
		native := readSpotifyNative()
		if native.DeviceID == "" {
			return nil, errors.New("Привяжите колонку через spotify.com/pair в настройках Spotify.")
		}
		return []spotifyDevice{{ID: native.DeviceID, Name: "Citation", Type: "Speaker", Active: !native.Stopped, Native: true}}, nil
	}
	var response struct{ Devices []spotifyDevice }
	e := s.apiContext(ctx, "GET", "/me/player/devices", nil, &response)
	return response.Devices, e
}
func speakerDevice(d spotifyDevice) bool {
	name := strings.ToLower(d.Name)
	return !d.Restricted && (strings.EqualFold(d.Type, "speaker") || strings.EqualFold(d.Type, "CastAudio") || strings.Contains(name, "citation") || strings.Contains(name, "harman"))
}

// wakeLocalSpotifyReceiver launches the Cast Spotify app and sends auth credentials.
func (s *spotifyClient) wakeLocalSpotifyReceiver() error {
	token, err := s.token(context.Background())
	if err != nil {
		return err
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", "127.0.0.1:8009", &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(6 * time.Second))
	c := castStatusClient{conn}

	const control = "urn:x-cast:com.google.cast.tp.connection"
	const receiver = "urn:x-cast:com.google.cast.receiver"
	const spotifyNs = "urn:x-cast:com.spotify.chromecast.secure.v1"

	if err := c.send("receiver-0", control, M{"type": "CONNECT"}); err != nil {
		return err
	}
	// Launch official Spotify Cast App ID CC32E753
	if err := c.send("receiver-0", receiver, M{"type": "LAUNCH", "appId": "CC32E753", "requestId": 1}); err != nil {
		return err
	}

	var transportID string
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		r, err := c.receive(receiver)
		if err != nil {
			break
		}
		if status, ok := r["status"].(map[string]any); ok {
			if apps, ok := status["applications"].([]any); ok {
				for _, a := range apps {
					if app, ok := a.(map[string]any); ok && app["appId"] == "CC32E753" {
						transportID, _ = app["transportId"].(string)
						break
					}
				}
			}
		}
		if transportID != "" {
			break
		}
	}
	if transportID == "" {
		return errors.New("failed to launch Spotify Cast app")
	}

	// Current Spotify Cast receivers use getInfo -> device-auth -> addUser,
	// not the removed setCredentials flow. See Spotcast's Spotify controller.
	s.mu.Lock()
	deviceID := s.deviceID
	s.mu.Unlock()
	if deviceID == "" {
		deviceID = fmt.Sprintf("%x", md5.Sum([]byte("Citation")))
	}
	if err := c.connectApp(transportID); err != nil {
		return err
	}
	authenticated := false
	deadline = time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if !authenticated {
			if err := c.send(transportID, spotifyNs, M{"type": "getInfo", "payload": M{"remoteName": "Citation", "deviceID": deviceID, "deviceAPI_isGroup": false}}); err != nil {
				return err
			}
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		ns, response, err := c.receiveAny()
		if err != nil {
			if e, ok := err.(net.Error); ok && e.Timeout() {
				continue
			}
			return err
		}
		if ns != spotifyNs {
			continue
		}
		switch response["type"] {
		case "getInfoResponse":
			payload, _ := response["payload"].(map[string]any)
			clientID, _ := payload["clientID"].(string)
			if clientID == "" {
				return errors.New("Spotify Cast omitted clientID")
			}
			blob, err := s.castDeviceToken(token, clientID, deviceID)
			if err != nil {
				return err
			}
			conn.SetDeadline(deadline)
			if err := c.send(transportID, spotifyNs, M{"type": "addUser", "payload": M{"blob": blob, "tokenType": "accesstoken"}}); err != nil {
				return err
			}
			authenticated = true
		case "addUserResponse":
			s.bindDeviceID(deviceID)
			return nil
		case "addUserError":
			return errors.New("Spotify Cast rejected device authentication")
		}
	}
	return errors.New("Spotify Cast authentication timed out")
}

func (s *spotifyClient) castDeviceToken(token, clientID, deviceID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	body, _ := json.Marshal(M{"clientId": clientID, "deviceId": deviceID})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://spclient.wg.spotify.com/device-auth/v1/refresh", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", errors.New("Spotify device authentication unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Spotify device authentication HTTP %d", resp.StatusCode)
	}
	var reply struct {
		AccessToken string `json:"accessToken"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply) != nil || reply.AccessToken == "" {
		return "", errors.New("Spotify device authentication returned no token")
	}
	return reply.AccessToken, nil
}

var localReceiverWaking atomic.Bool

func (s *spotifyClient) triggerWake() {
	if s.wakeReceiver != nil && localReceiverWaking.CompareAndSwap(false, true) {
		go func() {
			defer localReceiverWaking.Store(false)
			if err := s.wakeReceiver(); err != nil {
				log.Println("Spotify receiver wake:", clean(err))
			}
		}()
	}
}

func wakeLocalSpotifyReceiver() error {
	return directSpotify.wakeLocalSpotifyReceiver()
}

func (s *spotifyClient) bindDeviceID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deviceID != id {
		s.deviceID = id
	}
}

// boundDeviceID returns the speaker previously chosen for this account. It is
// used when a native status snapshot has no device ID yet; it never guesses a
// device from another installation.
func (s *spotifyClient) boundDeviceID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deviceID
}

func (s *spotifyClient) chooseSpeaker() (spotifyDevice, error) {
	if s == directSpotify {
		if native := readSpotifyNative(); native.DeviceID != "" {
			return spotifyDevice{ID: native.DeviceID, Type: "Speaker", Native: true, Active: !native.Stopped}, nil
		}
		return spotifyDevice{}, errors.New("привяжите колонку через spotify.com/pair в настройках Spotify")
	}
	s.mu.Lock()
	wanted := s.deviceID
	s.mu.Unlock()

	findLocalDevice := func(devs []spotifyDevice) (spotifyDevice, bool) {
		// Priority 1: Match already bound wanted ID
		if wanted != "" {
			for _, d := range devs {
				if d.ID == wanted && speakerDevice(d) {
					return d, true
				}
			}
		}
		// Priority 2: the only non-computer speaker in the account.
		var speakers []spotifyDevice
		for _, d := range devs {
			if speakerDevice(d) {
				speakers = append(speakers, d)
			}
		}
		if len(speakers) == 1 {
			return speakers[0], true
		}
		return spotifyDevice{}, false
	}

	devices, lookupError := s.devices()
	if dev, ok := findLocalDevice(devices); ok {
		s.bindDeviceID(dev.ID)
		return dev, nil
	}

	// Fast non-blocking path:
	// If wanted ID was previously saved, return it directly without blocking so resolve()
	// completes in <200ms. Receiver wake was triggered in background and will be ready
	// by the time playback starts.
	if wanted != "" {
		return spotifyDevice{ID: wanted, Name: "Citation", Type: "CastAudio", Missing: lookupError == nil}, nil
	}

	// If local speaker is not found and no wanted ID, wake up local Cast receiver
	_ = s.wakeLocalSpotifyReceiver()

	// Re-query devices after wake-up
	devices, _ = s.devices()
	if dev, ok := findLocalDevice(devices); ok {
		s.bindDeviceID(dev.ID)
		return dev, nil
	}

	return spotifyDevice{}, errors.New("не удалось обнаружить колонку в Spotify Connect")
}

type spotifyEntity struct {
	Name, URI, ID string
	Description   string
	Artists       []struct{ Name string }
	Owner         struct {
		DisplayName string `json:"display_name"`
		ID          string `json:"id"`
	} `json:"owner"`
	Tracks struct {
		Total int `json:"total"`
	} `json:"tracks"`
	Items struct {
		Total int `json:"total"`
	} `json:"items"`
}

func entityArtist(e spotifyEntity) string {
	names := []string{}
	for _, a := range e.Artists {
		names = append(names, a.Name)
	}
	return strings.Join(names, ", ")
}
func cleanMusicKeywords(s string) string {
	stopwords := []string{"что-нибудь", "что-то", "какую-нибудь", "какой-нибудь", "какую-то", "какой-то", "пожалуйста", "просто", "вруби", "включи", "сыграй", "поставь"}
	words := strings.Fields(s)
	var filtered []string
	for _, w := range words {
		lower := strings.ToLower(strings.Trim(w, ",.!?\"'«»"))
		isStop := false
		for _, sw := range stopwords {
			if lower == sw {
				isStop = true
				break
			}
		}
		if !isStop && lower != "" {
			filtered = append(filtered, w)
		}
	}
	res := strings.Join(filtered, " ")
	if res == "" {
		return s
	}
	return res
}

var moodMapping = map[string]string{
	"фэмили френдли":         "family friendly",
	"фэмили-френдли":         "family friendly",
	"семейная":               "family friendly",
	"семейное":               "family friendly",
	"семейный":               "family friendly",
	"для семьи":              "family friendly",
	"для всей семьи":         "family friendly",
	"спокойное":              "chill relaxation",
	"спокойную":              "chill relaxation",
	"спокойная":              "chill relaxation",
	"спокойный":              "chill relaxation",
	"спокойная музыка":       "chill relaxation",
	"расслабляющая":          "chill relaxation",
	"расслабляющую":          "chill relaxation",
	"расслабляющее":          "chill relaxation",
	"для релакса":            "relaxation",
	"релакс":                 "chill",
	"для сна":                "sleep",
	"музыка для сна":         "sleep",
	"для работы":             "work focus",
	"для учебы":              "study focus",
	"фоновая":                "background lo fi",
	"фоновое":                "background lo fi",
	"фоновую":                "background lo fi",
	"для фона":               "background lo fi",
	"веселое":                "happy upbeat",
	"веселую":                "happy upbeat",
	"веселая":                "happy upbeat",
	"позитивное":             "happy positive",
	"позитивная":             "happy positive",
	"позитивная музыка":      "happy positive",
	"позитивной музыки":      "happy positive",
	"позитивный микс":        "happy positive",
	"микс позитивной музыки": "happy positive",
	"злое":                   "angry",
	"злую":                   "angry",
	"злая":                   "angry",
	"злой":                   "angry",
	"яростное":               "angry",
	"агрессивное":            "angry",
	"агрессивную":            "angry",
	"агрессивная":            "angry",
	"angry":                  "angry",
	"aggressive":             "angry",
	"для вечеринки":          "party",
	"вечеринка":              "party",
	"тусовка":                "party",
	"бодрое":                 "energetic",
	"бодрую":                 "energetic",
	"бодрая":                 "energetic",
	"тренировка":             "workout",
	"для тренировки":         "workout",
	"для спорта":             "workout",
	"спорт":                  "workout",
	"рок":                    "rock",
	"джаз":                   "jazz",
	"поп":                    "pop",
	"рэп":                    "rap",
	"хип хоп":                "hip hop",
	"хип-хоп":                "hip hop",
	"электроника":            "electronic",
	"клубная":                "club",
	"классика":               "classical",
	"классическая":           "classical",
	"для машины":             "driving",
	"в дорогу":               "road trip",
	"романтичное":            "romantic",
	"романтическую":          "romantic",
	"для свидания":           "romantic",
	"мечтательное":           "dreamy",
	"атмосферное":            "atmospheric",
	"эпичное":                "epic",
	"мрачное":                "dark",
	"ностальгическое":        "nostalgia",
	"ностальгия":             "nostalgia",
	"для концентрации":       "work focus",
	"под танцы":              "dance",
}

func matchMood(text string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if mapped, ok := moodMapping[lower]; ok {
		return mapped, true
	}
	// Longest token-boundary match wins; map iteration must not choose the mood.
	bestKey, bestValue := "", ""
	for k, v := range moodMapping {
		if strings.Contains(" "+lower+" ", " "+k+" ") && len(k) > len(bestKey) {
			bestKey, bestValue = k, v
		}
	}
	if bestKey != "" {
		return bestValue, true
	}
	for _, word := range strings.Fields(lower) {
		for stem, theme := range map[string]string{"позитивн": "happy positive", "спокойн": "chill relaxation", "расслабляющ": "relaxation", "энергичн": "energetic", "бодр": "energetic", "весел": "happy upbeat", "весёл": "happy upbeat", "грустн": "sad", "печальн": "sad", "семейн": "family friendly", "классическ": "classical"} {
			if strings.HasPrefix(word, stem) {
				return theme, true
			}
		}
	}

	return "", false
}

func spotifyMoodScore(theme, title string) int {
	// Search ordering is not evidence of relevance. Require a matching
	// title word and prefer titles that match more of the requested theme.
	aliases := map[string]string{
		"chill relaxation": "chill relax relaxation relaxing calm ambient mellow",
		"relaxation":       "relax relaxation relaxing calm chill ambient",
		"happy upbeat":     "happy upbeat feelgood positive cheerful",
		"happy positive":   "happy positive upbeat feelgood cheerful",
		"angry":            "angry aggressive aggression rage anger furious ярость агрессия",
		"work focus":       "focus work concentration",
		"study focus":      "study focus concentration",
		"sad":              "sad melancholy melancholic",
		"sleep":            "sleep bedtime lullabies",
		"driving":          "driving drive road trip",
		"road trip":        "driving drive road trip",
		"energetic":        "energetic energy upbeat power",
		"workout":          "workout training gym fitness exercise",
		"party":            "party dance celebration",
		"romantic":         "romantic romance love date",
		"dreamy":           "dreamy dream dreamscape",
		"atmospheric":      "atmospheric atmosphere ambient",
		"epic":             "epic cinematic orchestral",
		"dark":             "dark haunting gothic",
		"nostalgia":        "nostalgia nostalgic throwback",
	}
	opposites := map[string]string{
		"workout":          "chill calm sleep bedtime relaxation",
		"energetic":        "chill calm sleep bedtime relaxation",
		"party":            "sleep bedtime sad melancholy",
		"sleep":            "workout gym party angry aggressive energetic upbeat",
		"work focus":       "party workout sleep bedtime",
		"study focus":      "party workout sleep bedtime",
		"chill relaxation": "workout gym party angry aggression",
		"angry":            "calm sleep bedtime relaxation",
	}
	terms := theme
	if expanded := aliases[theme]; expanded != "" {
		terms = expanded
	}
	words := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	rejected := strings.Fields("radio радио " + opposites[theme])
	for _, word := range words {
		if slices.Contains(rejected, word) {
			return 0
		}
	}
	score := 0
	for _, term := range strings.Fields(strings.ToLower(terms)) {
		switch term {
		case "music", "playlist", "mix", "songs", "tracks", "для", "музыка", "музыку", "плейлист", "микс":
			continue
		}
		for _, word := range words {
			if word == term {
				score += 20
				break
			}
		}
	}
	return score
}

func spotifyMoodGenreMatches(theme, request, title string) bool {
	switch theme {
	case "work focus", "study focus", "sleep":
		isJazz := func(text string) bool {
			key := musicKey(text)
			return strings.Contains(key, "jazz") || strings.Contains(key, "dzhaz")
		}
		return isJazz(request) == isJazz(title)
	default:
		return true
	}
}

func (s *spotifyClient) resolve(request M) (result M) {
	started := time.Now()
	defer func() {
		if result != nil {
			result["resolve_ms"] = time.Since(started).Milliseconds()
		}
	}()
	kind, _ := request["music_type"].(string)
	if kind == "" {
		kind = "track"
	}
	name, _ := request["requested_track"].(string)
	artist, _ := request["requested_artist"].(string)
	surprise := artist == "" && request["playlist_mode"] != "named" && spotifySurpriseRequest(name)
	if kind == "artist" && name == "" {
		name = artist
	}
	genericMusic := name == "" || strings.EqualFold(name, "музыка") || strings.EqualFold(name, "музыку") || strings.EqualFold(name, "что-нибудь") || strings.EqualFold(name, "моя музыка") || strings.EqualFold(name, "песня") || strings.EqualFold(name, "песню") || strings.EqualFold(name, "трек")
	personalMode := ""
	if surprise {
		name, kind, personalMode = "удиви меня", "playlist", "surprise"
	} else if (genericMusic || surprise) && artist == "" && request["playlist_mode"] != "named" {
		// A generic request can play a regular playlist from the user's library.
		// DJ and Daylist need extra Spotify features and can fail on this account.
		var library spotifyPlaylistPage
		libraryErr := s.api("GET", "/me/playlists?limit=50", nil, &library)
		if libraryErr == nil {
			_, previous, _ := s.playbackContext()
			if s == directSpotify {
				native := readSpotifyNative()
				if strings.HasPrefix(native.ContextURI, "spotify:playlist:") {
					previous = native.ContextURI
				}
			}
			var selected *spotifyEntity
			for i := range library.Items {
				playlist := &library.Items[i]
				if !strings.HasPrefix(playlist.URI, "spotify:playlist:") || playlist.URI == spotifyDJURI ||
					playlist.Name == "" || spotifyPersonalPlaylist(playlist.Name) != "" ||
					(s == directSpotify && playlist.Items.Total == 0) {
					continue
				}
				selected = playlist
				if playlist.URI != previous {
					break
				}
			}
			if selected != nil {
				spokenQuery, _ := request["query"].(string)
				return s.spotifySelection(selected, "playlist", selected.Name, "", spokenQuery)
			}
		}
		if s == directSpotify && libraryErr != nil {
			return M{"ok": false, "error": "Не удалось прочитать библиотеку Spotify на колонке."}
		}
		name, kind = "спокойная музыка", "playlist"
	} else if artist == "" {
		if personal := spotifyPersonalPlaylist(name); personal != "" {
			name, kind, personalMode = personal, "playlist", personal
		}
	}
	if s == directSpotify && personalMode == "DJ" {
		item := spotifyEntity{Name: "DJ", URI: spotifyDJURI}
		item.Owner.ID = "spotify"
		spokenQuery, _ := request["query"].(string)
		return s.spotifySelection(&item, "playlist", name, "", spokenQuery)
	}
	if s == directSpotify && personalMode == "daylist" {
		var library spotifyPlaylistPage
		if err := s.api("GET", "/me/playlists?limit=50", nil, &library); err == nil {
			for _, playlist := range library.Items {
				if isDaylistPlaylist(playlist.Name, playlist.Description, playlist.URI) && playlist.URI != spotifyDJURI {
					spokenQuery, _ := request["query"].(string)
					selection := s.spotifySelection(&playlist, "playlist", playlist.Name, "", spokenQuery)
					selection["context_type"] = "playlist"
					return selection
				}
			}
		}
		return M{"ok": false, "error": "Daylist не найден в библиотеке Spotify."}
	}

	cleanedName := cleanMusicKeywords(name)
	theme, knownMood := matchMood(cleanedName)
	discovery := kind == "playlist" && request["playlist_mode"] != "named" && (surprise || genericMusic || request["playlist_mode"] == "mood" || knownMood || strings.Contains(strings.ToLower(name), "музык"))
	if !knownMood {
		theme = cleanedName
		if extra, _ := request["search_query"].(string); strings.TrimSpace(extra) != "" {
			theme = extra
		}
	}
	lowerName := strings.ToLower(cleanedName)
	if strings.Contains(lowerName, "русск") && strings.Contains(lowerName, "рок") {
		theme = "russian rock"
	}
	log.Printf("spotify resolve: kind=%q name=%q artist=%q theme=%q discovery=%v knownMood=%v direct=%v", kind, name, artist, theme, discovery, knownMood, s == directSpotify)
	// Prefer editorial playlists over equally relevant user playlists.

	if name == "" {
		return M{"ok": false, "error": "Не расслышал название для поиска Spotify."}
	}
	if s == directSpotify {
		spokenQuery, _ := request["query"].(string)
		if kind == "track" || kind == "artist" || kind == "album" {
			query := strings.TrimSpace(name + " " + artist)
			if kind == "artist" {
				query = strings.TrimSpace(artist + " " + name)
			}
			if kind == "album" {
				query = name
			}
			uris, err := spotifySearchURIs(context.Background(), query)
			if err != nil {
				return M{"ok": false, "error": err.Error()}
			}
			var fallbacks []string
			if kind == "album" && artist != "" {
				fallbacks = append(fallbacks, strings.TrimSpace(name+" "+artist))
			}
			if kind == "track" && artist != "" && !strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(artist)) {
				fallbacks = append(fallbacks, name)
			}
			if kind == "artist" && artist != "" && !strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(artist)) {
				fallbacks = append(fallbacks, artist)
			}
			for _, fallback := range fallbacks {
				more, extraErr := spotifySearchURIs(context.Background(), fallback)
				if extraErr != nil {
					continue
				}
				seen := map[string]bool{}
				merged := make([]string, 0, len(uris)+len(more))
				for _, uri := range append(uris, more...) {
					if !seen[uri] {
						seen[uri] = true
						merged = append(merged, uri)
					}
				}
				uris = merged
			}
			selection := s.spotifySelection(&spotifyEntity{Name: name, URI: uris[0]}, kind, name, artist, spokenQuery)
			selection["spotify_search_uris"] = uris
			return selection
		}
		if kind == "playlist" && personalMode == "" {
			return s.resolvePairedPlaylist(name, theme, spokenQuery, discovery)
		}
	}
	query := name
	if kind == "track" {
		query = "track:" + name
		if artist != "" {
			query += " artist:" + artist
		}
	}
	var found []spotifyEntity

	if kind == "playlist" && !discovery && personalMode == "" {
		requestedKey := musicKey(name)
		for offset := 0; offset < 1000; offset += 50 {
			var own spotifyPlaylistPage
			if s.api("GET", fmt.Sprintf("/me/playlists?limit=50&offset=%d", offset), nil, &own) != nil {
				break
			}
			for _, playlist := range own.Items {
				if strings.HasPrefix(playlist.URI, "spotify:playlist:") && musicKey(playlist.Name) == requestedKey {
					found = append(found, playlist)
				}
			}
			if len(found) > 0 {
				break
			}
			if own.Next == "" || len(own.Items) == 0 {
				break
			}
		}
	}
	queries := []string{query}
	switch personalMode {
	case "surprise":
		queries = []string{"Daily Mix", "Discover Weekly", "Release Radar", "daylist"}
	}
	if discovery && personalMode == "" && knownMood {
		queries = []string{theme}
		if theme == "angry" {
			queries = append(queries, "aggressive")
		}
	}
	if extra, ok := request["search_query"].(string); discovery && personalMode == "" && !knownMood && ok && len(extra) > 0 && len(extra) <= 160 && musicKey(extra) != musicKey(query) {
		queries = append(queries, extra)
	}
	if len(found) != 0 {
		queries = nil
	}
	seenQueries := map[string]bool{}
	for _, catalogQuery := range queries {
		key := musicKey(catalogQuery)
		if seenQueries[key] {
			continue
		}
		seenQueries[key] = true

		var response struct {
			Tracks    struct{ Items []spotifyEntity }
			Artists   struct{ Items []spotifyEntity }
			Albums    struct{ Items []spotifyEntity }
			Playlists struct{ Items []spotifyEntity }
		}
		// REST search has its own limit contract; Web playlist search uses 50
		// separately. Do not copy the Web limit into Developer REST requests.
		limit := "10"
		q := url.Values{"q": {catalogQuery}, "type": {kind}, "limit": {limit}}
		if e := s.api("GET", "/search?"+q.Encode(), nil, &response); e != nil {
			return M{"ok": false, "error": e.Error()}
		}
		switch kind {
		case "track":
			found = response.Tracks.Items
		case "artist":
			found = response.Artists.Items
		case "album":
			found = response.Albums.Items
		case "playlist":
			found = append(found, response.Playlists.Items...)
		default:
			return M{"ok": false, "error": "Неподдерживаемый тип поиска Spotify."}
		}
	}
	if personalMode != "" {
		filtered := []spotifyEntity{}
		for _, item := range found {
			// Exact service-owned feature names only. Never accept a user list,
			// artist radio, or "This Is Daylist" as the personalized feature.
			feature := strings.ToLower(strings.TrimSpace(item.Name))
			matches := (feature == "daylist" || feature == "dj") && strings.EqualFold(feature, personalMode)
			if surprise {
				matches = spotifyRecommendedPlaylist(item.Name)
			}
			if item.Owner.ID == "spotify" && matches {
				filtered = append(filtered, item)
			}
		}
		found = filtered
		if len(found) == 0 {
			if surprise {
				return M{"ok": false, "error": "Spotify не вернул персональные подборки для этого аккаунта."}
			}
			return M{"ok": false, "error": "Персональный daylist / DJ недоступен в этом Spotify-аккаунте. Другой плейлист не подставляю."}
		}
	}

	var selected *spotifyEntity
	best := -1
	fallbackScore := -1
	var fallbackItem *spotifyEntity
	discoveryScores := make([]int, len(found))

	wordsMatch := func(s1, s2 string) bool {
		w1 := strings.Fields(strings.ToLower(s1))
		w2 := strings.ToLower(s2)
		matchCount := 0
		for _, w := range w1 {
			cleanW := musicKey(w)
			if len(cleanW) >= 3 && strings.Contains(musicKey(w2), cleanW) {
				matchCount++
			}
		}
		return len(w1) > 0 && matchCount > 0 && matchCount >= len(w1)-1
	}

	for i, item := range found {
		if item.URI == "" || strings.TrimSpace(item.Name) == "" {
			continue
		}
		if kind == "track" && !spotifyTrackTitleMatches(name, item.Name) {
			continue
		}
		if kind == "playlist" && request["playlist_mode"] == "named" && personalMode == "" && musicKey(item.Name) != musicKey(name) {
			continue
		}
		// Discovery must prefer an official Spotify editorial playlist. A user
		// playlist with a matching word must not hide an official result. If no
		// official result exists, keep the best mood-matching user playlist.
		if discovery && kind == "playlist" && personalMode == "" && !strings.EqualFold(item.Owner.ID, "spotify") {
			if !surprise {
				relevance := spotifyMoodScore(theme, item.Name)
				if relevance >= 20 && spotifyMoodGenreMatches(theme, name, item.Name) && relevance > fallbackScore {
					fallbackScore = relevance
					fallbackItem = &found[i]
				}
			}
			continue
		}
		score := 0
		if discovery {
			score = max(35, 100-i)
			if surprise {
				score = 100
			}
			if personalMode == "" {
				relevance := spotifyMoodScore(theme, item.Name)
				if relevance == 0 || !spotifyMoodGenreMatches(theme, name, item.Name) {
					continue
				}
				score = max(85, score) + relevance
			}
		} else if musicKey(item.Name) == musicKey(name) {
			score = 100
		} else if strings.Contains(musicKey(item.Name), musicKey(name)) || strings.Contains(musicKey(name), musicKey(item.Name)) {
			score = 80
		} else if closeMusicTitle(item.Name, name) || wordsMatch(name, item.Name) || wordsMatch(item.Name, name) {
			score = 65
		} else {
			continue
		}
		if artist != "" && kind != "artist" && kind != "playlist" && !strings.Contains(musicKey(entityArtist(item)), musicKey(artist)) {
			continue
		}
		// For playlists: heavily prioritize official Spotify-curated playlists / mixes over random users
		if kind == "playlist" {
			ownerID := strings.ToLower(item.Owner.ID)
			if ownerID == "spotify" {
				score += 500
			}
			totalTracks := item.Tracks.Total
			if totalTracks == 0 {
				totalTracks = item.Items.Total
			}
			if !discovery && totalTracks >= 100 {
				score += 40
			} else if !discovery && totalTracks >= 30 {
				score += 20
			} else if !discovery && totalTracks > 0 && totalTracks < 20 {
				score -= 30
			}
		}
		discoveryScores[i] = score
		if score > best {
			best = score
			selected = &found[i]
		}
	}
	if discovery && selected == nil && fallbackItem != nil {
		selected = fallbackItem
	}
	if discovery && selected != nil && strings.EqualFold(selected.Owner.ID, "spotify") {
		// Sample without replacement from the relevant official results. Duplicate
		// search hits must not make a playlist more likely or consume extra turns.
		candidates := []*spotifyEntity{}
		seen := map[string]bool{}
		for i := range found {
			item := &found[i]
			if item.Owner.ID == "spotify" && !seen[item.URI] && discoveryScores[i] >= best-15 {
				candidates = append(candidates, item)
				seen[item.URI] = true
			}
		}
		s.mu.Lock()
		if s.lastMoodURI == nil {
			s.lastMoodURI = map[string]string{}
		}
		if s.discoveryUsed == nil {
			s.discoveryUsed = map[string]map[string]bool{}
		}
		key := musicKey(name)
		if len(s.discoveryUsed) >= 64 && s.discoveryUsed[key] == nil {
			s.discoveryUsed = map[string]map[string]bool{}
			s.lastMoodURI = map[string]string{}
		}
		used := s.discoveryUsed[key]
		if used == nil {
			used = map[string]bool{}
		}
		available := []*spotifyEntity{}
		for _, item := range candidates {
			if !used[item.URI] {
				available = append(available, item)
			}
		}
		if len(available) == 0 {
			used = map[string]bool{}
			for _, item := range candidates {
				if len(candidates) == 1 || item.URI != s.lastMoodURI[key] {
					available = append(available, item)
				}
			}
		}
		if len(available) > 0 {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(available))))
			index := 0
			if err == nil {
				index = int(n.Int64())
			}
			selected = available[index]
			used[selected.URI] = true
		}
		s.discoveryUsed[key] = used
		s.lastMoodURI[key] = selected.URI
		s.mu.Unlock()
	}
	if selected == nil {
		if discovery && personalMode == "" {
			return M{"ok": false, "error": "Подходящую подборку Spotify для этого настроения не нашёл. Уточни настроение или жанр."}
		}
		return M{"ok": false, "error": "Точного совпадения в Spotify не нашёл; уточни название. Другой трек не подставляю."}
	}
	spokenQuery, _ := request["query"].(string)
	return s.spotifySelection(selected, kind, name, artist, spokenQuery)
}

func (s *spotifyClient) resolvePairedPlaylist(name, theme, spokenQuery string, discovery bool) M {
	var library []spotifyEntity
	complete := false
	for offset := 0; offset < 1000; offset += 50 {
		var page spotifyPlaylistPage
		if err := s.api(http.MethodGet, fmt.Sprintf("/me/playlists?limit=50&offset=%d", offset), nil, &page); err != nil {
			return M{"ok": false, "error": "Не удалось прочитать библиотеку Spotify на колонке."}
		}
		library = append(library, page.Items...)
		if page.Next == "" || len(page.Items) == 0 {
			complete = true
			break
		}
	}
	match := func(items []spotifyEntity) (*spotifyEntity, *spotifyEntity) {
		if discovery {
			return matchSpotifyMoodPlaylist(name, theme, items), nil
		}
		return closestSpotifyPlaylist(name, items)
	}
	selected, alternative := match(library)
	if alternative != nil {
		return M{"ok": false, "error": fmt.Sprintf("В библиотеке есть похожие плейлисты «%s» и «%s». Уточни название.", selected.Name, alternative.Name)}
	}
	if selected == nil {
		if !complete {
			return M{"ok": false, "error": "Не удалось проверить все плейлисты в библиотеке Spotify."}
		}
		query := name
		if discovery && strings.TrimSpace(theme) != "" {
			query = theme
		}
		items, err := pairedSpotifyCatalogPlaylists(context.Background(), query)
		if err != nil {
			log.Println("Spotify catalog search:", clean(err))
			return M{"ok": false, "error": "Не удалось выполнить поиск плейлистов в каталоге Spotify."}
		}
		selected, alternative = match(items)
		if alternative != nil {
			return M{"ok": false, "error": fmt.Sprintf("В Spotify есть похожие плейлисты «%s» и «%s». Уточни название.", selected.Name, alternative.Name)}
		}
		if selected == nil {
			return M{"ok": false, "error": "Подходящий плейлист не найден ни в библиотеке, ни в каталоге Spotify."}
		}
		log.Printf("Spotify catalog playlist selected: %q -> %q", query, selected.Name)
	}
	return s.spotifySelection(selected, "playlist", selected.Name, "", spokenQuery)
}

func closestSpotifyPlaylist(requested string, library []spotifyEntity) (best, alternative *spotifyEntity) {
	key := []rune(musicKey(requested))
	if len(key) == 0 {
		return nil, nil
	}
	// Allow common transcription errors in longer titles. A shared last word
	// or one typo in a very short title should not select another playlist.
	limit := min(5, len(key)/5)
	bestDistance, nextDistance := limit+1, limit+1
	seen := make(map[string]bool)
	for i := range library {
		playlist := &library[i]
		if !strings.HasPrefix(playlist.URI, "spotify:playlist:") || seen[playlist.URI] {
			continue
		}
		seen[playlist.URI] = true
		title := []rune(musicKey(playlist.Name))
		if len(title) == 0 || len(title) > len(key)+limit || len(title)+limit < len(key) {
			continue
		}
		distance := musicTitleDistance(key, title)
		if distance == 0 {
			return playlist, nil
		}
		if distance < bestDistance {
			alternative, nextDistance = best, bestDistance
			best, bestDistance = playlist, distance
		} else if distance < nextDistance {
			alternative, nextDistance = playlist, distance
		}
	}
	if nextDistance > bestDistance+1 {
		alternative = nil
	}
	return best, alternative
}

func matchSpotifyMoodPlaylist(request, theme string, library []spotifyEntity) *spotifyEntity {
	requestKey := musicKey(request)
	var best *spotifyEntity
	bestScore := 0
	for i := range library {
		playlist := &library[i]
		if !strings.HasPrefix(playlist.URI, "spotify:playlist:") || playlist.URI == spotifyDJURI ||
			strings.TrimSpace(playlist.Name) == "" || spotifyPersonalPlaylist(playlist.Name) != "" {
			continue
		}
		if requestKey != "" && musicKey(playlist.Name) == requestKey {
			return playlist
		}
		score := spotifyMoodScore(theme, playlist.Name)
		if score == 0 || !spotifyMoodGenreMatches(theme, request, playlist.Name) {
			continue
		}
		if playlist.Items.Total > 0 {
			score += 2
		}
		if strings.EqualFold(playlist.Owner.ID, "spotify") {
			score++
		}
		if score > bestScore {
			best, bestScore = playlist, score
		}
	}
	return best
}

func (s *spotifyClient) spotifySelection(selected *spotifyEntity, kind, name, artist, query string) M {
	device, e := s.chooseSpeaker()
	if e != nil {
		return M{"ok": false, "error": e.Error()}
	}
	label := selected.Name
	if kind == "playlist" {
		label = "плейлист «" + label + "»"
	} else if kind == "album" {
		label = "альбом «" + label + "»"
	} else {
		label = "«" + label + "»"
	}
	if who := entityArtist(*selected); who != "" {
		label += ", " + who
	}
	contextName := selected.Name
	contextType := kind
	if selected.URI == spotifyDJURI {
		contextName, contextType = "DJ", "dj"
	} else if kind == "artist" && artist != "" {
		contextName = artist
	}
	return M{"ok": true, "status": "ready_to_play", "backend": "spotify_api", "music_type": kind, "spotify_uri": selected.URI, "spotify_device_id": device.ID, "spotify_device_active": device.Active, "spotify_device_missing": device.Missing, "spotify_device_native": device.Native, "requested_track": name, "requested_artist": artist, "title": selected.Name, "playlist_owner": selected.Owner.ID, "spotify_shuffle": !strings.EqualFold(selected.Name, "DJ") || selected.Owner.ID != "spotify", "official_playlist": kind == "playlist" && selected.Owner.ID == "spotify", "artist": entityArtist(*selected), "query": query, "context_name": contextName, "context_uri": selected.URI, "context_type": contextType, "announcement": "Включаю " + label + " в Spotify."}
}

// A fragment such as Acht must not silently select Achtung! Optimism. Allow
// transliteration and small spelling differences, plus explicit version suffixes.
func spotifyTrackTitleMatches(requested, actual string) bool {
	if musicKey(requested) == musicKey(actual) || closeMusicTitle(requested, actual) {
		return true
	}
	for _, separator := range []string{" (", " - ", " ["} {
		if at := strings.Index(actual, separator); at > 0 && musicKey(requested) == musicKey(actual[:at]) {
			return true
		}
	}
	return false
}
func (s *spotifyClient) play(selection M) M { return s.playContext(context.Background(), selection) }
func (s *spotifyClient) playContext(ctx context.Context, selection M) (out M) {
	started := time.Now()
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	defer func() {
		if out != nil {
			out["play_ms"] = time.Since(started).Milliseconds()
		}
	}()
	uri, _ := selection["spotify_uri"].(string)
	device, _ := selection["spotify_device_id"].(string)
	nativeDevice, _ := selection["spotify_device_native"].(bool)
	kind, _ := selection["music_type"].(string)
	if device == "" || !strings.HasPrefix(uri, "spotify:") {
		return M{"ok": false, "error": "Нет выбранного устройства или записи Spotify."}
	}
	if nativeDevice {
		return playSpotifyNative(ctx, selection, device, uri, kind)
	}

	shuffle := kind == "playlist" || kind == "artist"
	// DJ's generated sequence includes its own segments and must remain ordered.
	explicitShuffle, shuffleSet := selection["spotify_shuffle"].(bool)
	shuffleSet = shuffleSet && (kind == "playlist" || kind == "artist")
	if shuffleSet && (kind == "playlist" || kind == "artist") {
		shuffle = explicitShuffle
	}
	var startupSilence func() error
	playIssued := false
	active, _ := selection["spotify_device_active"].(bool)
	body := M{"context_uri": uri}
	if kind == "track" {
		body = M{"uris": []string{uri}}
	}

	playOnDevice := func(devID string) error {
		if !active {
			if e := s.apiContext(ctx, "PUT", "/me/player", M{"device_ids": []string{devID}, "play": false}, nil); e == nil {
				active = true
			}
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		// Rebuild shuffle for each context launch: setting true on an already
		// shuffled receiver preserves its previous queue and starting track.
		if shuffle && s == directSpotify && startupSilence == nil {
			var e error
			startupSilence, e = ducking.holdSpotifyStartupSilence()
			if e != nil {
				return errors.New("Не удалось подготовить тихий запуск Spotify.")
			}
		}
		if shuffle || (shuffleSet && !explicitShuffle && kind == "playlist") {
			if e := s.apiContext(ctx, "PUT", "/me/player/shuffle?state=false&device_id="+url.QueryEscape(devID), nil, nil); e != nil {
				return e
			}
		}
		playIssued = true
		if e := s.apiContext(ctx, "PUT", "/me/player/play?device_id="+url.QueryEscape(devID), body, nil); e != nil {
			return e
		}
		return nil
	}

	// Retry loop with backoff on HTTP 404 (Device not found)
	// giving the newly launched Cast Spotify receiver time to register with Spotify cloud
	var err error
	confirmed := false
	attemptedNativeWake := false
	var activationMs int64
	registrationDeadline := time.Now().Add(12 * time.Second)
	defer func() {
		if (startupSilence != nil && !confirmed) || (ctx.Err() != nil && playIssued) {
			_, _ = wampCall("com.harman.gva.pause")
			_ = s.pause()
			if startupSilence != nil {
				_ = stopSpotifyCastApp()
			}
		}
		if startupSilence != nil {
			if e := startupSilence(); e != nil && out != nil {
				out["ok"] = false
				out["error"] = "Unable to restore Spotify startup volume: " + e.Error()
			}
			if out != nil {
				out["startup_silenced"] = true
			}
		}
		if out != nil {
			out["activation_ms"] = activationMs
		}
		if out != nil {
			if attemptedNativeWake {
				out["activation"] = "native_provider"
			} else {
				out["activation"] = "active_receiver"
			}
		}
	}()

	activate := func() error {
		attemptedNativeWake = true
		var e error
		if startupSilence == nil && s == directSpotify {
			startupSilence, e = ducking.holdSpotifyStartupSilence()
		}
		if e != nil {
			return errors.New("Не удалось заглушить стартовый поток Spotify; активация отменена.")
		}
		t := time.Now()
		e = s.activateReceiver(ctx)
		activationMs = time.Since(t).Milliseconds()
		if e != nil {
			return e
		}
		s.mu.Lock()
		bound := s.deviceID
		s.mu.Unlock()
		if bound != "" {
			device = bound
		}
		active = false
		// The linked provider normally activates this receiver itself. Do not
		// enqueue a redundant asynchronous transfer(play=false) after its wake:
		// that pause can overtake the following play command in Spotify.
		if devices, e := s.devicesContext(ctx); e == nil {
			for _, d := range devices {
				if d.ID == device && speakerDevice(d) {
					active = d.Active
					break
				}
			}
		}
		// Discoverability is eventually consistent: appearing in /devices does
		// not mean transfer/play already works. Give registration its own full
		// window AFTER activation, even when the initial device list was stale.
		registrationDeadline = time.Now().Add(12 * time.Second)
		return nil
	}
	if missing, _ := selection["spotify_device_missing"].(bool); missing && s.activateReceiver != nil {
		if e := ctx.Err(); e != nil {
			return M{"ok": false, "error": e.Error()}
		}
		if e := activate(); e != nil {
			return M{"ok": false, "error": e.Error()}
		}
	}
	start := time.Now()
	backoff := 400 * time.Millisecond

	for {
		if e := ctx.Err(); e != nil {
			return M{"ok": false, "error": e.Error()}
		}
		err = playOnDevice(device)
		if err == nil {
			break
		}

		is404 := strings.Contains(err.Error(), "404") || strings.Contains(strings.ToLower(err.Error()), "not found")
		if is404 && !nativeDevice && s.activateReceiver != nil && !attemptedNativeWake {
			if e := activate(); e != nil {
				return M{"ok": false, "error": e.Error()}
			}
			continue
		}

		if !is404 || nativeDevice || time.Now().After(registrationDeadline) {
			break
		}
		// A cached active flag is no longer trustworthy after a missing-device
		// response; retry the transfer as well as play after registration.
		active = false

		log.Printf("Spotify play attempt returned %v; waiting for speaker to wake (elapsed %v)...", err, time.Since(start).Round(time.Millisecond))
		if e := waitSpotifyRegistration(ctx, backoff); e != nil {
			return M{"ok": false, "error": e.Error()}
		}
		if backoff < 1500*time.Millisecond {
			backoff += 250 * time.Millisecond
		}

		// Re-check devices in case ID changed or was dynamically reported
		devs, _ := s.devicesContext(ctx)
		for _, d := range devs {
			if speakerDevice(d) && (d.ID == device || strings.Contains(strings.ToLower(d.Name), "citation") || strings.Contains(strings.ToLower(d.Name), "harman")) {
				if d.ID != "" && d.ID != device {
					log.Printf("Target device ID updated from %s to %s", device, d.ID)
					device = d.ID
					active = d.Active
					s.bindDeviceID(d.ID)
				}
				break
			}
		}
	}

	if err != nil {
		return M{"ok": false, "error": err.Error()}
	}

	if shuffle {
		// Play commands are asynchronous. Wait for the requested context before
		// enabling shuffle so the first track is allowed to finish.
		ready := false
		for i := 0; i < 16; i++ {
			var state struct {
				Playing bool `json:"is_playing"`
				Device  spotifyDevice
				Context struct{ URI string }
			}
			if e := ctx.Err(); e != nil {
				return M{"ok": false, "error": e.Error()}
			}
			if s.apiContext(ctx, "GET", "/me/player", nil, &state) == nil && state.Playing && state.Device.ID == device && state.Context.URI == uri {
				ready = true
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if !ready {
			return M{"ok": false, "error": "Spotify не подтвердил загрузку выбранного плейлиста."}
		}
		if e := s.apiContext(ctx, "PUT", "/me/player/shuffle?state=true&device_id="+url.QueryEscape(device), nil, nil); e != nil {
			return M{"ok": false, "error": e.Error()}
		}
	}

	for i := 0; i < 20; i++ {
		if e := ctx.Err(); e != nil {
			return M{"ok": false, "error": e.Error()}
		}
		var state struct {
			Playing bool `json:"is_playing"`
			Shuffle bool `json:"shuffle_state"`
			Device  spotifyDevice
			Item    spotifyEntity
			Context struct{ URI string }
		}
		if s.apiContext(ctx, "GET", "/me/player", nil, &state) == nil && state.Playing && (!shuffle || state.Shuffle) && (!shuffleSet || state.Shuffle == explicitShuffle) && state.Device.ID == device &&
			((kind == "track" && state.Item.URI == uri) || (kind != "track" && state.Context.URI == uri)) {

			if s == directSpotify {
				var local playbackStatus
				var e error
				if nativeDevice {
					local = readSpotifyNative().playback()
				} else {
					local, e = readPlaybackStatus()
				}
				if e != nil || !spotifyLocalPlaybackMatches(local, state.Item) {
					if e := waitSpotifyRegistration(ctx, 350*time.Millisecond); e != nil {
						return M{"ok": false, "error": e.Error()}
					}
					continue
				}
			}
			confirmed = true
			return M{"ok": true, "status": "playing", "backend": "spotify_api", "title": state.Item.Name, "artist": entityArtist(state.Item), "selected_uri": uri, "shuffle": state.Shuffle}
		}
		time.Sleep(350 * time.Millisecond)
	}
	return M{"ok": false, "status": "playback_unconfirmed", "error": "Spotify принял команду, но нужное устройство и URI не подтвердились."}
}

func spotifyLocalPlaybackMatches(local playbackStatus, item spotifyEntity) bool {
	if local.App != "Spotify" || local.State != "PLAYING" || item.Name == "" || musicKey(local.Title) != musicKey(item.Name) {
		return false
	}
	// Some Cast builds omit contentId or report an HTTPS resource. When a
	// Spotify track URI is present, require the same exact track as the API.
	return !strings.HasPrefix(local.ContentID, "spotify:track:") || local.ContentID == item.URI
}

func waitSpotifyRegistration(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

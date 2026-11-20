package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

func musicKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	key := b.String()
	key = strings.ReplaceAll(key, "ё", "е")
	return transliterateMusic(key)
}

func transliterateMusic(s string) string {
	const alphabet = "абвгдеёжзийклмнопрстуфхцчшщъыьэюя"
	values := []string{"a", "b", "v", "g", "d", "e", "e", "zh", "z", "i", "i", "k", "l", "m", "n", "o", "p", "r", "s", "t", "u", "f", "kh", "ts", "ch", "sh", "shch", "", "y", "", "e", "yu", "ya"}
	var out strings.Builder
	for _, r := range s {
		found := false
		for i, c := range []rune(alphabet) {
			if unicode.ToLower(r) == c {
				v := values[i]
				if unicode.IsUpper(r) && v != "" {
					v = strings.ToUpper(v[:1]) + v[1:]
				}
				out.WriteString(v)
				found = true
				break
			}
		}
		if !found {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func prepareMusic(args M) M {
	if reason, _ := args["validation_error"].(string); reason != "" {
		return M{"ok": false, "error": reason}
	}
	track, _ := args["track"].(string)
	artist, _ := args["artist"].(string)
	kind, _ := args["music_type"].(string)
	if kind == "" {
		kind = "track"
		if strings.TrimSpace(track) == "" && strings.TrimSpace(artist) != "" {
			kind = "artist"
		}
	}
	if kind != "track" && kind != "playlist" && kind != "album" && kind != "artist" {
		return M{"ok": false, "error": "Неизвестный тип музыки."}
	}
	track = strings.TrimSpace(track)
	artist = strings.TrimSpace(artist)
	if track == "" && artist == "" {
		if kind == "playlist" || args["spoken_request"] != nil {
			track = "музыка"
			kind = "playlist"
		}
	}
	if (track == "" && artist == "") || len(track) > 220 || len(artist) > 180 || strings.ContainsAny(track+artist, "\r\n\x00\"") {
		return M{"ok": false, "error": "Укажи название песни и исполнителя отдельными полями, без перевода и выдумывания."}
	}
	query := "Включи исполнителя «" + artist + "» в Spotify"
	label := artist
	if track != "" {
		query = "Включи песню «" + track + "»"
		label = "«" + track + "»"
		if artist != "" {
			query += " исполнителя «" + artist + "»"
			label += ", " + artist
		}
		query += " в Spotify"
	}
	if spoken, _ := args["spoken_request"].(string); strings.TrimSpace(spoken) != "" {
		query = strings.TrimSpace(spoken)
		lower := strings.ToLower(query)
		if !strings.HasPrefix(lower, "включи") && !strings.HasPrefix(lower, "поставь") && !strings.HasPrefix(lower, "play ") {
			query = "Включи " + query
		}
		if !strings.Contains(lower, "spotify") && !strings.Contains(lower, "спотиф") {
			query += " на Spotify"
		}
	}
	return M{"ok": true, "status": "ready_to_play", "music_type": kind, "query": query, "requested_track": track, "requested_artist": artist, "playlist_mode": args["playlist_mode"], "search_query": args["search_query"],
		"announcement": "Скажи только: Включаю " + label + " в Spotify. Музыка будет запущена после завершения твоей фразы. Не говори, что уже включил."}
}

var artistBoundary = regexp.MustCompile(`(?i)\s+(?:исполнителя|исполнитель|группы|группу|от группы|от исполнителя)\s+`)
var musicPrefix = regexp.MustCompile(`(?i)^\s*(?:(?:пожалуйста|включи|включить|поставь|проиграй|найди|мне|песню|трек|композицию)\s+)+`)
var musicProvider = regexp.MustCompile(`(?i)\s+(?:в|на|on)\s+(?:spotify|спотифай|спотифае)[.!?,\s]*$`)
var musicIntentBoundary = regexp.MustCompile(`(?i)(?:^|[\s,.!?])(?:включи|поставь|вруби|проиграй|play)\s+`)

func spokenMusicIntent(text string) bool {
	playback, music := false, false
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		switch word {
		case "громкость", "громкости", "громче", "погромче", "тише", "потише", "volume", "louder", "quieter", "hlasitosť", "hlasnejšie", "tichšie":
			return false
		case "включи", "включить", "поставь", "поставить", "вруби", "врубить", "проиграй", "проиграть", "сыграй", "сыграть", "запусти", "запустить", "давай",
			"play", "put", "start", "pusti", "pustit", "pustiť", "prehraj", "prehrať", "zahraj", "zahrať", "zapni", "zapnúť":
			playback = true
		case "музыку", "музыка", "музыки", "песню", "песня", "песни", "песенку", "трек", "треки", "плейлист", "альбом", "исполнителя", "исполнитель", "композицию", "группу", "группы", "подборку", "микс",
			"диджей", "диджея", "дейлист", "дэйлист", "daylist", "dj",
			"music", "song", "songs", "track", "tracks", "playlist", "album", "artist", "hudbu", "hudba", "skladbu", "pesničku":
			music = true
		}
	}
	if playback || music {
		return true
	}
	cleaned := cleanMusicKeywords(text)
	_, knownMood := matchMood(cleaned)
	return knownMood || spotifyPersonalPlaylist(cleaned) != ""
}

func spotifySurpriseRequest(text string) bool {
	key := musicKey(text)
	for _, wake := range []string{"heygoogle", "okaygoogle", "okeigugl", "okeigugol", "okeigoogle", "kheigugl", "eigoogle"} {
		key = strings.TrimPrefix(key, wake)
	}
	switch key {
	case "udivimenya", "udivimenyapozhaluista", "surpriseme":
		return true
	default:
		return false
	}
}

func hasLatinLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) && ((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return true
		}
	}
	return false
}

func hasCyrillicLetter(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

func literalMusicFields(args M, transcript string) M {
	if quickMediaAction(transcript) == "play" {
		return M{"music_type": "playlist", "playlist_mode": "mood", "spoken_request": transcript}
	}
	text := strings.Trim(strings.TrimSpace(musicProvider.ReplaceAllString(transcript, "")), " .!?«»\"")
	if spotifySurpriseRequest(text) {
		return M{"track": "удиви меня", "music_type": "playlist", "playlist_mode": "mood", "spoken_request": transcript}
	}
	if !spokenMusicIntent(text) {
		return M{"validation_error": fmt.Sprintf("Пользователь сказал %q, это обычная реплика в разговоре, а не команда включить музыку. Ответь пользователю обычным голосом на его реплику, не вызывай функции музыки.", text)}
	}
	kind, _ := args["music_type"].(string)
	if boundary := artistBoundary.FindStringIndex(text); boundary != nil && (kind == "" || kind == "track") {
		track := strings.Trim(musicPrefix.ReplaceAllString(text[:boundary[0]], ""), " .!?«»\"")
		artist := strings.Trim(text[boundary[1]:], " .!?«»\"")
		modelTrack, _ := args["track"].(string)
		if len(strings.Fields(modelTrack)) == len(strings.Fields(track)) && closeMusicTitle(modelTrack, track) {
			track = modelTrack
		}
		if track != "" && artist != "" {
			return M{"track": track, "artist": artist, "spoken_request": transcript}
		}
	}
	track, _ := args["track"].(string)
	artist, _ := args["artist"].(string)
	spoken := musicKey(text)
	if artist == "" && args["playlist_mode"] != "named" &&
		(kind == "playlist" || strings.Contains(spoken, "музык") || strings.Contains(spoken, "чтонибудь")) {
		moodText := text
		if commands := musicIntentBoundary.FindAllStringIndex(text, -1); len(commands) > 0 {
			moodText = text[commands[len(commands)-1][1]:]
		}
		moodText = cleanMusicKeywords(moodText)
		if _, ok := matchMood(moodText); ok {
			return M{"track": moodText, "music_type": "playlist", "playlist_mode": "mood", "spoken_request": transcript}
		}
	}
	if kind == "playlist" || strings.Contains(spoken, "музык") || strings.Contains(spoken, "чтонибудь") || strings.Contains(spoken, "песн") || strings.Contains(spoken, "трек") || strings.Contains(spoken, "плейлист") {
		result := M{}
		for k, v := range args {
			result[k] = v
		}
		result["spoken_request"] = transcript
		return result
	}
	spokenWithoutWake := text
	if commands := musicIntentBoundary.FindAllStringIndex(text, -1); len(commands) > 0 {
		spokenWithoutWake = text[commands[len(commands)-1][1]:]
	}
	crossScriptRequest := hasCyrillicLetter(spokenWithoutWake) && !hasLatinLetter(spokenWithoutWake)
	if artist != "" && !strings.Contains(spoken, musicKey(artist)) && !(crossScriptRequest && hasLatinLetter(artist)) {
		return M{"validation_error": "Исполнитель не совпадает с распознанной просьбой. Уточни исполнителя, не подставляй другого."}
	}
	if track != "" && !strings.Contains(spoken, musicKey(track)) && !(crossScriptRequest && hasLatinLetter(track)) {
		words := strings.Fields(track)
		for start := 1; start < len(words)-1; start++ {
			candidate := strings.Join(words[start:], " ")
			if strings.Contains(spoken, musicKey(candidate)) {
				return M{"track": candidate, "artist": artist, "spoken_request": transcript}
			}
		}
		return M{"validation_error": "Название не совпадает с распознанной просьбой. Уточни название, не дописывай и не заменяй песню."}
	}
	result := M{}
	for k, v := range args {
		result[k] = v
	}
	result["spoken_request"] = transcript
	return result
}

func musicMatches(request M, result M) bool {
	if request["backend"] == "spotify_api" {
		if request["spotify_generic"] == true {
			return result["backend"] == "spotify_api" && result["ok"] == true &&
				(result["spotify_generic_source"] == "DJ" || result["spotify_generic_source"] == "daylist")
		}
		return result["backend"] == "spotify_api" && result["selected_uri"] == request["spotify_uri"] && result["ok"] == true
	}
	track, _ := request["requested_track"].(string)
	if request["music_type"] != "track" && request["music_type"] != nil {
		return false
	}
	artist, _ := request["requested_artist"].(string)
	title, _ := result["title"].(string)
	actualArtist, _ := result["artist"].(string)
	artistMatch := artist == "" || strings.Contains(musicKey(actualArtist), musicKey(artist))
	titleMatch := track == "" || strings.Contains(musicKey(title), musicKey(track))
	if !titleMatch && artist != "" && artistMatch {
		base := strings.Split(title, " - ")[0]
		titleMatch = closeMusicTitle(track, base)
	}
	return artistMatch && titleMatch
}

func closeMusicTitle(a, b string) bool {
	x, y := []rune(musicKey(a)), []rune(musicKey(b))
	if string(x) == string(y) {
		return true
	}
	if len(x) < 5 || len(y) < 5 {
		return false
	}
	return musicTitleDistance(x, y) <= 2
}

func musicTitleDistance(x, y []rune) int {
	row := make([]int, len(y)+1)
	for j := range row {
		row[j] = j
	}
	for i, u := range x {
		previous := row[0]
		row[0] = i + 1
		for j, v := range y {
			old := row[j+1]
			cost := 0
			if u != v {
				cost = 1
			}
			row[j+1] = min(row[j]+1, old+1, previous+cost)
			previous = old
		}
	}
	return row[len(y)]
}

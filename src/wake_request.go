package main

import (
	"slices"
	"strings"
	"unicode"
)

// englishCommandWords is intentionally a command/intent vocabulary, not a
// general English dictionary. It lets real English requests through while
// rejecting TV/ASR fragments in other Latin-script languages.
// Cyrillic requests are accepted on their own letters (see
// hasUserLanguageText); Latin-script requests need one of these words.
var englishCommandWords = map[string]bool{
	"please": true, "thanks": true, "thank": true, "you": true,
	"hello": true, "hi": true, "hey": true, "okay": true, "ok": true, "yes": true, "no": true,
	"yeah": true, "nope": true, "play": true, "pause": true, "stop": true, "resume": true,
	"next": true, "previous": true, "skip": true, "back": true, "forward": true, "again": true,
	"repeat": true, "shuffle": true, "music": true, "song": true, "songs": true, "track": true,
	"tracks": true, "album": true, "artist": true, "playlist": true, "spotify": true, "radio": true,
	"youtube": true, "volume": true, "louder": true, "quieter": true, "mute": true, "unmute": true,
	"sound": true, "audio": true, "bass": true, "treble": true, "equalizer": true, "time": true,
	"clock": true, "date": true, "today": true, "tomorrow": true, "weather": true, "forecast": true,
	"temperature": true, "rain": true, "snow": true, "cold": true, "hot": true, "warm": true,
	"outside": true, "what": true, "when": true, "where": true, "who": true, "why": true, "how": true,
	"which": true, "can": true, "could": true, "would": true, "will": true, "tell": true,
	"say": true, "give": true, "show": true, "find": true, "search": true, "open": true, "close": true,
	"start": true, "turn": true, "on": true, "off": true, "set": true, "change": true, "make": true,
	"joke": true, "fun": true, "fact": true, "news": true, "timer": true, "alarm": true, "reminder": true,
	"calculate": true, "math": true, "home": true, "city": true, "light": true, "lights": true,
	"color": true, "colour": true, "brightness": true, "call": true, "message": true, "connect": true,
	"disconnect": true, "device": true, "speaker": true, "cast": true, "google": true,
	"map": true, "maps": true, "navigate": true, "navigation": true, "directions": true, "route": true,
}

var languageWakePhrases = []string{
	"hey google", "okay google", "ok google", "hey dj", "okay dj", "ok dj",
	"хэй гугл", "хей гугл", "окей гугл", "окэй гугл", "эй гугл", "ок гугл",
	"хэй гугол", "хей гугол", "окей гугол", "окэй гугол", "эй гугол", "ок гугол",
	"hey гугл", "okay гугл", "ok гугл", "хэй google", "хей google", "окей google", "окэй google",
	"хэй диджей", "хей диджей", "эй диджей", "окей диджей", "окэй диджей", "ок диджей",
	"hey диджей", "okay диджей", "ok диджей", "hey dj", "okay dj", "ok dj",
}

// compactWakePrefixes are wake phrases without spaces, matching what the input
// transcriber emits for "HeyGooglepause" or "окейгуглгромче". Longer forms must
// precede their bare suffix ("окейгугл" before "гугл").
var compactWakePrefixes = []string{
	"okaygoogle", "heygoogle", "okgoogle",
	"окейгугл", "окэйгугл", "хэйгугл", "хейгугл", "эйгугл", "окгугл",
	"окейгугол", "окэйгугол", "хэйгугол", "хейгугол", "эйгугол", "окгугол",
	"okayгугл", "heyгугл", "okгугл",
	"окейgoogle", "окэйgoogle", "хэйgoogle", "хейgoogle", "эйgoogle", "окgoogle",
	"google", "гугл", "гугол",
	"heydj", "okaydj", "okdj", "deejay", "dj",
	"хэйдиджей", "хейдиджей", "эйдиджей", "окейдиджей", "окэйдиджей", "окдиджей",
	"heyдиджей", "okayдиджей", "окдиджей",
	"хэйdj", "хейdj", "эйdj", "окейdj", "окэйdj", "окdj",
	"диджей",
}

// requestText normalizes a transcript for request gating. It lowercases and
// accent-folds the text, drops wake phrases and leading filler articles, and
// returns a space-separated form plus a compact letters/digits form. ok is
// false when the text uses a script outside the supported Russian/Latin set.
func requestText(text string) (spaced, compact string, ok bool) {
	lower := strings.ToLower(text)
	for _, wake := range languageWakePhrases {
		lower = strings.ReplaceAll(lower, wake, " ")
	}
	var b strings.Builder
	for _, r := range lower {
		r = foldLatinAccents(r)
		if r == 'ё' {
			r = 'е'
		}
		switch {
		case r >= 'а' && r <= 'я', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case unicode.IsLetter(r):
			return "", "", false
		default:
			b.WriteRune(' ')
		}
	}
	spaced = strings.Join(strings.Fields(b.String()), " ")
	for {
		trimmed := stripLeadingRequestFiller(spaced)
		if trimmed == spaced {
			break
		}
		spaced = trimmed
	}
	var c strings.Builder
	for _, r := range spaced {
		if r != ' ' {
			c.WriteRune(r)
		}
	}
	return spaced, c.String(), true
}

// stripLeadingRequestFiller removes one leading wake phrase or filler article.
// It is applied repeatedly so "de Google Maps" becomes "maps" and
// "HeyGooglepause" becomes "pause".
func stripLeadingRequestFiller(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	switch fields[0] {
	case "de", "the", "a":
		return strings.Join(fields[1:], " ")
	}
	for _, wake := range compactWakePrefixes {
		if fields[0] == wake {
			return strings.Join(fields[1:], " ")
		}
		if strings.HasPrefix(fields[0], wake) {
			fields[0] = strings.TrimPrefix(fields[0], wake)
			return strings.Join(fields, " ")
		}
	}
	return s
}

// hasUserLanguageText is the language-and-evidence gate. After wake-word
// stripping it accepts:
//   - a Russian/Cyrillic request with at least two letters ("да", "нет",
//     "как дела"); a single letter ("а", "и") is noise;
//   - a Latin-script request containing a known command word, so TV/ASR
//     fragments such as "abracadabra", "candela", "wa" or "de Google" fail.
//
// Digits alone no longer authorize a request ("1" is noise, not a command).
func hasUserLanguageText(text string) bool {
	spaced, _, ok := requestText(text)
	if !ok {
		return false
	}
	letters, cyrillic := 0, false
	for _, r := range spaced {
		if unicode.IsLetter(r) {
			letters++
		}
		if r >= 'а' && r <= 'я' {
			cyrillic = true
		}
	}
	if cyrillic && letters >= 2 {
		return true
	}
	for _, word := range strings.Fields(spaced) {
		if englishCommandWords[word] {
			return true
		}
	}
	return false
}

func hasMeaningfulUserRequest(text string) bool {
	if !hasUserLanguageText(text) {
		return false
	}
	_, remaining, ok := requestText(text)
	if !ok || remaining == "" {
		return false
	}
	// The embedded cues and common ASR hallucinations are model/speaker audio,
	// not a request. Reject strings made only from those pieces, including
	// repetitions such as "queque" from "¿Qué? ¿Qué?".
	if onlyEchoArtifacts(remaining) {
		return false
	}
	// Known acoustic wake artifacts produced by ASR on hotword tails
	artifacts := []string{
		"que", "qué", "cartao", "cartão", "cualllamada", "llamada", "cual", "eh", "ah", "oh",
		"quecomoayuda", "comoayuda", "ayuda", "como", "queayuda", "quecomo", "de", "la", "el",
		"harman", "kardon", "harmankardon", "citation",
	}
	for _, a := range artifacts {
		if remaining == a {
			return false
		}
	}
	for _, prefix := range []string{"quecomoayuda", "cualllamada"} {
		if strings.HasPrefix(remaining, prefix) {
			return false
		}
	}
	for _, wake := range compactWakePrefixes {
		if strings.HasPrefix(wake, remaining) {
			return false // e.g. streamed "Hey", then " Goo", then "gle"
		}
	}
	return true
}

// foldLatinAccents makes Spanish/Portuguese ASR artifacts comparable to the
// ASCII forms used in the echo/artifact lists.
func foldLatinAccents(r rune) rune {
	switch r {
	case 'á', 'à', 'â', 'ä', 'ã', 'å':
		return 'a'
	case 'é', 'è', 'ê', 'ë':
		return 'e'
	case 'í', 'ì', 'î', 'ï':
		return 'i'
	case 'ó', 'ò', 'ô', 'ö', 'õ':
		return 'o'
	case 'ú', 'ù', 'û', 'ü':
		return 'u'
	case 'ñ':
		return 'n'
	case 'ç':
		return 'c'
	}
	return r
}

// onlyEchoArtifacts reports whether a compact normalized transcript consists
// solely of cue words or known ASR artifacts. It is intentionally a whitelist:
// a genuine short answer such as "да" or "нет" is not in this list.
func onlyEchoArtifacts(s string) bool {
	if s == "" {
		return true
	}
	parts := []string{
		"que", "como", "ayuda", "llamada", "hola", "buenosdias", "denada",
		"novasacomer", "aga", "aha", "hm", "mm", "m",
		"slushayu", "seichas", "sekundu", "seychas",
		"хм", "ага", "слушаю", "сейчас", "секунду",
		// Short ASR fragments seen in the live self-echo loop. These are not
		// useful commands in the supported Russian/English modes.
		"a", "e", "i", "o", "u", "y", "en", "de", "la", "el", "los", "las",
		"un", "una", "the", "and", "to", "of", "in", "on", "at", "it", "is",
		"as", "by", "or", "if", "so", "ne", "pas", "google", "гугл",
		// The speaker's own brand tone/name is a frequent ASR self-echo on
		// follow-up turns. It is not a user request.
		"harman", "kardon", "harmankardon", "citation",
		"pronto", "igual", "continua", "devolver",
	}
	// Longest first, otherwise "e" would consume the start of "engoogle"
	// before the full artifact can match.
	slices.SortFunc(parts, func(a, b string) int { return len(b) - len(a) })
	for len(s) > 0 {
		matched := false
		for _, part := range parts {
			if strings.HasPrefix(s, part) {
				s = strings.TrimPrefix(s, part)
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func normalizedSpeechWords(text string) []string {
	lower := strings.ToLower(text)
	for _, wake := range languageWakePhrases {
		lower = strings.ReplaceAll(lower, wake, " ")
	}
	folded := make([]rune, 0, len(lower))
	for _, r := range lower {
		r = foldLatinAccents(r)
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			folded = append(folded, r)
		} else {
			folded = append(folded, ' ')
		}
	}
	return strings.Fields(string(folded))
}

// isLikelyAssistantEcho suppresses a transcript that closely matches the last
// assistant reply. This catches the Russian echo case ("Слушаю вас") that the
// Spanish artifact list cannot.
func isLikelyAssistantEcho(input, lastOutput string) bool {
	if strings.TrimSpace(lastOutput) == "" {
		return false
	}
	inputWords := normalizedSpeechWords(input)
	if len(inputWords) < 2 {
		return false
	}
	lastWords := normalizedSpeechWords(lastOutput)
	if len(lastWords) == 0 {
		return false
	}
	lastSet := make(map[string]bool, len(lastWords))
	for _, word := range lastWords {
		lastSet[word] = true
	}
	matches := 0
	for _, word := range inputWords {
		if lastSet[word] {
			matches++
		}
	}
	if matches < 2 {
		return false
	}
	return matches*10 >= len(inputWords)*6
}

// At 24kHz/S16 mono, four seconds are 192000 bytes. Audio that arrives before
// the input transcript is kept only in bounded RAM. An overflow fails closed:
// no partial reply and no tool may be released until reset/new user activity.
const wakePendingAudioLimit = 4 * 24000 * 2

type wakeRequestGuard struct {
	meaningful bool
	pending    []byte
	overflow   bool
}

// Pass the complete accumulated INPUT transcript, never model output text.
func (g *wakeRequestGuard) acceptTranscript(text string) bool {
	if hasMeaningfulUserRequest(text) {
		g.meaningful = true
	}
	return g.allowResponse()
}

func (g *wakeRequestGuard) allowResponse() bool { return g.meaningful && !g.overflow }

// markMeaningful allows the response when speech has been confirmed by VAD
// or external evidence prior to full transcript processing.
func (g *wakeRequestGuard) markMeaningful() {
	g.meaningful = true
}

// Returns playable audio, or nil while awaiting a meaningful input transcript.
// After acceptTranscript succeeds, call takeAudio to release earlier frames;
// queueAudio also flushes them ahead of a later model frame if needed.
func (g *wakeRequestGuard) queueAudio(pcm []byte) []byte {
	if g.overflow {
		return nil
	}
	if g.meaningful {
		if len(g.pending) == 0 {
			return pcm
		}
		out := append(g.pending, pcm...)
		g.pending = nil
		return out
	}
	if len(pcm) > wakePendingAudioLimit-len(g.pending) {
		g.pending = nil
		g.overflow = true
		return nil
	}
	g.pending = append(g.pending, pcm...)
	return nil
}

func (g *wakeRequestGuard) takeAudio() []byte {
	if !g.allowResponse() {
		return nil
	}
	out := g.pending
	g.pending = nil
	return out
}

func (g *wakeRequestGuard) reset() { *g = wakeRequestGuard{} }

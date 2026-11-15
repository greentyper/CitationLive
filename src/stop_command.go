package main

import (
	"strings"
)

func isStopCommand(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.Trim(t, ".!?,;:- \t\r\n")
	t = strings.ReplaceAll(t, "ё", "е")
	if t == "" {
		return false
	}

	// Match complete English control phrases, not words inside song titles
	// such as "Don't Stop Me Now" or a request to play a song called Pause.
	english := t
	for _, wake := range []string{"hey google", "okay google", "ok google"} {
		if strings.HasPrefix(english, wake+" ") || strings.HasPrefix(english, wake+",") {
			english = strings.TrimSpace(strings.TrimPrefix(english, wake))
			english = strings.TrimLeft(english, ", ")
			break
		}
	}
	english = strings.TrimSpace(strings.TrimPrefix(english, "please "))
	english = strings.TrimSpace(strings.TrimSuffix(english, " please"))
	english = strings.Trim(english, ", ")
	switch english {
	case "pause", "pause music", "pause the music", "pause playback", "pause spotify",
		"stop", "stop music", "stop the music", "stop playback", "stop spotify",
		"stop talking", "be quiet", "cancel", "never mind",
		"camolci", "camolchi", "zamolchi", "zamolci", "pomolchi", "pomolci",
		"hvatit", "otmena", "tishi", "tiho", "vse", "vsehvatit", "vse stop", "vse hvatit":
		return true
	}

	stopPhrases := []string{
		"стоп", "помолчи", "замолчи", "хватит", "все", "отмена", "тихо",
		"заткнись", "молчи", "тишина", "остановись", "прекрати", "выключись",
		"все помолчи", "все хватит", "все стоп", "стоп музыка", "останови музыку",
		"выключи музыку", "пауза", "паузу",
		"camolci", "camolchi", "zamolchi", "zamolci", "pomolchi", "hvatit",
	}

	for _, p := range stopPhrases {
		if t == p {
			return true
		}
		if p == "все" {
			continue // do not prefix-match "все" into "все песни"
		}
		if strings.HasPrefix(t, p+" ") || strings.HasSuffix(t, " "+p) || strings.Contains(t, " "+p+" ") {
			return true
		}
	}
	return false
}

// isMediaPauseCommand separates "stop talking" from "pause the music".
// Telling the assistant to be quiet must not pause the user's Spotify.
func isMediaPauseCommand(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.ReplaceAll(t, "ё", "е")
	for _, p := range []string{",", ".", "!", "?", ";", ":", "-", "\"", "'", "«", "»"} {
		t = strings.ReplaceAll(t, p, " ")
	}
	t = strings.Join(strings.Fields(t), " ")
	for _, wake := range []string{"hey google", "okay google", "ok google", "хэй гугл", "хей гугл", "окей гугл", "окэй гугл", "эй гугл", "ок гугл"} {
		if strings.HasPrefix(t, wake+" ") {
			t = strings.TrimSpace(strings.TrimPrefix(t, wake))
			break
		}
	}
	switch t {
	case "стоп", "stop", "пауза", "паузу", "pause",
		"стоп музыка", "стоп музыку", "останови музыку", "остановить музыку", "выключи музыку", "выключить музыку",
		"pause music", "pause playback", "pause spotify", "stop music", "stop playback", "stop spotify":
		return true
	}
	return false
}

// shouldHonorEndSession trusts the model's own end_session tool call. It only
// refuses when the model's final text asks the user to repeat or asks a
// follow-up question; otherwise the model decides when the conversation ends.
func shouldHonorEndSession(outputText string) bool {
	t := strings.TrimSpace(strings.ToLower(outputText))
	if t == "" {
		return true
	}
	if strings.Contains(t, "расслышал") || strings.Contains(t, "повторите") || strings.Contains(t, "повтори") {
		return false
	}
	// Only a question at the very end means the model is still asking the user
	// something. A question mark inside a joke or quotation must not keep the
	// session open after the model has decided to end it.
	trimmed := strings.TrimRight(t, " \t\r\n.!…")
	return !strings.HasSuffix(trimmed, "?")
}

func isModelStopConfirmation(outputText string) bool {
	t := strings.ToLower(outputText)
	for _, s := range []string{"замолкаю", "молчу", "отключаюсь", "выключаюсь"} {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

func isSilentOrEmptyOutput(text string) bool {
	t := strings.TrimSpace(strings.ToLower(text))
	t = strings.Trim(t, ".!?,;:- \t\r\n")
	if t == "" {
		return true
	}
	for _, s := range []string{
		"<!-- silence -->", "<no speech>", "<pause>", "{pause}",
		"ничего", "щас", "ща", "секунду", "минуту",
	} {
		if t == s {
			return true
		}
	}
	return strings.Contains(t, "silence") || strings.Contains(t, "no speech")
}

package main

import "strings"

func controlPhrase(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.ReplaceAll(t, "ё", "е")
	for _, p := range []string{",", ".", "!", "?", ";", ":", "-", "\"", "'", "«", "»"} {
		t = strings.ReplaceAll(t, p, " ")
	}
	t = strings.Join(strings.Fields(t), " ")
	for _, noise := range []string{"de ", "the ", "a ", "эй "} {
		if strings.HasPrefix(t, noise) {
			t = strings.TrimSpace(strings.TrimPrefix(t, noise))
			break
		}
	}
	for _, wake := range []string{
		"hey google", "okay google", "ok google",
		"хэй гугл", "хей гугл", "окей гугл", "окэй гугл", "эй гугл", "ок гугл",
		"хэй гугол", "хей гугол", "окей гугол", "окэй гугол", "эй гугол", "ок гугол",
		"hey гугл", "okay гугл", "ok гугл", "хэй google", "хей google", "окей google", "окэй google",
		"google", "гугл", "гугол",
		"hey dj", "okay dj", "ok dj", "диджей", "heyдиджей", "хэйдиджей", "хейдиджей",
	} {
		if strings.HasPrefix(t, wake+" ") {
			t = strings.TrimSpace(strings.TrimPrefix(t, wake))
			break
		} else if t == wake {
			t = ""
			break
		}
	}
	for _, polite := range []string{"please ", "пожалуйста "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, polite))
	}
	for _, polite := range []string{" please", " пожалуйста"} {
		t = strings.TrimSpace(strings.TrimSuffix(t, polite))
	}
	return strings.TrimSpace(t)
}
func quickMediaAction(text string) string {
	phrase := controlPhrase(text)
	phrase = strings.TrimPrefix(strings.TrimPrefix(phrase, "ну "), "давай ")
	for _, provider := range []string{" в spotify", " на spotify", " в спотифай", " на спотифае", " on spotify"} {
		phrase = strings.TrimSuffix(phrase, provider)
	}
	switch phrase {
	case "дальше", "следующий", "следующая", "следующий трек", "следующая песня", "переключи", "переключи трек", "переключи песню", "скип", "скипни", "пропусти", "пропусти трек", "next", "skip", "next track", "next song", "skip track", "skip song":
		return "next"
	case "назад", "предыдущий", "предыдущая", "предыдущий трек", "предыдущая песня", "верни трек", "верни песню", "previous", "previous track", "previous song", "go back":
		return "previous"
	case "пауза", "pause", "stop", "стоп", "останови", "останови музыку", "остановить", "остановить музыку", "поставь на паузу", "на паузу", "паузу", "стопни":
		return "pause"
	case "продолжи", "продолжай", "продолжи музыку", "продолжить музыку", "продолжить", "возобнови музыку", "возобнови", "возобновить", "возобновить музыку", "возобнови трек", "возобновить трек", "возобнови песню", "возобновить песню",
		"resume", "resume music", "resume playback", "unpause",
		"включи воспроизведение", "включить воспроизведение", "запусти воспроизведение", "запустить воспроизведение",
		"продолжить воспроизведение", "продолжи воспроизведение":
		return "resume"
	case "музыку", "сыграй музыку", "играй музыку", "играть музыку", "play music", "play some music", "play something",
		"включи музыку", "включить музыку", "вруби музыку", "врубай музыку", "запусти музыку",
		"включи трек", "включить трек", "включи песню", "включить песню", "сыграй что нибудь", "включи что нибудь":
		return "play"
	}
	return ""
}
func conversationDonePhrase(phrase string) bool {
	switch phrase {
	case "спасибо", "спасибо хватит", "спасибо не надо", "не надо", "мне не надо", "не нужно", "мне не нужно", "не надо спасибо", "не нужно спасибо", "отбой", "больше ничего", "ничего больше", "ничего не надо", "все спасибо", "всё спасибо", "отбой спасибо", "ладно хватит", "не стоит", "ничего",
		"thanks", "thank you", "that's all", "that is all", "nothing else", "no more help", "no thanks", "no thank you", "no need", "nevermind", "never mind",
		"camolci", "camolchi", "zamolchi", "hvatit", "spasibo", "vse spasibo", "vse":
		return true
	}
	for _, suffix := range []string{
		"не надо", "мне не надо", "не нужно", "мне не нужно", "ничего не надо",
		"спасибо не надо", "не надо спасибо", "не нужно спасибо", "ладно хватит", "спасибо хватит",
		"отбой", "больше ничего", "ничего больше", "no thanks", "no thank you", "never mind", "nevermind",
	} {
		if strings.HasSuffix(phrase, " "+suffix) || (len(phrase) > len(suffix) && strings.HasSuffix(phrase, suffix) && !isAlphaNumericLastRune(phrase[:len(phrase)-len(suffix)])) {
			return true
		}
	}
	return false
}

func isAlphaNumericLastRune(s string) bool {
	if s == "" {
		return false
	}
	runes := []rune(s)
	last := runes[len(runes)-1]
	return (last >= 'a' && last <= 'z') || (last >= 'а' && last <= 'я') || (last >= '0' && last <= '9')
}

func conversationDone(text string) bool {
	if conversationDonePhrase(controlPhrase(text)) {
		return true
	}
	parts := strings.FieldsFunc(text, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == '\n'
	})
	if len(parts) > 1 {
		last := controlPhrase(parts[len(parts)-1])
		if conversationDonePhrase(last) {
			return true
		}
	}
	return false
}

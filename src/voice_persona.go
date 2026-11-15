package main

import (
	"fmt"
	"strings"
)

// Official Gemini Live voice list with the gender published by Google.
var geminiVoiceGender = map[string]string{
	"achernar": "female", "achird": "male", "algenib": "male", "algieba": "male",
	"alnilam": "male", "aoede": "female", "autonoe": "female", "callirrhoe": "female",
	"charon": "male", "despina": "female", "enceladus": "male", "erinome": "female",
	"fenrir": "male", "gacrux": "female", "iapetus": "male", "kore": "female",
	"laomedeia": "female", "leda": "female", "orus": "male", "puck": "male",
	"pulcherrima": "female", "rasalgethi": "male", "sadachbia": "male", "sadaltager": "male",
	"schedar": "male", "sulafat": "female", "umbriel": "male", "vindemiatrix": "female",
	"zephyr": "female", "zubenelgenubi": "male",
}

func voicePersona(voice string) string {
	voice = strings.TrimSpace(voice)
	if voice == "" {
		return ""
	}
	intro := fmt.Sprintf(" Твоё имя — %s. Если спрашивают, как тебя зовут, ответь «%s».", voice, voice)
	switch geminiVoiceGender[strings.ToLower(voice)] {
	case "male":
		return intro + " Говори о себе в мужском роде."
	case "female":
		return intro + " Говори о себе в женском роде."
	default:
		return intro + " Говори о себе нейтрально, если язык это позволяет."
	}
}

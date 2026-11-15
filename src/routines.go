package main

// Local built-in routines. Google Home routines have no public local API; these
// routines use the speaker's own volume, media pause and sleep timer. Gemini
// speaks the final line and can add weather/reminders through its normal tools.

import (
	"context"
	"fmt"
	"strings"
)

var routineSetVolume = func(level int) error {
	_, err := wampCall("com.harman.volumeSet", level)
	return err
}
var routinePauseMedia = pauseMediaForSleep
var routineSleepTimer = func(minutes int) M {
	return timerManager.toolSetSleepTimer(M{"minutes": float64(minutes)})
}

func routineToolDeclarations() []M {
	return []M{{
		"name":        "run_routine",
		"description": "Run a built-in local routine: good_morning, good_night, leaving or home. Use for 'доброе утро', 'спокойной ночи', 'я ухожу', 'я дома'. The routine adjusts volume, media and sleep timer; then answer with the returned instruction.",
		"parameters": M{
			"type": "OBJECT",
			"properties": M{
				"name": M{"type": "STRING", "enum": []string{"good_morning", "good_night", "leaving", "home"}},
			},
			"required": []string{"name"},
		},
	}}
}

func handleRoutineTool(ctx context.Context, name string, args M) (M, bool) {
	if name != "run_routine" {
		return nil, false
	}
	routine, _ := args["name"].(string)
	routine = strings.ToLower(strings.TrimSpace(routine))
	var executed []string
	switch routine {
	case "good_morning":
		if err := routineSetVolume(35); err != nil {
			return M{"ok": false, "error": clean(err)}, true
		}
		executed = append(executed, "volume=35")
		return M{
			"ok": true, "routine": routine, "executed": executed,
			"instruction": "Скажи «Доброе утро», кратко добавь погоду и ближайшие напоминания, если они есть.",
		}, true
	case "good_night":
		routinePauseMedia()
		executed = append(executed, "media=paused")
		res := routineSleepTimer(30)
		if res["ok"] == true {
			executed = append(executed, "sleep_timer=30m")
		}
		_ = routineSetVolume(10)
		executed = append(executed, "volume=10")
		return M{
			"ok": true, "routine": routine, "executed": executed,
			"instruction": "Скажи «Спокойной ночи» и коротко подтверди, что музыка выключится через 30 минут.",
		}, true
	case "leaving":
		routinePauseMedia()
		_ = routineSetVolume(10)
		return M{
			"ok": true, "routine": routine, "executed": []string{"media=paused", "volume=10"},
			"instruction": "Скажи «Хорошего дня».",
		}, true
	case "home":
		_ = routineSetVolume(35)
		return M{
			"ok": true, "routine": routine, "executed": []string{"volume=35"},
			"instruction": "Скажи «С возвращением».",
		}, true
	default:
		return M{"ok": false, "error": fmt.Sprintf("unknown routine %q", routine)}, true
	}
}

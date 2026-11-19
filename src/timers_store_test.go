package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestOneShotAlarmKeepsDeadlineAcrossRestart(t *testing.T) {
	now := time.Date(2026, time.November, 19, 7, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "timers.json")
	original := newTimerScheduler(path, nil)
	original.now = func() time.Time { return now.Add(-time.Hour) }
	result := original.toolSetAlarm(M{"hour": 7, "minute": 0})
	if result["ok"] != true {
		t.Fatalf("create alarm: %v", result)
	}

	for _, test := range []struct {
		name string
		boot time.Time
	}{
		{"before deadline", now.Add(-time.Minute)},
		{"after deadline", now.Add(time.Minute)},
	} {
		t.Run(test.name, func(t *testing.T) {
			restored := newTimerScheduler(path, nil)
			restored.now = func() time.Time { return test.boot }
			if err := restored.load(); err != nil {
				t.Fatal(err)
			}
			if len(restored.alarms) != 1 || restored.alarms[0].Next != now.UnixNano() {
				t.Fatalf("wrong restored alarm: %+v", restored.alarms)
			}
			events := restored.collectDue(now.Add(time.Minute))
			if len(events) != 1 || events[0].Kind != "alarm" || events[0].ID != result["id"] {
				t.Fatalf("expected one alarm event, got %+v", events)
			}
			if events := restored.collectDue(now.Add(24 * time.Hour)); len(events) != 0 {
				t.Fatalf("one-shot alarm fired again: %+v", events)
			}
		})
	}

	t.Run("scheduler startup", func(t *testing.T) {
		restored := newTimerScheduler(path, nil)
		restored.now = func() time.Time { return now.Add(time.Minute) }
		cue := make(chan struct{}, 1)
		restored.playCue = func() { cue <- struct{}{} }
		restored.start()
		defer restored.stop()

		select {
		case <-cue:
		case <-time.After(2 * time.Second):
			t.Fatal("scheduler did not ring the overdue alarm at startup")
		}
		if stopped := restored.toolStopAlarm(M{"id": result["id"]}); stopped["ok"] != true {
			t.Fatalf("stop alarm: %v", stopped)
		}
	})
}

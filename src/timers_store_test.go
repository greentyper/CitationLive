package main

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestLocalAgendaFiltersByDayAndItemKind(t *testing.T) {
	preferenceState.Lock()
	previousZone := preferenceState.Timezone
	preferenceState.Timezone = "Asia/Novosibirsk"
	preferenceState.Unlock()
	t.Cleanup(func() {
		preferenceState.Lock()
		preferenceState.Timezone = previousZone
		preferenceState.Unlock()
	})

	dir := t.TempDir()
	now := time.Date(2026, time.November, 20, 17, 30, 0, 0, time.UTC)
	newScheduler := func() *timerScheduler {
		scheduler := newTimerScheduler(filepath.Join(dir, "timers.json"), newJSONFileReminderBackend(filepath.Join(dir, "reminders.json")))
		scheduler.now = func() time.Time { return now }
		return scheduler
	}
	scheduler := newScheduler()
	for _, args := range []M{
		{"text": "Today event", "at": "09:00", "remote_kind": "event"},
		{"text": "Today task", "at": "2026-11-21T10:00:00+07:00"},
		{"text": "Tomorrow event", "at": "2026-11-22T00:00:00+07:00", "remote_kind": "event"},
	} {
		if got := scheduler.toolSetReminder(args); got["ok"] != true {
			t.Fatalf("create local item: %+v", got)
		}
	}

	reloaded := newScheduler()
	if err := reloaded.load(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args M
		want string
		at   string
	}{
		{M{"date": "today", "kind": "event"}, "Today event", "2026-11-21T09:00:00+07:00"},
		{M{"date": "2026-11-21", "kind": "task"}, "Today task", "2026-11-21T10:00:00+07:00"},
		{M{"date": "tomorrow"}, "Tomorrow event", "2026-11-22T00:00:00+07:00"},
	} {
		result, _ := handleTimerToolOn(reloaded, "list_reminders", test.args)
		items, _ := result["reminders"].([]M)
		if len(items) != 1 || items[0]["text"] != test.want || items[0]["at"] != test.at {
			t.Errorf("list with %v returned %+v", test.args, result)
		}
	}
	if result, _ := handleTimerToolOn(reloaded, "list_reminders", M{}); result["count"] != 3 {
		t.Errorf("unfiltered list returned %+v", result)
	}
}

func TestPendingReminderDeletionKeepsOneTombstone(t *testing.T) {
	reminder := Reminder{ID: "r1", Text: "Reminder"}
	pending := reminderRecord{Reminder: reminder, Deleted: true, Pending: true}
	got := mergeReminders([]Reminder{reminder}, []reminderRecord{pending})
	if len(got) != 1 || got[0] != pending {
		t.Fatalf("expected one pending deletion, got %+v", got)
	}
}

func TestImportedGoogleTaskDoesNotExpireOnSpeaker(t *testing.T) {
	scheduler := newTimerScheduler("", nil)
	task := GoogleTask{ID: "external", Title: "Keep this task", Due: "2026-11-18T00:00:00Z"}
	scheduler.reminders = []reminderRecord{{Reminder: reminderFromTask(task)}}
	now := time.Date(2026, time.November, 19, 8, 0, 0, 0, time.UTC)
	if next := scheduler.nextDueLocked(now); !next.IsZero() {
		t.Errorf("external task woke the scheduler at %v", next)
	}
	if events := scheduler.collectDue(now); len(events) != 0 || scheduler.reminders[0].Deleted {
		t.Fatalf("external task was expired: events=%+v reminders=%+v", events, scheduler.reminders)
	}
}

func TestReminderRescheduledBetweenCollectionAndDelivery(t *testing.T) {
	now := time.Date(2026, time.November, 19, 8, 0, 0, 0, time.UTC)
	scheduler := newTimerScheduler(filepath.Join(t.TempDir(), "timers.json"), nil)
	scheduler.now = func() time.Time { return now }
	record := reminderRecord{Reminder: Reminder{ID: "r1", Text: "Reminder", Due: now.Format(time.RFC3339)}}
	scheduler.reminders = []reminderRecord{record}
	events := scheduler.collectDue(now)
	if len(events) != 1 {
		t.Fatalf("expected a due reminder, got %+v", events)
	}

	// A completed sync can replace the due time while the scheduler delivers a cue.
	record.Due = now.Add(time.Hour).Format(time.RFC3339)
	scheduler.replaceReminderRecords([]reminderRecord{record})
	scheduler.deliver(events[0])
	if got := scheduler.snapshotReminderRecords(); len(got) != 1 || got[0] != record {
		t.Fatalf("the due event removed a rescheduled reminder: %+v", got)
	}
}

func TestAlarmVolumeRestoredAfterLastAlarmIncludingMute(t *testing.T) {
	scheduler := newTimerScheduler("", nil)
	scheduler.alarmVolume = 50
	scheduler.getVolume = func() (int, error) { return 0, nil }
	var levels []int
	scheduler.setVolume = func(level int) error {
		levels = append(levels, level)
		return nil
	}
	scheduler.alarms = []alarmEntry{{ID: "a1", Active: true}, {ID: "a2", Active: true}}
	scheduler.applyAlarmVolume()
	scheduler.alarms[0].Active = false
	scheduler.restoreAlarmVolume()
	if !scheduler.alarmVolumeActive || !slices.Equal(levels, []int{24}) {
		t.Errorf("volume override ended while another alarm was active: %v", levels)
	}
	scheduler.alarms[1].Active = false
	scheduler.restoreAlarmVolume()
	if !slices.Equal(levels, []int{24, 0}) {
		t.Fatalf("speaker did not return to mute: %v", levels)
	}
}

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

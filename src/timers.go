package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Local timers, alarms and the stopwatch. Reminders are cached locally but are
// synchronised through ReminderBackend (Google Calendar/Tasks when configured,
// a local JSON file otherwise).
//
// This file deliberately never calls the stock Assistant APIs. A fired timer
// plays a locally generated cue through the native Cast mixer and, when the
// integration layer has installed timerAnnounce, asks Gemini to announce it.

const (
	timerMaxDuration     = 365 * 24 * time.Hour
	timerMaxSleep        = 30 * time.Second
	timerBackendTimeout  = 5 * time.Second
	timerSyncTimeout     = 20 * time.Second
	timerAlarmInterval   = 3 * time.Second
	timerAlarmTimeout    = 60 * time.Second
	timerDefaultSnooze   = 9 * time.Minute
	timerReminderSyncGap = 5 * time.Minute
)

// timerAnnounce is installed by the integration layer. It must not block; the
// scheduler calls it from its own goroutine.
var timerAnnounce func(string)

type timerEntry struct {
	ID        string `json:"id"`
	Label     string `json:"label,omitempty"`
	CreatedAt int64  `json:"created_at"`
	Deadline  int64  `json:"deadline,omitempty"`  // UnixNano; zero while paused
	Remaining int64  `json:"remaining,omitempty"` // UnixNano; meaningful while paused
	Paused    bool   `json:"paused,omitempty"`
}

type alarmEntry struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Hour        int    `json:"hour"`
	Minute      int    `json:"minute"`
	RepeatDays  []int  `json:"repeat_days,omitempty"` // 0=Sunday .. 6=Saturday
	Next        int64  `json:"next,omitempty"`        // UnixNano
	SnoozeUntil int64  `json:"snooze_until,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	Active      bool   `json:"active,omitempty"` // ringing/just-fired; reset on load
}

// reminderRecord wraps the shared Reminder model with sync bookkeeping. The
// embedded struct keeps timers.json compatible with a plain Reminder array.
type reminderRecord struct {
	Reminder
	Pending bool `json:"pending,omitempty"`
	Deleted bool `json:"deleted,omitempty"`
}

type stopwatchState struct {
	Running     bool  `json:"running"`
	StartedAt   int64 `json:"started_at,omitempty"`  // UnixNano
	Accumulated int64 `json:"accumulated,omitempty"` // UnixNano
}

type timerEvent struct {
	Kind string // timer, alarm, reminder
	ID   string
	Text string
}

type timerScheduler struct {
	mu            sync.Mutex
	reminderOpMu  sync.Mutex
	persistMu     sync.Mutex
	alarmVolumeMu sync.Mutex

	path    string
	backend ReminderBackend

	now        func() time.Time
	playCue    func()
	pauseMedia func()
	getVolume  func() (int, error)
	setVolume  func(int) error

	timers            []timerEntry
	alarms            []alarmEntry
	reminders         []reminderRecord
	stopwatch         stopwatchState
	sleepDeadline     int64
	sleepLabel        string
	alarmVolume       int
	alarmPrevVolume   int
	alarmVolumeActive bool

	alarmStops  map[string]chan struct{}
	lastAlarmID string

	idSeq int64

	wake       chan struct{}
	stopCh     chan struct{}
	started    bool
	stopped    bool
	lastTick   time.Time
	alarmEvery time.Duration
	alarmFor   time.Duration
	syncEvery  time.Duration
}

var timerManager = newTimerScheduler(
	filepath.Join(stateDirectory, "timers.json"),
	newJSONFileReminderBackend(filepath.Join(stateDirectory, "reminders.json")),
)

func newTimerScheduler(path string, backend ReminderBackend) *timerScheduler {
	return &timerScheduler{
		path:       path,
		backend:    backend,
		now:        time.Now,
		playCue:    playTimerCue,
		pauseMedia: pauseMediaForSleep,
		getVolume:  nativeVolume,
		setVolume: func(level int) error {
			_, err := wampCall("com.harman.volumeSet", level)
			return err
		},
		alarmStops: make(map[string]chan struct{}),
		wake:       make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
		alarmEvery: timerAlarmInterval,
		alarmFor:   timerAlarmTimeout,
		syncEvery:  timerReminderSyncGap,
	}
}

// initTimerManager loads persisted state, reschedules everything and starts the
// scheduler. It is safe to call more than once.
func initTimerManager() {
	timerManager.start()
}

// loadTimerState reloads timers.json without starting the scheduler. It is
// exposed for integration tests and explicit reconfiguration.
func loadTimerState() {
	_ = timerManager.load()
}

// setReminderBackend injects the Google Calendar/Tasks implementation. A nil
// backend keeps reminders local-only. Existing pending changes are retried by
// the next sync.
func setReminderBackend(backend ReminderBackend) {
	timerManager.setBackend(backend)
}

func (m *timerScheduler) setBackend(backend ReminderBackend) {
	m.reminderOpMu.Lock()
	m.backend = backend
	m.reminderOpMu.Unlock()
	m.notify()

	m.mu.Lock()
	started := m.started && !m.stopped
	m.mu.Unlock()
	if started && backend != nil {
		go m.syncReminders()
	}
}

func (m *timerScheduler) start() {
	m.mu.Lock()
	if m.started || m.stopped {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()

	_ = m.load()
	m.mu.Lock()
	m.lastTick = m.now()
	m.mu.Unlock()

	go m.loop()
	go func() { m.syncReminders() }()
	if m.syncEvery > 0 {
		go m.syncLoop()
	}
}

func (m *timerScheduler) stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	close(m.stopCh)
	ids := make([]string, 0, len(m.alarmStops))
	for id := range m.alarmStops {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.stopAlarmLoop(id)
	}
	m.restoreAlarmVolume()
}

func (m *timerScheduler) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *timerScheduler) syncLoop() {
	for {
		select {
		case <-m.stopCh:
			return
		case <-time.After(m.syncEvery):
			m.syncReminders()
		}
	}
}

func (m *timerScheduler) loop() {
	for {
		m.mu.Lock()
		now := m.now()
		next := m.nextDueLocked(now)
		m.mu.Unlock()

		wait := time.Hour
		if !next.IsZero() {
			wait = next.Sub(now)
			if wait < 0 {
				wait = 0
			}
			if wait > timerMaxSleep {
				wait = timerMaxSleep
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-m.stopCh:
			timer.Stop()
			return
		case <-m.wake:
			timer.Stop()
		case <-timer.C:
		}
		m.tick(m.now())
	}
}

// tick is the testable scheduler step. Tests drive it with a fake clock instead
// of sleeping.
func (m *timerScheduler) tick(now time.Time) {
	m.mu.Lock()
	previous := m.lastTick
	m.lastTick = now
	m.mu.Unlock()

	events := m.collectDue(now)
	if len(events) > 0 {
		_ = m.save()
	}
	for _, event := range events {
		m.deliver(event)
	}

	if !previous.IsZero() {
		delta := now.Sub(previous)
		// A wall-clock jump (manual change or NTP correction) can invalidate the
		// absolute next-alarm timestamp. Recompute it from the time-of-day and
		// repeat days, which is what the native Google alarm does as well.
		if delta < 0 || delta > 2*timerMaxSleep {
			m.recomputeAlarms(now)
		}
	}
}

func (m *timerScheduler) nextDueLocked(now time.Time) time.Time {
	var next time.Time
	consider := func(nanos int64) {
		if nanos <= 0 {
			return
		}
		t := time.Unix(0, nanos)
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for _, timer := range m.timers {
		if !timer.Paused {
			consider(timer.Deadline)
		}
	}
	for _, alarm := range m.alarms {
		if alarm.SnoozeUntil > 0 {
			consider(alarm.SnoozeUntil)
		} else {
			consider(alarm.Next)
		}
	}
	for _, reminder := range m.reminders {
		if due, ok := scheduledReminderDue(reminder); ok {
			consider(due.UnixNano())
		}
	}
	consider(m.sleepDeadline)
	return next
}

// collectDue mutates state and returns the events that must be announced. It
// never plays audio, so tests can inspect scheduling without touching the
// mixer.
func (m *timerScheduler) collectDue(now time.Time) []timerEvent {
	m.mu.Lock()
	defer m.mu.Unlock()

	var events []timerEvent

	keptTimers := m.timers[:0]
	for _, timer := range m.timers {
		if !timer.Paused && timer.Deadline > 0 && now.UnixNano() >= timer.Deadline {
			events = append(events, timerEvent{Kind: "timer", ID: timer.ID, Text: timer.Label})
			continue
		}
		keptTimers = append(keptTimers, timer)
	}
	m.timers = keptTimers

	keptAlarms := m.alarms[:0]
	for _, alarm := range m.alarms {
		due := false
		if alarm.SnoozeUntil > 0 {
			due = now.UnixNano() >= alarm.SnoozeUntil
		} else {
			due = alarm.Next > 0 && now.UnixNano() >= alarm.Next
		}
		if !due {
			keptAlarms = append(keptAlarms, alarm)
			continue
		}

		events = append(events, timerEvent{Kind: "alarm", ID: alarm.ID, Text: alarm.Label})
		m.lastAlarmID = alarm.ID
		alarm.SnoozeUntil = 0
		alarm.Active = true
		if len(alarm.RepeatDays) == 0 {
			// Keep the fired one-shot alarm long enough for snooze_alarm to find
			// it. cancel_alarm, stop_alarm or the ring timeout removes it.
			alarm.Next = 0
		} else {
			alarm.Next = nextAlarmOccurrence(now, alarm.Hour, alarm.Minute, alarm.RepeatDays).UnixNano()
		}
		keptAlarms = append(keptAlarms, alarm)
	}
	m.alarms = keptAlarms

	if m.sleepDeadline > 0 && now.UnixNano() >= m.sleepDeadline {
		events = append(events, timerEvent{Kind: "sleep", Text: m.sleepLabel})
		m.sleepDeadline = 0
		m.sleepLabel = ""
	}

	for _, reminder := range m.reminders {
		if due, ok := scheduledReminderDue(reminder); ok && !due.After(now) {
			events = append(events, timerEvent{Kind: "reminder", ID: reminder.ID, Text: reminder.Text})
		}
	}

	return events
}

func (m *timerScheduler) deliver(event timerEvent) {
	text := timerAnnouncement(event)
	log.Printf("timer due: kind=%s id=%s text=%q", event.Kind, event.ID, event.Text)
	if event.Kind == "reminder" {
		// Personal reminders and Google Tasks must never be spoken aloud:
		// other people in the room can hear them. Mark the reminder handled
		// silently and keep the audio path free.
		m.reminderFired(event.ID)
		return
	}
	if event.Kind == "sleep" {
		if m.pauseMedia != nil {
			m.pauseMedia()
		}
		if timerAnnounce != nil {
			announceTimer(text)
		}
		return
	}
	if event.Kind == "alarm" {
		// The first cue is played here; startAlarmLoop repeats it after each
		// interval until the alarm is stopped, snoozed or times out.
		m.applyAlarmVolume()
		m.playCue()
		if timerAnnounce != nil {
			announceTimer(text)
		}
		m.startAlarmLoop(event)
		return
	}

	m.playCue()
	if timerAnnounce != nil {
		announceTimer(text)
	}
}

// announceTimer isolates the integration callback so a panic cannot kill the
// scheduler goroutine.
func announceTimer(text string) {
	if timerAnnounce == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Println("timer announce panic:", recovered)
		}
	}()
	timerAnnounce(text)
}

func timerAnnouncement(event timerEvent) string {
	label := strings.TrimSpace(event.Text)
	switch event.Kind {
	case "timer":
		if label != "" {
			return "Таймер: " + label
		}
		return "Таймер сработал"
	case "alarm":
		if label != "" {
			return "Будильник: " + label
		}
		return "Будильник"
	case "sleep":
		return "Таймер сна: музыка выключена."
	case "reminder":
		if label != "" {
			return "Напоминание: " + label
		}
		return "Напоминание"
	}
	return label
}

func (m *timerScheduler) startAlarmLoop(event timerEvent) {
	stop := make(chan struct{})
	m.mu.Lock()
	// A cancel/stop/snooze may have arrived while the first cue was playing.
	// Only ring while the alarm is still active.
	active := false
	for _, alarm := range m.alarms {
		if alarm.ID == event.ID && alarm.Active {
			active = true
			break
		}
	}
	if !active || m.stopped {
		m.mu.Unlock()
		return
	}
	if old, ok := m.alarmStops[event.ID]; ok {
		close(old)
	}
	m.alarmStops[event.ID] = stop
	interval, timeout := m.alarmEvery, m.alarmFor
	m.mu.Unlock()

	go func() {
		deadline := time.Now().Add(timeout)
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
			}
			if !time.Now().Before(deadline) {
				m.alarmTimedOut(event.ID, stop)
				return
			}
			m.playCue()
			timer.Reset(interval)
		}
	}()
}

func (m *timerScheduler) stopAlarmLoop(id string) {
	m.mu.Lock()
	if stop, ok := m.alarmStops[id]; ok {
		delete(m.alarmStops, id)
		close(stop)
	}
	m.mu.Unlock()
	m.restoreAlarmVolume()
}

func (m *timerScheduler) alarmTimedOut(id string, stop chan struct{}) {
	m.mu.Lock()
	current, ok := m.alarmStops[id]
	if !ok || current != stop {
		m.mu.Unlock()
		return
	}
	delete(m.alarmStops, id)

	kept := m.alarms[:0]
	for _, alarm := range m.alarms {
		if alarm.ID == id {
			if len(alarm.RepeatDays) == 0 {
				continue
			}
			alarm.Active = false
		}
		kept = append(kept, alarm)
	}
	m.alarms = kept
	m.mu.Unlock()
	m.restoreAlarmVolume()

	_ = m.save()
	m.notify()
}

func (m *timerScheduler) reminderFired(id string) {
	m.reminderOpMu.Lock()
	defer m.reminderOpMu.Unlock()

	m.mu.Lock()
	found := false
	now := m.now()
	for i := range m.reminders {
		if m.reminders[i].ID == id {
			// Sync may have changed the reminder before we took reminderOpMu.
			if due, ok := scheduledReminderDue(m.reminders[i]); ok && !due.After(now) {
				m.reminders[i].Deleted = true
				m.reminders[i].Pending = true
				found = true
			}
			break
		}
	}
	m.mu.Unlock()
	if !found {
		return
	}
	_ = m.save()

	backend := m.backend
	if backend == nil {
		m.removeReminderRecord(id)
		_ = m.save()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timerBackendTimeout)
	defer cancel()
	if err := backend.Delete(ctx, id); err == nil || errors.Is(err, ErrReminderNotFound) {
		m.removeReminderRecord(id)
		_ = m.save()
	}
}

func (m *timerScheduler) recomputeAlarms(now time.Time) {
	m.mu.Lock()
	m.recomputeAlarmsLocked(now)
	m.mu.Unlock()
	m.notify()
}

func (m *timerScheduler) recomputeAlarmsLocked(now time.Time) {
	for i := range m.alarms {
		alarm := &m.alarms[i]
		if alarm.Active {
			continue
		}
		if alarm.SnoozeUntil > now.UnixNano() {
			continue
		}
		alarm.Next = nextAlarmOccurrence(now, alarm.Hour, alarm.Minute, alarm.RepeatDays).UnixNano()
	}
}

// nextAlarmOccurrence is pure scheduling math. It always recomputes from the
// local time-of-day and repeat days, so a clock change or DST transition cannot
// leave an alarm pinned to a stale absolute instant.
func nextAlarmOccurrence(now time.Time, hour, minute int, days []int) time.Time {
	location := now.Location()
	if len(days) == 0 {
		candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, location)
		if !candidate.After(now) {
			candidate = time.Date(now.Year(), now.Month(), now.Day()+1, hour, minute, 0, 0, location)
		}
		return candidate
	}

	allowed := make(map[time.Weekday]bool, len(days))
	for _, day := range days {
		if day >= 0 && day < 7 {
			allowed[time.Weekday(day)] = true
		}
	}
	for offset := 0; offset <= 7; offset++ {
		candidate := time.Date(now.Year(), now.Month(), now.Day()+offset, hour, minute, 0, 0, location)
		if candidate.After(now) && allowed[candidate.Weekday()] {
			return candidate
		}
	}
	return time.Date(now.Year(), now.Month(), now.Day()+7, hour, minute, 0, 0, location)
}

// ---------------------------------------------------------------------------
// Tool declarations and dispatch
// ---------------------------------------------------------------------------

func timerToolDeclarations() []M {
	empty := M{"type": "OBJECT", "properties": M{}}
	return []M{
		{
			"name":        "set_timer",
			"description": "Create a local countdown timer. Use duration_seconds for a number of seconds, or duration for a human string such as '10 minutes', '1h30m', '1 час 30 минут'. Optional label is announced when it fires.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"duration_seconds": M{"type": "INTEGER", "description": "Duration in seconds. Preferred for exact values."},
				"duration":         M{"type": "STRING", "description": "Human duration, for example '10 minutes', '90 seconds', '1h30m'."},
				"hours":            M{"type": "INTEGER"},
				"minutes":          M{"type": "INTEGER"},
				"seconds":          M{"type": "INTEGER"},
				"label":            M{"type": "STRING", "description": "Optional name, for example 'tea' or 'pasta'."},
			}},
		},
		{
			"name":        "list_timers",
			"description": "List active local countdown timers with their remaining time.",
			"parameters":  empty,
		},
		{
			"name":        "cancel_timer",
			"description": "Cancel a timer by id or 1-based index. Use all=true to cancel every timer.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"id":    M{"type": "STRING"},
				"index": M{"type": "INTEGER"},
				"all":   M{"type": "BOOLEAN"},
			}},
		},
		{
			"name":        "pause_timer",
			"description": "Pause a running timer by id or 1-based index.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"id":    M{"type": "STRING"},
				"index": M{"type": "INTEGER"},
			}},
		},
		{
			"name":        "resume_timer",
			"description": "Resume a paused timer by id or 1-based index.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"id":    M{"type": "STRING"},
				"index": M{"type": "INTEGER"},
			}},
		},
		{
			"name":        "add_timer_time",
			"description": "Add time to a timer. Use duration_seconds or a human duration. Works for running and paused timers.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"id":               M{"type": "STRING"},
				"index":            M{"type": "INTEGER"},
				"duration_seconds": M{"type": "INTEGER"},
				"duration":         M{"type": "STRING"},
				"hours":            M{"type": "INTEGER"},
				"minutes":          M{"type": "INTEGER"},
				"seconds":          M{"type": "INTEGER"},
			}},
		},
		{
			"name":        "set_alarm",
			"description": "Create a local alarm at a time of day. time is HH:MM in speaker local time. repeat_days is an optional comma-separated list of weekdays (mon,tue,wed,thu,fri,sat,sun), or 'daily', 'weekdays', 'weekends'. Without repeat_days the alarm rings once.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"time":        M{"type": "STRING", "description": "Time of day as HH:MM, for example '07:30'."},
				"hour":        M{"type": "INTEGER", "description": "Alternative to time: hour 0..23."},
				"minute":      M{"type": "INTEGER", "description": "Alternative to time: minute 0..59."},
				"repeat_days": M{"type": "STRING", "description": "Comma-separated weekdays, or daily/weekdays/weekends."},
				"label":       M{"type": "STRING", "description": "Optional name, for example 'work' or 'medicine'."},
			}},
		},
		{
			"name":        "list_alarms",
			"description": "List alarms with their next occurrence and snooze state.",
			"parameters":  empty,
		},
		{
			"name":        "cancel_alarm",
			"description": "Cancel an alarm by id or 1-based index. Use all=true to cancel every alarm. Also stops a ringing alarm.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"id":    M{"type": "STRING"},
				"index": M{"type": "INTEGER"},
				"all":   M{"type": "BOOLEAN"},
			}},
		},
		{
			"name":        "stop_alarm",
			"description": "Stop the currently ringing alarm. A repeating alarm stays scheduled; a one-shot alarm is removed.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"id":    M{"type": "STRING"},
				"index": M{"type": "INTEGER"},
			}},
		},
		{
			"name":        "snooze_alarm",
			"description": "Snooze a ringing or scheduled alarm. Default snooze is 9 minutes. If id/index is omitted and exactly one alarm exists, that alarm is used.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"id":      M{"type": "STRING"},
				"index":   M{"type": "INTEGER"},
				"minutes": M{"type": "INTEGER", "description": "Snooze length in minutes, default 9."},
			}},
		},
		{
			"name":        "set_reminder",
			"description": "Create or update a reminder with text at a time. Reminders synchronise with Google Calendar/Tasks when configured. at accepts HH:MM, RFC3339 with offset, or 'YYYY-MM-DD HH:MM'; use get_time's iso/local_iso and never append Z to a local time. in_seconds is an alternative. remote_kind=event creates a Google Calendar event instead of a task.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"text":        M{"type": "STRING", "description": "What to remind the user about."},
				"at":          M{"type": "STRING", "description": "HH:MM, RFC3339, or YYYY-MM-DD HH:MM."},
				"in_seconds":  M{"type": "INTEGER", "description": "Alternative relative delay in seconds."},
				"date":        M{"type": "STRING", "description": "Optional YYYY-MM-DD used with a HH:MM time."},
				"id":          M{"type": "STRING", "description": "Existing reminder id to update instead of creating a new one."},
				"remote_kind": M{"type": "STRING", "enum": []string{"task", "event"}, "description": "task (default) or event for Google Calendar."},
			}},
		},
		{
			"name":        "list_reminders",
			"description": "List reminders. `at` is local RFC3339 with offset; `at_utc` is UTC; each item also has `local_time`, `local_date`, `weekday`, `timezone`, `when` and `local_iso`. Always speak the local fields. Shows local pending changes when Google Calendar/Tasks is offline.",
			"parameters":  empty,
		},
		{
			"name":        "cancel_reminder",
			"description": "Cancel a reminder by id or 1-based index. Use all=true to cancel every reminder.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"id":    M{"type": "STRING"},
				"index": M{"type": "INTEGER"},
				"all":   M{"type": "BOOLEAN"},
			}},
		},
		{
			"name":        "sync_reminders",
			"description": "Retry pending reminder changes and synchronise with Google Calendar/Tasks now.",
			"parameters":  empty,
		},
		{
			"name":        "set_alarm_volume",
			"description": "Set the volume level used when an alarm rings (0-100). The previous volume is restored when the alarm is stopped, snoozed or times out.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"level": M{"type": "INTEGER", "description": "Alarm volume 0..100."},
			}, "required": []string{"level"}},
		},
		{
			"name":        "get_alarm_volume",
			"description": "Read the configured alarm volume.",
			"parameters":  empty,
		},
		{
			"name":        "set_sleep_timer",
			"description": "Stop media playback after a duration. Use minutes or in_seconds. This is a sleep timer, not a cooking timer.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"minutes":    M{"type": "INTEGER", "description": "Delay in minutes, e.g. 30."},
				"in_seconds": M{"type": "INTEGER", "description": "Alternative delay in seconds."},
				"label":      M{"type": "STRING", "description": "Optional label."},
			}},
		},
		{
			"name":        "cancel_sleep_timer",
			"description": "Cancel the active sleep timer.",
			"parameters":  empty,
		},
		{
			"name":        "get_sleep_timer",
			"description": "Read the remaining sleep timer.",
			"parameters":  empty,
		},
		{
			"name":        "start_stopwatch",
			"description": "Start, pause, resume, reset or read the local stopwatch. action defaults to start. Use read for 'how long?'.",
			"parameters": M{"type": "OBJECT", "properties": M{
				"action": M{"type": "STRING", "enum": []string{"start", "pause", "resume", "reset", "read"}},
			}},
		},
		{
			"name":        "stop_stopwatch",
			"description": "Stop the stopwatch and return the elapsed time. The elapsed value is kept until reset.",
			"parameters":  empty,
		},
	}
}

func handleTimerTool(name string, args M) (M, bool) {
	// Be forgiving if the integration layer forgot the explicit init call.
	timerManager.start()
	switch name {
	case "set_reminder", "list_reminders", "cancel_reminder", "sync_reminders":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
	}
	return handleTimerToolOn(timerManager, name, args)
}

// handleTimerToolOn is the testable dispatcher. It returns handled=false only
// for names that do not belong to the timer feature.
func handleTimerToolOn(m *timerScheduler, name string, args M) (M, bool) {
	if m == nil {
		return M{"ok": false, "error": "timer manager is not configured"}, true
	}
	if args == nil {
		args = M{}
	}
	switch name {
	case "set_timer":
		return m.toolSetTimer(args), true
	case "list_timers":
		return m.toolListTimers(), true
	case "cancel_timer":
		return m.toolCancelTimer(args), true
	case "pause_timer":
		return m.toolPauseTimer(args), true
	case "resume_timer":
		return m.toolResumeTimer(args), true
	case "add_timer_time", "add_timer":
		return m.toolAddTimerTime(args), true
	case "set_alarm":
		return m.toolSetAlarm(args), true
	case "list_alarms":
		return m.toolListAlarms(), true
	case "cancel_alarm":
		return m.toolCancelAlarm(args), true
	case "stop_alarm":
		return m.toolStopAlarm(args), true
	case "snooze_alarm":
		return m.toolSnoozeAlarm(args), true
	case "set_reminder":
		return m.toolSetReminder(args), true
	case "list_reminders":
		return m.toolListReminders(), true
	case "cancel_reminder":
		return m.toolCancelReminder(args), true
	case "sync_reminders":
		return m.syncReminders(), true
	case "set_alarm_volume":
		return m.toolSetAlarmVolume(args), true
	case "get_alarm_volume":
		return m.toolGetAlarmVolume(), true
	case "set_sleep_timer":
		return m.toolSetSleepTimer(args), true
	case "cancel_sleep_timer":
		return m.toolCancelSleepTimer(), true
	case "get_sleep_timer":
		return m.toolGetSleepTimer(), true
	case "start_stopwatch":
		return m.toolStartStopwatch(args), true
	case "stop_stopwatch":
		return m.toolStopStopwatch(), true
	case "pause_stopwatch":
		return m.toolStartStopwatch(M{"action": "pause"}), true
	case "resume_stopwatch":
		return m.toolStartStopwatch(M{"action": "resume"}), true
	case "reset_stopwatch":
		return m.toolStartStopwatch(M{"action": "reset"}), true
	case "read_stopwatch":
		return m.toolStartStopwatch(M{"action": "read"}), true
	default:
		return nil, false
	}
}

// ---------------------------------------------------------------------------
// Timer tools
// ---------------------------------------------------------------------------

func (m *timerScheduler) toolSetTimer(args M) M {
	duration, err := parseTimerDuration(args)
	if err != nil {
		return timerError(err)
	}
	label := firstString(args, "label", "name", "title")
	now := m.now()

	m.mu.Lock()
	id := m.nextIDLocked("t")
	deadline := now.Add(duration).UnixNano()
	m.timers = append(m.timers, timerEntry{ID: id, Label: label, CreatedAt: now.UnixNano(), Deadline: deadline})
	m.mu.Unlock()

	_ = m.save()
	m.notify()
	return M{
		"ok": true, "id": id, "kind": "timer", "label": label,
		"duration_seconds":  ceilSeconds(duration),
		"remaining_seconds": ceilSeconds(duration),
		"fire_at":           time.Unix(0, deadline).Format(time.RFC3339),
	}
}

func (m *timerScheduler) toolListTimers() M {
	now := m.now()
	m.mu.Lock()
	timers := append([]timerEntry(nil), m.timers...)
	m.mu.Unlock()

	items := make([]M, 0, len(timers))
	for _, timer := range timers {
		remaining := time.Duration(timer.Remaining)
		if !timer.Paused {
			remaining = time.Duration(timer.Deadline - now.UnixNano())
		}
		if remaining < 0 {
			remaining = 0
		}
		item := M{
			"id": timer.ID, "label": timer.Label, "paused": timer.Paused,
			"remaining_seconds": ceilSeconds(remaining),
		}
		if !timer.Paused && timer.Deadline > 0 {
			item["fire_at"] = time.Unix(0, timer.Deadline).Format(time.RFC3339)
		}
		items = append(items, item)
	}
	return M{"ok": true, "count": len(items), "timers": items}
}

func (m *timerScheduler) toolCancelTimer(args M) M {
	m.mu.Lock()
	if boolArg(args, "all") {
		count := len(m.timers)
		m.timers = nil
		m.mu.Unlock()
		_ = m.save()
		m.notify()
		return M{"ok": true, "cancelled": count}
	}
	index, err := findTimerIndexLocked(m.timers, args)
	if err != nil {
		m.mu.Unlock()
		return timerError(err)
	}
	id := m.timers[index].ID
	m.timers = append(m.timers[:index], m.timers[index+1:]...)
	m.mu.Unlock()

	_ = m.save()
	m.notify()
	return M{"ok": true, "cancelled": 1, "id": id}
}

func (m *timerScheduler) toolPauseTimer(args M) M {
	now := m.now()
	m.mu.Lock()
	index, err := findTimerIndexLocked(m.timers, args)
	if err != nil {
		m.mu.Unlock()
		return timerError(err)
	}
	timer := &m.timers[index]
	if timer.Paused {
		m.mu.Unlock()
		return timerError(errors.New("timer is already paused"))
	}
	remaining := timer.Deadline - now.UnixNano()
	if remaining < 0 {
		remaining = 0
	}
	timer.Remaining = remaining
	timer.Paused = true
	timer.Deadline = 0
	id := timer.ID
	m.mu.Unlock()

	_ = m.save()
	m.notify()
	return M{"ok": true, "id": id, "paused": true, "remaining_seconds": ceilSeconds(time.Duration(remaining))}
}

func (m *timerScheduler) toolResumeTimer(args M) M {
	now := m.now()
	m.mu.Lock()
	index, err := findTimerIndexLocked(m.timers, args)
	if err != nil {
		m.mu.Unlock()
		return timerError(err)
	}
	timer := &m.timers[index]
	if !timer.Paused {
		m.mu.Unlock()
		return timerError(errors.New("timer is not paused"))
	}
	timer.Deadline = now.Add(time.Duration(timer.Remaining)).UnixNano()
	timer.Paused = false
	remaining := timer.Remaining
	timer.Remaining = 0
	id := timer.ID
	m.mu.Unlock()

	_ = m.save()
	m.notify()
	return M{"ok": true, "id": id, "paused": false, "remaining_seconds": ceilSeconds(time.Duration(remaining))}
}

func (m *timerScheduler) toolAddTimerTime(args M) M {
	duration, err := parseTimerDuration(args)
	if err != nil {
		return timerError(err)
	}
	now := m.now()
	m.mu.Lock()
	index, err := findTimerIndexLocked(m.timers, args)
	if err != nil {
		m.mu.Unlock()
		return timerError(err)
	}
	timer := &m.timers[index]
	if timer.Paused {
		timer.Remaining += int64(duration)
	} else {
		timer.Deadline += int64(duration)
	}
	id := timer.ID
	remaining := time.Duration(timer.Remaining)
	if !timer.Paused {
		remaining = time.Duration(timer.Deadline - now.UnixNano())
	}
	m.mu.Unlock()

	_ = m.save()
	m.notify()
	return M{"ok": true, "id": id, "added_seconds": ceilSeconds(duration), "remaining_seconds": ceilSeconds(remaining)}
}

// ---------------------------------------------------------------------------
// Alarm tools
// ---------------------------------------------------------------------------

func (m *timerScheduler) toolSetAlarm(args M) M {
	hour, minute, err := alarmTimeArgs(args)
	if err != nil {
		return timerError(err)
	}
	days, err := parseRepeatDays(firstArg(args, "repeat_days", "repeat", "days", "weekdays"))
	if err != nil {
		return timerError(err)
	}
	label := firstString(args, "label", "name", "title")
	now := m.now()
	next := nextAlarmOccurrence(now, hour, minute, days)

	m.mu.Lock()
	id := m.nextIDLocked("a")
	m.alarms = append(m.alarms, alarmEntry{
		ID: id, Label: label, Hour: hour, Minute: minute, RepeatDays: days,
		Next: next.UnixNano(), CreatedAt: now.UnixNano(),
	})
	m.mu.Unlock()

	_ = m.save()
	m.notify()
	return M{
		"ok": true, "id": id, "kind": "alarm", "label": label,
		"time": formatClock(hour, minute), "repeat_days": weekdayNames(days),
		"next_at": next.Format(time.RFC3339), "next_in_seconds": ceilSeconds(next.Sub(now)),
	}
}

func (m *timerScheduler) toolListAlarms() M {
	now := m.now()
	m.mu.Lock()
	alarms := append([]alarmEntry(nil), m.alarms...)
	active := make(map[string]bool, len(m.alarmStops))
	for id := range m.alarmStops {
		active[id] = true
	}
	m.mu.Unlock()

	items := make([]M, 0, len(alarms))
	for _, alarm := range alarms {
		item := M{
			"id": alarm.ID, "label": alarm.Label,
			"time":        formatClock(alarm.Hour, alarm.Minute),
			"repeat_days": weekdayNames(alarm.RepeatDays),
			"active":      active[alarm.ID],
		}
		if alarm.Next > 0 {
			item["next_at"] = time.Unix(0, alarm.Next).Format(time.RFC3339)
			item["next_in_seconds"] = ceilSeconds(time.Duration(alarm.Next - now.UnixNano()))
		}
		if alarm.SnoozeUntil > 0 {
			item["snoozed_until"] = time.Unix(0, alarm.SnoozeUntil).Format(time.RFC3339)
			item["snooze_in_seconds"] = ceilSeconds(time.Duration(alarm.SnoozeUntil - now.UnixNano()))
		}
		items = append(items, item)
	}
	return M{"ok": true, "count": len(items), "alarms": items}
}

func (m *timerScheduler) toolCancelAlarm(args M) M {
	if boolArg(args, "all") {
		m.mu.Lock()
		ids := make([]string, 0, len(m.alarms))
		for _, alarm := range m.alarms {
			ids = append(ids, alarm.ID)
		}
		count := len(m.alarms)
		m.alarms = nil
		m.mu.Unlock()
		for _, id := range ids {
			m.stopAlarmLoop(id)
		}
		_ = m.save()
		m.notify()
		return M{"ok": true, "cancelled": count}
	}

	m.mu.Lock()
	index, err := findAlarmIndexLocked(m.alarms, args, m.lastAlarmID)
	if err != nil {
		m.mu.Unlock()
		return timerError(err)
	}
	id := m.alarms[index].ID
	m.alarms = append(m.alarms[:index], m.alarms[index+1:]...)
	m.mu.Unlock()

	m.stopAlarmLoop(id)
	_ = m.save()
	m.notify()
	return M{"ok": true, "cancelled": 1, "id": id}
}

func (m *timerScheduler) toolStopAlarm(args M) M {
	m.mu.Lock()
	index, err := findAlarmIndexLocked(m.alarms, args, m.lastAlarmID)
	if err != nil {
		m.mu.Unlock()
		return timerError(err)
	}
	alarm := m.alarms[index]
	id := alarm.ID
	if len(alarm.RepeatDays) == 0 {
		m.alarms = append(m.alarms[:index], m.alarms[index+1:]...)
	} else {
		m.alarms[index].Active = false
		m.alarms[index].SnoozeUntil = 0
	}
	m.mu.Unlock()

	m.stopAlarmLoop(id)
	_ = m.save()
	m.notify()
	return M{"ok": true, "id": id, "stopped": true}
}

func (m *timerScheduler) toolSnoozeAlarm(args M) M {
	minutes, ok := intArg(args, "minutes", "snooze_minutes")
	if !ok {
		minutes = int(timerDefaultSnooze / time.Minute)
	}
	if minutes < 1 || minutes > 1440 {
		return timerError(errors.New("snooze minutes must be 1..1440"))
	}

	now := m.now()
	m.mu.Lock()
	index, err := findAlarmIndexLocked(m.alarms, args, m.lastAlarmID)
	if err != nil {
		m.mu.Unlock()
		return timerError(err)
	}
	alarm := &m.alarms[index]
	alarm.SnoozeUntil = now.Add(time.Duration(minutes) * time.Minute).UnixNano()
	alarm.Active = false
	id := alarm.ID
	snoozedUntil := alarm.SnoozeUntil
	m.mu.Unlock()

	m.stopAlarmLoop(id)
	_ = m.save()
	m.notify()
	return M{
		"ok": true, "id": id, "snooze_minutes": minutes,
		"snoozed_until": time.Unix(0, snoozedUntil).Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// Reminder tools
// ---------------------------------------------------------------------------

func (m *timerScheduler) toolSetReminder(args M) M {
	text := firstString(args, "text", "message", "title")
	if text == "" {
		return timerError(ErrReminderTextEmpty)
	}
	now := m.now()
	due, err := reminderDueArg(args, now)
	if err != nil {
		return timerError(err)
	}
	due = due.UTC()
	if !due.After(now) {
		return timerError(errors.New("reminder time must be in the future"))
	}
	remoteKind := strings.TrimSpace(firstString(args, "remote_kind", "kind"))
	if remoteKind != "" && remoteKind != RemoteKindTask && remoteKind != RemoteKindEvent {
		return timerError(errors.New("remote_kind must be task or event"))
	}
	if id := firstString(args, "id", "reminder_id"); id != "" {
		return m.updateReminder(id, text, due)
	}
	return m.createReminder(text, due, now, remoteKind)
}

func (m *timerScheduler) toolSetSleepTimer(args M) M {
	var duration time.Duration
	if v, ok := numberArg(args["minutes"]); ok && v > 0 {
		duration = time.Duration(v * float64(time.Minute))
	} else if v, ok := numberArg(args["in_seconds"]); ok && v > 0 {
		duration = time.Duration(v * float64(time.Second))
	} else if v, ok := numberArg(args["duration_seconds"]); ok && v > 0 {
		duration = time.Duration(v * float64(time.Second))
	} else {
		return timerError(errors.New("minutes or in_seconds is required"))
	}
	if duration > 24*time.Hour {
		return timerError(errors.New("sleep timer must be at most 24 hours"))
	}
	now := m.now()
	deadline := now.Add(duration)
	m.mu.Lock()
	m.sleepDeadline = deadline.UnixNano()
	m.sleepLabel = firstString(args, "label", "text")
	m.mu.Unlock()
	_ = m.save()
	m.notify()
	return M{"ok": true, "at": deadline.Format(time.RFC3339), "seconds": int(duration.Seconds())}
}

func (m *timerScheduler) toolSetAlarmVolume(args M) M {
	level, ok := intArg(args, "level", "volume")
	if !ok || level < 0 || level > 100 {
		return timerError(errors.New("alarm volume must be 0..100"))
	}
	m.mu.Lock()
	m.alarmVolume = level
	m.mu.Unlock()
	_ = m.save()
	return M{"ok": true, "level": level, "instruction": "Коротко подтверди, что громкость будильника изменена."}
}

func (m *timerScheduler) toolGetAlarmVolume() M {
	m.mu.Lock()
	level := m.alarmVolume
	m.mu.Unlock()
	return M{"ok": true, "level": level}
}

func (m *timerScheduler) toolCancelSleepTimer() M {
	m.mu.Lock()
	had := m.sleepDeadline > 0
	m.sleepDeadline = 0
	m.sleepLabel = ""
	m.mu.Unlock()
	_ = m.save()
	m.notify()
	return M{"ok": true, "cancelled": had}
}

func (m *timerScheduler) toolGetSleepTimer() M {
	m.mu.Lock()
	deadline := m.sleepDeadline
	label := m.sleepLabel
	m.mu.Unlock()
	if deadline <= 0 {
		return M{"ok": true, "active": false}
	}
	remaining := time.Unix(0, deadline).Sub(m.now())
	if remaining < 0 {
		remaining = 0
	}
	return M{"ok": true, "active": true, "at": time.Unix(0, deadline).Format(time.RFC3339), "seconds": int(remaining.Seconds()), "label": label}
}

func (m *timerScheduler) createReminder(text string, due, now time.Time, remoteKind string) M {
	m.reminderOpMu.Lock()
	defer m.reminderOpMu.Unlock()

	m.mu.Lock()
	id := m.nextIDLocked("r")
	record := reminderRecord{
		Reminder: Reminder{
			ID: id, Text: text, Due: due.Format(time.RFC3339),
			Source: ReminderSourceLocal, RemoteKind: remoteKind,
		},
		Pending: true,
	}
	m.reminders = append(m.reminders, record)
	m.mu.Unlock()
	_ = m.save()

	offline := false
	var backendErr error
	finalID := id
	finalSource := ReminderSourceLocal
	if backend := m.backend; backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), timerBackendTimeout)
		created, err := backend.Create(ctx, record.Reminder)
		cancel()
		if err == nil {
			created = normalizeCreatedReminder(created, record.Reminder)
			finalID = created.ID
			finalSource = created.Source
			m.replaceReminderRecord(id, reminderRecord{Reminder: created})
			if _, googleBackend := backend.(*CalendarSync); googleBackend {
				sctx, scancel := context.WithTimeout(context.Background(), 12*time.Second)
				syncErr := backend.Sync(sctx)
				scancel()
				if syncErr != nil {
					offline = true
					backendErr = syncErr
				}
				if list, listErr := backend.List(context.Background()); listErr == nil {
					for _, remote := range list {
						if remote.ID == created.ID || (created.RemoteID != "" && remote.RemoteID == created.RemoteID) {
							created = remote
							finalSource = remote.Source
							m.replaceReminderRecord(id, reminderRecord{Reminder: remote})
							break
						}
					}
				}
			}
		} else {
			offline = true
			backendErr = err
		}
	} else {
		m.markReminderSynced(id)
	}

	_ = m.save()
	m.notify()
	result := M{
		"ok": true, "id": finalID, "text": text,
		"at": due.Format(time.RFC3339), "source": finalSource,
	}
	if offline {
		result["offline"] = true
		result["pending_sync"] = true
		if backendErr != nil {
			result["error"] = clean(backendErr)
		}
	}
	return result
}

func (m *timerScheduler) updateReminder(id, text string, due time.Time) M {
	m.reminderOpMu.Lock()
	defer m.reminderOpMu.Unlock()

	m.mu.Lock()
	index := -1
	for i := range m.reminders {
		if m.reminders[i].ID == id && !m.reminders[i].Deleted {
			index = i
			break
		}
	}
	if index < 0 {
		m.mu.Unlock()
		return timerError(ErrReminderNotFound)
	}
	record := m.reminders[index]
	record.Text = text
	record.Due = due.Format(time.RFC3339)
	record.Pending = true
	m.reminders[index] = record
	m.mu.Unlock()
	_ = m.save()

	offline := false
	var backendErr error
	if backend := m.backend; backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), timerBackendTimeout)
		updated, err := backend.Update(ctx, record.Reminder)
		cancel()
		if err == nil {
			updated = normalizeCreatedReminder(updated, record.Reminder)
			m.replaceReminderRecord(record.ID, reminderRecord{Reminder: updated})
		} else {
			offline = true
			backendErr = err
		}
	} else {
		m.markReminderSynced(record.ID)
	}

	_ = m.save()
	m.notify()
	result := M{"ok": true, "id": record.ID, "text": text, "at": due.Format(time.RFC3339), "updated": true}
	if offline {
		result["offline"] = true
		result["pending_sync"] = true
		if backendErr != nil {
			result["error"] = clean(backendErr)
		}
	}
	return result
}

func (m *timerScheduler) toolListReminders() M {
	m.reminderOpMu.Lock()
	defer m.reminderOpMu.Unlock()

	now := m.now()
	local := m.snapshotReminderRecords()
	offline := false
	var backendErr error

	if backend := m.backend; backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), timerBackendTimeout)
		remote, err := backend.List(ctx)
		cancel()
		if err == nil {
			local = mergeReminders(remote, local)
			m.replaceReminderRecords(local)
			_ = m.save()
			m.notify()
		} else {
			offline = true
			backendErr = err
		}
	}
	return remindersResult(local, now, offline, backendErr)
}

func (m *timerScheduler) toolCancelReminder(args M) M {
	m.reminderOpMu.Lock()
	defer m.reminderOpMu.Unlock()

	var ids []string
	removeLocally := false
	offline := false
	var backendErr error
	m.mu.Lock()
	if boolArg(args, "all") {
		for i := range m.reminders {
			if !m.reminders[i].Deleted {
				ids = append(ids, m.reminders[i].ID)
			}
		}
	} else {
		index, err := findReminderIndexLocked(m.reminders, args)
		if err != nil {
			m.mu.Unlock()
			return timerError(err)
		}
		ids = append(ids, m.reminders[index].ID)
	}
	backend := m.backend
	for _, id := range ids {
		if backend == nil {
			m.removeReminderRecordLocked(id)
			removeLocally = true
			continue
		}
		for i := range m.reminders {
			if m.reminders[i].ID == id {
				m.reminders[i].Deleted = true
				m.reminders[i].Pending = true
			}
		}
	}
	m.mu.Unlock()

	if removeLocally {
		_ = m.save()
	}
	if backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), timerBackendTimeout)
		defer cancel()
		for _, id := range ids {
			if err := backend.Delete(ctx, id); err == nil || errors.Is(err, ErrReminderNotFound) {
				m.removeReminderRecord(id)
			} else {
				offline = true
				backendErr = err
			}
		}
		_ = m.save()
	}

	result := M{"ok": true, "cancelled": len(ids)}
	if offline {
		result["offline"] = true
		result["pending_sync"] = true
		if backendErr != nil {
			result["error"] = clean(backendErr)
		}
	}
	m.notify()
	return result
}

// syncReminders retries local pending changes and then asks the backend to
// synchronise. It is deliberately serialised with create/update/delete/list so
// two tool calls cannot upload the same local reminder twice.
func (m *timerScheduler) syncReminders() M {
	m.reminderOpMu.Lock()
	defer m.reminderOpMu.Unlock()

	local := m.snapshotReminderRecords()
	backend := m.backend
	if backend == nil {
		return M{"ok": true, "synced": false, "local_only": true, "count": countVisibleReminders(local)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timerSyncTimeout)
	defer cancel()

	offline := false
	var firstErr error
	changed := false
	for i := range local {
		record := local[i]
		if !record.Pending {
			continue
		}
		if record.Deleted {
			if err := backend.Delete(ctx, record.ID); err != nil && !errors.Is(err, ErrReminderNotFound) {
				offline = true
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			local[i].ID = ""
			changed = true
			continue
		}
		if record.RemoteID == "" {
			created, err := backend.Create(ctx, record.Reminder)
			if err != nil {
				offline = true
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			created = normalizeCreatedReminder(created, record.Reminder)
			local[i] = reminderRecord{Reminder: created}
			changed = true
		} else {
			updated, err := backend.Update(ctx, record.Reminder)
			if err != nil {
				offline = true
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			updated = normalizeCreatedReminder(updated, record.Reminder)
			local[i] = reminderRecord{Reminder: updated}
			changed = true
		}
	}

	compact := local[:0]
	for _, record := range local {
		if record.ID != "" {
			compact = append(compact, record)
		}
	}
	local = compact
	if changed {
		m.replaceReminderRecords(local)
		_ = m.save()
	}

	if !offline {
		if err := backend.Sync(ctx); err != nil {
			offline = true
			firstErr = err
		}
	}
	if !offline {
		remote, err := backend.List(ctx)
		if err != nil {
			offline = true
			firstErr = err
		} else {
			local = mergeReminders(remote, local)
			m.replaceReminderRecords(local)
			_ = m.save()
		}
	}

	if offline {
		result := M{
			"ok": false, "offline": true, "error": clean(firstErr),
			"count": countVisibleReminders(local), "pending_sync": countPendingReminders(local),
		}
		m.notify()
		return result
	}
	m.notify()
	return M{"ok": true, "synced": true, "count": countVisibleReminders(local)}
}

func reminderLocation(zone string) (*time.Location, string) {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		preferenceState.Lock()
		zone = preferenceState.Timezone
		preferenceState.Unlock()
	}
	if loc, err := time.LoadLocation(zone); err == nil {
		return loc, zone
	}
	if loc, err := time.LoadLocation("Asia/Novosibirsk"); err == nil {
		return loc, "Asia/Novosibirsk"
	}
	return time.Local, time.Local.String()
}

func remindersResult(records []reminderRecord, now time.Time, offline bool, backendErr error) M {
	visible := make([]reminderRecord, 0, len(records))
	pending := 0
	for _, record := range records {
		if record.Pending {
			pending++
		}
		if record.Deleted {
			continue
		}
		visible = append(visible, record)
	}
	sort.SliceStable(visible, func(i, j int) bool {
		left, leftOK := reminderDue(visible[i].Reminder)
		right, rightOK := reminderDue(visible[j].Reminder)
		if leftOK != rightOK {
			return leftOK
		}
		return left.Before(right)
	})

	items := make([]M, 0, len(visible))
	for _, record := range visible {
		item := M{"id": record.ID, "text": record.Text, "source": record.Source}
		if due, ok := reminderDue(record.Reminder); ok {
			loc, zone := reminderLocation(record.TimeZone)
			local := due.In(loc)
			weekdays := []string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}
			item["at"] = local.Format(time.RFC3339)
			item["at_utc"] = due.UTC().Format(time.RFC3339)
			item["in_seconds"] = ceilSeconds(due.Sub(now))
			item["local_time"] = local.Format("15:04")
			item["local_date"] = local.Format("02.01.2006")
			item["weekday"] = weekdays[local.Weekday()]
			item["timezone"] = zone
			item["when"] = local.Format("02.01.2006 15:04")
			item["local_iso"] = local.Format(time.RFC3339)
		}
		if record.RemoteID != "" {
			item["remote_id"] = record.RemoteID
		}
		if record.RemoteKind != "" {
			item["remote_kind"] = record.RemoteKind
		}
		if record.Pending {
			item["pending_sync"] = true
		}
		items = append(items, item)
	}

	result := M{"ok": true, "count": len(items), "reminders": items}
	if pending > 0 {
		result["pending_sync"] = pending
	}
	if offline {
		result["offline"] = true
		if backendErr != nil {
			result["error"] = clean(backendErr)
		}
	}
	return result
}

func (m *timerScheduler) snapshotReminderRecords() []reminderRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]reminderRecord(nil), m.reminders...)
}

func (m *timerScheduler) replaceReminderRecords(records []reminderRecord) {
	m.mu.Lock()
	m.reminders = append([]reminderRecord(nil), records...)
	m.mu.Unlock()
}

func (m *timerScheduler) replaceReminderRecord(oldID string, record reminderRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.reminders {
		if m.reminders[i].ID == oldID {
			m.reminders[i] = record
			return
		}
	}
	m.reminders = append(m.reminders, record)
}

func (m *timerScheduler) removeReminderRecord(id string) {
	m.mu.Lock()
	m.removeReminderRecordLocked(id)
	m.mu.Unlock()
}

func (m *timerScheduler) removeReminderRecordLocked(id string) {
	kept := m.reminders[:0]
	for _, record := range m.reminders {
		if record.ID != id {
			kept = append(kept, record)
		}
	}
	m.reminders = kept
}

func (m *timerScheduler) markReminderSynced(id string) {
	m.mu.Lock()
	for i := range m.reminders {
		if m.reminders[i].ID == id {
			m.reminders[i].Pending = false
		}
	}
	m.mu.Unlock()
}

func mergeReminders(remote []Reminder, local []reminderRecord) []reminderRecord {
	byID := make(map[string]reminderRecord, len(remote)+len(local))
	order := make([]string, 0, len(remote)+len(local))
	put := func(record reminderRecord) {
		if record.ID == "" {
			return
		}
		if _, exists := byID[record.ID]; !exists {
			order = append(order, record.ID)
		}
		byID[record.ID] = record
	}

	for _, reminder := range remote {
		if reminder.ID == "" || reminder.Source == "" {
			reminder.Source = ReminderSourceLocal
		}
		put(reminderRecord{Reminder: reminder})
	}
	for _, record := range local {
		if record.ID == "" {
			continue
		}
		if record.Deleted {
			if record.Pending {
				put(record)
			}
			continue
		}
		if record.Pending {
			put(record)
		} else if _, exists := byID[record.ID]; !exists {
			// Keep a reminder that exists only on this speaker. A reminder with
			// a remote identity is remote-authoritative, so an omission from
			// backend.List means it was deleted remotely.
			if record.RemoteID == "" && record.Source == ReminderSourceLocal {
				put(record)
			}
		}
	}
	out := make([]reminderRecord, 0, len(order))
	for _, id := range order {
		if record, ok := byID[id]; ok {
			out = append(out, record)
		}
	}
	return out
}

func normalizeCreatedReminder(created, fallback Reminder) Reminder {
	if created.ID == "" {
		created.ID = fallback.ID
	}
	if created.Text == "" {
		created.Text = fallback.Text
	}
	if created.Due == "" {
		created.Due = fallback.Due
	}
	if created.Source == "" {
		created.Source = fallback.Source
	}
	return created
}

func reminderDue(reminder Reminder) (time.Time, bool) {
	if strings.TrimSpace(reminder.Due) == "" {
		return time.Time{}, false
	}
	due, err := time.Parse(time.RFC3339, reminder.Due)
	if err != nil {
		return time.Time{}, false
	}
	return due, true
}

func scheduledReminderDue(record reminderRecord) (time.Time, bool) {
	// Calendar events and imported tasks can be listed and edited, but passing
	// their date must not delete them from the user's account.
	if record.Deleted || record.RemoteKind == RemoteKindEvent || isImportedGoogleTask(record.Reminder) {
		return time.Time{}, false
	}
	return reminderDue(record.Reminder)
}

func countVisibleReminders(records []reminderRecord) int {
	count := 0
	for _, record := range records {
		if !record.Deleted {
			count++
		}
	}
	return count
}

func countPendingReminders(records []reminderRecord) int {
	count := 0
	for _, record := range records {
		if record.Pending {
			count++
		}
	}
	return count
}

func reminderDueArg(args M, now time.Time) (time.Time, error) {
	if date := firstString(args, "date"); date != "" {
		if clock := firstString(args, "at", "due", "time"); clock != "" {
			if parsed, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, now.Location()); err == nil {
				return parsed, nil
			}
		}
	}
	if value := firstString(args, "at", "due", "time"); value != "" {
		return parseReminderTime(value, now)
	}
	if value := firstArg(args, "in_seconds", "delay_seconds"); value != nil {
		seconds, ok := numberArg(value)
		if !ok || seconds <= 0 {
			return time.Time{}, errors.New("in_seconds must be a positive number")
		}
		return now.Add(time.Duration(seconds * float64(time.Second))), nil
	}
	if hasAny(args, "duration_seconds", "duration", "hours", "minutes", "seconds") {
		duration, err := parseTimerDuration(args)
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(duration), nil
	}
	return time.Time{}, errors.New("reminder time is required (at HH:MM or RFC3339, or in_seconds)")
}

func parseReminderTime(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "02.01.2006 15:04"} {
		if parsed, err := time.ParseInLocation(layout, value, now.Location()); err == nil {
			return parsed, nil
		}
	}
	hour, minute, err := parseTimerClock(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid reminder time %q", value)
	}
	return nextAlarmOccurrence(now, hour, minute, nil), nil
}

// ---------------------------------------------------------------------------
// Stopwatch
// ---------------------------------------------------------------------------

func (m *timerScheduler) toolStartStopwatch(args M) M {
	action := strings.ToLower(firstString(args, "action", "command"))
	if action == "" {
		action = "start"
	}
	now := m.now()

	m.mu.Lock()
	switch action {
	case "start":
		if m.stopwatch.Running {
			m.mu.Unlock()
			return timerError(errors.New("stopwatch is already running"))
		}
		m.stopwatch = stopwatchState{Running: true, StartedAt: now.UnixNano()}
	case "resume":
		if m.stopwatch.Running {
			m.mu.Unlock()
			return timerError(errors.New("stopwatch is already running"))
		}
		m.stopwatch.Running = true
		m.stopwatch.StartedAt = now.UnixNano()
	case "pause":
		if !m.stopwatch.Running {
			m.mu.Unlock()
			return timerError(errors.New("stopwatch is not running"))
		}
		m.stopwatch.Accumulated += now.UnixNano() - m.stopwatch.StartedAt
		m.stopwatch.StartedAt = 0
		m.stopwatch.Running = false
	case "reset":
		m.stopwatch = stopwatchState{}
	case "read", "status":
		// no state change
	default:
		m.mu.Unlock()
		return timerError(fmt.Errorf("unknown stopwatch action %q", action))
	}
	state := m.stopwatch
	elapsed := stopwatchElapsed(state, now)
	m.mu.Unlock()

	result := stopwatchResult(state, elapsed, now)
	result["action"] = action
	return result
}

func (m *timerScheduler) toolStopStopwatch() M {
	now := m.now()
	m.mu.Lock()
	if m.stopwatch.Running {
		m.stopwatch.Accumulated += now.UnixNano() - m.stopwatch.StartedAt
		m.stopwatch.StartedAt = 0
		m.stopwatch.Running = false
	}
	state := m.stopwatch
	elapsed := stopwatchElapsed(state, now)
	m.mu.Unlock()

	result := stopwatchResult(state, elapsed, now)
	result["action"] = "stop"
	return result
}

func stopwatchElapsed(state stopwatchState, now time.Time) time.Duration {
	elapsed := time.Duration(state.Accumulated)
	if state.Running {
		elapsed += time.Duration(now.UnixNano() - state.StartedAt)
	}
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

func stopwatchResult(state stopwatchState, elapsed time.Duration, now time.Time) M {
	result := M{
		"ok": true, "running": state.Running,
		"elapsed_seconds": math.Round(elapsed.Seconds()*10) / 10,
		"elapsed":         elapsed.Round(100 * time.Millisecond).String(),
	}
	if state.Running {
		result["started_at"] = time.Unix(0, state.StartedAt).Format(time.RFC3339)
	}
	_ = now
	return result
}

// ---------------------------------------------------------------------------
// Cue playback
// ---------------------------------------------------------------------------

var (
	timerBeepOnce sync.Once
	timerBeepData []byte
)

// timerBeepPCM builds a short two-tone 24 kHz mono S16 earcon. It is generated
// locally so no stock Assistant TTS or cloud interaction is involved.
func timerBeepPCM() []byte {
	timerBeepOnce.Do(func() {
		const rate = 24000
		const samples = rate * 45 / 100 // 450 ms
		pcm := make([]byte, samples*2)
		fade := rate / 50 // 20 ms fade to avoid clicks
		for i := 0; i < samples; i++ {
			frequency := 880.0
			if i >= samples/2 {
				frequency = 1174.66
			}
			envelope := 1.0
			if i < fade {
				envelope = float64(i) / float64(fade)
			} else if i > samples-fade {
				envelope = float64(samples-i) / float64(fade)
			}
			sample := int16(0.35 * 32767 * envelope * math.Sin(2*math.Pi*frequency*float64(i)/rate))
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
		}
		timerBeepData = pcm
	})
	return timerBeepData
}

func pauseMediaForSleep() {
	if directSpotify.connected() {
		_ = directSpotify.pause()
	}
	_, _ = wampCall("com.harman.gva.pause")
}

func (m *timerScheduler) applyAlarmVolume() {
	m.alarmVolumeMu.Lock()
	defer m.alarmVolumeMu.Unlock()

	m.mu.Lock()
	level := m.alarmVolume
	skip := m.stopped || m.alarmVolumeActive
	m.mu.Unlock()
	if level <= 0 || skip || m.getVolume == nil || m.setVolume == nil {
		return
	}
	previous, err := m.getVolume()
	nativeLevel := (min(level, 100)*nativeVolumeMax + 50) / 100
	if err != nil || m.setVolume(nativeLevel) != nil {
		return
	}
	m.mu.Lock()
	m.alarmPrevVolume = previous
	m.alarmVolumeActive = true
	m.mu.Unlock()
}

func (m *timerScheduler) restoreAlarmVolume() {
	m.alarmVolumeMu.Lock()
	defer m.alarmVolumeMu.Unlock()

	m.mu.Lock()
	if !m.stopped {
		for _, alarm := range m.alarms {
			if alarm.Active {
				m.mu.Unlock()
				return
			}
		}
	}
	active := m.alarmVolumeActive
	previous := m.alarmPrevVolume
	m.alarmVolumeActive = false
	m.alarmPrevVolume = 0
	m.mu.Unlock()
	if active && m.setVolume != nil {
		_ = m.setVolume(previous)
	}
}

func playTimerCue() {
	pcm := timerBeepPCM()
	markLocalCueWindow(pcm)
	extendAssistantAudioGuard(time.Second)

	out, err := openMixerOutput()
	if err != nil {
		log.Println("timer cue open:", clean(err))
		return
	}
	defer out.close()
	if err := out.write(pcm); err != nil {
		log.Println("timer cue write:", clean(err))
		return
	}
	_ = out.finish()
}

// ---------------------------------------------------------------------------
// Argument parsing helpers
// ---------------------------------------------------------------------------

func parseTimerDuration(args M) (time.Duration, error) {
	if value, ok := args["duration_seconds"]; ok {
		seconds, valid := numberArg(value)
		if !valid {
			return 0, errors.New("duration_seconds must be a number")
		}
		return validateTimerDuration(time.Duration(seconds * float64(time.Second)))
	}
	if value, ok := args["duration"]; ok {
		if text, valid := value.(string); valid {
			return parseHumanDuration(text)
		}
		if seconds, valid := numberArg(value); valid {
			return validateTimerDuration(time.Duration(seconds * float64(time.Second)))
		}
		return 0, errors.New("duration must be a string or number")
	}

	total := time.Duration(0)
	found := false
	for _, name := range []string{"hours", "hour", "hrs", "h"} {
		if seconds, ok := numberArg(args[name]); ok {
			total += time.Duration(seconds * float64(time.Hour))
			found = true
		}
	}
	for _, name := range []string{"minutes", "minute", "mins", "m"} {
		if seconds, ok := numberArg(args[name]); ok {
			total += time.Duration(seconds * float64(time.Minute))
			found = true
		}
	}
	for _, name := range []string{"seconds", "second", "secs", "s"} {
		if seconds, ok := numberArg(args[name]); ok {
			total += time.Duration(seconds * float64(time.Second))
			found = true
		}
	}
	if !found {
		return 0, errors.New("duration is required (duration_seconds, duration, or hours/minutes/seconds)")
	}
	return validateTimerDuration(total)
}

func parseHumanDuration(value string) (time.Duration, error) {
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "ё", "е")))
	normalized = strings.ReplaceAll(normalized, ",", " ")
	if normalized == "" {
		return 0, errors.New("duration is empty")
	}
	switch normalized {
	case "полчаса":
		return 30 * time.Minute, nil
	case "полтора часа", "полтора час":
		return 90 * time.Minute, nil
	}

	if parsed, err := time.ParseDuration(strings.ReplaceAll(normalized, " ", "")); err == nil {
		return validateTimerDuration(parsed)
	}

	normalized = russianDurationUnits.Replace(normalized)
	normalized = strings.ReplaceAll(normalized, " ", "")
	total := time.Duration(0)
	for len(normalized) > 0 {
		index := 0
		for index < len(normalized) && ((normalized[index] >= '0' && normalized[index] <= '9') || normalized[index] == '.') {
			index++
		}
		if index == 0 {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		number, err := strconv.ParseFloat(normalized[:index], 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		normalized = normalized[index:]

		unitEnd := 0
		for unitEnd < len(normalized) && normalized[unitEnd] >= 'a' && normalized[unitEnd] <= 'z' {
			unitEnd++
		}
		unit := normalized[:unitEnd]
		normalized = normalized[unitEnd:]

		multiplier, ok := durationUnit(unit)
		if !ok {
			return 0, fmt.Errorf("unknown duration unit %q", unit)
		}
		total += time.Duration(number * float64(multiplier))
	}
	return validateTimerDuration(total)
}

var russianDurationUnits = strings.NewReplacer(
	"часов", "h", "часа", "h", "часу", "h", "час", "h",
	"минут", "m", "минуты", "m", "минуту", "m", "минута", "m", "мин", "m",
	"секунд", "s", "секунды", "s", "секунду", "s", "секунда", "s", "сек", "s",
	"дней", "d", "дня", "d", "день", "d",
)

func durationUnit(unit string) (time.Duration, bool) {
	switch strings.ToLower(unit) {
	case "", "s", "sec", "secs", "second", "seconds":
		return time.Second, true
	case "m", "min", "mins", "minute", "minutes":
		return time.Minute, true
	case "h", "hr", "hrs", "hour", "hours":
		return time.Hour, true
	case "d", "day", "days":
		return 24 * time.Hour, true
	case "ms", "millisecond", "milliseconds":
		return time.Millisecond, true
	}
	return 0, false
}

func validateTimerDuration(duration time.Duration) (time.Duration, error) {
	if duration <= 0 {
		return 0, errors.New("duration must be positive")
	}
	if duration > timerMaxDuration {
		return 0, errors.New("duration is too long")
	}
	return duration, nil
}

func parseTimerClock(value string) (int, int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0, errors.New("time is required")
	}
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ':' || r == '.' })
	if len(parts) < 1 || len(parts) > 2 {
		return 0, 0, fmt.Errorf("time must be HH:MM, got %q", value)
	}
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("time must be HH:MM, got %q", value)
	}
	minute := 0
	if len(parts) == 2 {
		minute, err = strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return 0, 0, fmt.Errorf("time must be HH:MM, got %q", value)
		}
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("time must be HH:MM within 00:00..23:59, got %q", value)
	}
	return hour, minute, nil
}

func alarmTimeArgs(args M) (int, int, error) {
	if value := firstString(args, "time", "at"); value != "" {
		return parseTimerClock(value)
	}
	hour, hasHour := intArg(args, "hour")
	minute, hasMinute := intArg(args, "minute")
	if !hasHour {
		return 0, 0, errors.New("alarm time is required (time HH:MM or hour/minute)")
	}
	if !hasMinute {
		minute = 0
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, errors.New("alarm hour must be 0..23 and minute 0..59")
	}
	return hour, minute, nil
}

func parseRepeatDays(value any) ([]int, error) {
	if value == nil {
		return nil, nil
	}
	var tokens []string
	switch typed := value.(type) {
	case string:
		tokens = []string{typed}
	case []string:
		tokens = typed
	case []any:
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, errors.New("repeat_days must be a string or an array of strings")
			}
			tokens = append(tokens, text)
		}
	default:
		return nil, errors.New("repeat_days must be a string or an array of strings")
	}

	seen := make(map[int]bool, 7)
	add := func(day int) { seen[day] = true }
	for _, token := range tokens {
		lower := strings.ToLower(strings.ReplaceAll(token, "ё", "е"))
		for _, part := range strings.FieldsFunc(lower, func(r rune) bool {
			return r == ',' || r == ' ' || r == '/' || r == ';' || r == '|'
		}) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			switch part {
			case "none", "never", "once", "no", "нет", "никогда":
				continue
			case "daily", "everyday", "every", "day", "каждый", "день", "ежедневно":
				for day := 0; day < 7; day++ {
					add(day)
				}
			case "weekdays", "weekday", "будни", "будням", "рабочие", "будние":
				for day := 1; day <= 5; day++ {
					add(day)
				}
			case "weekends", "weekend", "выходные", "выходным":
				add(0)
				add(6)
			default:
				if start, end, ok := parseDayRange(part); ok {
					for day := start; ; day = (day + 1) % 7 {
						add(day)
						if day == end {
							break
						}
					}
					continue
				}
				day, ok := weekdayIndex(part)
				if !ok {
					return nil, fmt.Errorf("unknown repeat day %q", part)
				}
				add(day)
			}
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	days := make([]int, 0, len(seen))
	for day := range seen {
		days = append(days, day)
	}
	sort.Ints(days)
	return days, nil
}

func parseDayRange(value string) (int, int, bool) {
	index := strings.Index(value, "-")
	if index <= 0 || index >= len(value)-1 {
		return 0, 0, false
	}
	start, startOK := weekdayIndex(value[:index])
	end, endOK := weekdayIndex(value[index+1:])
	if !startOK || !endOK {
		return 0, 0, false
	}
	return start, end, true
}

func weekdayIndex(value string) (int, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return 0, false
	}
	if number, err := strconv.Atoi(value); err == nil {
		if number >= 0 && number <= 6 {
			return number, true
		}
		if number == 7 {
			return 0, true
		}
		return 0, false
	}
	names := map[string]int{
		"sun": 0, "sunday": 0, "su": 0, "вс": 0, "воскресенье": 0, "воскресенья": 0,
		"mon": 1, "monday": 1, "mo": 1, "пн": 1, "понедельник": 1, "понедельника": 1,
		"tue": 2, "tues": 2, "tuesday": 2, "tu": 2, "вт": 2, "вторник": 2, "вторника": 2,
		"wed": 3, "weds": 3, "wednesday": 3, "we": 3, "ср": 3, "среда": 3, "среду": 3, "среды": 3,
		"thu": 4, "thur": 4, "thurs": 4, "thursday": 4, "th": 4, "чт": 4, "четверг": 4, "четверга": 4,
		"fri": 5, "friday": 5, "fr": 5, "пт": 5, "пятница": 5, "пятницу": 5, "пятницы": 5,
		"sat": 6, "saturday": 6, "sa": 6, "сб": 6, "суббота": 6, "субботу": 6, "субботы": 6,
	}
	day, ok := names[value]
	return day, ok
}

func weekdayNames(days []int) []string {
	names := []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
	result := make([]string, 0, len(days))
	for _, day := range days {
		if day >= 0 && day < len(names) {
			result = append(result, names[day])
		}
	}
	return result
}

func formatClock(hour, minute int) string {
	return fmt.Sprintf("%02d:%02d", hour, minute)
}

func findTimerIndexLocked(timers []timerEntry, args M) (int, error) {
	if id := firstString(args, "id", "timer_id"); id != "" {
		for index := range timers {
			if timers[index].ID == id {
				return index, nil
			}
		}
		return -1, fmt.Errorf("timer %q not found", id)
	}
	if index, ok := intArg(args, "index"); ok {
		if index < 1 || index > len(timers) {
			return -1, fmt.Errorf("timer index %d is out of range", index)
		}
		return index - 1, nil
	}
	if len(timers) == 1 {
		return 0, nil
	}
	return -1, errors.New("specify timer id or index")
}

func findAlarmIndexLocked(alarms []alarmEntry, args M, lastAlarmID string) (int, error) {
	if id := firstString(args, "id", "alarm_id"); id != "" {
		for index := range alarms {
			if alarms[index].ID == id {
				return index, nil
			}
		}
		return -1, fmt.Errorf("alarm %q not found", id)
	}
	if index, ok := intArg(args, "index"); ok {
		if index < 1 || index > len(alarms) {
			return -1, fmt.Errorf("alarm index %d is out of range", index)
		}
		return index - 1, nil
	}
	if lastAlarmID != "" {
		for index := range alarms {
			if alarms[index].ID == lastAlarmID {
				return index, nil
			}
		}
	}
	if len(alarms) == 1 {
		return 0, nil
	}
	return -1, errors.New("specify alarm id or index")
}

func findReminderIndexLocked(reminders []reminderRecord, args M) (int, error) {
	if id := firstString(args, "id", "reminder_id"); id != "" {
		for index := range reminders {
			if reminders[index].ID == id && !reminders[index].Deleted {
				return index, nil
			}
		}
		return -1, fmt.Errorf("reminder %q not found", id)
	}
	if index, ok := intArg(args, "index"); ok {
		visible := make([]int, 0, len(reminders))
		for i := range reminders {
			if !reminders[i].Deleted {
				visible = append(visible, i)
			}
		}
		sort.SliceStable(visible, func(i, j int) bool {
			left, leftOK := reminderDue(reminders[visible[i]].Reminder)
			right, rightOK := reminderDue(reminders[visible[j]].Reminder)
			if leftOK != rightOK {
				return leftOK
			}
			return left.Before(right)
		})
		if index < 1 || index > len(visible) {
			return -1, fmt.Errorf("reminder index %d is out of range", index)
		}
		return visible[index-1], nil
	}
	visible := make([]int, 0, len(reminders))
	for i := range reminders {
		if !reminders[i].Deleted {
			visible = append(visible, i)
		}
	}
	if len(visible) == 1 {
		return visible[0], nil
	}
	return -1, errors.New("specify reminder id or index")
}

func (m *timerScheduler) nextIDLocked(prefix string) string {
	m.idSeq++
	return fmt.Sprintf("%s%d", prefix, m.idSeq)
}

func firstArg(args M, names ...string) any {
	for _, name := range names {
		if value, ok := args[name]; ok && value != nil {
			return value
		}
	}
	return nil
}

func hasAny(args M, names ...string) bool {
	for _, name := range names {
		if value, ok := args[name]; ok && value != nil {
			return true
		}
	}
	return false
}

func firstString(args M, names ...string) string {
	for _, name := range names {
		value, ok := args[name]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			return strings.TrimSpace(typed)
		case float64:
			if typed == math.Trunc(typed) {
				return strconv.FormatInt(int64(typed), 10)
			}
		case int:
			return strconv.Itoa(typed)
		case int64:
			return strconv.FormatInt(typed, 10)
		case json.Number:
			return typed.String()
		}
	}
	return ""
}

func numberArg(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return number, err == nil
	}
	return 0, false
}

func intArg(args M, names ...string) (int, bool) {
	for _, name := range names {
		value, ok := args[name]
		if !ok || value == nil {
			continue
		}
		number, valid := numberArg(value)
		if !valid || math.Trunc(number) != number {
			return 0, false
		}
		return int(number), true
	}
	return 0, false
}

func boolArg(args M, names ...string) bool {
	for _, name := range names {
		value, ok := args[name]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed
		case string:
			return strings.EqualFold(strings.TrimSpace(typed), "true")
		case float64:
			return typed != 0
		case int:
			return typed != 0
		}
	}
	return false
}

func ceilSeconds(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int(math.Ceil(duration.Seconds()))
}

func timerError(err error) M {
	if err == nil {
		return M{"ok": false, "error": "unknown error"}
	}
	return M{"ok": false, "error": clean(err)}
}

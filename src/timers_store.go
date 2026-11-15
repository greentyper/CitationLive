package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// persistedTimerState is the on-device source of truth for timers, alarms and
// the local reminder cache. Reminders are mirrored through ReminderBackend when
// one is configured; the cache here keeps the scheduler alive across restarts
// and while Google Calendar/Tasks is unreachable.
type persistedTimerState struct {
	Version       int              `json:"version"`
	Timers        []timerEntry     `json:"timers"`
	Alarms        []alarmEntry     `json:"alarms"`
	Reminders     []reminderRecord `json:"reminders"`
	SleepDeadline int64            `json:"sleep_deadline,omitempty"`
	SleepLabel    string           `json:"sleep_label,omitempty"`
	AlarmVolume   int              `json:"alarm_volume,omitempty"`
}

func (m *timerScheduler) load() error {
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state persistedTimerState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()

	m.timers = m.timers[:0]
	for _, timer := range state.Timers {
		if timer.ID == "" {
			continue
		}
		if timer.Paused {
			if timer.Remaining < 0 {
				timer.Remaining = 0
			}
		} else if timer.Deadline <= 0 {
			continue
		}
		m.timers = append(m.timers, timer)
	}

	m.alarms = m.alarms[:0]
	for _, alarm := range state.Alarms {
		if alarm.ID == "" || alarm.Hour < 0 || alarm.Hour > 23 || alarm.Minute < 0 || alarm.Minute > 59 {
			continue
		}
		// No alarm loop survives a process restart. Active tells us the alarm
		// was ringing when the process stopped; a one-shot that already fired
		// must not be rescheduled for tomorrow.
		wasActive := alarm.Active
		alarm.Active = false
		if len(alarm.RepeatDays) == 0 && wasActive && alarm.Next == 0 && alarm.SnoozeUntil == 0 {
			continue
		}
		// Keep an expired snooze so collectDue rings it once, then the normal
		// schedule resumes.
		if alarm.SnoozeUntil == 0 || alarm.SnoozeUntil > now.UnixNano() {
			alarm.Next = nextAlarmOccurrence(now, alarm.Hour, alarm.Minute, alarm.RepeatDays).UnixNano()
		}
		m.alarms = append(m.alarms, alarm)
	}

	m.reminders = append([]reminderRecord(nil), state.Reminders...)
	for i := range m.reminders {
		if m.reminders[i].Source == "" {
			m.reminders[i].Source = ReminderSourceLocal
		}
	}
	m.sleepDeadline = state.SleepDeadline
	m.sleepLabel = state.SleepLabel
	m.alarmVolume = state.AlarmVolume

	m.seedIDSeqLocked()
	m.lastTick = now
	m.notify()
	return nil
}

func (m *timerScheduler) save() error {
	m.mu.Lock()
	state := persistedTimerState{
		Version:       1,
		Timers:        append([]timerEntry(nil), m.timers...),
		Alarms:        append([]alarmEntry(nil), m.alarms...),
		Reminders:     append([]reminderRecord(nil), m.reminders...),
		SleepDeadline: m.sleepDeadline,
		SleepLabel:    m.sleepLabel,
		AlarmVolume:   m.alarmVolume,
	}
	m.mu.Unlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicFile(m.path, data, 0600)
}

func (m *timerScheduler) seedIDSeqLocked() {
	update := func(id string) {
		index := len(id)
		for index > 0 && id[index-1] >= '0' && id[index-1] <= '9' {
			index--
		}
		if index == len(id) {
			return
		}
		number, err := strconv.ParseInt(id[index:], 10, 64)
		if err == nil && number > m.idSeq {
			m.idSeq = number
		}
	}
	for _, timer := range m.timers {
		update(timer.ID)
	}
	for _, alarm := range m.alarms {
		update(alarm.ID)
	}
	for _, reminder := range m.reminders {
		update(reminder.ID)
	}
}

// jsonFileReminderBackend is the local JSON fallback. It is deliberately
// replaceable: the integration layer can inject a Google Calendar/Tasks
// backend with setReminderBackend and this file is then only a cache.
type jsonFileReminderBackend struct {
	mu   sync.Mutex
	path string
}

func newJSONFileReminderBackend(path string) *jsonFileReminderBackend {
	return &jsonFileReminderBackend{path: path}
}

type reminderFile struct {
	Reminders []Reminder `json:"reminders"`
}

func (b *jsonFileReminderBackend) loadLocked() ([]Reminder, error) {
	data, err := os.ReadFile(b.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}

	var wrapped reminderFile
	if err := json.Unmarshal(data, &wrapped); err == nil {
		return append([]Reminder(nil), wrapped.Reminders...), nil
	}
	var list []Reminder
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (b *jsonFileReminderBackend) saveLocked(reminders []Reminder) error {
	data, err := json.MarshalIndent(reminderFile{Reminders: reminders}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicFile(b.path, data, 0600)
}

func (b *jsonFileReminderBackend) List(ctx context.Context) ([]Reminder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	reminders, err := b.loadLocked()
	if err != nil {
		return nil, err
	}
	return append([]Reminder(nil), reminders...), nil
}

func (b *jsonFileReminderBackend) Create(ctx context.Context, reminder Reminder) (Reminder, error) {
	if err := ctx.Err(); err != nil {
		return Reminder{}, err
	}
	if strings.TrimSpace(reminder.Text) == "" {
		return Reminder{}, ErrReminderTextEmpty
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	reminders, err := b.loadLocked()
	if err != nil {
		return Reminder{}, err
	}
	if reminder.ID == "" {
		reminder.ID = fmt.Sprintf("local-%d", time.Now().UnixNano())
	}
	if reminder.Source == "" {
		reminder.Source = ReminderSourceLocal
	}
	replaced := false
	for i := range reminders {
		if reminders[i].ID == reminder.ID {
			reminders[i] = reminder
			replaced = true
			break
		}
	}
	if !replaced {
		reminders = append(reminders, reminder)
	}
	if err := b.saveLocked(reminders); err != nil {
		return Reminder{}, err
	}
	return reminder, nil
}

func (b *jsonFileReminderBackend) Update(ctx context.Context, reminder Reminder) (Reminder, error) {
	if err := ctx.Err(); err != nil {
		return Reminder{}, err
	}
	if strings.TrimSpace(reminder.Text) == "" {
		return Reminder{}, ErrReminderTextEmpty
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	reminders, err := b.loadLocked()
	if err != nil {
		return Reminder{}, err
	}
	for i := range reminders {
		if reminders[i].ID == reminder.ID {
			reminders[i] = reminder
			if err := b.saveLocked(reminders); err != nil {
				return Reminder{}, err
			}
			return reminder, nil
		}
	}
	return Reminder{}, ErrReminderNotFound
}

func (b *jsonFileReminderBackend) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	reminders, err := b.loadLocked()
	if err != nil {
		return err
	}
	kept := reminders[:0]
	for _, reminder := range reminders {
		if reminder.ID != id {
			kept = append(kept, reminder)
		}
	}
	return b.saveLocked(kept)
}

// Sync is a no-op for the local fallback: this backend is already the local
// store. A Google-backed implementation uses Sync to reconcile remote changes.
func (b *jsonFileReminderBackend) Sync(ctx context.Context) error {
	return ctx.Err()
}

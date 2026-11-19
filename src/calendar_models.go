package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Reminder sources. A reminder either lives only on the speaker ("local") or
// mirrors an object owned by Google Calendar / Google Tasks.
const (
	ReminderSourceLocal    = "local"
	ReminderSourceCalendar = "calendar"
	ReminderSourceTask     = "task"

	// RemoteKind values identify which Google API owns a synced object.
	RemoteKindEvent = "event"
	RemoteKindTask  = "task"
)

var (
	ErrReminderNotFound  = errors.New("reminder not found")
	ErrReminderTextEmpty = errors.New("reminder text is required")
)

// EventReminder is a single popup/email override on a calendar event.
type EventReminder struct {
	Method  string `json:"method"`
	Minutes int    `json:"minutes"`
}

// CalendarEvent is the local representation of a Google Calendar event.
// Start and End are RFC3339 timestamps. All-day events are normalised to
// midnight UTC so callers never have to handle the Google "date" variant.
type CalendarEvent struct {
	ID          string          `json:"id"`
	Summary     string          `json:"summary"`
	Description string          `json:"description,omitempty"`
	Start       string          `json:"start"`
	End         string          `json:"end"`
	TimeZone    string          `json:"timezone,omitempty"`
	Reminders   []EventReminder `json:"reminders,omitempty"`
	Status      string          `json:"status,omitempty"`
	ETag        string          `json:"etag,omitempty"`
	Updated     string          `json:"updated,omitempty"`
}

// GoogleTask is the local representation of a Google Tasks task.
type GoogleTask struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Notes     string `json:"notes,omitempty"`
	Due       string `json:"due,omitempty"`
	Status    string `json:"status,omitempty"`
	Completed string `json:"completed,omitempty"`
	ETag      string `json:"etag,omitempty"`
	Updated   string `json:"updated,omitempty"`
}

// Reminder is the unified object consumed by the timer/reminder manager.
//
// RemoteID and RemoteKind are sync bookkeeping: a local reminder starts with
// RemoteID == "" and receives an ID after it is pushed as a Google Task (or,
// when explicitly requested, a calendar event). RemoteID is empty for
// reminders that exist only on the speaker.
type Reminder struct {
	ID         string `json:"id"`
	Text       string `json:"text"`
	Due        string `json:"due,omitempty"`
	TimeZone   string `json:"timezone,omitempty"`
	Source     string `json:"source"`
	ETag       string `json:"etag,omitempty"`
	Updated    string `json:"updated,omitempty"`
	RemoteID   string `json:"remote_id,omitempty"`
	RemoteKind string `json:"remote_kind,omitempty"`
}

// ReminderBackend is implemented by CalendarSync so the timer manager can use
// local reminders and Google-backed reminders through one interface.
type ReminderBackend interface {
	List(ctx context.Context) ([]Reminder, error)
	Create(ctx context.Context, reminder Reminder) (Reminder, error)
	Update(ctx context.Context, reminder Reminder) (Reminder, error)
	Delete(ctx context.Context, id string) error
	Sync(ctx context.Context) error
}

// TokenProvider supplies an OAuth bearer token. The production OAuth
// implementation lives outside this file; tests use a static/fake provider.
type TokenProvider interface {
	AccessToken(ctx context.Context) (string, error)
}

// StaticToken is a small TokenProvider for tests and one-shot CLI tools.
type StaticToken string

func (t StaticToken) AccessToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t == "" {
		return "", errors.New("google api: empty access token")
	}
	return string(t), nil
}

// GoogleAPIError describes a non-2xx response from Google Calendar/Tasks.
type GoogleAPIError struct {
	StatusCode int
	Status     string
	Message    string
	Body       string
}

func (e *GoogleAPIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("google api HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("google api HTTP %d", e.StatusCode)
}

// IsGoogleAPIStatus reports whether err is a GoogleAPIError with statusCode.
func IsGoogleAPIStatus(err error, statusCode int) bool {
	var apiErr *GoogleAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == statusCode
}

// normalizeRFC3339 converts a timestamp to second-precision UTC RFC3339.
// Values that do not parse are returned unchanged; callers that need strict
// validation use normalizeDue instead.
func normalizeRFC3339(value string) string {
	if value == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format(time.RFC3339)
}

// normalizeDue validates a reminder time while preserving its calendar date
// and offset for date-only task providers.
func normalizeDue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "", fmt.Errorf("due must be RFC3339: %w", err)
	}
	return parsed.Format(time.RFC3339), nil
}

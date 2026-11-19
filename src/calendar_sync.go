package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	googleCalendarAPIBase = "https://www.googleapis.com/calendar/v3"
	googleTasksAPIBase    = "https://tasks.googleapis.com/tasks/v1"

	calendarSyncFileName    = "calendar_sync.json"
	calendarSyncStateVer    = 1
	googleMaxResponseBytes  = 8 << 20
	googleMaxErrorBodyBytes = 2 << 10
)

// defaultGoogleHTTPClient is shared by both REST clients. A timeout keeps a
// stalled Google endpoint from blocking the reminder manager forever.
var defaultGoogleHTTPClient = &http.Client{Timeout: 15 * time.Second}

// ---------------------------------------------------------------------------
// REST transport
// ---------------------------------------------------------------------------

type googleErrorEnvelope struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func parseGoogleAPIError(statusCode int, status string, body []byte) error {
	apiErr := &GoogleAPIError{
		StatusCode: statusCode,
		Status:     status,
		Body:       googleErrorSnippet(body),
	}
	var envelope googleErrorEnvelope
	if json.Unmarshal(body, &envelope) == nil {
		if envelope.Error.Message != "" {
			apiErr.Message = envelope.Error.Message
		}
		if envelope.Error.Status != "" {
			apiErr.Status = envelope.Error.Status
		}
		if envelope.Error.Code != 0 {
			apiErr.StatusCode = envelope.Error.Code
		}
	}
	if apiErr.Message == "" {
		apiErr.Message = http.StatusText(statusCode)
	}
	return apiErr
}

func googleErrorSnippet(body []byte) string {
	if len(body) > googleMaxErrorBodyBytes {
		body = body[:googleMaxErrorBodyBytes]
	}
	return strings.TrimSpace(string(body))
}

// googleRequest performs an authenticated Google REST call and returns the raw
// body. Non-2xx responses become *GoogleAPIError; malformed success bodies are
// rejected by decodeGoogleJSON.
func googleRequest(ctx context.Context, client *http.Client, tokens TokenProvider, method, endpoint string, payload any) ([]byte, error) {
	if tokens == nil {
		return nil, errors.New("google api: token provider is not configured")
	}
	token, err := tokens.AccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("google api: access token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("google api: empty access token")
	}
	if client == nil {
		client = defaultGoogleHTTPClient
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("google api: encode request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("google api: build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("google api: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, googleMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("google api: read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, parseGoogleAPIError(response.StatusCode, response.Status, data)
	}
	return data, nil
}

func decodeGoogleJSON(data []byte, out any) error {
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("google api: invalid JSON response: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Google Calendar client
// ---------------------------------------------------------------------------

type googleEventTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type googleReminderOverride struct {
	Method  string `json:"method"`
	Minutes int    `json:"minutes"`
}

type googleReminders struct {
	UseDefault bool                     `json:"useDefault"`
	Overrides  []googleReminderOverride `json:"overrides"`
}

type googleEvent struct {
	ID          string           `json:"id,omitempty"`
	Summary     string           `json:"summary,omitempty"`
	Description string           `json:"description,omitempty"`
	Start       googleEventTime  `json:"start"`
	End         googleEventTime  `json:"end"`
	Reminders   *googleReminders `json:"reminders,omitempty"`
	Status      string           `json:"status,omitempty"`
	ETag        string           `json:"etag,omitempty"`
	Updated     string           `json:"updated,omitempty"`
}

type googleEventList struct {
	Items         []googleEvent `json:"items"`
	NextPageToken string        `json:"nextPageToken"`
}

// GoogleCalendarClient talks to the Calendar v3 REST API. BaseURL and
// HTTPClient are exported so tests can point the client at httptest.
type GoogleCalendarClient struct {
	Tokens     TokenProvider
	HTTPClient *http.Client
	BaseURL    string
	CalendarID string
}

func NewGoogleCalendarClient(tokens TokenProvider) *GoogleCalendarClient {
	return &GoogleCalendarClient{Tokens: tokens, CalendarID: "primary"}
}

func (c *GoogleCalendarClient) baseURL() string {
	if strings.TrimSpace(c.BaseURL) != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return googleCalendarAPIBase
}

func (c *GoogleCalendarClient) calendarID() string {
	if strings.TrimSpace(c.CalendarID) == "" {
		return "primary"
	}
	return c.CalendarID
}

func (c *GoogleCalendarClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return defaultGoogleHTTPClient
}

// ListEvents returns events between timeMin and timeMax (RFC3339). Both
// bounds may be empty. Recurring events are expanded and results are ordered
// by start time.
func (c *GoogleCalendarClient) ListEvents(ctx context.Context, timeMin, timeMax string) ([]CalendarEvent, error) {
	events := make([]CalendarEvent, 0)
	pageToken := ""
	seenTokens := make(map[string]bool)
	for {
		query := url.Values{}
		query.Set("singleEvents", "true")
		query.Set("orderBy", "startTime")
		query.Set("maxResults", "2500")
		if timeMin != "" {
			query.Set("timeMin", timeMin)
		}
		if timeMax != "" {
			query.Set("timeMax", timeMax)
		}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		endpoint := c.baseURL() + "/calendars/" + url.PathEscape(c.calendarID()) + "/events?" + query.Encode()
		data, err := googleRequest(ctx, c.httpClient(), c.Tokens, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		var page googleEventList
		if err := decodeGoogleJSON(data, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if strings.EqualFold(item.Status, "cancelled") {
				continue
			}
			events = append(events, eventFromGoogle(item))
		}
		if page.NextPageToken == "" {
			return events, nil
		}
		if seenTokens[page.NextPageToken] {
			return nil, errors.New("google calendar: repeated page token")
		}
		seenTokens[page.NextPageToken] = true
		pageToken = page.NextPageToken
	}
}

func (c *GoogleCalendarClient) CreateEvent(ctx context.Context, event CalendarEvent) (CalendarEvent, error) {
	payload, err := eventToGoogle(event)
	if err != nil {
		return CalendarEvent{}, err
	}
	// etag/updated are output-only on insert.
	payload.ETag = ""
	payload.Updated = ""
	endpoint := c.baseURL() + "/calendars/" + url.PathEscape(c.calendarID()) + "/events"
	data, err := googleRequest(ctx, c.httpClient(), c.Tokens, http.MethodPost, endpoint, payload)
	if err != nil {
		return CalendarEvent{}, err
	}
	var created googleEvent
	if err := decodeGoogleJSON(data, &created); err != nil {
		return CalendarEvent{}, err
	}
	return eventFromGoogle(created), nil
}

func (c *GoogleCalendarClient) UpdateEvent(ctx context.Context, event CalendarEvent) (CalendarEvent, error) {
	if strings.TrimSpace(event.ID) == "" {
		return CalendarEvent{}, errors.New("calendar event id is required")
	}
	payload, err := eventToGoogle(event)
	if err != nil {
		return CalendarEvent{}, err
	}
	// etag/updated are output-only on update.
	payload.ETag = ""
	payload.Updated = ""
	endpoint := c.baseURL() + "/calendars/" + url.PathEscape(c.calendarID()) + "/events/" + url.PathEscape(event.ID)
	data, err := googleRequest(ctx, c.httpClient(), c.Tokens, http.MethodPut, endpoint, payload)
	if err != nil {
		return CalendarEvent{}, err
	}
	var updated googleEvent
	if err := decodeGoogleJSON(data, &updated); err != nil {
		return CalendarEvent{}, err
	}
	return eventFromGoogle(updated), nil
}

// DeleteEvent removes an event. A 404 is treated as success so a retried
// delete after a lost response is idempotent.
func (c *GoogleCalendarClient) DeleteEvent(ctx context.Context, eventID string) error {
	if strings.TrimSpace(eventID) == "" {
		return errors.New("calendar event id is required")
	}
	endpoint := c.baseURL() + "/calendars/" + url.PathEscape(c.calendarID()) + "/events/" + url.PathEscape(eventID)
	if _, err := googleRequest(ctx, c.httpClient(), c.Tokens, http.MethodDelete, endpoint, nil); err != nil {
		if IsGoogleAPIStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func eventToGoogle(event CalendarEvent) (googleEvent, error) {
	summary := strings.TrimSpace(event.Summary)
	if summary == "" {
		return googleEvent{}, errors.New("calendar event summary is required")
	}
	start, err := googleEventTimeFromModel(event.Start, event.TimeZone)
	if err != nil {
		return googleEvent{}, fmt.Errorf("calendar event start: %w", err)
	}
	end, err := googleEventTimeFromModel(event.End, event.TimeZone)
	if err != nil {
		return googleEvent{}, fmt.Errorf("calendar event end: %w", err)
	}
	payload := googleEvent{
		ID:          event.ID,
		Summary:     summary,
		Description: event.Description,
		Start:       start,
		End:         end,
		Status:      event.Status,
		ETag:        event.ETag,
		Updated:     event.Updated,
	}
	if len(event.Reminders) > 0 {
		payload.Reminders = &googleReminders{
			UseDefault: false,
			Overrides:  make([]googleReminderOverride, 0, len(event.Reminders)),
		}
		for _, reminder := range event.Reminders {
			method := strings.TrimSpace(reminder.Method)
			if method == "" {
				method = "popup"
			}
			payload.Reminders.Overrides = append(payload.Reminders.Overrides, googleReminderOverride{
				Method:  method,
				Minutes: reminder.Minutes,
			})
		}
	}
	return payload, nil
}

func googleEventTimeFromModel(value, timeZone string) (googleEventTime, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return googleEventTime{}, errors.New("timestamp is required")
	}
	if len(value) == len("2006-01-02") {
		if _, err := time.Parse("2006-01-02", value); err != nil {
			return googleEventTime{}, err
		}
		return googleEventTime{Date: value}, nil
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		return googleEventTime{}, err
	}
	return googleEventTime{DateTime: value, TimeZone: timeZone}, nil
}

func eventFromGoogle(event googleEvent) CalendarEvent {
	start, timeZone := googleEventTimeToModel(event.Start)
	end, endTimeZone := googleEventTimeToModel(event.End)
	if timeZone == "" {
		timeZone = endTimeZone
	}
	result := CalendarEvent{
		ID:          event.ID,
		Summary:     event.Summary,
		Description: event.Description,
		Start:       start,
		End:         end,
		TimeZone:    timeZone,
		Status:      event.Status,
		ETag:        event.ETag,
		Updated:     event.Updated,
	}
	if event.Reminders != nil {
		for _, override := range event.Reminders.Overrides {
			result.Reminders = append(result.Reminders, EventReminder{
				Method:  override.Method,
				Minutes: override.Minutes,
			})
		}
	}
	return result
}

func googleEventTimeToModel(value googleEventTime) (string, string) {
	if value.DateTime != "" {
		return normalizeRFC3339(value.DateTime), value.TimeZone
	}
	if value.Date != "" {
		if parsed, err := time.Parse("2006-01-02", value.Date); err == nil {
			return parsed.UTC().Format(time.RFC3339), value.TimeZone
		}
		return value.Date + "T00:00:00Z", value.TimeZone
	}
	return "", value.TimeZone
}

// ---------------------------------------------------------------------------
// Google Tasks client
// ---------------------------------------------------------------------------

type googleTask struct {
	ID        string `json:"id,omitempty"`
	Title     string `json:"title,omitempty"`
	Notes     string `json:"notes,omitempty"`
	Due       string `json:"due,omitempty"`
	Status    string `json:"status,omitempty"`
	Completed string `json:"completed,omitempty"`
	ETag      string `json:"etag,omitempty"`
	Updated   string `json:"updated,omitempty"`
	Deleted   bool   `json:"deleted,omitempty"`
}

type googleTaskList struct {
	Items         []googleTask `json:"items"`
	NextPageToken string       `json:"nextPageToken"`
}

// GoogleTasksClient talks to the Tasks v1 REST API.
type GoogleTasksClient struct {
	Tokens     TokenProvider
	HTTPClient *http.Client
	BaseURL    string
}

func NewGoogleTasksClient(tokens TokenProvider) *GoogleTasksClient {
	return &GoogleTasksClient{Tokens: tokens}
}

func (c *GoogleTasksClient) baseURL() string {
	if strings.TrimSpace(c.BaseURL) != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return googleTasksAPIBase
}

func (c *GoogleTasksClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return defaultGoogleHTTPClient
}

func taskListID(tasklist string) string {
	if strings.TrimSpace(tasklist) == "" {
		return "@default"
	}
	return tasklist
}

// ListTasks returns every task in tasklist, including completed and hidden
// tasks. Pass "" or "@default" for the default list.
func (c *GoogleTasksClient) ListTasks(ctx context.Context, tasklist string) ([]GoogleTask, error) {
	list := taskListID(tasklist)
	tasks := make([]GoogleTask, 0)
	pageToken := ""
	seenTokens := make(map[string]bool)
	for {
		query := url.Values{}
		query.Set("showCompleted", "true")
		query.Set("showHidden", "true")
		query.Set("maxResults", "100")
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		endpoint := c.baseURL() + "/lists/" + url.PathEscape(list) + "/tasks?" + query.Encode()
		data, err := googleRequest(ctx, c.httpClient(), c.Tokens, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		var page googleTaskList
		if err := decodeGoogleJSON(data, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if item.Deleted {
				continue
			}
			tasks = append(tasks, taskFromGoogle(item))
		}
		if page.NextPageToken == "" {
			return tasks, nil
		}
		if seenTokens[page.NextPageToken] {
			return nil, errors.New("google tasks: repeated page token")
		}
		seenTokens[page.NextPageToken] = true
		pageToken = page.NextPageToken
	}
}

func (c *GoogleTasksClient) CreateTask(ctx context.Context, tasklist string, task GoogleTask) (GoogleTask, error) {
	payload, err := taskToGoogle(task)
	if err != nil {
		return GoogleTask{}, err
	}
	if payload.Status == "" {
		payload.Status = "needsAction"
	}
	// id/etag/updated/completed are output-only on insert.
	payload.ID = ""
	payload.ETag = ""
	payload.Updated = ""
	payload.Completed = ""
	endpoint := c.baseURL() + "/lists/" + url.PathEscape(taskListID(tasklist)) + "/tasks"
	data, err := googleRequest(ctx, c.httpClient(), c.Tokens, http.MethodPost, endpoint, payload)
	if err != nil {
		return GoogleTask{}, err
	}
	var created googleTask
	if err := decodeGoogleJSON(data, &created); err != nil {
		return GoogleTask{}, err
	}
	return taskFromGoogle(created), nil
}

func (c *GoogleTasksClient) UpdateTask(ctx context.Context, tasklist string, task GoogleTask) (GoogleTask, error) {
	if strings.TrimSpace(task.ID) == "" {
		return GoogleTask{}, errors.New("google task id is required")
	}
	payload, err := taskToGoogle(task)
	if err != nil {
		return GoogleTask{}, err
	}
	// id/etag/updated/completed are output-only on update.
	payload.ID = ""
	payload.ETag = ""
	payload.Updated = ""
	payload.Completed = ""
	endpoint := c.baseURL() + "/lists/" + url.PathEscape(taskListID(tasklist)) + "/tasks/" + url.PathEscape(task.ID)
	data, err := googleRequest(ctx, c.httpClient(), c.Tokens, http.MethodPatch, endpoint, payload)
	if err != nil {
		return GoogleTask{}, err
	}
	var updated googleTask
	if err := decodeGoogleJSON(data, &updated); err != nil {
		return GoogleTask{}, err
	}
	return taskFromGoogle(updated), nil
}

// DeleteTask removes a task. A 404 is treated as success for idempotent retries.
func (c *GoogleTasksClient) DeleteTask(ctx context.Context, tasklist, taskID string) error {
	if strings.TrimSpace(taskID) == "" {
		return errors.New("google task id is required")
	}
	endpoint := c.baseURL() + "/lists/" + url.PathEscape(taskListID(tasklist)) + "/tasks/" + url.PathEscape(taskID)
	if _, err := googleRequest(ctx, c.httpClient(), c.Tokens, http.MethodDelete, endpoint, nil); err != nil {
		if IsGoogleAPIStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func taskToGoogle(task GoogleTask) (googleTask, error) {
	title := strings.TrimSpace(task.Title)
	if title == "" {
		return googleTask{}, errors.New("google task title is required")
	}
	due, err := taskDueWire(task.Due)
	if err != nil {
		return googleTask{}, err
	}
	return googleTask{
		ID:        task.ID,
		Title:     title,
		Notes:     task.Notes,
		Due:       due,
		Status:    task.Status,
		Completed: task.Completed,
		ETag:      task.ETag,
		Updated:   task.Updated,
	}, nil
}

// taskDueWire sends the calendar date of a reminder as UTC midnight because
// Google Tasks discards the time of day.
func taskDueWire(due string) (string, error) {
	if due == "" {
		return "", nil
	}
	parsed, err := time.Parse(time.RFC3339, due)
	if err != nil {
		return "", fmt.Errorf("google task due must be RFC3339: %w", err)
	}
	return parsed.Format("2006-01-02") + "T00:00:00.000Z", nil
}

func taskFromGoogle(task googleTask) GoogleTask {
	return GoogleTask{
		ID:        task.ID,
		Title:     task.Title,
		Notes:     task.Notes,
		Due:       normalizeRFC3339(task.Due),
		Status:    task.Status,
		Completed: normalizeRFC3339(task.Completed),
		ETag:      task.ETag,
		Updated:   task.Updated,
	}
}

// ---------------------------------------------------------------------------
// Sync state
// ---------------------------------------------------------------------------

const (
	opCreateTask  = "create_task"
	opUpdateTask  = "update_task"
	opDeleteTask  = "delete_task"
	opCreateEvent = "create_event"
	opUpdateEvent = "update_event"
	opDeleteEvent = "delete_event"
)

// PendingOp is a persisted local mutation waiting to be pushed to Google.
// Delete operations keep the remote identity and the last-seen version so a
// remote edit can win over a queued local delete.
type PendingOp struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	ReminderID  string `json:"reminder_id"`
	RemoteID    string `json:"remote_id,omitempty"`
	RemoteKind  string `json:"remote_kind,omitempty"`
	BaseETag    string `json:"base_etag,omitempty"`
	BaseUpdated string `json:"base_updated,omitempty"`
	Attempts    int    `json:"attempts,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	QueuedAt    string `json:"queued_at"`
}

type calendarSyncState struct {
	Version     int             `json:"version"`
	UpdatedAt   string          `json:"updated_at,omitempty"`
	WindowStart string          `json:"window_start,omitempty"`
	WindowEnd   string          `json:"window_end,omitempty"`
	Reminders   []Reminder      `json:"reminders"`
	Events      []CalendarEvent `json:"events,omitempty"`
	Tasks       []GoogleTask    `json:"tasks,omitempty"`
	Pending     []PendingOp     `json:"pending,omitempty"`
}

var localReminderSeq atomic.Uint64

func newLocalReminderID(now time.Time) string {
	return fmt.Sprintf("local-%d-%d", now.UnixNano(), localReminderSeq.Add(1))
}

func newPendingOpID(now time.Time) string {
	return fmt.Sprintf("op-%d-%d", now.UnixNano(), localReminderSeq.Add(1))
}

// CalendarSync merges local reminders with Google Calendar events and Google
// Tasks. The cache is persisted atomically to <dir>/calendar_sync.json.
//
// Conflict policy: remote wins. If the remote object changed since the last
// successful sync, its fields replace local fields and any queued local edit
// or delete for that object is discarded. A remote deletion also removes the
// local mirror. Local-only reminders are never touched by a pull.
type CalendarSync struct {
	dir      string
	Calendar *GoogleCalendarClient
	Tasks    *GoogleTasksClient
	TaskList string
	Now      func() time.Time

	// WindowPast/WindowFuture define the event pull window. Events outside the
	// window are kept in the cache and are not treated as remotely deleted.
	WindowPast   time.Duration
	WindowFuture time.Duration

	mu     sync.Mutex
	loaded bool
	state  calendarSyncState
}

func NewCalendarSync(dir string, tokens TokenProvider) *CalendarSync {
	return &CalendarSync{
		dir:          dir,
		Calendar:     NewGoogleCalendarClient(tokens),
		Tasks:        NewGoogleTasksClient(tokens),
		TaskList:     "@default",
		Now:          time.Now,
		WindowPast:   24 * time.Hour,
		WindowFuture: 90 * 24 * time.Hour,
	}
}

// NewCalendarSyncWithClients is useful for tests and for sharing a custom
// HTTP client/token provider between both REST clients.
func NewCalendarSyncWithClients(dir string, calendar *GoogleCalendarClient, tasks *GoogleTasksClient) *CalendarSync {
	sync := NewCalendarSync(dir, nil)
	sync.Calendar = calendar
	sync.Tasks = tasks
	return sync
}

func (s *CalendarSync) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *CalendarSync) statePath() string {
	return filepath.Join(s.dir, calendarSyncFileName)
}

func (s *CalendarSync) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		return
	}
	var state calendarSyncState
	if json.Unmarshal(data, &state) != nil {
		// A corrupt cache is not fatal: rebuild it from Google on the next sync.
		return
	}
	if state.Version != calendarSyncStateVer {
		return
	}
	if state.Reminders == nil {
		state.Reminders = []Reminder{}
	}
	s.state = state
}

func (s *CalendarSync) saveLocked() error {
	s.state.Version = calendarSyncStateVer
	s.state.UpdatedAt = s.now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(&s.state, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicFile(s.statePath(), data, 0600)
}

func (s *CalendarSync) findLocked(id string) int {
	for i := range s.state.Reminders {
		if s.state.Reminders[i].ID == id {
			return i
		}
	}
	return -1
}

func (s *CalendarSync) removeReminderLocked(id string) {
	index := s.findLocked(id)
	if index < 0 {
		return
	}
	s.state.Reminders = append(s.state.Reminders[:index], s.state.Reminders[index+1:]...)
}

func (s *CalendarSync) dropPendingLocked(reminderID string) {
	filtered := s.state.Pending[:0]
	for _, op := range s.state.Pending {
		if op.ReminderID != reminderID {
			filtered = append(filtered, op)
		}
	}
	s.state.Pending = filtered
}

// queueLocked records (and coalesces) the operation needed to push a reminder.
func (s *CalendarSync) queueLocked(reminderID, kind, remoteID, remoteKind string) {
	now := s.now()
	for i := range s.state.Pending {
		existing := &s.state.Pending[i]
		if existing.ReminderID != reminderID {
			continue
		}
		switch {
		case existing.Kind == opCreateTask && kind == opUpdateTask:
			kind = opCreateTask
		case existing.Kind == opCreateEvent && kind == opUpdateEvent:
			kind = opCreateEvent
		case existing.Kind == opCreateTask && kind == opDeleteTask:
			s.state.Pending = append(s.state.Pending[:i], s.state.Pending[i+1:]...)
			return
		case existing.Kind == opCreateEvent && kind == opDeleteEvent:
			s.state.Pending = append(s.state.Pending[:i], s.state.Pending[i+1:]...)
			return
		case existing.Kind == opUpdateTask && kind == opDeleteTask:
			kind = opDeleteTask
		case existing.Kind == opUpdateEvent && kind == opDeleteEvent:
			kind = opDeleteEvent
		}
		existing.Kind = kind
		existing.LastError = ""
		if remoteID != "" {
			existing.RemoteID = remoteID
		}
		if remoteKind != "" {
			existing.RemoteKind = remoteKind
		}
		return
	}
	s.state.Pending = append(s.state.Pending, PendingOp{
		ID:         newPendingOpID(now),
		Kind:       kind,
		ReminderID: reminderID,
		RemoteID:   remoteID,
		RemoteKind: remoteKind,
		QueuedAt:   now.UTC().Format(time.RFC3339),
	})
}

func createOpFor(remoteKind string) string {
	if remoteKind == RemoteKindEvent {
		return opCreateEvent
	}
	return opCreateTask
}

func updateOpFor(remoteKind string) string {
	if remoteKind == RemoteKindEvent {
		return opUpdateEvent
	}
	return opUpdateTask
}

func deleteOpFor(remoteKind string) string {
	if remoteKind == RemoteKindEvent {
		return opDeleteEvent
	}
	return opDeleteTask
}

// ---------------------------------------------------------------------------
// ReminderBackend implementation
// ---------------------------------------------------------------------------

// List returns all known reminders sorted by due date. Reminders without a due
// date sort last.
func (s *CalendarSync) List(ctx context.Context) ([]Reminder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	result := make([]Reminder, len(s.state.Reminders))
	copy(result, s.state.Reminders)
	sort.SliceStable(result, func(i, j int) bool {
		left, leftOK := parseReminderDue(result[i].Due)
		right, rightOK := parseReminderDue(result[j].Due)
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK && !left.Equal(right) {
			return left.Before(right)
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func parseReminderDue(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// Create stores a new local reminder and queues it for push. It does not touch
// the network; call Sync to push.
func (s *CalendarSync) Create(ctx context.Context, reminder Reminder) (Reminder, error) {
	if err := ctx.Err(); err != nil {
		return Reminder{}, err
	}
	text := strings.TrimSpace(reminder.Text)
	if text == "" {
		return Reminder{}, ErrReminderTextEmpty
	}
	due, err := normalizeDue(strings.TrimSpace(reminder.Due))
	if err != nil {
		return Reminder{}, err
	}
	remoteKind := reminder.RemoteKind
	if remoteKind == "" {
		remoteKind = RemoteKindTask
	}
	if remoteKind != RemoteKindTask && remoteKind != RemoteKindEvent {
		return Reminder{}, fmt.Errorf("unsupported reminder remote kind %q", remoteKind)
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	id := strings.TrimSpace(reminder.ID)
	if id == "" {
		id = newLocalReminderID(now)
	}
	for s.findLocked(id) >= 0 {
		id = newLocalReminderID(now)
	}
	stored := Reminder{
		ID:         id,
		Text:       text,
		Due:        due,
		Source:     ReminderSourceLocal,
		Updated:    now.UTC().Format(time.RFC3339),
		RemoteKind: remoteKind,
	}
	s.state.Reminders = append(s.state.Reminders, stored)
	s.queueLocked(stored.ID, createOpFor(remoteKind), "", remoteKind)
	if err := s.saveLocked(); err != nil {
		return Reminder{}, err
	}
	return stored, nil
}

// Update edits a reminder locally and queues the change. Identity and remote
// bookkeeping fields are preserved from the stored record.
func (s *CalendarSync) Update(ctx context.Context, reminder Reminder) (Reminder, error) {
	if err := ctx.Err(); err != nil {
		return Reminder{}, err
	}
	text := strings.TrimSpace(reminder.Text)
	if text == "" {
		return Reminder{}, ErrReminderTextEmpty
	}
	due, err := normalizeDue(strings.TrimSpace(reminder.Due))
	if err != nil {
		return Reminder{}, err
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	index := s.findLocked(reminder.ID)
	if index < 0 {
		return Reminder{}, ErrReminderNotFound
	}
	stored := s.state.Reminders[index]
	stored.Text = text
	stored.Due = due
	stored.Updated = now.UTC().Format(time.RFC3339)
	s.state.Reminders[index] = stored
	if stored.RemoteID == "" {
		s.queueLocked(stored.ID, createOpFor(stored.RemoteKind), "", stored.RemoteKind)
	} else {
		s.queueLocked(stored.ID, updateOpFor(stored.RemoteKind), stored.RemoteID, stored.RemoteKind)
	}
	if err := s.saveLocked(); err != nil {
		return Reminder{}, err
	}
	return stored, nil
}

// Delete removes a reminder locally and queues a remote delete when it has
// already been pushed.
func (s *CalendarSync) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	index := s.findLocked(id)
	if index < 0 {
		return ErrReminderNotFound
	}
	stored := s.state.Reminders[index]
	s.state.Reminders = append(s.state.Reminders[:index], s.state.Reminders[index+1:]...)
	s.dropPendingLocked(id)
	if stored.RemoteID != "" {
		s.queueLocked(id, deleteOpFor(stored.RemoteKind), stored.RemoteID, stored.RemoteKind)
		s.setPendingBaseLocked(id, stored.ETag, stored.Updated)
	}
	return s.saveLocked()
}

// setPendingBaseLocked records the version the local delete was based on.
func (s *CalendarSync) setPendingBaseLocked(reminderID, etag, updated string) {
	for i := range s.state.Pending {
		op := &s.state.Pending[i]
		if op.ReminderID == reminderID && isDeleteOp(op.Kind) {
			op.BaseETag = etag
			op.BaseUpdated = updated
		}
	}
}

// ---------------------------------------------------------------------------
// Sync
// ---------------------------------------------------------------------------

// Sync pulls remote events/tasks, merges them using the remote-wins policy,
// then pushes queued local mutations. Pulling first is what lets a remote edit
// win over a local edit that has not been pushed yet. When the network is
// unavailable the pending queue is kept on disk and the error is returned; the
// next call retries the same operations.
func (s *CalendarSync) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if s.Calendar == nil || s.Tasks == nil {
		return errors.New("calendar sync: calendar and task clients are required")
	}
	events, tasks, err := s.pullLocked(ctx)
	if err != nil {
		_ = s.saveLocked()
		return fmt.Errorf("calendar sync: pull remote changes: %w", err)
	}
	s.mergeLocked(events, tasks)
	if err := s.flushLocked(ctx); err != nil {
		_ = s.saveLocked()
		return fmt.Errorf("calendar sync: push local changes: %w", err)
	}
	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("calendar sync: save state: %w", err)
	}
	return nil
}

func (s *CalendarSync) flushLocked(ctx context.Context) error {
	for len(s.state.Pending) > 0 {
		op := s.state.Pending[0]
		reminder := Reminder{ID: op.ReminderID, RemoteID: op.RemoteID, RemoteKind: op.RemoteKind}
		if index := s.findLocked(op.ReminderID); index >= 0 {
			reminder = s.state.Reminders[index]
		} else if !isDeleteOp(op.Kind) {
			// The reminder disappeared before its create/update was pushed.
			s.state.Pending = s.state.Pending[1:]
			continue
		}
		err := s.applyOpLocked(ctx, op, reminder)
		if err != nil {
			op.Attempts++
			op.LastError = err.Error()
			s.state.Pending[0] = op
			return err
		}
		s.state.Pending = s.state.Pending[1:]
		if err := s.saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

func isDeleteOp(kind string) bool {
	return kind == opDeleteTask || kind == opDeleteEvent
}

func (s *CalendarSync) applyOpLocked(ctx context.Context, op PendingOp, reminder Reminder) error {
	switch op.Kind {
	case opDeleteTask, opDeleteEvent:
		remoteID := reminder.RemoteID
		if remoteID == "" {
			remoteID = op.RemoteID
		}
		if remoteID != "" {
			var err error
			if op.Kind == opDeleteEvent {
				err = s.Calendar.DeleteEvent(ctx, remoteID)
			} else {
				err = s.Tasks.DeleteTask(ctx, s.TaskList, remoteID)
			}
			if err != nil {
				return err
			}
		}
		s.removeReminderLocked(reminder.ID)
		if op.Kind == opDeleteEvent {
			s.dropCachedEventLocked(remoteID)
		} else {
			s.dropCachedTaskLocked(remoteID)
		}
		return nil
	}

	index := s.findLocked(reminder.ID)
	if index < 0 {
		return nil
	}
	switch op.Kind {
	case opCreateTask:
		created, err := s.Tasks.CreateTask(ctx, s.TaskList, GoogleTask{
			Title:  reminder.Text,
			Due:    reminder.Due,
			Status: "needsAction",
		})
		if err != nil {
			return err
		}
		reminder.RemoteID = created.ID
		reminder.RemoteKind = RemoteKindTask
		reminder.Source = ReminderSourceTask
		reminder.ETag = created.ETag
		reminder.Updated = created.Updated
		s.state.Reminders[index] = reminder
		s.cacheTaskLocked(created)
	case opUpdateTask:
		task := s.taskForReminderLocked(reminder)
		updated, err := s.Tasks.UpdateTask(ctx, s.TaskList, task)
		if err != nil {
			return err
		}
		reminder.ETag = updated.ETag
		reminder.Updated = updated.Updated
		// Tasks returns only a date. Keep the time of a reminder created here.
		if isImportedGoogleTask(reminder) {
			reminder.Due = updated.Due
		}
		reminder.Source = ReminderSourceTask
		s.state.Reminders[index] = reminder
		s.cacheTaskLocked(updated)
	case opCreateEvent:
		event := s.eventForReminderLocked(reminder)
		created, err := s.Calendar.CreateEvent(ctx, event)
		if err != nil {
			return err
		}
		reminder.RemoteID = created.ID
		reminder.RemoteKind = RemoteKindEvent
		reminder.Source = ReminderSourceCalendar
		reminder.ETag = created.ETag
		reminder.Updated = created.Updated
		reminder.Due = created.Start
		reminder.Text = created.Summary
		s.state.Reminders[index] = reminder
		s.cacheEventLocked(created)
	case opUpdateEvent:
		event := s.eventForReminderLocked(reminder)
		updated, err := s.Calendar.UpdateEvent(ctx, event)
		if err != nil {
			return err
		}
		reminder.ETag = updated.ETag
		reminder.Updated = updated.Updated
		reminder.Due = updated.Start
		reminder.Text = updated.Summary
		reminder.Source = ReminderSourceCalendar
		s.state.Reminders[index] = reminder
		s.cacheEventLocked(updated)
	default:
		return fmt.Errorf("unknown pending operation %q", op.Kind)
	}
	return nil
}

func (s *CalendarSync) taskForReminderLocked(reminder Reminder) GoogleTask {
	for _, task := range s.state.Tasks {
		if task.ID == reminder.RemoteID {
			task.Title = reminder.Text
			task.Due = reminder.Due
			if task.Status == "" {
				task.Status = "needsAction"
			}
			return task
		}
	}
	return GoogleTask{
		ID:     reminder.RemoteID,
		Title:  reminder.Text,
		Due:    reminder.Due,
		Status: "needsAction",
	}
}

func (s *CalendarSync) eventForReminderLocked(reminder Reminder) CalendarEvent {
	for _, event := range s.state.Events {
		if event.ID != reminder.RemoteID {
			continue
		}
		event.Summary = reminder.Text
		if reminder.Due != "" {
			if start, err := time.Parse(time.RFC3339, reminder.Due); err == nil {
				if oldStart, oldStartErr := time.Parse(time.RFC3339, event.Start); oldStartErr == nil {
					if oldEnd, oldEndErr := time.Parse(time.RFC3339, event.End); oldEndErr == nil && oldEnd.After(oldStart) {
						event.End = start.Add(oldEnd.Sub(oldStart)).UTC().Format(time.RFC3339)
					}
				}
				event.Start = start.UTC().Format(time.RFC3339)
			}
		}
		return event
	}
	start := reminder.Due
	if start == "" {
		start = s.now().UTC().Format(time.RFC3339)
	}
	startTime, err := time.Parse(time.RFC3339, start)
	if err != nil {
		startTime = s.now()
		start = startTime.UTC().Format(time.RFC3339)
	}
	return CalendarEvent{
		Summary:  reminder.Text,
		Start:    start,
		End:      startTime.Add(time.Hour).UTC().Format(time.RFC3339),
		TimeZone: "UTC",
		Status:   "confirmed",
		Reminders: []EventReminder{
			{Method: "popup", Minutes: 0},
		},
	}
}

func (s *CalendarSync) pullLocked(ctx context.Context) ([]CalendarEvent, []GoogleTask, error) {
	now := s.now().UTC()
	windowStart := now.Add(-s.windowPast())
	windowEnd := now.Add(s.windowFuture())
	events, err := s.Calendar.ListEvents(ctx, windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339))
	if err != nil {
		return nil, nil, fmt.Errorf("list calendar events: %w", err)
	}
	tasks, err := s.Tasks.ListTasks(ctx, s.TaskList)
	if err != nil {
		return nil, nil, fmt.Errorf("list google tasks: %w", err)
	}
	s.state.WindowStart = windowStart.Format(time.RFC3339)
	s.state.WindowEnd = windowEnd.Format(time.RFC3339)
	return events, tasks, nil
}

func (s *CalendarSync) windowPast() time.Duration {
	if s.WindowPast > 0 {
		return s.WindowPast
	}
	return 24 * time.Hour
}

func (s *CalendarSync) windowFuture() time.Duration {
	if s.WindowFuture > 0 {
		return s.WindowFuture
	}
	return 90 * 24 * time.Hour
}

func (s *CalendarSync) mergeLocked(events []CalendarEvent, tasks []GoogleTask) {
	remote := make(map[string]Reminder, len(events)+len(tasks))
	for _, event := range events {
		reminder := reminderFromEvent(event)
		remote[remoteKey(reminder)] = reminder
	}
	for _, task := range tasks {
		reminder := reminderFromTask(task)
		remote[remoteKey(reminder)] = reminder
	}
	windowStart, _ := time.Parse(time.RFC3339, s.state.WindowStart)
	windowEnd, _ := time.Parse(time.RFC3339, s.state.WindowEnd)
	pendingDeletes := s.pendingDeletesLocked()
	previousTaskDates := make(map[string]string, len(s.state.Tasks))
	for _, task := range s.state.Tasks {
		previousTaskDates[task.ID] = task.Due
	}

	merged := make([]Reminder, 0, len(s.state.Reminders)+len(remote))
	seen := make(map[string]bool, len(remote))
	for _, local := range s.state.Reminders {
		if local.RemoteID == "" {
			merged = append(merged, local)
			continue
		}
		key := remoteKey(local)
		if op, deleting := pendingDeletes[key]; deleting {
			remoteReminder, found := remote[key]
			if !found || !remoteVersionChanged(op.BaseETag, op.BaseUpdated, remoteReminder) {
				// Remote is unchanged (or already gone): the local delete wins.
				continue
			}
			// The remote object changed after the local delete was queued.
			// Remote wins, so cancel the delete and fall through to merge it.
			s.dropPendingLocked(local.ID)
			delete(pendingDeletes, key)
		}
		remoteReminder, found := remote[key]
		if !found {
			// Events outside the pull window cannot be proven deleted.
			if local.RemoteKind == RemoteKindEvent && !eventInWindow(local.Due, windowStart, windowEnd) {
				merged = append(merged, local)
				continue
			}
			// Remote deletion wins.
			s.dropPendingLocked(local.ID)
			continue
		}
		seen[key] = true
		if sameRemoteVersion(local, remoteReminder) {
			// No remote change: preserve local edits that are still queued.
			if local.TimeZone == "" {
				local.TimeZone = remoteReminder.TimeZone
			}
			merged = append(merged, local)
			continue
		}
		// Remote changed: remote fields win, queued local edits are discarded.
		remoteReminder.ID = local.ID
		remoteReminder.RemoteID = local.RemoteID
		remoteReminder.RemoteKind = local.RemoteKind
		if local.RemoteKind == RemoteKindTask && !isImportedGoogleTask(local) && remoteReminder.Due != "" {
			previous, known := previousTaskDates[local.RemoteID]
			if !known {
				previous, _ = taskDueWire(local.Due)
			}
			if normalizeRFC3339(previous) == normalizeRFC3339(remoteReminder.Due) {
				remoteReminder.Due = local.Due
			}
		}
		if local.Source == ReminderSourceLocal {
			remoteReminder.Source = ReminderSourceLocal
		}
		merged = append(merged, remoteReminder)
		s.dropPendingLocked(local.ID)
	}
	for key, reminder := range remote {
		if seen[key] {
			continue
		}
		if op, deleting := pendingDeletes[key]; deleting {
			if !remoteVersionChanged(op.BaseETag, op.BaseUpdated, reminder) {
				// Remote unchanged: let the queued delete proceed.
				continue
			}
			// Remote changed: remote wins and the queued delete is discarded.
			s.dropPendingLocked(op.ReminderID)
		}
		merged = append(merged, reminder)
	}
	s.state.Reminders = merged
	s.state.Events = mergeEventCache(s.state.Events, events, windowStart, windowEnd)
	s.state.Tasks = append([]GoogleTask(nil), tasks...)
}

func reminderFromEvent(event CalendarEvent) Reminder {
	due := event.Start
	if due == "" {
		due = event.End
	}
	text := strings.TrimSpace(event.Summary)
	if text == "" {
		text = strings.TrimSpace(event.Description)
	}
	if text == "" {
		text = "(calendar event)"
	}
	return Reminder{
		ID:         "calendar:" + event.ID,
		Text:       text,
		Due:        due,
		TimeZone:   event.TimeZone,
		Source:     ReminderSourceCalendar,
		ETag:       event.ETag,
		Updated:    event.Updated,
		RemoteID:   event.ID,
		RemoteKind: RemoteKindEvent,
	}
}

func reminderFromTask(task GoogleTask) Reminder {
	text := strings.TrimSpace(task.Title)
	if text == "" {
		text = strings.TrimSpace(task.Notes)
	}
	if text == "" {
		text = "(google task)"
	}
	return Reminder{
		ID:         "task:" + task.ID,
		Text:       text,
		Due:        task.Due,
		Source:     ReminderSourceTask,
		ETag:       task.ETag,
		Updated:    task.Updated,
		RemoteID:   task.ID,
		RemoteKind: RemoteKindTask,
	}
}

func isImportedGoogleTask(reminder Reminder) bool {
	return reminder.RemoteKind == RemoteKindTask && strings.HasPrefix(reminder.ID, "task:")
}

func remoteKey(reminder Reminder) string {
	return reminder.RemoteKind + ":" + reminder.RemoteID
}

func (s *CalendarSync) pendingDeletesLocked() map[string]PendingOp {
	deletes := make(map[string]PendingOp)
	for _, op := range s.state.Pending {
		if !isDeleteOp(op.Kind) || op.RemoteID == "" {
			continue
		}
		kind := op.RemoteKind
		if kind == "" {
			if op.Kind == opDeleteEvent {
				kind = RemoteKindEvent
			} else {
				kind = RemoteKindTask
			}
		}
		deletes[kind+":"+op.RemoteID] = op
	}
	return deletes
}

// remoteVersionChanged compares a queued operation's base version with the
// remote object. With no version metadata at all, the object is assumed
// unchanged so an explicit local delete is not blocked forever.
func remoteVersionChanged(baseETag, baseUpdated string, remote Reminder) bool {
	if baseETag != "" || remote.ETag != "" {
		return baseETag != remote.ETag
	}
	if baseUpdated != "" || remote.Updated != "" {
		return baseUpdated != remote.Updated
	}
	return false
}

func sameRemoteVersion(local, remote Reminder) bool {
	if local.ETag != "" || remote.ETag != "" {
		return local.ETag == remote.ETag
	}
	if local.Updated != "" || remote.Updated != "" {
		return local.Updated == remote.Updated
	}
	// No version metadata: identical content is treated as unchanged.
	return local.Text == remote.Text && local.Due == remote.Due
}

func eventInWindow(due string, windowStart, windowEnd time.Time) bool {
	if due == "" || windowStart.IsZero() || windowEnd.IsZero() {
		return false
	}
	parsed, err := time.Parse(time.RFC3339, due)
	if err != nil {
		return false
	}
	return !parsed.Before(windowStart) && !parsed.After(windowEnd)
}

func mergeEventCache(old, fresh []CalendarEvent, windowStart, windowEnd time.Time) []CalendarEvent {
	merged := make([]CalendarEvent, 0, len(fresh)+len(old))
	seen := make(map[string]bool, len(fresh))
	for _, event := range fresh {
		merged = append(merged, event)
		seen[event.ID] = true
	}
	for _, event := range old {
		if seen[event.ID] {
			continue
		}
		// Keep cached events outside the queried window; drop ones that were
		// inside it and therefore absent from a complete pull.
		if eventInWindow(event.Start, windowStart, windowEnd) {
			continue
		}
		merged = append(merged, event)
	}
	return merged
}

func (s *CalendarSync) cacheTaskLocked(task GoogleTask) {
	for i := range s.state.Tasks {
		if s.state.Tasks[i].ID == task.ID {
			s.state.Tasks[i] = task
			return
		}
	}
	s.state.Tasks = append(s.state.Tasks, task)
}

func (s *CalendarSync) dropCachedTaskLocked(id string) {
	filtered := s.state.Tasks[:0]
	for _, task := range s.state.Tasks {
		if task.ID != id {
			filtered = append(filtered, task)
		}
	}
	s.state.Tasks = filtered
}

func (s *CalendarSync) cacheEventLocked(event CalendarEvent) {
	for i := range s.state.Events {
		if s.state.Events[i].ID == event.ID {
			s.state.Events[i] = event
			return
		}
	}
	s.state.Events = append(s.state.Events, event)
}

func (s *CalendarSync) dropCachedEventLocked(id string) {
	filtered := s.state.Events[:0]
	for _, event := range s.state.Events {
		if event.ID != id {
			filtered = append(filtered, event)
		}
	}
	s.state.Events = filtered
}

var _ ReminderBackend = (*CalendarSync)(nil)

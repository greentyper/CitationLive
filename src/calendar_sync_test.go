package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestGoogleListsReadAllPagesAndRejectRepeatedTokens(t *testing.T) {
	for _, endpoint := range []string{"calendar", "tasks"} {
		t.Run(endpoint, func(t *testing.T) {
			for _, repeatToken := range []bool{false, true} {
				t.Run(strconv.FormatBool(repeatToken), func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						token := r.URL.Query().Get("pageToken")
						page, _ := strconv.Atoi(token)
						next := ""
						if repeatToken {
							next = "1"
						} else if page < 20 {
							next = strconv.Itoa(page + 1)
						}
						_ = json.NewEncoder(w).Encode(M{
							"items":         []M{{"id": strconv.Itoa(page)}},
							"nextPageToken": next,
						})
					}))
					defer server.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					var count int
					var err error
					if endpoint == "calendar" {
						client := &GoogleCalendarClient{BaseURL: server.URL, Tokens: StaticToken("test")}
						var events []CalendarEvent
						events, err = client.ListEvents(ctx, "", "")
						count = len(events)
					} else {
						client := &GoogleTasksClient{BaseURL: server.URL, Tokens: StaticToken("test")}
						var tasks []GoogleTask
						tasks, err = client.ListTasks(ctx, "")
						count = len(tasks)
					}
					if repeatToken {
						if err == nil || count != 0 {
							t.Fatalf("repeated cursor returned a partial list as success: count=%d err=%v", count, err)
						}
					} else if err != nil || count != 21 {
						t.Fatalf("expected 21 pages: count=%d err=%v", count, err)
					}
				})
			}
		})
	}
}

func TestGoogleTaskUpdateRetainsSpeakerReminderTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(M{
			"id": "remote-id", "title": "Reminder", "due": "2026-11-19T00:00:00Z",
		})
	}))
	defer server.Close()
	sync := NewCalendarSyncWithClients(t.TempDir(), nil,
		&GoogleTasksClient{BaseURL: server.URL, Tokens: StaticToken("test")})
	due := "2026-11-19T17:30:00+07:00"
	reminder := Reminder{ID: "r1", Text: "Reminder", Due: due, RemoteID: "remote-id", RemoteKind: RemoteKindTask}
	sync.state.Reminders = []Reminder{reminder}
	if err := sync.applyOpLocked(context.Background(), PendingOp{Kind: opUpdateTask}, reminder); err != nil {
		t.Fatal(err)
	}
	if got := sync.state.Reminders[0].Due; got != due {
		t.Fatalf("update replaced speaker time %q with %q", due, got)
	}
}

func TestGoogleTaskUsesLocalCalendarDate(t *testing.T) {
	due := "2026-11-19T00:30:00+07:00"
	sync := NewCalendarSync(t.TempDir(), nil)
	reminder, err := sync.Create(context.Background(), Reminder{Text: "Reminder", Due: due})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := taskDueWire(reminder.Due); err != nil || got != "2026-11-19T00:00:00.000Z" {
		t.Fatalf("wrong calendar date on wire: %q, %v", got, err)
	}
	reminder.RemoteID = "task"
	reminder.ETag = "old"
	sync.state.Reminders = []Reminder{reminder}
	sync.state.Tasks = []GoogleTask{{ID: "task", Due: "2026-11-19T00:00:00Z"}}
	sync.mergeLocked(nil, []GoogleTask{{ID: "task", Title: "Renamed", Due: "2026-11-19T00:00:00Z", ETag: "new"}})
	got := sync.state.Reminders[0]
	if got.Text != "Renamed" || got.Due != due {
		t.Fatalf("task rename changed clock time: %+v", got)
	}
}

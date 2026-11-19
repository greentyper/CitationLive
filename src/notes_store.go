package main

// Local notes and checklists for the enrolled user.
// The store is local, mode 0600, and is only exposed to the model through
// the note tools when the current turn passed the local Voice Match gate.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	notesFileVersion = 1
	maxNotes         = 300
	maxNoteItems     = 200
)

type NoteItem struct {
	Text string `json:"text"`
	Done bool   `json:"done"`
}

type Note struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Body       string     `json:"body,omitempty"`
	Items      []NoteItem `json:"items,omitempty"`
	Created    string     `json:"created"`
	Updated    string     `json:"updated"`
	RemoteID   string     `json:"remote_id,omitempty"`
	RemoteList string     `json:"remote_list,omitempty"`
}

type notesFile struct {
	Version int    `json:"version"`
	Notes   []Note `json:"notes"`
}

type notesStore struct {
	mu     sync.Mutex
	path   string
	notes  []Note
	loaded bool
	seq    int64
}

var localNotes = &notesStore{path: filepath.Join(stateDirectory, "notes.json")}

func (m *notesStore) loadLocked() {
	if m.loaded {
		return
	}
	m.loaded = true
	data, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var file notesFile
	if json.Unmarshal(data, &file) != nil {
		return
	}
	m.notes = append([]Note(nil), file.Notes...)
	for _, note := range file.Notes {
		if strings.HasPrefix(note.ID, "n") {
			var n int64
			if _, err := fmt.Sscanf(note.ID, "n%d", &n); err == nil && n > m.seq {
				m.seq = n
			}
		}
	}
}

func (m *notesStore) saveLocked(notes []Note) error {
	file := notesFile{Version: notesFileVersion, Notes: notes}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomicFile(m.path, data, 0600); err != nil {
		return err
	}
	m.notes = notes
	return nil
}

func normalizeNoteKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

func cloneNote(note Note) Note {
	note.Items = append([]NoteItem(nil), note.Items...)
	return note
}

// findLocked resolves a note by exact id, then by exact title, then by a
// unique title substring. It must be called with m.mu held.
func (m *notesStore) findLocked(id, title string) (int, error) {
	id = strings.TrimSpace(id)
	title = strings.TrimSpace(title)
	if id == "" && title == "" {
		return -1, errors.New("note id or title is required")
	}
	if id != "" {
		for i := range m.notes {
			if m.notes[i].ID == id {
				return i, nil
			}
		}
	}
	if title != "" {
		key := normalizeNoteKey(title)
		for i := range m.notes {
			if normalizeNoteKey(m.notes[i].Title) == key {
				return i, nil
			}
		}
		var matches []int
		for i := range m.notes {
			if strings.Contains(normalizeNoteKey(m.notes[i].Title), key) {
				matches = append(matches, i)
			}
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return -1, errors.New("several notes match this title; use note_id")
		}
	}
	return -1, errors.New("note not found")
}

func (m *notesStore) Create(title, body string) (Note, error) {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title == "" {
		return Note{}, errors.New("note title is required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	if len(m.notes) >= maxNotes {
		return Note{}, fmt.Errorf("note limit reached (%d)", maxNotes)
	}
	note := Note{
		ID:      fmt.Sprintf("n%d", m.seq+1),
		Title:   title,
		Body:    body,
		Created: now,
		Updated: now,
	}
	if err := m.saveLocked(append(slices.Clone(m.notes), note)); err != nil {
		return Note{}, err
	}
	m.seq++
	return cloneNote(note), nil
}

func (m *notesStore) Get(id, title string) (Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	index, err := m.findLocked(id, title)
	if err != nil {
		return Note{}, err
	}
	return cloneNote(m.notes[index]), nil
}

func (m *notesStore) List() []Note {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	out := make([]Note, 0, len(m.notes))
	for _, note := range m.notes {
		out = append(out, cloneNote(note))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Updated > out[j].Updated
	})
	return out
}

func (m *notesStore) AddItem(id, title, text string) (Note, int, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Note{}, 0, errors.New("list item text is required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	index, err := m.findLocked(id, title)
	if err != nil {
		return Note{}, 0, err
	}
	if len(m.notes[index].Items) >= maxNoteItems {
		return Note{}, 0, fmt.Errorf("note item limit reached (%d)", maxNoteItems)
	}
	note := cloneNote(m.notes[index])
	note.Items = append(note.Items, NoteItem{Text: text})
	note.Updated = now
	notes := slices.Clone(m.notes)
	notes[index] = note
	if err := m.saveLocked(notes); err != nil {
		return Note{}, 0, err
	}
	return cloneNote(note), len(note.Items), nil
}

func (m *notesStore) findItemLocked(note Note, text string, index int) (int, error) {
	if index > 0 {
		if index > len(note.Items) {
			return 0, fmt.Errorf("list item index out of range (1..%d)", len(note.Items))
		}
		return index - 1, nil
	}
	key := normalizeNoteKey(text)
	for i := range note.Items {
		if normalizeNoteKey(note.Items[i].Text) == key {
			return i, nil
		}
	}
	var matches []int
	for i := range note.Items {
		if strings.Contains(normalizeNoteKey(note.Items[i].Text), key) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return 0, errors.New("several list items match; use index")
	}
	return 0, errors.New("list item not found")
}

func (m *notesStore) CompleteItem(id, title, text string, index int) (Note, int, bool, error) {
	text = strings.TrimSpace(text)
	if text == "" && index <= 0 {
		return Note{}, 0, false, errors.New("list item text or index is required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	noteIndex, err := m.findLocked(id, title)
	if err != nil {
		return Note{}, 0, false, err
	}
	itemIndex, err := m.findItemLocked(m.notes[noteIndex], text, index)
	if err != nil {
		return Note{}, 0, false, err
	}
	note := cloneNote(m.notes[noteIndex])
	alreadyDone := note.Items[itemIndex].Done
	if !alreadyDone {
		note.Items[itemIndex].Done = true
		note.Updated = now
		notes := slices.Clone(m.notes)
		notes[noteIndex] = note
		if err := m.saveLocked(notes); err != nil {
			return Note{}, 0, false, err
		}
	}
	return cloneNote(note), itemIndex + 1, alreadyDone, nil
}

func (m *notesStore) Delete(id, title string) (Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	index, err := m.findLocked(id, title)
	if err != nil {
		return Note{}, err
	}
	deleted := cloneNote(m.notes[index])
	notes := slices.Delete(slices.Clone(m.notes), index, index+1)
	if err := m.saveLocked(notes); err != nil {
		return Note{}, err
	}
	return deleted, nil
}

// SetRemote records the Google Tasks identity after a successful create.
func (m *notesStore) SetRemote(id, remoteID, remoteList string) (Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	index, err := m.findLocked(id, "")
	if err != nil {
		return Note{}, err
	}
	notes := slices.Clone(m.notes)
	notes[index].RemoteID = strings.TrimSpace(remoteID)
	notes[index].RemoteList = strings.TrimSpace(remoteList)
	if err := m.saveLocked(notes); err != nil {
		return Note{}, err
	}
	return cloneNote(m.notes[index]), nil
}

func noteFromRemote(task GoogleTask, existing Note, list, now string) Note {
	body, items := parseNoteGoogleBody(task.Notes)
	if task.Status == "completed" {
		for i := range items {
			items[i].Done = true
		}
	}
	title := strings.TrimSpace(task.Title)
	if title == "" {
		title = strings.TrimSpace(existing.Title)
	}
	if title == "" {
		title = "Google Task"
	}
	updated := strings.TrimSpace(task.Updated)
	if updated == "" {
		updated = now
	}
	created := strings.TrimSpace(existing.Created)
	if created == "" {
		created = updated
	}
	id := existing.ID
	if id == "" {
		id = "g:" + task.ID
	}
	return Note{
		ID:         id,
		Title:      title,
		Body:       body,
		Items:      items,
		Created:    created,
		Updated:    updated,
		RemoteID:   task.ID,
		RemoteList: list,
	}
}

// MergeRemote folds Google Tasks into the local cache. Remote-backed notes use
// the remote title/body/status, local-only notes are kept, and notes whose
// remote task disappeared are removed. The merged list is persisted.
func (m *notesStore) MergeRemote(tasks []GoogleTask, list string) ([]Note, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	taskByID := make(map[string]GoogleTask, len(tasks))
	for _, task := range tasks {
		if strings.TrimSpace(task.ID) != "" {
			taskByID[task.ID] = task
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()

	merged := make([]Note, 0, len(m.notes)+len(taskByID))
	matched := make(map[string]bool, len(taskByID))
	isProtected := func(note Note) bool {
		return note.RemoteID == "" || (note.RemoteList != "" && note.RemoteList != list)
	}
	protected := 0
	for i := range m.notes {
		note := m.notes[i]
		if isProtected(note) {
			merged = append(merged, note)
			protected++
			continue
		}
		task, ok := taskByID[note.RemoteID]
		if !ok {
			// A remote deletion wins for notes mirrored from Google.
			continue
		}
		matched[task.ID] = true
		merged = append(merged, noteFromRemote(task, note, list, now))
	}
	for _, task := range tasks {
		if strings.TrimSpace(task.ID) == "" || matched[task.ID] {
			continue
		}
		matched[task.ID] = true
		merged = append(merged, noteFromRemote(task, Note{}, list, now))
	}
	if limit := max(maxNotes, protected); len(merged) > limit {
		sort.SliceStable(merged, func(i, j int) bool {
			leftProtected, rightProtected := isProtected(merged[i]), isProtected(merged[j])
			if leftProtected != rightProtected {
				return leftProtected
			}
			return merged[i].Updated > merged[j].Updated
		})
		merged = merged[:limit]
	}
	err := m.saveLocked(merged)
	out := make([]Note, 0, len(merged))
	for _, note := range merged {
		out = append(out, cloneNote(note))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Updated > out[j].Updated
	})
	return out, err
}

// noteGoogleBody renders a note as Google Tasks notes text: the free-form body
// followed by checklist lines.
func noteGoogleBody(note Note) string {
	body := strings.TrimSpace(note.Body)
	if len(note.Items) == 0 {
		return body
	}
	lines := make([]string, 0, len(note.Items)+2)
	if body != "" {
		lines = append(lines, body, "")
	}
	for _, item := range note.Items {
		mark := "[ ]"
		if item.Done {
			mark = "[x]"
		}
		lines = append(lines, mark+" "+strings.TrimSpace(item.Text))
	}
	return strings.Join(lines, "\n")
}

func parseNoteGoogleBody(notes string) (string, []NoteItem) {
	var bodyLines []string
	var items []NoteItem
	for _, line := range strings.Split(notes, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "[ ] "):
			items = append(items, NoteItem{Text: strings.TrimSpace(trimmed[4:])})
		case strings.HasPrefix(lower, "[x] "):
			items = append(items, NoteItem{Text: strings.TrimSpace(trimmed[4:]), Done: true})
		default:
			bodyLines = append(bodyLines, line)
		}
	}
	return strings.TrimSpace(strings.Join(bodyLines, "\n")), items
}

func noteItemsJSON(items []NoteItem) []M {
	out := make([]M, 0, len(items))
	for i, item := range items {
		out = append(out, M{"index": i + 1, "text": item.Text, "done": item.Done})
	}
	return out
}

func noteOpenItems(note Note) int {
	open := 0
	for _, item := range note.Items {
		if !item.Done {
			open++
		}
	}
	return open
}

func noteJSON(note Note) M {
	return M{
		"id":          note.ID,
		"title":       note.Title,
		"body":        note.Body,
		"items":       noteItemsJSON(note.Items),
		"item_count":  len(note.Items),
		"open_items":  noteOpenItems(note),
		"created":     note.Created,
		"updated":     note.Updated,
		"remote_id":   note.RemoteID,
		"remote_list": note.RemoteList,
	}
}

func noteSummaryJSON(note Note) M {
	return M{
		"id":         note.ID,
		"title":      note.Title,
		"item_count": len(note.Items),
		"open_items": noteOpenItems(note),
		"updated":    note.Updated,
	}
}

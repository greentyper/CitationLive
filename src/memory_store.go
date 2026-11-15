package main

// Personal memory for explicitly remembered user preferences and facts.
// The store is local, mode 0600, and is only exposed to the model through
// get_personal_context when the current turn passed the local Voice Match gate.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const memoryFileVersion = 1

type MemoryEntry struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Category string `json:"category,omitempty"`
	Created  string `json:"created"`
	Updated  string `json:"updated,omitempty"`
}

type memoryFile struct {
	Version int           `json:"version"`
	Entries []MemoryEntry `json:"entries"`
}

type memoryStore struct {
	mu      sync.Mutex
	path    string
	entries []MemoryEntry
	loaded  bool
	seq     int64
}

var personalMemory = &memoryStore{path: filepath.Join(stateDirectory, "memory.json")}

func (m *memoryStore) loadLocked() {
	if m.loaded {
		return
	}
	m.loaded = true
	data, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var file memoryFile
	if json.Unmarshal(data, &file) != nil {
		return
	}
	m.entries = append([]MemoryEntry(nil), file.Entries...)
	for _, entry := range file.Entries {
		if strings.HasPrefix(entry.ID, "m") {
			var n int64
			if _, err := fmt.Sscanf(entry.ID, "m%d", &n); err == nil && n > m.seq {
				m.seq = n
			}
		}
	}
}

func (m *memoryStore) saveLocked() error {
	file := memoryFile{Version: memoryFileVersion, Entries: m.entries}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicFile(m.path, data, 0600)
}

func normalizeMemoryText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

func (m *memoryStore) Add(text, category string) (MemoryEntry, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return MemoryEntry{}, errors.New("memory text is required")
	}
	category = strings.TrimSpace(category)
	now := time.Now().UTC().Format(time.RFC3339)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	key := normalizeMemoryText(text)
	for i := range m.entries {
		if normalizeMemoryText(m.entries[i].Text) == key {
			m.entries[i].Category = category
			m.entries[i].Updated = now
			if err := m.saveLocked(); err != nil {
				return MemoryEntry{}, err
			}
			return m.entries[i], nil
		}
	}
	m.seq++
	entry := MemoryEntry{
		ID:       fmt.Sprintf("m%d", m.seq),
		Text:     text,
		Category: category,
		Created:  now,
		Updated:  now,
	}
	m.entries = append(m.entries, entry)
	if len(m.entries) > 200 {
		m.entries = m.entries[len(m.entries)-200:]
	}
	if err := m.saveLocked(); err != nil {
		return MemoryEntry{}, err
	}
	return entry, nil
}

func (m *memoryStore) List() []MemoryEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	out := append([]MemoryEntry(nil), m.entries...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Updated > out[j].Updated
	})
	return out
}

func (m *memoryStore) Delete(id, text string) (int, error) {
	id = strings.TrimSpace(id)
	text = strings.TrimSpace(text)
	if id == "" && text == "" {
		return 0, errors.New("memory id or text is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	key := normalizeMemoryText(text)
	kept := m.entries[:0]
	deleted := 0
	for _, entry := range m.entries {
		match := id != "" && entry.ID == id
		if !match && key != "" {
			match = normalizeMemoryText(entry.Text) == key || strings.Contains(normalizeMemoryText(entry.Text), key)
		}
		if match {
			deleted++
			continue
		}
		kept = append(kept, entry)
	}
	m.entries = kept
	if deleted > 0 {
		if err := m.saveLocked(); err != nil {
			return 0, err
		}
	}
	return deleted, nil
}

func (m *memoryStore) Search(query string, limit int) []MemoryEntry {
	all := m.List()
	if limit <= 0 {
		limit = 8
	}
	query = normalizeMemoryText(query)
	if query == "" {
		if len(all) > limit {
			all = all[:limit]
		}
		return all
	}
	terms := strings.Fields(query)
	type scored struct {
		entry MemoryEntry
		score int
	}
	var matches []scored
	for _, entry := range all {
		text := normalizeMemoryText(entry.Text + " " + entry.Category)
		score := 0
		for _, term := range terms {
			if len(term) < 2 {
				continue
			}
			if strings.Contains(text, term) {
				score++
			}
		}
		if score > 0 {
			matches = append(matches, scored{entry: entry, score: score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].entry.Updated > matches[j].entry.Updated
	})
	out := make([]MemoryEntry, 0, limit)
	for _, match := range matches {
		if len(out) >= limit {
			break
		}
		out = append(out, match.entry)
	}
	return out
}

func memoryEntriesJSON(entries []MemoryEntry) []M {
	out := make([]M, 0, len(entries))
	for _, entry := range entries {
		out = append(out, M{
			"id": entry.ID, "text": entry.Text, "category": entry.Category,
			"created": entry.Created, "updated": entry.Updated,
		})
	}
	return out
}

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNotesWriteFailureLeavesChecklistUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.json")
	store := &notesStore{path: path}
	note, err := store.Create("Groceries", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddItem(note.ID, "", "Tea"); err != nil {
		t.Fatal(err)
	}

	backup := path + ".backup"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.CompleteItem(note.ID, "", "", 1); err == nil {
		t.Fatal("completing an item should fail when notes cannot be written")
	}
	current, err := store.Get(note.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if current.Items[0].Done {
		t.Error("a failed write marked the item complete in memory")
	}
	if _, err := store.Create("Duplicate on retry", ""); err == nil {
		t.Fatal("creating a note should fail when notes cannot be written")
	}
	if got := len(store.List()); got != 1 {
		t.Errorf("failed creation changed note count to %d", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if _, _, alreadyDone, err := store.CompleteItem(note.ID, "", "", 1); err != nil || alreadyDone {
		t.Errorf("retry: alreadyDone=%v, err=%v", alreadyDone, err)
	}
	reloaded := &notesStore{path: path}
	persisted, err := reloaded.Get(note.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.Items[0].Done {
		t.Error("retry did not persist the completed checklist item")
	}
}

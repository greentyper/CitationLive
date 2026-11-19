package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryWriteFailureLeavesEntriesUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	store := &memoryStore{path: path}
	entry, err := store.Add("Black tea", "drink")
	if err != nil {
		t.Fatal(err)
	}

	backup := path + ".backup"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("Black tea", "updated"); err == nil {
		t.Error("updating an entry should fail when memory cannot be written")
	}
	if _, err := store.Add("Coffee", "drink"); err == nil {
		t.Error("adding an entry should fail when memory cannot be written")
	}
	if got := store.List(); len(got) != 1 || got[0] != entry {
		t.Errorf("failed adds changed memory: %+v", got)
	}
	if _, err := store.Delete(entry.ID, ""); err == nil {
		t.Error("deleting an entry should fail when memory cannot be written")
	}
	if got := store.List(); len(got) != 1 || got[0] != entry {
		t.Errorf("failed deletion changed memory: %+v", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if count, err := store.Delete(entry.ID, ""); err != nil || count != 1 {
		t.Errorf("retry: deleted=%d, err=%v", count, err)
	}
	reloaded := &memoryStore{path: path}
	if got := reloaded.List(); len(got) != 0 {
		t.Errorf("retry left entries on disk: %+v", got)
	}
}

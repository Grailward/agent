package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyncStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync_state.json")

	s := LoadSyncState(path)
	if _, ok := s.Get("Kristan.d2s"); ok {
		t.Fatal("fresh state should be empty")
	}
	if err := s.Set("Kristan.d2s", "abc123"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if err := s.Set("SharedStashSoftCoreV2.d2i", "def456"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Reload from disk and confirm persistence.
	reloaded := LoadSyncState(path)
	if sha, ok := reloaded.Get("Kristan.d2s"); !ok || sha != "abc123" {
		t.Fatalf("Kristan not persisted: got (%q, %v)", sha, ok)
	}
	if sha, ok := reloaded.Get("SharedStashSoftCoreV2.d2i"); !ok || sha != "def456" {
		t.Fatalf("stash not persisted: got (%q, %v)", sha, ok)
	}
}

func TestSyncStateMissingFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	s := LoadSyncState(path)
	if _, ok := s.Get("anything.d2s"); ok {
		t.Fatal("missing file should load as empty state")
	}
	// Must still be usable (degrade, not crash).
	if err := s.Set("anything.d2s", "x"); err != nil {
		t.Fatalf("Set on empty state failed: %v", err)
	}
}

func TestSyncStateCorruptFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync_state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}
	s := LoadSyncState(path)
	if _, ok := s.Get("anything.d2s"); ok {
		t.Fatal("corrupt file should load as empty state")
	}
	// A subsequent Set repairs the file.
	if err := s.Set("Hero.d2s", "sha"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	reloaded := LoadSyncState(path)
	if sha, ok := reloaded.Get("Hero.d2s"); !ok || sha != "sha" {
		t.Fatalf("repaired state not persisted: got (%q, %v)", sha, ok)
	}
}

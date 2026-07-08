package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// SyncState records, per save filename (basename), the sha256 of the last
// snapshot the agent confirmed with the server — via a successful upload or a
// successful pull-write. It is the "last sync" column of the two-way decision
// matrix.
//
// It is owned by the single scan goroutine (the same one that runs uploads and
// pulls), so it needs no lock. A missing or corrupt file loads as empty state
// rather than an error, so a bad file degrades instead of crashing.
type SyncState struct {
	path  string
	files map[string]string
}

// SyncStatePath returns the sync_state.json path beside config.json.
func SyncStatePath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sync_state.json"), nil
}

// LoadSyncState reads the state file at path. A missing or unreadable/corrupt
// file yields empty (but usable) state.
func LoadSyncState(path string) *SyncState {
	s := &SyncState{path: path, files: make(map[string]string)}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return s
	}
	s.files = m
	return s
}

// Get returns the last-synced sha for a filename and whether it was present.
func (s *SyncState) Get(filename string) (string, bool) {
	sha, ok := s.files[filename]
	return sha, ok
}

// Set records a filename's sha and persists the state. A persistence failure is
// returned but never fatal; the in-memory map is always updated.
func (s *SyncState) Set(filename, sha string) error {
	s.files[filename] = sha
	return s.save()
}

func (s *SyncState) save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.files, "", "  ")
	if err != nil {
		return err
	}
	// Atomic-ish write in the config dir (not the destructive saves path): temp
	// file + rename so a crash mid-write can't corrupt the state.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".sync_state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.path)
}

package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// newDirWatcher builds a Watcher rooted at initial, with a hermetic config path
// (HOME/XDG under a tempdir) so applyDirChange's persistConfig writes there and
// never touches the real user config.
func newDirWatcher(t *testing.T, initial string) *Watcher {
	t.Helper()
	cfgHome := t.TempDir()
	t.Setenv("HOME", cfgHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cfgHome, "config"))
	return &Watcher{
		Config:   &Config{SavesDir: initial, URL: DefaultURL, PollInterval: 2},
		Pending:  map[string]FileStat{},
		Uploaded: map[string]string{},
		lastLine: map[string]string{},
	}
}

// TestApplyDirChangeSwapsAndResetsMaps: a valid new folder is adopted, the
// lock-free scan maps (keyed by paths under the old folder) are cleared, and the
// basename-keyed sync_state is deliberately left intact.
func TestApplyDirChangeSwapsAndResetsMaps(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	w := newDirWatcher(t, oldDir)

	statePath := filepath.Join(t.TempDir(), "sync_state.json")
	w.syncState = LoadSyncState(statePath)
	if err := w.syncState.Set("Hero.d2s", "cafebabe"); err != nil {
		t.Fatal(err)
	}

	// Seed the scan maps with entries keyed under the OLD folder.
	oldPath := filepath.Join(oldDir, "Hero.d2s")
	w.Pending[oldPath] = FileStat{Size: 10}
	w.Uploaded[oldPath] = "abc"
	w.lastLine[oldPath] = "Hero.d2s: created"

	if !w.applyDirChange(newDir) {
		t.Fatal("applyDirChange returned false for a valid new directory")
	}
	if got := w.SavesDir(); got != newDir {
		t.Fatalf("SavesDir = %q, want %q", got, newDir)
	}
	if len(w.Pending) != 0 || len(w.Uploaded) != 0 || len(w.lastLine) != 0 {
		t.Fatalf("scan maps not cleared: pending=%d uploaded=%d lastLine=%d",
			len(w.Pending), len(w.Uploaded), len(w.lastLine))
	}
	// sync_state (per basename) must survive the swap so the sha matrix still
	// decides correctly for the new folder.
	if sha, ok := w.syncState.Get("Hero.d2s"); !ok || sha != "cafebabe" {
		t.Fatalf("sync_state must be left intact across a folder change: (%q, %v)", sha, ok)
	}
}

// TestApplyDirChangePersistsConfig: the new folder is written to config.json.
func TestApplyDirChangePersistsConfig(t *testing.T) {
	w := newDirWatcher(t, t.TempDir())
	newDir := t.TempDir()

	if !w.applyDirChange(newDir) {
		t.Fatal("applyDirChange returned false for a valid new directory")
	}

	path, err := GetConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("config was not persisted: %v", err)
	}
	if cfg.SavesDir != newDir {
		t.Fatalf("persisted SavesDir = %q, want %q", cfg.SavesDir, newDir)
	}
}

// TestApplyDirChangeRejectsInvalid: a missing path, a plain file, an empty
// string, or the same folder are all no-ops — the current folder and the scan
// maps are untouched.
func TestApplyDirChangeRejectsInvalid(t *testing.T) {
	oldDir := t.TempDir()

	file := filepath.Join(oldDir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		dir  string
	}{
		{"missing path", filepath.Join(oldDir, "does-not-exist")},
		{"file not dir", file},
		{"empty", ""},
		{"whitespace", "   "},
		{"same folder", oldDir},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newDirWatcher(t, oldDir)
			seeded := filepath.Join(oldDir, "Hero.d2s")
			w.Pending[seeded] = FileStat{Size: 1}
			w.Uploaded[seeded] = "abc"

			if w.applyDirChange(tc.dir) {
				t.Fatalf("applyDirChange(%q) = true, want false (invalid/unchanged)", tc.dir)
			}
			if got := w.SavesDir(); got != oldDir {
				t.Fatalf("SavesDir changed to %q on a rejected request; want %q", got, oldDir)
			}
			if len(w.Pending) != 1 || len(w.Uploaded) != 1 {
				t.Fatalf("scan maps were reset on a rejected request: pending=%d uploaded=%d",
					len(w.Pending), len(w.Uploaded))
			}
		})
	}
}

// TestSavesDirAccessorRace runs the accessor concurrently with a live swap: the
// tray-side readers call SavesDir() while the scan-side path swaps the folder.
// Under -race this catches any unsynchronized access to Config.SavesDir.
func TestSavesDirAccessorRace(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	w := newDirWatcher(t, oldDir)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_ = w.SavesDir()
			}
		}()
	}

	w.applyDirChange(newDir) // models the scan goroutine performing the swap
	wg.Wait()

	if got := w.SavesDir(); got != newDir {
		t.Fatalf("final SavesDir = %q, want %q", got, newDir)
	}
}

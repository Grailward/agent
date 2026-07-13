package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEvaluateClassifiesManifest drives evaluate() end-to-end: a manifest from
// httptest, local files on disk, and a recorded sync state. It asserts which
// entries become pull candidates and with which decision, and that unsafe
// filenames are skipped.
func TestEvaluateClassifiesManifest(t *testing.T) {
	saves := t.TempDir()

	// Local files.
	inSync := syntheticSave(64, 0x01)
	fastFwdLocal := syntheticSave(64, 0x02)
	conflictLocal := syntheticSave(64, 0x03)
	writeFile(t, saves, "InSync.d2s", inSync)
	writeFile(t, saves, "FastFwd.d2s", fastFwdLocal)
	writeFile(t, saves, "Conflict.d2s", conflictLocal)
	// "New.d2i" is intentionally absent on disk.

	// Server shas: matches for InSync; advanced for FastFwd/Conflict/New.
	serverFastFwd := sha256hex(syntheticSave(80, 0x12))
	serverConflict := sha256hex(syntheticSave(80, 0x13))
	serverNew := sha256hex(syntheticSave(80, 0x14))

	manifest := fmt.Sprintf(`{
		"characters": [
			{"filename":"InSync.d2s","sha256":"%s","download_path":"/dl/InSync"},
			{"filename":"FastFwd.d2s","sha256":"%s","download_path":"/dl/FastFwd"},
			{"filename":"Conflict.d2s","sha256":"%s","download_path":"/dl/Conflict"},
			{"filename":"../evil.d2s","sha256":"deadbeef","download_path":"/dl/evil"}
		],
		"shared_stashes": [
			{"filename":"New.d2i","sha256":"%s","download_path":"/dl/New"}
		]
	}`, sha256hex(inSync), serverFastFwd, serverConflict, serverNew)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, manifest)
	}))
	defer srv.Close()

	statePath := filepath.Join(t.TempDir(), "sync_state.json")
	state := LoadSyncState(statePath)
	// FastFwd's local bytes are the last confirmed sync -> fast-forward when
	// the server moves ahead.
	state.Set("FastFwd.d2s", sha256hex(fastFwdLocal))

	w := &Watcher{
		Config:    &Config{SavesDir: saves, URL: srv.URL, SyncMode: SyncModeTwoWay},
		Client:    NewClient(srv.URL, "tok"),
		lastLine:  make(map[string]string),
		syncState: state,
	}

	cands, _, err := w.evaluate()
	if err != nil {
		t.Fatalf("evaluate failed: %v", err)
	}

	got := map[string]SyncDecision{}
	for _, c := range cands {
		got[c.filename] = c.decision
	}

	// InSync must not be a candidate; evil must be skipped.
	if _, ok := got["InSync.d2s"]; ok {
		t.Fatal("InSync.d2s should not be a pull candidate")
	}
	if _, ok := got["../evil.d2s"]; ok {
		t.Fatal("unsafe filename must be skipped")
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 candidates, got %d: %+v", len(got), got)
	}
	if got["FastFwd.d2s"] != DecisionFastForward {
		t.Fatalf("FastFwd decision = %d, want fast-forward", got["FastFwd.d2s"])
	}
	if got["New.d2i"] != DecisionFastForward { // new-from-server collapses to the FF write path
		t.Fatalf("New decision = %d, want new/fast-forward write path", got["New.d2i"])
	}
	if got["Conflict.d2s"] != DecisionConflict {
		t.Fatalf("Conflict decision = %d, want conflict", got["Conflict.d2s"])
	}

	// InSync should have been recorded into sync_state.
	if sha, ok := state.Get("InSync.d2s"); !ok || sha != sha256hex(inSync) {
		t.Fatalf("in-sync sha not recorded: (%q, %v)", sha, ok)
	}
}

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
		t.Fatal(err)
	}
}

// pullFixture wires a single-file two-way pull against an httptest server, with
// the interactive dialogs and the game-open guard injected. It records what the
// server saw so a test can assert whether a write actually happened.
type pullFixture struct {
	saves       string
	localBytes  []byte
	serverBytes []byte
	serverSHA   string
	dest        string
	uploads     int
	downloads   int
	lastUpload  map[string]any
	w           *Watcher
}

func newPullFixture(t *testing.T, uploadFails bool) *pullFixture {
	t.Helper()
	// Hermetic config/backups dir so backupsDir() never touches the real one.
	cfgHome := t.TempDir()
	t.Setenv("HOME", cfgHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cfgHome, "config"))

	f := &pullFixture{
		saves:       t.TempDir(),
		localBytes:  syntheticSave(64, 0xAA),
		serverBytes: syntheticSave(80, 0xBB),
	}
	f.serverSHA = sha256hex(f.serverBytes)
	f.dest = filepath.Join(f.saves, "Hero.d2s")
	writeFile(t, f.saves, "Hero.d2s", f.localBytes)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sync", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"characters":[{"filename":"Hero.d2s","sha256":"%s","download_path":"/dl/Hero"}],"shared_stashes":[]}`, f.serverSHA)
	})
	mux.HandleFunc("/api/v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		f.uploads++
		raw, _ := io.ReadAll(r.Body)
		f.lastUpload = map[string]any{}
		json.Unmarshal(raw, &f.lastUpload)
		if uploadFails {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"backup rejected"}`)
			return
		}
		io.WriteString(w, `{"status":"created"}`)
	})
	mux.HandleFunc("/dl/Hero", func(w http.ResponseWriter, r *http.Request) {
		f.downloads++
		w.Header().Set("X-Sha256", f.serverSHA)
		w.Write(f.serverBytes)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	statePath := filepath.Join(t.TempDir(), "sync_state.json")
	f.w = &Watcher{
		Config:      &Config{SavesDir: f.saves, URL: srv.URL, SyncMode: SyncModeTwoWay},
		Client:      NewClient(srv.URL, "tok"),
		Pending:     map[string]FileStat{},
		Uploaded:    map[string]string{},
		lastLine:    map[string]string{},
		syncState:   LoadSyncState(statePath),
		gameRunning: func(string, time.Time) (bool, string) { return false, "" },
	}
	return f
}

func (f *pullFixture) localUnchanged(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(f.dest)
	if err != nil {
		t.Fatalf("reading dest: %v", err)
	}
	if sha256hex(got) != sha256hex(f.localBytes) {
		t.Fatal("local file was modified but the write should have been skipped/aborted")
	}
}

func (f *pullFixture) assertServerWritten(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(f.dest)
	if err != nil {
		t.Fatalf("reading dest: %v", err)
	}
	if sha256hex(got) != f.serverSHA {
		t.Fatal("local file was not overwritten with the server bytes")
	}
	if sha, ok := f.w.syncState.Get("Hero.d2s"); !ok || sha != f.serverSHA {
		t.Fatalf("sync_state not updated to server sha: (%q, %v)", sha, ok)
	}
}

// TestRunPullFastForwardConfirmed: confirmation OK -> the write happens, the
// overwritten file is backed up, and sync_state advances.
func TestRunPullFastForwardConfirmed(t *testing.T) {
	f := newPullFixture(t, false)
	f.w.syncState.Set("Hero.d2s", sha256hex(f.localBytes)) // local == last sync -> fast-forward
	f.w.confirmPull = func(string) (bool, error) { return true, nil }

	f.w.runPull()

	f.assertServerWritten(t)
	if f.downloads != 1 {
		t.Fatalf("expected exactly 1 download, got %d", f.downloads)
	}
	bak, err := os.ReadFile(filepath.Join(f.w.backupsDir(), "Hero.d2s"))
	if err != nil {
		t.Fatalf("expected a backup of the overwritten file: %v", err)
	}
	if sha256hex(bak) != sha256hex(f.localBytes) {
		t.Fatal("backup does not match the original local bytes")
	}
}

// TestRunPullFastForwardSkipped: declining the batch confirmation writes nothing.
func TestRunPullFastForwardSkipped(t *testing.T) {
	f := newPullFixture(t, false)
	f.w.syncState.Set("Hero.d2s", sha256hex(f.localBytes))
	f.w.confirmPull = func(string) (bool, error) { return false, nil }

	f.w.runPull()

	f.localUnchanged(t)
	if f.downloads != 0 {
		t.Fatalf("a skipped pull must not download, got %d", f.downloads)
	}
}

// TestRunPullConflictUseServerOK: choosing the server backs local up (as a
// non-current snapshot) then writes; sync_state advances.
func TestRunPullConflictUseServerOK(t *testing.T) {
	f := newPullFixture(t, false)
	// No sync_state -> the local edit and server both diverge -> conflict.
	f.w.resolveConflict = func(string) (ConflictChoice, error) { return ConflictUseServer, nil }

	f.w.runPull()

	f.assertServerWritten(t)
	if f.downloads != 1 {
		t.Fatalf("expected exactly 1 download, got %d", f.downloads)
	}
	if f.uploads != 1 {
		t.Fatalf("expected exactly 1 backup upload, got %d", f.uploads)
	}
	if v, ok := f.lastUpload["set_current"].(bool); !ok || v {
		t.Fatalf("backup upload must send set_current=false, got %v", f.lastUpload["set_current"])
	}
}

// TestRunPullConflictUseServerUploadFails: if the local-backup upload fails,
// nothing is downloaded or written and sync_state is untouched.
func TestRunPullConflictUseServerUploadFails(t *testing.T) {
	f := newPullFixture(t, true) // POST /snapshots returns 500
	f.w.resolveConflict = func(string) (ConflictChoice, error) { return ConflictUseServer, nil }

	f.w.runPull()

	f.localUnchanged(t)
	if f.downloads != 0 {
		t.Fatalf("a failed backup upload must abort before download, got %d downloads", f.downloads)
	}
	if f.uploads != 1 {
		t.Fatalf("expected the one (failed) backup upload attempt, got %d", f.uploads)
	}
	if _, ok := f.w.syncState.Get("Hero.d2s"); ok {
		t.Fatal("sync_state must not advance on an aborted conflict")
	}
}

// TestRunPullConflictSkipped: skipping a conflict writes nothing.
func TestRunPullConflictSkipped(t *testing.T) {
	f := newPullFixture(t, false)
	f.w.resolveConflict = func(string) (ConflictChoice, error) { return ConflictSkip, nil }

	f.w.runPull()

	f.localUnchanged(t)
	if f.downloads != 0 || f.uploads != 0 {
		t.Fatalf("a skipped conflict must not touch the server: downloads=%d uploads=%d", f.downloads, f.uploads)
	}
}

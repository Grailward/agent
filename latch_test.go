package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// statusRec records the last state/message the watcher reported to the tray.
// The status callback fires synchronously on the caller's goroutine, but the
// mutex keeps it clean under -race when an httptest handler shares the watcher.
type statusRec struct {
	mu      sync.Mutex
	lastS   State
	lastMsg string
}

func (r *statusRec) fn(s State, m string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastS = s
	r.lastMsg = m
}

func (r *statusRec) last() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastS
}

func (r *statusRec) msg() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastMsg
}

// hasErr / errCount inspect the latch under w.mu (test-only, same package).
func (w *Watcher) hasErr(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.errs[key]
	return ok
}

func (w *Watcher) errCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.errs)
}

// TestErrorLatchOverridesHealthyReports is the core of problem 1: once an error
// is latched the tray stays red, and a routine "Watching for changes" report
// cannot paint over it. Only the file's own recovery clears it.
func TestErrorLatchOverridesHealthyReports(t *testing.T) {
	w := &Watcher{lastLine: map[string]string{}, errs: map[string]string{}}
	rec := &statusRec{}
	w.SetStatusFunc(rec.fn)

	w.setErr("A.d2s", "A.d2s: download failed")
	if rec.last() != StateError {
		t.Fatalf("setErr should render StateError, got %v", rec.last())
	}

	// The routine 15s scan reports a healthy syncing line — it must NOT clear the latch.
	w.status(StateSyncing, "Watching for changes")
	if rec.last() != StateError {
		t.Fatal("a routine healthy scan overrode the persistent error latch")
	}
	if rec.msg() != "A.d2s: download failed" {
		t.Fatalf("latch message was lost: %q", rec.msg())
	}

	// The equivalent success (this file pulled) is the only thing that clears it.
	w.clearErr("A.d2s")
	if rec.last() != StateSyncing {
		t.Fatalf("clearErr on the last key should render StateSyncing, got %v", rec.last())
	}
}

// TestErrorLatchNotMaskedByBatchSibling covers the within-batch masking: file A
// fails, sibling file B in the same batch succeeds — A's red must survive B's
// success, and only resolving A itself clears the tray.
func TestErrorLatchNotMaskedByBatchSibling(t *testing.T) {
	w := &Watcher{lastLine: map[string]string{}, errs: map[string]string{}}
	rec := &statusRec{}
	w.SetStatusFunc(rec.fn)

	w.setErr("A.d2s", "A.d2s: download failed")
	// Sibling B succeeds: clears its own (never-set) key, then reports its pull.
	w.clearErr("B.d2s")
	w.status(StateSyncing, "Pulled B.d2s")
	if rec.last() != StateError {
		t.Fatal("file B's success masked file A's persistent error")
	}

	w.clearErr("A.d2s")
	w.status(StateSyncing, "Pulled A.d2s")
	if rec.last() != StateSyncing {
		t.Fatalf("resolving A should return the tray to syncing, got %v", rec.last())
	}
}

// TestErrorLatchClearsOnlyOwnKey proves per-key isolation and the multi-error
// summary: clearing one key leaves the others latched.
func TestErrorLatchClearsOnlyOwnKey(t *testing.T) {
	w := &Watcher{lastLine: map[string]string{}, errs: map[string]string{}}
	rec := &statusRec{}
	w.SetStatusFunc(rec.fn)

	w.setErr("A.d2s", "A.d2s: download failed")
	w.setErr("B.d2s", "B.d2s: pull aborted")
	if rec.last() != StateError {
		t.Fatal("expected StateError with two latched errors")
	}
	if !strings.Contains(rec.msg(), "2 pending sync errors") {
		t.Fatalf("expected a two-error summary, got %q", rec.msg())
	}

	w.clearErr("A.d2s") // B must remain
	if rec.last() != StateError {
		t.Fatal("clearing A must not clear B")
	}
	if rec.msg() != "B.d2s: pull aborted" {
		t.Fatalf("expected B's own message after clearing A, got %q", rec.msg())
	}

	w.clearErr("B.d2s")
	if rec.last() != StateSyncing {
		t.Fatalf("clearing the last key should return to syncing, got %v", rec.last())
	}
}

// TestSetMapSyncOffClearsOnlySidecarLatches covers the "sidecar latch stuck
// forever" hole: turning map sync off stops applySidecars from ever clearing a
// sidecar latch, so SetMapSync(false) must drop them all — and only them.
func TestSetMapSyncOffClearsOnlySidecarLatches(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("HOME", cfgHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cfgHome, "config"))

	on := true
	w := &Watcher{
		Config:   &Config{SyncMapFiles: &on},
		lastLine: map[string]string{},
		errs:     map[string]string{},
	}
	rec := &statusRec{}
	w.SetStatusFunc(rec.fn)

	w.setErr(errKeyManifest, "Cannot reach server for pull")
	w.setErr("Hero.d2s", "Hero.d2s: download failed")
	w.setErr(sidecarErrKey("Mira.d2s"), "Mira.d2s: map sidecar download failed")
	w.setErr(sidecarErrKey("Zed.d2s"), "Zed.d2s: map sidecar download failed")

	w.SetMapSync(false)

	if w.hasErr(sidecarErrKey("Mira.d2s")) || w.hasErr(sidecarErrKey("Zed.d2s")) {
		t.Fatal("map sync off must drop every sidecar latch")
	}
	if !w.hasErr("Hero.d2s") {
		t.Fatal("map sync off must not touch a per-save latch")
	}
	if !w.hasErr(errKeyManifest) {
		t.Fatal("map sync off must not touch the manifest latch")
	}
	// The surviving save/manifest errors keep the tray red.
	if rec.last() != StateError {
		t.Fatalf("save and manifest errors remain, tray should stay red, got %v", rec.last())
	}
}

// TestApplyDirChangeClearsLatch covers the stale-folder hole: swapping the saves
// folder must wipe the whole latch so the old folder's errors can't repaint red
// under the "Saves folder changed" line.
func TestApplyDirChangeClearsLatch(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("HOME", cfgHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cfgHome, "config"))

	oldDir := t.TempDir()
	newDir := t.TempDir()
	w := &Watcher{
		Config:   &Config{SavesDir: oldDir},
		Pending:  map[string]FileStat{},
		Uploaded: map[string]string{},
		lastLine: map[string]string{},
		errs:     map[string]string{},
	}
	rec := &statusRec{}
	w.SetStatusFunc(rec.fn)

	w.setErr(errKeyManifest, "Cannot reach server for pull")
	w.setErr("Hero.d2s", "Hero.d2s: download failed")
	w.setErr(sidecarErrKey("Mira.d2s"), "Mira.d2s: map sidecar download failed")

	if !w.applyDirChange(newDir) {
		t.Fatal("expected the folder change to apply")
	}
	if n := w.errCount(); n != 0 {
		t.Fatalf("a saves-folder change must clear the whole latch, %d left", n)
	}
	if rec.last() != StateSyncing {
		t.Fatalf("after clearing the latch the tray should be syncing, got %v (%q)", rec.last(), rec.msg())
	}
}

// TestPruneFileErrsDropsOrphansKeepsKnown covers the orphan-key hole: when a save
// leaves the manifest, its per-file and sidecar latches are pruned on the next
// successful fetch; files still listed keep their latches, and the manifest latch
// is exempt (it has its own heal cycle).
func TestPruneFileErrsDropsOrphansKeepsKnown(t *testing.T) {
	w := &Watcher{lastLine: map[string]string{}, errs: map[string]string{}}
	rec := &statusRec{}
	w.SetStatusFunc(rec.fn)

	w.setErr(errKeyManifest, "Cannot reach server for pull")
	w.setErr("Hero.d2s", "Hero.d2s: download failed")   // still on the server
	w.setErr("Ghost.d2s", "Ghost.d2s: download failed") // dropped from the server
	w.setErr(sidecarErrKey("Hero.d2s"), "Hero.d2s: map sidecar download failed")
	w.setErr(sidecarErrKey("Ghost.d2s"), "Ghost.d2s: map sidecar download failed")

	known := map[string]bool{"Hero.d2s": true} // Ghost.d2s absent from the manifest

	w.pruneFileErrs(known)

	if w.hasErr("Ghost.d2s") || w.hasErr(sidecarErrKey("Ghost.d2s")) {
		t.Fatal("a file dropped from the manifest must have its latches pruned")
	}
	if !w.hasErr("Hero.d2s") || !w.hasErr(sidecarErrKey("Hero.d2s")) {
		t.Fatal("latches for files still in the manifest must survive the prune")
	}
	if !w.hasErr(errKeyManifest) {
		t.Fatal("the manifest latch is exempt from the file prune")
	}
}

// TestPeriodicSyncCheckLatchIntegration drives the manifest path end-to-end: a
// failed fetch latches red, a routine healthy scan cannot clear it, and only a
// recovered fetch does — even when that recovery reports nothing itself (the
// in-sync case, count 0, relies on the latch clear to re-render).
func TestPeriodicSyncCheckLatchIntegration(t *testing.T) {
	saves := t.TempDir()
	inSync := syntheticSave(64, 0x01)
	writeFile(t, saves, "InSync.d2s", inSync)
	inSyncSHA := sha256hex(inSync)

	var fail atomic.Bool
	fail.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sync", func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"boom"}`)
			return
		}
		fmt.Fprintf(w, `{"characters":[{"filename":"InSync.d2s","sha256":"%s","download_path":"/dl/x"}],"shared_stashes":[]}`, inSyncSHA)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	statePath := filepath.Join(t.TempDir(), "sync_state.json")
	w := &Watcher{
		Config:    &Config{SavesDir: saves, URL: srv.URL, SyncMode: SyncModeTwoWay},
		Client:    NewClient(srv.URL, "tok"),
		Pending:   map[string]FileStat{},
		Uploaded:  map[string]string{},
		lastLine:  map[string]string{},
		errs:      map[string]string{},
		syncState: LoadSyncState(statePath),
	}
	rec := &statusRec{}
	w.SetStatusFunc(rec.fn)

	// 1. Manifest fetch fails -> tray latches red.
	w.periodicSyncCheck()
	if rec.last() != StateError {
		t.Fatalf("expected StateError after manifest failure, got %v (%q)", rec.last(), rec.msg())
	}

	// 2. A routine healthy scan reports a syncing line but must not clear the latch.
	w.status(StateSyncing, "Watching for changes")
	if rec.last() != StateError {
		t.Fatal("a routine healthy scan cleared the persistent manifest error")
	}

	// 3. The fetch recovers -> latch clears, tray returns to syncing.
	fail.Store(false)
	w.periodicSyncCheck()
	if rec.last() != StateSyncing {
		t.Fatalf("expected StateSyncing after manifest recovery, got %v (%q)", rec.last(), rec.msg())
	}
}

// TestPulledLogLineIncludesShortSHA is problem 2: two pulls of the same file with
// different bytes must produce two distinct log lines (the short sha keeps them
// apart) instead of the second being swallowed by the logOnce dedup.
func TestPulledLogLineIncludesShortSHA(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("HOME", cfgHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cfgHome, "config"))

	saves := t.TempDir()
	writeFile(t, saves, "Hero.d2s", syntheticSave(64, 0xAA))

	v1 := syntheticSave(80, 0xB1)
	v2 := syntheticSave(96, 0xB2)
	sha1 := sha256hex(v1)
	sha2 := sha256hex(v2)

	mux := http.NewServeMux()
	mux.HandleFunc("/dl/v1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sha256", sha1)
		w.Write(v1)
	})
	mux.HandleFunc("/dl/v2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sha256", sha2)
		w.Write(v2)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	statePath := filepath.Join(t.TempDir(), "sync_state.json")
	w := &Watcher{
		Config:      &Config{SavesDir: saves, URL: srv.URL, SyncMode: SyncModeTwoWay},
		Client:      NewClient(srv.URL, "tok"),
		Pending:     map[string]FileStat{},
		Uploaded:    map[string]string{},
		lastLine:    map[string]string{},
		errs:        map[string]string{},
		syncState:   LoadSyncState(statePath),
		gameRunning: func(string, time.Time) (bool, string) { return false, "" },
	}

	var buf bytes.Buffer
	orig, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(orig); log.SetFlags(flags) }()

	before := time.Now()
	w.applyCandidate(candidate{entry: ManifestEntry{Filename: "Hero.d2s", SHA256: sha1, DownloadPath: "/dl/v1"}, decision: DecisionFastForward, filename: "Hero.d2s"}, before)
	w.applyCandidate(candidate{entry: ManifestEntry{Filename: "Hero.d2s", SHA256: sha2, DownloadPath: "/dl/v2"}, decision: DecisionFastForward, filename: "Hero.d2s"}, before)

	out := buf.String()
	l1 := "Hero.d2s: pulled from server (" + shortSHA(sha1) + ")"
	l2 := "Hero.d2s: pulled from server (" + shortSHA(sha2) + ")"
	if !strings.Contains(out, l1) {
		t.Fatalf("missing first applied-pull line %q in:\n%s", l1, out)
	}
	if !strings.Contains(out, l2) {
		t.Fatalf("missing second applied-pull line %q in:\n%s", l2, out)
	}
	if c := strings.Count(out, "Hero.d2s: pulled from server ("); c != 2 {
		t.Fatalf("expected exactly 2 applied-pull log lines, got %d:\n%s", c, out)
	}
}

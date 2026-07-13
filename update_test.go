package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// updateRec records the update-offer states the watcher reports to the tray.
type updateRec struct {
	mu        sync.Mutex
	seq       []updateUIState
	lastState updateUIState
	lastVer   string
}

func (r *updateRec) fn(s updateUIState, v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq = append(r.seq, s)
	r.lastState = s
	r.lastVer = v
}

func (r *updateRec) last() (updateUIState, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastState, r.lastVer
}

func (r *updateRec) sawAvailable() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.seq {
		if s == updateAvailable {
			return true
		}
	}
	return false
}

// manifestJSON builds a two-platform manifest so selectUpdateFile finds an entry
// on either host (darwin/windows).
func manifestJSON(version string) string {
	return fmt.Sprintf(`{
      "version": %q,
      "released_at": "2026-07-13T00:00:00Z",
      "files": [
        {"os":"darwin","arch":"universal","name":"grailward-agent-macos.zip","size":10,"sha256":"aa","url":"http://example/mac.zip"},
        {"os":"windows","arch":"amd64","name":"grailward-agent-windows-amd64.exe","size":10,"sha256":"bb","url":"http://example/win.exe"}
      ]
    }`, version)
}

func TestParseUpdateManifest(t *testing.T) {
	m, err := parseUpdateManifest([]byte(manifestJSON("v0.5.0")))
	if err != nil {
		t.Fatalf("valid manifest failed to parse: %v", err)
	}
	if m.Version != "v0.5.0" || len(m.Files) != 2 {
		t.Fatalf("parsed manifest wrong: %+v", m)
	}
	if _, err := parseUpdateManifest([]byte(`not json`)); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if _, err := parseUpdateManifest([]byte(`{"files":[{"os":"darwin"}]}`)); err == nil {
		t.Fatal("expected error for a manifest missing version")
	}
	if _, err := parseUpdateManifest([]byte(`{"version":"v1.0.0","files":[]}`)); err == nil {
		t.Fatal("expected error for a manifest with no files")
	}
}

func TestCompareSemverAndIsNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.5.0", "v0.4.1", 1, true},
		{"v0.4.1", "v0.5.0", -1, true},
		{"v0.5.0", "v0.5.0", 0, true},
		{"v1.0.0", "v0.9.9", 1, true},
		{"v0.5.1", "v0.5.0", 1, true},
		{"v0.5.0", "v0.5.0-rc1", 1, true}, // a full release outranks its prerelease
		{"v0.5.0-rc2", "v0.5.0-rc1", 1, true},
		// A numeric 4th component is a real, higher version — never a downgrade.
		{"v0.5.0.1", "v0.5.0", 1, true},
		{"v0.5.0", "v0.5.0.1", -1, true},
		{"v0.5.0.2", "v0.5.0.1", 1, true},
		// Prerelease tails compare their trailing number numerically, so rc10 > rc2.
		{"v0.5.0-rc2", "v0.5.0-rc10", -1, true},
		{"v0.5.0-rc10", "v0.5.0-rc2", 1, true},
		{"v0.5.0-rc10", "v0.5.0-rc10", 0, true},
		{"v0.5.0", "dev", 0, false}, // unparseable current => not comparable
		{"garbage", "v0.5.0", 0, false},
	}
	for _, c := range cases {
		got, ok := compareSemver(c.a, c.b)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("compareSemver(%q,%q) = (%d,%v), want (%d,%v)", c.a, c.b, got, ok, c.want, c.ok)
		}
	}

	if !isNewer("v0.5.0", "v0.4.1") {
		t.Fatal("v0.5.0 should be newer than v0.4.1")
	}
	if isNewer("v0.4.1", "v0.5.0") {
		t.Fatal("v0.4.1 is not newer than v0.5.0")
	}
	if isNewer("v0.5.0", "v0.5.0") {
		t.Fatal("equal versions are not newer")
	}
	if isNewer("v0.5.0", "dev") {
		t.Fatal("a dev build must never be considered updatable")
	}
	// A build with a 4th component must not be offered a downgrade to the bare release.
	if isNewer("v0.5.0", "v0.5.0.1") {
		t.Fatal("v0.5.0 must not be offered to a v0.5.0.1 build (downgrade)")
	}
	if !isNewer("v0.5.0.1", "v0.5.0") {
		t.Fatal("v0.5.0.1 should be newer than v0.5.0")
	}
}

func TestSelectUpdateFile(t *testing.T) {
	m, _ := parseUpdateManifest([]byte(manifestJSON("v0.5.0")))

	if f, ok := selectUpdateFile(m, "darwin", "arm64"); !ok || f.Name != "grailward-agent-macos.zip" {
		t.Fatalf("darwin/arm64 should pick the universal zip, got %+v ok=%v", f, ok)
	}
	if f, ok := selectUpdateFile(m, "darwin", "amd64"); !ok || f.OS != "darwin" {
		t.Fatalf("darwin/amd64 should pick the universal zip, got %+v ok=%v", f, ok)
	}
	if f, ok := selectUpdateFile(m, "windows", "amd64"); !ok || f.Name != "grailward-agent-windows-amd64.exe" {
		t.Fatalf("windows/amd64 should pick the exe, got %+v ok=%v", f, ok)
	}
	if _, ok := selectUpdateFile(m, "linux", "amd64"); ok {
		t.Fatal("no artifact for linux — should not match")
	}
	if _, ok := selectUpdateFile(m, "windows", "arm64"); ok {
		t.Fatal("no windows/arm64 artifact — should not match")
	}
}

func TestCheckSHA256(t *testing.T) {
	data := []byte("payload")
	if err := checkSHA256(data, sha256hex(data)); err != nil {
		t.Fatalf("matching sha should verify: %v", err)
	}
	if err := checkSHA256(data, sha256hex([]byte("other"))); err == nil {
		t.Fatal("mismatched sha must error")
	}
}

func TestMacUpdateTarget(t *testing.T) {
	bundle, ok := macUpdateTarget("/Applications/Grailward Agent.app/Contents/MacOS/grailward-agent")
	if !ok || bundle != "/Applications/Grailward Agent.app" {
		t.Fatalf("bundle detection = (%q,%v), want the .app path", bundle, ok)
	}
	if _, ok := macUpdateTarget("/usr/local/bin/grailward-agent"); ok {
		t.Fatal("a raw binary path must not be detected as a bundle")
	}
	if _, ok := macUpdateTarget("/some/Weird.app/MacOS/grailward-agent"); ok {
		t.Fatal("a path without .../Contents/MacOS must not match")
	}
}

// TestCheckForUpdateOffersNewer: a newer manifest surfaces an offer and never
// touches the sync-error latch.
func TestCheckForUpdateOffersNewer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, manifestJSON("v0.5.0"))
	}))
	defer srv.Close()

	w := &Watcher{lastLine: map[string]string{}, errs: map[string]string{}, updateURL: srv.URL, currentVersion: "v0.4.1"}
	rec := &updateRec{}
	w.SetUpdateFunc(rec.fn)

	w.checkForUpdate()

	if state, ver := rec.last(); state != updateAvailable || ver != "v0.5.0" {
		t.Fatalf("expected an available offer for v0.5.0, got (%v, %q)", state, ver)
	}
	if w.errCount() != 0 {
		t.Fatal("an update check must never populate the sync-error latch")
	}
}

// TestCheckForUpdateSameVersionNoOffer: the current version is up to date.
func TestCheckForUpdateSameVersionNoOffer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, manifestJSON("v0.5.0"))
	}))
	defer srv.Close()

	w := &Watcher{lastLine: map[string]string{}, errs: map[string]string{}, updateURL: srv.URL, currentVersion: "v0.5.0"}
	rec := &updateRec{}
	w.SetUpdateFunc(rec.fn)

	w.checkForUpdate()

	if rec.sawAvailable() {
		t.Fatal("the same version must not produce an offer")
	}
}

// TestCheckForUpdateOlderNoOffer: a manifest older than the running build is ignored.
func TestCheckForUpdateOlderNoOffer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, manifestJSON("v0.4.0"))
	}))
	defer srv.Close()

	w := &Watcher{lastLine: map[string]string{}, errs: map[string]string{}, updateURL: srv.URL, currentVersion: "v0.5.0"}
	rec := &updateRec{}
	w.SetUpdateFunc(rec.fn)

	w.checkForUpdate()

	if rec.sawAvailable() {
		t.Fatal("an older manifest must not produce an offer")
	}
}

// TestCheckForUpdateBrokenManifestSilent: a broken manifest is silent — no offer,
// no red latch, just a log line.
func TestCheckForUpdateBrokenManifestSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `nope`)
	}))
	defer srv.Close()

	w := &Watcher{lastLine: map[string]string{}, errs: map[string]string{}, updateURL: srv.URL, currentVersion: "v0.4.1"}
	rec := &updateRec{}
	w.SetUpdateFunc(rec.fn)

	w.checkForUpdate()

	if rec.sawAvailable() {
		t.Fatal("a broken manifest must not produce an offer")
	}
	if w.errCount() != 0 {
		t.Fatal("a broken update manifest must never populate the sync-error latch")
	}
}

// TestCheckForUpdateDevBuildSkips: a non-semver build never even fetches the
// manifest (it can't be older than a release, so there's nothing to offer).
func TestCheckForUpdateDevBuildSkips(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		fmt.Fprint(w, manifestJSON("v9.9.9"))
	}))
	defer srv.Close()

	w := &Watcher{lastLine: map[string]string{}, errs: map[string]string{}, updateURL: srv.URL, currentVersion: "dev"}
	rec := &updateRec{}
	w.SetUpdateFunc(rec.fn)

	w.checkForUpdate()

	if hit {
		t.Fatal("a dev build must not fetch the update manifest")
	}
	if rec.sawAvailable() {
		t.Fatal("a dev build must never receive an update offer")
	}
}

// updateTestWatcher wires a watcher with all interactive/OS seams stubbed for the
// runUpdate tests, plus an httptest server serving artifact as the download.
func updateTestWatcher(t *testing.T, artifact []byte, applied *[]byte, appliedFile **UpdateFile) (*Watcher, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(artifact)
	}))
	w := &Watcher{
		Config:        &Config{SavesDir: t.TempDir()},
		lastLine:      map[string]string{},
		errs:          map[string]string{},
		confirmUpdate: func(string) (bool, error) { return true, nil },
		gameRunning:   func(string, time.Time) (bool, string) { return false, "" },
		showMessage:   func(string) error { return nil },
		applyUpdate: func(exe string, f *UpdateFile, data []byte) error {
			if applied != nil {
				*applied = append([]byte(nil), data...)
			}
			if appliedFile != nil {
				*appliedFile = f
			}
			return nil
		},
	}
	return w, srv
}

// TestRunUpdateHappyPath: confirm → guard clear → download → sha ok → apply gets
// the verified bytes.
func TestRunUpdateHappyPath(t *testing.T) {
	artifact := []byte("brand-new-build-bytes")
	var applied []byte
	var appliedFile *UpdateFile
	w, srv := updateTestWatcher(t, artifact, &applied, &appliedFile)
	defer srv.Close()

	file := &UpdateFile{OS: "darwin", Name: "grailward-agent-macos.zip", SHA256: sha256hex(artifact), URL: srv.URL + "/dl"}
	w.setUpdateOffer("v0.5.0", file)

	w.runUpdate()

	if string(applied) != string(artifact) {
		t.Fatalf("apply did not receive the downloaded bytes: got %q", applied)
	}
	if appliedFile != file {
		t.Fatal("apply did not receive the offered file")
	}
}

// TestRunUpdateShaMismatchMarksFailed: a checksum mismatch aborts before apply and
// flips the menu item to "failed".
func TestRunUpdateShaMismatchMarksFailed(t *testing.T) {
	artifact := []byte("served-bytes")
	applyCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(artifact)
	}))
	defer srv.Close()

	w := &Watcher{
		Config:        &Config{SavesDir: t.TempDir()},
		lastLine:      map[string]string{},
		errs:          map[string]string{},
		confirmUpdate: func(string) (bool, error) { return true, nil },
		gameRunning:   func(string, time.Time) (bool, string) { return false, "" },
		applyUpdate:   func(string, *UpdateFile, []byte) error { applyCalled = true; return nil },
	}
	rec := &updateRec{}
	w.SetUpdateFunc(rec.fn)

	// The manifest advertises a sha that the served bytes will NOT match.
	file := &UpdateFile{OS: "darwin", SHA256: sha256hex([]byte("different")), URL: srv.URL + "/dl"}
	w.setUpdateOffer("v0.5.0", file)

	w.runUpdate()

	if applyCalled {
		t.Fatal("apply must not run when the checksum does not match")
	}
	if state, _ := rec.last(); state != updateFailed {
		t.Fatalf("a failed apply should mark the item failed, got %v", state)
	}
	if w.errCount() != 0 {
		t.Fatal("an update failure must never populate the sync-error latch")
	}
}

// TestRunUpdateGameOpenDefers: a game session refuses the apply and keeps the offer
// for a retry.
func TestRunUpdateGameOpenDefers(t *testing.T) {
	applyCalled := false
	w := &Watcher{
		Config:        &Config{SavesDir: t.TempDir()},
		lastLine:      map[string]string{},
		errs:          map[string]string{},
		confirmUpdate: func(string) (bool, error) { return true, nil },
		gameRunning:   func(string, time.Time) (bool, string) { return true, "a game session looks active" },
		showMessage:   func(string) error { return nil },
		applyUpdate:   func(string, *UpdateFile, []byte) error { applyCalled = true; return nil },
	}
	w.setUpdateOffer("v0.5.0", &UpdateFile{OS: "darwin", URL: "http://unused"})

	w.runUpdate()

	if applyCalled {
		t.Fatal("apply must not run while a game session looks active")
	}
	w.mu.Lock()
	state := w.updateState
	w.mu.Unlock()
	if state != updateAvailable {
		t.Fatalf("the offer must be retained after a game-open deferral, got %v", state)
	}
}

// TestRunUpdateGameOpensDuringDownload: the game is closed at the first guard but
// opens during the download; the pre-swap recheck must catch it (TOCTOU), defer,
// and apply nothing while keeping the offer.
func TestRunUpdateGameOpensDuringDownload(t *testing.T) {
	artifact := []byte("bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(artifact)
	}))
	defer srv.Close()

	calls := 0
	applyCalled := false
	w := &Watcher{
		Config:        &Config{SavesDir: t.TempDir()},
		lastLine:      map[string]string{},
		errs:          map[string]string{},
		confirmUpdate: func(string) (bool, error) { return true, nil },
		gameRunning: func(string, time.Time) (bool, string) {
			calls++
			return calls > 1, "a game session started during the download" // clear first, open second
		},
		showMessage: func(string) error { return nil },
		applyUpdate: func(string, *UpdateFile, []byte) error { applyCalled = true; return nil },
	}
	w.setUpdateOffer("v0.5.0", &UpdateFile{OS: "darwin", SHA256: sha256hex(artifact), URL: srv.URL + "/dl"})

	w.runUpdate()

	if applyCalled {
		t.Fatal("a game opening during the download must abort the swap")
	}
	if calls < 2 {
		t.Fatalf("expected a second (pre-swap) game-guard check, got %d", calls)
	}
	w.mu.Lock()
	state := w.updateState
	w.mu.Unlock()
	if state != updateAvailable {
		t.Fatalf("the offer must be retained after a TOCTOU deferral, got %v", state)
	}
}

// TestRunUpdateUserDeclines: dismissing the confirmation leaves everything as-is.
func TestRunUpdateUserDeclines(t *testing.T) {
	applyCalled := false
	w := &Watcher{
		Config:        &Config{SavesDir: t.TempDir()},
		lastLine:      map[string]string{},
		errs:          map[string]string{},
		confirmUpdate: func(string) (bool, error) { return false, nil },
		applyUpdate:   func(string, *UpdateFile, []byte) error { applyCalled = true; return nil },
	}
	w.setUpdateOffer("v0.5.0", &UpdateFile{OS: "darwin", URL: "http://unused"})

	w.runUpdate()

	if applyCalled {
		t.Fatal("apply must not run when the user declines")
	}
}

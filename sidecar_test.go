package main

import (
	"encoding/base64"
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

// newSidecarWatcher builds a two-way Watcher rooted at a temp saves folder with a
// hermetic config/backups dir, map sync ON, and the game-open guard defaulting to
// "not running". Individual tests tweak the config/seams as needed.
func newSidecarWatcher(t *testing.T, saves, url string) *Watcher {
	t.Helper()
	cfgHome := t.TempDir()
	t.Setenv("HOME", cfgHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cfgHome, "config"))
	statePath := filepath.Join(t.TempDir(), "sync_state.json")
	return &Watcher{
		Config:          &Config{SavesDir: saves, URL: url, SyncMode: SyncModeTwoWay},
		Client:          NewClient(url, "tok"),
		Pending:         map[string]FileStat{},
		Uploaded:        map[string]string{},
		lastLine:        map[string]string{},
		pendingSidecars: map[string]string{},
		Machine:         "TestMachine",
		syncState:       LoadSyncState(statePath),
		gameRunning:     func(string, time.Time) (bool, string) { return false, "" },
	}
}

// --- Push side -------------------------------------------------------------

// TestPushSidecarsCollectAndSend: after a .d2s upload, only the sidecars sharing
// its base (not .key, not another character's) are sent in one batch, and each
// sent file's sha is recorded in sync_state.
func TestPushSidecarsCollectAndSend(t *testing.T) {
	saves := t.TempDir()
	d2s := syntheticSave(64, 0x01)
	mapBytes := []byte("\x00synthetic-map-bytes")
	ma0Bytes := []byte("\x00synthetic-ma0-bytes")
	writeFile(t, saves, "Hero.d2s", d2s)
	writeFile(t, saves, "Hero.map", mapBytes)
	writeFile(t, saves, "Hero.ma0", ma0Bytes)
	writeFile(t, saves, "Hero.key", []byte("out-of-scope")) // excluded
	writeFile(t, saves, "Other.map", []byte("other char"))  // different base, excluded

	var gotPath, gotMachine string
	var gotReq SidecarUploadRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"created","character":{"name":"Hero","level":5}}`)
	})
	mux.HandleFunc("/api/v1/characters/Hero/sidecars", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotReq)
		gotMachine = gotReq.SourceMachine
		io.WriteString(w, `{"status":"ok","files":[{"filename":"Hero.map","status":"created"},{"filename":"Hero.ma0","status":"created"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := newSidecarWatcher(t, saves, srv.URL)
	w.upload("Hero.d2s", filepath.Join(saves, "Hero.d2s"), d2s, sha256hex(d2s))

	if gotPath != "/api/v1/characters/Hero/sidecars" {
		t.Fatalf("PUT path = %q, want /api/v1/characters/Hero/sidecars", gotPath)
	}
	if gotMachine != "TestMachine" {
		t.Fatalf("source_machine = %q, want TestMachine", gotMachine)
	}

	got := map[string]string{}
	for _, f := range gotReq.Files {
		dec, err := base64.StdEncoding.DecodeString(f.RawBase64)
		if err != nil {
			t.Fatalf("sidecar %s base64 invalid: %v", f.Filename, err)
		}
		if sha256hex(dec) != f.SHA256 {
			t.Fatalf("sidecar %s payload sha mismatches its declared sha", f.Filename)
		}
		got[f.Filename] = f.SHA256
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 sidecars sent, got %d: %+v", len(got), got)
	}
	if got["Hero.map"] != sha256hex(mapBytes) || got["Hero.ma0"] != sha256hex(ma0Bytes) {
		t.Fatalf("sidecar shas wrong: %+v", got)
	}
	if sha, ok := w.syncState.Get("Hero.map"); !ok || sha != sha256hex(mapBytes) {
		t.Fatalf("Hero.map not recorded in sync_state: (%q, %v)", sha, ok)
	}
	if sha, ok := w.syncState.Get("Hero.ma0"); !ok || sha != sha256hex(ma0Bytes) {
		t.Fatalf("Hero.ma0 not recorded in sync_state: (%q, %v)", sha, ok)
	}
	if len(w.pendingSidecars) != 0 {
		t.Fatalf("no retry expected on success, got %+v", w.pendingSidecars)
	}
}

// TestPushSidecarsSkipsAlreadySynced: a sidecar whose sha already sits in
// sync_state is not re-sent; only the changed one goes up.
func TestPushSidecarsSkipsAlreadySynced(t *testing.T) {
	saves := t.TempDir()
	d2s := syntheticSave(64, 0x02)
	mapBytes := []byte("\x00map-unchanged")
	ma0Bytes := []byte("\x00ma0-changed")
	writeFile(t, saves, "Hero.d2s", d2s)
	writeFile(t, saves, "Hero.map", mapBytes)
	writeFile(t, saves, "Hero.ma0", ma0Bytes)

	var gotReq SidecarUploadRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"created","character":{"name":"Hero","level":9}}`)
	})
	mux.HandleFunc("/api/v1/characters/Hero/sidecars", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotReq)
		io.WriteString(w, `{"status":"ok","files":[{"filename":"Hero.ma0","status":"created"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := newSidecarWatcher(t, saves, srv.URL)
	w.syncState.Set("Hero.map", sha256hex(mapBytes)) // already synced

	w.upload("Hero.d2s", filepath.Join(saves, "Hero.d2s"), d2s, sha256hex(d2s))

	if len(gotReq.Files) != 1 || gotReq.Files[0].Filename != "Hero.ma0" {
		t.Fatalf("expected only Hero.ma0 to be sent, got %+v", gotReq.Files)
	}
}

// TestPushSidecarsRetryAfterFailure: a failed batch queues the character; the
// next scan retries it (and succeeds) without ever failing the .d2s flow.
func TestPushSidecarsRetryAfterFailure(t *testing.T) {
	saves := t.TempDir()
	d2s := syntheticSave(64, 0x03)
	mapBytes := []byte("\x00map-for-retry")
	writeFile(t, saves, "Hero.d2s", d2s)
	writeFile(t, saves, "Hero.map", mapBytes)

	fail := true
	var sidecarHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"created","character":{"name":"Hero","level":1}}`)
	})
	mux.HandleFunc("/api/v1/characters/Hero/sidecars", func(w http.ResponseWriter, r *http.Request) {
		sidecarHits++
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"sidecar store unavailable"}`)
			return
		}
		io.WriteString(w, `{"status":"ok","files":[{"filename":"Hero.map","status":"created"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := newSidecarWatcher(t, saves, srv.URL)

	// First upload: sidecar push fails, character queued for retry.
	w.upload("Hero.d2s", filepath.Join(saves, "Hero.d2s"), d2s, sha256hex(d2s))
	if name, ok := w.pendingSidecars["Hero"]; !ok || name != "Hero" {
		t.Fatalf("failed push must queue a retry, pending=%+v", w.pendingSidecars)
	}
	if _, ok := w.syncState.Get("Hero.map"); ok {
		t.Fatal("a failed push must not record the sha in sync_state")
	}

	// Recover the server; a scan retries the queued sidecar batch at its top.
	fail = false
	w.Scan()

	if len(w.pendingSidecars) != 0 {
		t.Fatalf("retry should have cleared the queue, pending=%+v", w.pendingSidecars)
	}
	if sha, ok := w.syncState.Get("Hero.map"); !ok || sha != sha256hex(mapBytes) {
		t.Fatalf("retry did not record Hero.map: (%q, %v)", sha, ok)
	}
	if sidecarHits != 2 {
		t.Fatalf("expected one failed + one successful sidecar attempt, got %d", sidecarHits)
	}
}

// TestPushSidecarsToggleOff: with map sync off nothing is collected or sent.
func TestPushSidecarsToggleOff(t *testing.T) {
	saves := t.TempDir()
	d2s := syntheticSave(64, 0x04)
	writeFile(t, saves, "Hero.d2s", d2s)
	writeFile(t, saves, "Hero.map", []byte("\x00map"))

	var sidecarHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"created","character":{"name":"Hero","level":1}}`)
	})
	mux.HandleFunc("/api/v1/characters/Hero/sidecars", func(w http.ResponseWriter, r *http.Request) {
		sidecarHits++
		io.WriteString(w, `{"status":"ok","files":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := newSidecarWatcher(t, saves, srv.URL)
	off := false
	w.Config.SyncMapFiles = &off

	w.upload("Hero.d2s", filepath.Join(saves, "Hero.d2s"), d2s, sha256hex(d2s))

	if sidecarHits != 0 {
		t.Fatalf("map sync off must not push sidecars, got %d hits", sidecarHits)
	}
	if len(w.pendingSidecars) != 0 {
		t.Fatalf("map sync off must not queue retries, got %+v", w.pendingSidecars)
	}
}

// --- Pull side -------------------------------------------------------------

// sidecarManifest renders a one-character manifest whose entry advertises two
// sidecars and a batch download path.
func sidecarManifest(d2sSHA, mapSHA string, mapSize int, ma0SHA string, ma0Size int) string {
	return fmt.Sprintf(`{"characters":[{"filename":"Hero.d2s","sha256":"%s","download_path":"/dl/Hero",`+
		`"sidecars":[{"filename":"Hero.map","sha256":"%s","size":%d},{"filename":"Hero.ma0","sha256":"%s","size":%d}],`+
		`"sidecars_download_path":"/api/v1/characters/Hero/sidecars"}],"shared_stashes":[]}`,
		d2sSHA, mapSHA, mapSize, ma0SHA, ma0Size)
}

// TestPullCoupledSidecars: a fast-forward .d2s pull also downloads and writes the
// character's sidecars — backing up the one it overwrites, verifying shas, and
// updating the watcher maps + sync_state.
func TestPullCoupledSidecars(t *testing.T) {
	saves := t.TempDir()
	localD2s := syntheticSave(64, 0xAA)
	serverD2s := syntheticSave(80, 0xBB)
	writeFile(t, saves, "Hero.d2s", localD2s)
	oldMap := []byte("\x00old-map-local")
	writeFile(t, saves, "Hero.map", oldMap) // exists -> must be backed up
	// Hero.ma0 absent locally -> new from server.

	serverD2sSHA := sha256hex(serverD2s)
	serverMap := []byte("\x00new-map-from-server")
	serverMa0 := []byte("\x00new-ma0-from-server")
	serverMapSHA := sha256hex(serverMap)
	serverMa0SHA := sha256hex(serverMa0)

	var sidecarHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sync", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sidecarManifest(serverD2sSHA, serverMapSHA, len(serverMap), serverMa0SHA, len(serverMa0)))
	})
	mux.HandleFunc("/dl/Hero", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sha256", serverD2sSHA)
		w.Write(serverD2s)
	})
	mux.HandleFunc("/api/v1/characters/Hero/sidecars", func(w http.ResponseWriter, r *http.Request) {
		sidecarHits++
		fmt.Fprintf(w, `{"files":[{"filename":"Hero.map","sha256":"%s","raw_base64":"%s"},{"filename":"Hero.ma0","sha256":"%s","raw_base64":"%s"}]}`,
			serverMapSHA, base64.StdEncoding.EncodeToString(serverMap),
			serverMa0SHA, base64.StdEncoding.EncodeToString(serverMa0))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := newSidecarWatcher(t, saves, srv.URL)
	w.syncState.Set("Hero.d2s", sha256hex(localD2s)) // local == last sync -> fast-forward
	w.confirmPull = func(string) (bool, error) { return true, nil }

	w.runPull()

	if got, _ := os.ReadFile(filepath.Join(saves, "Hero.d2s")); sha256hex(got) != serverD2sSHA {
		t.Fatal(".d2s was not pulled")
	}
	if got, _ := os.ReadFile(filepath.Join(saves, "Hero.map")); sha256hex(got) != serverMapSHA {
		t.Fatal("Hero.map was not overwritten with server bytes")
	}
	if got, _ := os.ReadFile(filepath.Join(saves, "Hero.ma0")); sha256hex(got) != serverMa0SHA {
		t.Fatal("Hero.ma0 (new from server) was not written")
	}
	bak, err := os.ReadFile(filepath.Join(w.backupsDir(), "Hero.map"))
	if err != nil || sha256hex(bak) != sha256hex(oldMap) {
		t.Fatalf("expected a backup of the overwritten Hero.map: err=%v", err)
	}
	if sha, ok := w.syncState.Get("Hero.map"); !ok || sha != serverMapSHA {
		t.Fatalf("Hero.map sha not recorded: (%q, %v)", sha, ok)
	}
	if sha, ok := w.syncState.Get("Hero.ma0"); !ok || sha != serverMa0SHA {
		t.Fatalf("Hero.ma0 sha not recorded: (%q, %v)", sha, ok)
	}
	if w.Uploaded[filepath.Join(saves, "Hero.map")] != serverMapSHA {
		t.Fatal("watcher Uploaded map not updated for Hero.map (would risk a re-upload)")
	}
	if sidecarHits != 1 {
		t.Fatalf("expected exactly one sidecar batch download, got %d", sidecarHits)
	}
}

// TestPullSidecarShaMismatchAborts: a batch payload whose bytes do not match the
// declared/manifest sha is rejected without touching the local sidecar.
func TestPullSidecarShaMismatchAborts(t *testing.T) {
	saves := t.TempDir()
	localD2s := syntheticSave(64, 0xAA)
	serverD2s := syntheticSave(80, 0xBB)
	writeFile(t, saves, "Hero.d2s", localD2s)
	oldMap := []byte("\x00old-map-must-survive")
	writeFile(t, saves, "Hero.map", oldMap)

	serverD2sSHA := sha256hex(serverD2s)
	serverMap := []byte("\x00intended-map")
	serverMapSHA := sha256hex(serverMap)
	corrupt := []byte("\x00corrupted-in-transit") // does not hash to serverMapSHA

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sync", func(w http.ResponseWriter, r *http.Request) {
		// Only Hero.map in the manifest for this focused check.
		fmt.Fprintf(w, `{"characters":[{"filename":"Hero.d2s","sha256":"%s","download_path":"/dl/Hero",`+
			`"sidecars":[{"filename":"Hero.map","sha256":"%s","size":%d}],`+
			`"sidecars_download_path":"/api/v1/characters/Hero/sidecars"}],"shared_stashes":[]}`,
			serverD2sSHA, serverMapSHA, len(serverMap))
	})
	mux.HandleFunc("/dl/Hero", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sha256", serverD2sSHA)
		w.Write(serverD2s)
	})
	mux.HandleFunc("/api/v1/characters/Hero/sidecars", func(w http.ResponseWriter, r *http.Request) {
		// Declared sha is the manifest's, but the payload is corrupt.
		fmt.Fprintf(w, `{"files":[{"filename":"Hero.map","sha256":"%s","raw_base64":"%s"}]}`,
			serverMapSHA, base64.StdEncoding.EncodeToString(corrupt))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := newSidecarWatcher(t, saves, srv.URL)
	w.syncState.Set("Hero.d2s", sha256hex(localD2s))
	w.confirmPull = func(string) (bool, error) { return true, nil }

	w.runPull()

	// The .d2s still pulled; the corrupt sidecar left the local file untouched.
	if got, _ := os.ReadFile(filepath.Join(saves, "Hero.map")); sha256hex(got) != sha256hex(oldMap) {
		t.Fatal("Hero.map was modified despite a sha mismatch")
	}
	if _, ok := w.syncState.Get("Hero.map"); ok {
		t.Fatal("sync_state must not advance for an aborted sidecar")
	}
}

// TestSidecarOnlyPullIsSilentAndUncounted: when the .d2s is in sync but a sidecar
// differs, the carry is applied without any dialog and does not inflate the
// newer-save count.
func TestSidecarOnlyPullIsSilentAndUncounted(t *testing.T) {
	saves := t.TempDir()
	d2s := syntheticSave(64, 0xCC)
	writeFile(t, saves, "Hero.d2s", d2s)
	oldMap := []byte("\x00stale-local-map")
	writeFile(t, saves, "Hero.map", oldMap)

	d2sSHA := sha256hex(d2s) // local == server -> in sync
	serverMap := []byte("\x00fresh-server-map")
	serverMapSHA := sha256hex(serverMap)

	oneSidecarManifest := fmt.Sprintf(`{"characters":[{"filename":"Hero.d2s","sha256":"%s","download_path":"/dl/Hero",`+
		`"sidecars":[{"filename":"Hero.map","sha256":"%s","size":%d}],`+
		`"sidecars_download_path":"/api/v1/characters/Hero/sidecars"}],"shared_stashes":[]}`,
		d2sSHA, serverMapSHA, len(serverMap))

	var d2sDownloads int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sync", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, oneSidecarManifest)
	})
	mux.HandleFunc("/dl/Hero", func(w http.ResponseWriter, r *http.Request) {
		d2sDownloads++
		w.Header().Set("X-Sha256", d2sSHA)
		w.Write(d2s)
	})
	mux.HandleFunc("/api/v1/characters/Hero/sidecars", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"files":[{"filename":"Hero.map","sha256":"%s","raw_base64":"%s"}]}`,
			serverMapSHA, base64.StdEncoding.EncodeToString(serverMap))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := newSidecarWatcher(t, saves, srv.URL)
	w.syncState.Set("Hero.d2s", d2sSHA)
	w.confirmPull = func(string) (bool, error) {
		t.Fatal("sidecar-only pull must not prompt for confirmation")
		return false, nil
	}
	w.resolveConflict = func(string) (ConflictChoice, error) {
		t.Fatal("sidecar-only pull must not open a conflict dialog")
		return ConflictSkip, nil
	}

	// The sidecar-only carry must not count as a newer save.
	cands, _, err := w.evaluate()
	if err != nil {
		t.Fatalf("evaluate failed: %v", err)
	}
	if newerCount(cands) != 0 {
		t.Fatalf("sidecar-only must not count toward newer saves, got %d", newerCount(cands))
	}
	if len(cands) != 1 || cands[0].decision != DecisionSidecarOnly {
		t.Fatalf("expected one sidecar-only candidate, got %+v", cands)
	}

	w.runPull()

	if got, _ := os.ReadFile(filepath.Join(saves, "Hero.map")); sha256hex(got) != serverMapSHA {
		t.Fatal("sidecar-only carry did not update Hero.map")
	}
	if d2sDownloads != 0 {
		t.Fatalf("an in-sync .d2s must not be re-downloaded, got %d", d2sDownloads)
	}
}

// TestSidecarOnlyRespectsGuard: with the game-open guard tripping, a sidecar-only
// carry writes nothing.
func TestSidecarOnlyRespectsGuard(t *testing.T) {
	saves := t.TempDir()
	d2s := syntheticSave(64, 0xCC)
	writeFile(t, saves, "Hero.d2s", d2s)
	oldMap := []byte("\x00stale-local-map")
	writeFile(t, saves, "Hero.map", oldMap)

	d2sSHA := sha256hex(d2s)
	serverMap := []byte("\x00fresh-server-map")
	serverMapSHA := sha256hex(serverMap)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sync", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"characters":[{"filename":"Hero.d2s","sha256":"%s","download_path":"/dl/Hero",`+
			`"sidecars":[{"filename":"Hero.map","sha256":"%s","size":%d}],`+
			`"sidecars_download_path":"/api/v1/characters/Hero/sidecars"}],"shared_stashes":[]}`,
			d2sSHA, serverMapSHA, len(serverMap))
	})
	mux.HandleFunc("/api/v1/characters/Hero/sidecars", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("guard should have blocked before any sidecar download")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := newSidecarWatcher(t, saves, srv.URL)
	w.syncState.Set("Hero.d2s", d2sSHA)
	w.gameRunning = func(string, time.Time) (bool, string) { return true, "game running" }

	w.runPull()

	if got, _ := os.ReadFile(filepath.Join(saves, "Hero.map")); sha256hex(got) != sha256hex(oldMap) {
		t.Fatal("sidecar-only carry wrote despite the game-open guard")
	}
}

// TestSidecarOnlyToggleOff: with map sync off, no sidecar-only candidate is even
// produced and nothing is written.
func TestSidecarOnlyToggleOff(t *testing.T) {
	saves := t.TempDir()
	d2s := syntheticSave(64, 0xCC)
	writeFile(t, saves, "Hero.d2s", d2s)
	oldMap := []byte("\x00stale-local-map")
	writeFile(t, saves, "Hero.map", oldMap)

	d2sSHA := sha256hex(d2s)
	serverMapSHA := sha256hex([]byte("\x00fresh-server-map"))

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sync", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"characters":[{"filename":"Hero.d2s","sha256":"%s","download_path":"/dl/Hero",`+
			`"sidecars":[{"filename":"Hero.map","sha256":"%s","size":10}],`+
			`"sidecars_download_path":"/api/v1/characters/Hero/sidecars"}],"shared_stashes":[]}`,
			d2sSHA, serverMapSHA)
	})
	mux.HandleFunc("/api/v1/characters/Hero/sidecars", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("map sync off must not touch sidecars")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := newSidecarWatcher(t, saves, srv.URL)
	w.syncState.Set("Hero.d2s", d2sSHA)
	off := false
	w.Config.SyncMapFiles = &off

	cands, _, err := w.evaluate()
	if err != nil {
		t.Fatalf("evaluate failed: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("map sync off must produce no sidecar-only candidate, got %+v", cands)
	}

	w.runPull()

	if got, _ := os.ReadFile(filepath.Join(saves, "Hero.map")); sha256hex(got) != sha256hex(oldMap) {
		t.Fatal("map sync off must not write sidecars")
	}
}

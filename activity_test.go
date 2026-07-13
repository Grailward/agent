package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// activityRec records the transfer-activity flips the watcher reports. The
// callback fires synchronously on the scan goroutine, but the mutex keeps it clean
// under -race when an httptest handler shares the watcher.
type activityRec struct {
	mu   sync.Mutex
	seq  []bool
	last bool
}

func (r *activityRec) fn(active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq = append(r.seq, active)
	r.last = active
}

func (r *activityRec) events() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.seq...)
}

// TestIconForState pins the coarse state -> icon mapping the tray relies on.
func TestIconForState(t *testing.T) {
	cases := []struct {
		state State
		want  []byte
	}{
		{StateSyncing, iconSyncing},
		{StatePaused, iconPaused},
		{StateError, iconError},
	}
	for _, c := range cases {
		if got := iconForState(c.state); !bytes.Equal(got, c.want) {
			t.Fatalf("iconForState(%v) returned the wrong icon", c.state)
		}
	}
}

// TestIconStateFreezeAndRestore is the core of the activity indicator: while a
// transfer is active the state icon is frozen (report returns nil), and closing
// the transfer restores the icon for the LAST reported state — so a batch that
// ended red (the error latch) comes back red, not gold.
func TestIconStateFreezeAndRestore(t *testing.T) {
	var s iconState

	// Idle: a report paints its state icon immediately.
	if got := s.report(StateSyncing); !bytes.Equal(got, iconSyncing) {
		t.Fatal("idle report should return the syncing icon")
	}

	// Transfer starts: show the activity icon.
	if got := s.setActive(true); !bytes.Equal(got, iconTransferring) {
		t.Fatal("setActive(true) should return the transferring icon")
	}

	// Mid-transfer reports are frozen (return nil) but still record the state, so a
	// "Synced X" line can't flip the icon back to gold early.
	if got := s.report(StateSyncing); got != nil {
		t.Fatal("a report during an active transfer must not change the icon")
	}
	// A latched error reported mid-transfer is remembered as the state to restore.
	if got := s.report(StateError); got != nil {
		t.Fatal("an error report during an active transfer must not change the icon")
	}

	// Transfer ends: restore the last reported state — red wins.
	if got := s.setActive(false); !bytes.Equal(got, iconError) {
		t.Fatal("closing a transfer that ended red must restore the error icon")
	}

	// Back to idle: a healthy report repaints gold.
	if got := s.report(StateSyncing); !bytes.Equal(got, iconSyncing) {
		t.Fatal("after the transfer, a healthy report should return the syncing icon")
	}
}

// TestTransferScopeMachine drives the watcher's scope directly: a transfer inside
// a scope reports [true,false]; the first of several transfers is the only flip
// (no per-file flicker); a scope with no transfer reports nothing (a routine scan
// stays quiet).
func TestTransferScopeMachine(t *testing.T) {
	t.Run("transfer flips on then off", func(t *testing.T) {
		w := &Watcher{}
		rec := &activityRec{}
		w.SetActivityFunc(rec.fn)

		w.enterTransfers()
		w.noteTransfer()
		w.noteTransfer() // second file in the same scope: no extra flip
		w.leaveTransfers()

		got := rec.events()
		if len(got) != 2 || got[0] != true || got[1] != false {
			t.Fatalf("expected [true false], got %v", got)
		}
	})

	t.Run("scope without transfer stays quiet", func(t *testing.T) {
		w := &Watcher{}
		rec := &activityRec{}
		w.SetActivityFunc(rec.fn)

		w.enterTransfers()
		w.leaveTransfers()

		if got := rec.events(); len(got) != 0 {
			t.Fatalf("a scope with no transfer must not report activity, got %v", got)
		}
	})

	t.Run("stray note outside any scope is ignored", func(t *testing.T) {
		w := &Watcher{}
		rec := &activityRec{}
		w.SetActivityFunc(rec.fn)

		w.noteTransfer() // no open scope: must not stick the icon on

		if got := rec.events(); len(got) != 0 {
			t.Fatalf("a note outside a scope must be ignored, got %v", got)
		}
	})
}

// TestScanUploadShowsTransferActivity drives a real upload through Scan and proves
// the tray sees the activity icon come on for the transfer and go off when the
// scan returns.
func TestScanUploadShowsTransferActivity(t *testing.T) {
	saves := t.TempDir()
	data := syntheticSave(64, 0x01)
	writeFile(t, saves, "Hero.d2s", data)
	path := filepath.Join(saves, "Hero.d2s")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	w := &Watcher{
		Config:   &Config{SavesDir: saves, URL: srv.URL},
		Client:   NewClient(srv.URL, "tok"),
		Pending:  map[string]FileStat{path: {Mtime: info.ModTime(), Size: info.Size()}}, // already debounced
		Uploaded: map[string]string{},
		lastLine: map[string]string{},
		errs:     map[string]string{},
	}
	rec := &activityRec{}
	w.SetActivityFunc(rec.fn)

	w.Scan()

	got := rec.events()
	if len(got) != 2 || got[0] != true || got[1] != false {
		t.Fatalf("expected an upload to report [true false], got %v", got)
	}
	if w.Uploaded[path] != sha256hex(data) {
		t.Fatal("the file should have been uploaded (test setup sanity)")
	}
}

// TestScanWithoutTransferStaysQuiet proves the routine 15s scan that finds nothing
// to send never flips the activity icon (no periodic blink).
func TestScanWithoutTransferStaysQuiet(t *testing.T) {
	saves := t.TempDir()
	// A brand-new file is only recorded on this scan (debounce), never uploaded.
	writeFile(t, saves, "Hero.d2s", syntheticSave(64, 0x02))

	w := &Watcher{
		Config:   &Config{SavesDir: saves},
		Pending:  map[string]FileStat{},
		Uploaded: map[string]string{},
		lastLine: map[string]string{},
		errs:     map[string]string{},
	}
	rec := &activityRec{}
	w.SetActivityFunc(rec.fn)

	w.Scan()

	if got := rec.events(); len(got) != 0 {
		t.Fatalf("a scan with nothing to transfer must not report activity, got %v", got)
	}
}

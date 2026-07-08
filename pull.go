package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// candidate is one manifest file the pull may act on, with its classification.
type candidate struct {
	entry    ManifestEntry
	decision SyncDecision
	filename string // sanitized basename
}

// ConflictChoice is the user's answer to a per-file conflict prompt.
type ConflictChoice int

const (
	ConflictSkip      ConflictChoice = iota // default / dialog dismissed
	ConflictKeepLocal                       // upload local as current
	ConflictUseServer                       // back up local, then download server
)

// conflictMessage is the shared prompt body for a conflicted file, so both
// platforms word it identically.
func conflictMessage(filename string) string {
	return fmt.Sprintf(
		"\"%s\" changed on this machine and on the server since the last sync.\n\n"+
			"Keep local: upload your copy and make it the current save.\n"+
			"Use server: back your copy up to the server, then download the server's copy.\n"+
			"Skip: do nothing for now.\n\n"+
			"Make sure Diablo II: Resurrected is fully closed before choosing Use server.",
		filename)
}

// lastSyncSHA returns the last confirmed-sync sha for a filename ("" if none).
func (w *Watcher) lastSyncSHA(filename string) string {
	if w.syncState == nil {
		return ""
	}
	sha, _ := w.syncState.Get(filename)
	return sha
}

// recordSync persists the last confirmed-sync sha for a filename.
func (w *Watcher) recordSync(filename, sha string) {
	if w.syncState == nil {
		return
	}
	if err := w.syncState.Set(filename, sha); err != nil {
		log.Printf("Could not persist sync state for %s: %v", filename, err)
	}
}

// localSHA reads a file in the saves folder and returns its sha256, or
// ("", false) when the file is absent/unreadable.
func (w *Watcher) localSHA(filename string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(w.Config.SavesDir, filename))
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), true
}

// backupsDir is where pre-overwrite backups live — beside the config, never
// inside the saves folder.
func (w *Watcher) backupsDir() string {
	dir, err := ConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "grailward-agent", "backups")
	}
	return filepath.Join(dir, "backups")
}

// evaluate fetches the manifest and classifies every entry against the local
// files and the recorded sync state. It returns the files that are candidates
// for a pull (fast-forwards, new-from-server, conflicts); in-sync files are
// recorded and plain local edits are left to the scan loop.
func (w *Watcher) evaluate() ([]candidate, error) {
	m, err := w.Client.FetchManifest()
	if err != nil {
		return nil, err
	}
	var cands []candidate
	for _, e := range m.entries() {
		clean, ok := sanitizeFilename(e.Filename)
		if !ok {
			w.logOnce("__reject__"+e.Filename, "Ignoring unsafe filename from server: "+e.Filename)
			continue
		}
		localSHA, present := w.localSHA(clean)
		lastSync := w.lastSyncSHA(clean)
		switch decide(present, localSHA, lastSync, e.SHA256) {
		case DecisionInSync:
			// Record when local matches server but the state is stale/missing.
			if present && lastSync != e.SHA256 {
				w.recordSync(clean, e.SHA256)
			}
		case DecisionPushLocal:
			// The scan loop uploads this; nothing for the pull to do.
		case DecisionFastForward, DecisionNewFromServer:
			cands = append(cands, candidate{entry: e, decision: DecisionFastForward, filename: clean})
		case DecisionConflict:
			cands = append(cands, candidate{entry: e, decision: DecisionConflict, filename: clean})
		}
	}
	return cands, nil
}

// periodicSyncCheck runs on the scan goroutine's 5-minute timer. It only
// signals: it updates the newer-save count (and thus the tray label/tooltip)
// and never opens a dialog or writes. It respects pause and two-way mode.
func (w *Watcher) periodicSyncCheck() {
	if w.Paused() || w.SyncMode() != SyncModeTwoWay {
		return
	}
	cands, err := w.evaluate()
	if err != nil {
		var pd *PullDisabledError
		if errors.As(err, &pd) {
			w.logOnce("__pull__", "Server pull is disabled: "+pd.Message)
			w.setNewer(0)
		} else {
			w.logOnce("__pull__", "Could not check the server for newer saves: "+err.Error())
		}
		return
	}
	n := len(cands)
	w.setNewer(n)
	if n > 0 {
		w.status(StateSyncing, fmt.Sprintf("%d newer save(s) on server — use Pull latest now", n))
	}
}

// refreshNewer recomputes the newer-save count without any dialog, swallowing
// transient errors (used to update the badge after a pull run).
func (w *Watcher) refreshNewer() {
	if cands, err := w.evaluate(); err == nil {
		w.setNewer(len(cands))
	}
}

// runPull performs an interactive pull on the scan goroutine: one batch prompt
// for fast-forwards/new saves and one three-way prompt per conflict. No write
// happens without explicit confirmation.
func (w *Watcher) runPull() {
	if w.SyncMode() != SyncModeTwoWay {
		return
	}
	w.ensureSeams()
	cands, err := w.evaluate()
	if err != nil {
		w.reportPullError(err)
		return
	}

	var batch, conflicts []candidate
	for _, c := range cands {
		if c.decision == DecisionConflict {
			conflicts = append(conflicts, c)
		} else {
			batch = append(batch, c)
		}
	}

	if len(batch) == 0 && len(conflicts) == 0 {
		w.setNewer(0)
		w.status(StateSyncing, "Up to date with server")
		return
	}

	// Snapshot the moment before any write, so the macOS game-open heuristic can
	// tell prior game activity apart from the agent's own in-run writes.
	before := time.Now()

	// Fast-forwards and new saves: a single batch confirmation.
	if len(batch) > 0 {
		names := candidateNames(batch)
		msg := fmt.Sprintf("Server has newer saves for: %s.\n\nMake sure Diablo II: Resurrected is fully closed, then pull now?", names)
		ok, err := w.confirmPull(msg)
		if err != nil {
			log.Printf("Pull confirmation dialog failed: %v", err)
		} else if ok {
			for _, c := range batch {
				w.applyCandidate(c, before)
			}
		} else {
			w.logOnce("__pull__", "Pull skipped by user for: "+names)
		}
	}

	// Conflicts: one three-way choice each.
	for _, c := range conflicts {
		choice, err := w.resolveConflict(c.filename)
		if err != nil {
			log.Printf("Conflict dialog failed for %s: %v", c.filename, err)
			continue
		}
		switch choice {
		case ConflictKeepLocal:
			w.pushLocalNow(c)
		case ConflictUseServer:
			w.useServer(c, before)
		default: // ConflictSkip
			w.logOnce(c.filename, c.filename+": conflict skipped by user")
		}
	}

	w.refreshNewer()
}

// reportPullError degrades gracefully on a manifest fetch failure: a disabled
// server (503) is not an error state, a transient failure logs once.
func (w *Watcher) reportPullError(err error) {
	var pd *PullDisabledError
	if errors.As(err, &pd) {
		w.logOnce("__pull__", "Server pull is disabled: "+pd.Message)
		w.status(StateSyncing, "Server pull disabled")
		w.setNewer(0)
		return
	}
	w.logOnce("__pull__", "Could not reach the server for pull: "+err.Error())
	w.status(StateError, "Cannot reach server for pull")
}

// applyCandidate downloads and writes one server save (a fast-forward or a new
// file). The game-open guard is checked immediately before the write.
func (w *Watcher) applyCandidate(c candidate, before time.Time) {
	if !w.guardAllowsWrite(before) {
		return
	}
	data, xsha, err := w.Client.Download(c.entry.DownloadPath)
	if err != nil {
		w.logOnce(c.filename, c.filename+": download failed — "+err.Error())
		w.status(StateError, c.filename+": download failed")
		return
	}
	if err := pullWrite(w.Config.SavesDir, w.backupsDir(), c.filename, data, c.entry.SHA256, xsha); err != nil {
		w.logOnce(c.filename, c.filename+": pull aborted — "+err.Error())
		w.status(StateError, c.filename+": pull aborted")
		return
	}
	w.recordPulled(c.filename, c.entry.SHA256)
	w.logOnce(c.filename, c.filename+": pulled from server")
	w.status(StateSyncing, "Pulled "+c.filename)
}

// useServer resolves a conflict in the server's favor: the local bytes are
// guaranteed into the server's history (as a non-current backup) BEFORE the
// server copy overwrites them. The write only proceeds on a successful upload.
func (w *Watcher) useServer(c candidate, before time.Time) {
	if !w.guardAllowsWrite(before) {
		return
	}
	dest := filepath.Join(w.Config.SavesDir, c.filename)
	localBytes, err := os.ReadFile(dest)
	if err != nil {
		w.logOnce(c.filename, c.filename+": cannot read local file to back up — "+err.Error())
		w.status(StateError, c.filename+": conflict aborted")
		return
	}
	sum := sha256.Sum256(localBytes)
	localSHA := hex.EncodeToString(sum[:])

	resp, err := w.Client.UploadSnapshotBackup(c.filename, localBytes, localSHA, w.Machine)
	if err != nil {
		w.logOnce(c.filename, c.filename+": backup upload failed — "+err.Error())
		w.status(StateError, c.filename+": conflict aborted")
		return
	}
	if resp.Error != "" {
		w.logOnce(c.filename, c.filename+": backup upload rejected — "+resp.Error)
		w.status(StateError, c.filename+": conflict aborted")
		return
	}
	// Local bytes are safe on the server; now overwrite with the server copy.
	w.logOnce(c.filename, c.filename+": local backed up to server, pulling server copy")
	w.applyCandidate(c, before)
}

// pushLocalNow resolves a conflict in the local copy's favor by uploading it as
// the current save. No disk write occurs, so no game-open guard is needed.
func (w *Watcher) pushLocalNow(c candidate) {
	dest := filepath.Join(w.Config.SavesDir, c.filename)
	data, err := os.ReadFile(dest)
	if err != nil {
		w.logOnce(c.filename, c.filename+": cannot read local file to push — "+err.Error())
		w.status(StateError, c.filename+": push aborted")
		return
	}
	sum := sha256.Sum256(data)
	w.upload(c.filename, dest, data, hex.EncodeToString(sum[:]))
}

// recordPulled updates the sync state and the watcher's own maps after a
// successful pull-write, so the next scan does not re-upload what we just wrote.
func (w *Watcher) recordPulled(filename, sha string) {
	dest := filepath.Join(w.Config.SavesDir, filename)
	if info, err := os.Stat(dest); err == nil {
		w.Pending[dest] = FileStat{Mtime: info.ModTime(), Size: info.Size()}
	}
	w.Uploaded[dest] = sha
	w.recordSync(filename, sha)
}

// guardAllowsWrite is the reinforcement check (never a substitute for the
// confirmation) that refuses to write while a game session looks active. before
// bounds the mtime heuristic to activity that predates this pull run.
func (w *Watcher) guardAllowsWrite(before time.Time) bool {
	w.ensureSeams()
	if running, reason := w.gameRunning(w.Config.SavesDir, before); running {
		log.Printf("Refusing to write: %s. Close Diablo II: Resurrected and try again.", reason)
		w.status(StateError, "Game open — write refused")
		return false
	}
	return true
}

// ensureSeams lazily installs the real platform implementations for any unset
// test seam, so a Watcher built as a struct literal (in tests) is still safe.
// Runs only on the scan goroutine, so it needs no lock.
func (w *Watcher) ensureSeams() {
	if w.confirmPull == nil {
		w.confirmPull = ConfirmPull
	}
	if w.resolveConflict == nil {
		w.resolveConflict = ResolveConflict
	}
	if w.gameRunning == nil {
		w.gameRunning = GameLikelyRunning
	}
}

// candidateNames joins candidate filenames for a prompt.
func candidateNames(cands []candidate) string {
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = c.filename
	}
	return strings.Join(names, ", ")
}

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// errKeyManifest is the error-latch key shared by the periodic check and the
// on-demand pull for the manifest fetch. A fetch failure latches it; the next
// successful fetch — or a deliberately disabled server — clears it.
const errKeyManifest = "__manifest__"

// sidecarErrPrefix namespaces the per-character sidecar error-latch keys so they
// can be cleared as a group (e.g. when map sync is switched off).
const sidecarErrPrefix = "__sidecar__"

// sidecarErrKey is the per-character error-latch key for a map-sidecar carry, so
// one character's failed carry never masks another's (mirrors the per-file save
// keying).
func sidecarErrKey(d2sName string) string { return sidecarErrPrefix + d2sName }

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
	data, err := os.ReadFile(filepath.Join(w.SavesDir(), filename))
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
// recorded and plain local edits are left to the scan loop. The second return is
// the set of sanitized filenames the manifest actually lists, so the caller can
// prune latched errors for files the server no longer knows about.
func (w *Watcher) evaluate() ([]candidate, map[string]bool, error) {
	m, err := w.Client.FetchManifest()
	if err != nil {
		return nil, nil, err
	}
	mapSync := w.MapSyncEnabled()
	var cands []candidate
	known := make(map[string]bool)
	for _, e := range m.entries() {
		clean, ok := sanitizeFilename(e.Filename)
		if !ok {
			w.logOnce("__reject__"+e.Filename, "Ignoring unsafe filename from server: "+e.Filename)
			continue
		}
		known[clean] = true
		localSHA, present := w.localSHA(clean)
		lastSync := w.lastSyncSHA(clean)
		switch decide(present, localSHA, lastSync, e.SHA256) {
		case DecisionInSync:
			// Record when local matches server but the state is stale/missing.
			if present && lastSync != e.SHA256 {
				w.recordSync(clean, e.SHA256)
			}
			// The save is in sync, but its map sidecars may not be — carry them
			// silently. Skipped entirely when map sync is off.
			if mapSync && w.sidecarsDiffer(e, clean) {
				cands = append(cands, candidate{entry: e, decision: DecisionSidecarOnly, filename: clean})
			}
		case DecisionPushLocal:
			// The scan loop uploads this; nothing for the pull to do.
		case DecisionFastForward, DecisionNewFromServer:
			cands = append(cands, candidate{entry: e, decision: DecisionFastForward, filename: clean})
		case DecisionConflict:
			cands = append(cands, candidate{entry: e, decision: DecisionConflict, filename: clean})
		}
	}
	return cands, known, nil
}

// newerCount counts the candidates that represent a newer save on the server —
// everything except silent sidecar-only carries, which never light up the badge.
func newerCount(cands []candidate) int {
	n := 0
	for _, c := range cands {
		if c.decision != DecisionSidecarOnly {
			n++
		}
	}
	return n
}

// sidecarsDiffer reports whether any map-exploration sidecar the manifest lists
// for a character is missing locally or differs by sha — i.e. there is a silent
// sidecar-only pull to do even though the .d2s itself is in sync. Only characters
// (.d2s) carry sidecars.
func (w *Watcher) sidecarsDiffer(e ManifestEntry, d2sName string) bool {
	if e.SidecarsDownloadPath == "" || len(e.Sidecars) == 0 {
		return false
	}
	if !strings.EqualFold(filepath.Ext(d2sName), ".d2s") {
		return false
	}
	base := sidecarBase(d2sName)
	for _, s := range e.Sidecars {
		clean, ok := sanitizeSidecarFilename(s.Filename, base)
		if !ok {
			continue // an unsafe entry can never drive a write
		}
		local, present := w.localSHA(clean)
		if !present || !strings.EqualFold(local, s.SHA256) {
			return true
		}
	}
	return false
}

// periodicSyncCheck runs on the scan goroutine's 5-minute timer. It only
// signals: it updates the newer-save count (and thus the tray label/tooltip)
// and never opens a dialog or writes. It respects pause and two-way mode.
func (w *Watcher) periodicSyncCheck() {
	if w.Paused() || w.SyncMode() != SyncModeTwoWay {
		return
	}
	cands, _, err := w.evaluate()
	if err != nil {
		var pd *PullDisabledError
		if errors.As(err, &pd) {
			w.logOnce("__pull__", "Server pull is disabled: "+pd.Message)
			w.clearErr(errKeyManifest) // a disabled server is a state, not a failure
			w.setNewer(0)
		} else {
			w.logOnce("__pull__", "Could not check the server for newer saves: "+err.Error())
			w.setErr(errKeyManifest, "Cannot reach server for pull")
		}
		return
	}
	w.clearErr(errKeyManifest) // the manifest fetch succeeded
	n := newerCount(cands)
	w.setNewer(n)
	if n > 0 {
		w.status(StateSyncing, fmt.Sprintf("%d newer save(s) on server — use Pull latest now", n))
	}
}

// refreshNewer recomputes the newer-save count without any dialog, swallowing
// transient errors (used to update the badge after a pull run).
func (w *Watcher) refreshNewer() {
	if cands, _, err := w.evaluate(); err == nil {
		w.setNewer(newerCount(cands))
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
	cands, known, err := w.evaluate()
	if err != nil {
		w.reportPullError(err)
		return
	}
	w.clearErr(errKeyManifest) // the manifest fetch succeeded
	w.pruneFileErrs(known)     // forget latches for files the server dropped

	var batch, conflicts, sidecarOnly []candidate
	for _, c := range cands {
		switch c.decision {
		case DecisionConflict:
			conflicts = append(conflicts, c)
		case DecisionSidecarOnly:
			sidecarOnly = append(sidecarOnly, c)
		default:
			batch = append(batch, c)
		}
	}

	if len(batch) == 0 && len(conflicts) == 0 && len(sidecarOnly) == 0 {
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

	// Sidecar-only carries: the save is already in sync, so these are applied
	// silently (no dialog) — but always behind the game-open guard, and the server
	// wins (last-writer-wins; the save-exit push covers the other direction).
	for _, c := range sidecarOnly {
		w.applySidecars(c, before)
	}

	w.refreshNewer()
}

// reportPullError degrades gracefully on a manifest fetch failure: a disabled
// server (503) is not an error state, a transient failure logs once.
func (w *Watcher) reportPullError(err error) {
	var pd *PullDisabledError
	if errors.As(err, &pd) {
		w.logOnce("__pull__", "Server pull is disabled: "+pd.Message)
		w.clearErr(errKeyManifest) // a disabled server is a state, not a failure
		w.status(StateSyncing, "Server pull disabled")
		w.setNewer(0)
		return
	}
	w.logOnce("__pull__", "Could not reach the server for pull: "+err.Error())
	w.setErr(errKeyManifest, "Cannot reach server for pull")
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
		w.setErr(c.filename, c.filename+": download failed")
		return
	}
	if err := pullWrite(w.SavesDir(), w.backupsDir(), c.filename, data, c.entry.SHA256, xsha); err != nil {
		w.logOnce(c.filename, c.filename+": pull aborted — "+err.Error())
		w.setErr(c.filename, c.filename+": pull aborted")
		return
	}
	w.recordPulled(c.filename, c.entry.SHA256)
	w.clearErr(c.filename) // this file's pull succeeded — drop its latched error
	// The short sha keeps each applied pull a distinct log line, so a later pull
	// of the same file (different bytes) is never swallowed by the logOnce dedup.
	w.logOnce(c.filename, c.filename+": pulled from server ("+shortSHA(c.entry.SHA256)+")")
	w.status(StateSyncing, "Pulled "+c.filename)
	// The map-exploration sidecars ride along with the character save.
	w.applySidecars(c, before)
}

// applySidecars downloads and writes the map-exploration sidecars for a character
// candidate. It backs the map (server wins, last-writer-wins) for two paths: the
// coupled pull (right after a successful .d2s write) and the silent sidecar-only
// carry. Each file is sanitized against the character base, its payload sha is
// verified against both the batch JSON and the manifest, and it is written with
// the same atomic + backup discipline as a save. A per-file failure is logged and
// skipped — the .d2s (if any) is already applied and the rest recovers next cycle.
func (w *Watcher) applySidecars(c candidate, before time.Time) {
	if !w.MapSyncEnabled() {
		return
	}
	if c.entry.SidecarsDownloadPath == "" || len(c.entry.Sidecars) == 0 {
		return
	}
	if !strings.EqualFold(filepath.Ext(c.filename), ".d2s") {
		return // sidecars belong to characters only
	}
	base := sidecarBase(c.filename)

	// Determine which sidecars actually differ from local; skip the in-sync ones
	// so we never back up or rewrite an identical file.
	want := map[string]string{} // sanitized filename -> manifest sha
	for _, s := range c.entry.Sidecars {
		clean, ok := sanitizeSidecarFilename(s.Filename, base)
		if !ok {
			w.logOnce("__sidecar__"+c.filename+"/"+s.Filename, "Ignoring unsafe sidecar name from server: "+s.Filename)
			continue
		}
		if local, present := w.localSHA(clean); present && strings.EqualFold(local, s.SHA256) {
			continue // already in sync
		}
		want[clean] = s.SHA256
	}
	if len(want) == 0 {
		w.clearErr(sidecarErrKey(c.filename)) // sidecars are in sync
		return
	}

	// Never write while a game session looks active (reinforcement, always on).
	if !w.guardAllowsWrite(before) {
		return
	}

	resp, err := w.Client.DownloadSidecars(c.entry.SidecarsDownloadPath)
	if err != nil {
		w.logOnce("__sidecar__"+c.filename, c.filename+": sidecar download failed — "+err.Error())
		w.setErr(sidecarErrKey(c.filename), c.filename+": map sidecar download failed")
		return
	}
	for _, f := range resp.Files {
		clean, ok := sanitizeSidecarFilename(f.Filename, base)
		if !ok {
			w.logOnce("__sidecar__"+c.filename+"/"+f.Filename, "Ignoring unsafe sidecar name from server: "+f.Filename)
			continue
		}
		manifestSHA, wanted := want[clean]
		if !wanted {
			continue // not requested (already in sync or absent from the manifest)
		}
		data, err := base64.StdEncoding.DecodeString(f.RawBase64)
		if err != nil {
			w.logOnce("__sidecar__"+clean, clean+": sidecar payload could not be decoded")
			continue
		}
		// The payload must match both the batch JSON sha and the manifest sha.
		if err := verifySHA(clean, data, f.SHA256, manifestSHA); err != nil {
			w.logOnce("__sidecar__"+clean, clean+": "+err.Error())
			continue
		}
		if err := writeAtomic(w.SavesDir(), w.backupsDir(), clean, data); err != nil {
			w.logOnce("__sidecar__"+clean, clean+": sidecar write aborted — "+err.Error())
			continue
		}
		w.recordPulled(clean, manifestSHA)
		w.logOnce("__sidecar__"+clean, clean+": map sidecar pulled from server ("+shortSHA(manifestSHA)+")")
	}
	// The sidecar batch was fetched and applied — clear this character's latch.
	w.clearErr(sidecarErrKey(c.filename))
}

// useServer resolves a conflict in the server's favor: the local bytes are
// guaranteed into the server's history (as a non-current backup) BEFORE the
// server copy overwrites them. The write only proceeds on a successful upload.
func (w *Watcher) useServer(c candidate, before time.Time) {
	if !w.guardAllowsWrite(before) {
		return
	}
	dest := filepath.Join(w.SavesDir(), c.filename)
	localBytes, err := os.ReadFile(dest)
	if err != nil {
		w.logOnce(c.filename, c.filename+": cannot read local file to back up — "+err.Error())
		w.setErr(c.filename, c.filename+": conflict aborted")
		return
	}
	sum := sha256.Sum256(localBytes)
	localSHA := hex.EncodeToString(sum[:])

	resp, err := w.Client.UploadSnapshotBackup(c.filename, localBytes, localSHA, w.Machine)
	if err != nil {
		w.logOnce(c.filename, c.filename+": backup upload failed — "+err.Error())
		w.setErr(c.filename, c.filename+": conflict aborted")
		return
	}
	if resp.Error != "" {
		w.logOnce(c.filename, c.filename+": backup upload rejected — "+resp.Error)
		w.setErr(c.filename, c.filename+": conflict aborted")
		return
	}
	// Local bytes are safe on the server; now overwrite with the server copy.
	w.logOnce(c.filename, c.filename+": local backed up to server, pulling server copy")
	w.applyCandidate(c, before)
}

// pushLocalNow resolves a conflict in the local copy's favor by uploading it as
// the current save. No disk write occurs, so no game-open guard is needed.
func (w *Watcher) pushLocalNow(c candidate) {
	dest := filepath.Join(w.SavesDir(), c.filename)
	data, err := os.ReadFile(dest)
	if err != nil {
		w.logOnce(c.filename, c.filename+": cannot read local file to push — "+err.Error())
		w.setErr(c.filename, c.filename+": push aborted")
		return
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	w.upload(c.filename, dest, data, sha)
	// A successful upload (Uploaded advanced to this sha) resolves the conflict
	// in local's favour — drop any latched error for this file.
	if w.Uploaded[dest] == sha {
		w.clearErr(c.filename)
	}
}

// recordPulled updates the sync state and the watcher's own maps after a
// successful pull-write, so the next scan does not re-upload what we just wrote.
func (w *Watcher) recordPulled(filename, sha string) {
	dest := filepath.Join(w.SavesDir(), filename)
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
	if running, reason := w.gameRunning(w.SavesDir(), before); running {
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

// pruneFileErrs drops per-file (and their matching sidecar) latches for files
// that are no longer in the manifest, so a save deleted server-side can't leave
// the tray red forever with no operation left to clear it. known is the set of
// sanitized filenames the just-fetched manifest listed. The manifest latch is
// exempt — it has its own heal cycle (a later successful fetch clears it).
func (w *Watcher) pruneFileErrs(known map[string]bool) {
	w.clearErrsFunc(func(key string) bool {
		if key == errKeyManifest {
			return false
		}
		name := strings.TrimPrefix(key, sidecarErrPrefix) // no-op for plain per-file keys
		return !known[name]
	})
}

// shortSHA returns the first 7 hex chars of a sha256 (git-style), used to make
// each applied-pull log line distinct so the logOnce dedup doesn't suppress a
// second, genuinely-different pull of the same file.
func shortSHA(sha string) string {
	if len(sha) < 7 {
		return sha
	}
	return sha[:7]
}

// candidateNames joins candidate filenames for a prompt.
func candidateNames(cands []candidate) string {
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = c.filename
	}
	return strings.Join(names, ", ")
}

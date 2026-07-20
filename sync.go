package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Manifest is the server's view of the account's current saves, returned by
// GET /api/v1/sync. It drives the two-way decision matrix.
type Manifest struct {
	Characters    []ManifestEntry `json:"characters"`
	SharedStashes []ManifestEntry `json:"shared_stashes"`
}

// ManifestEntry describes one save slot on the server. Characters and shared
// stashes carry the same fields the agent needs (filename, sha, download_path);
// the extra descriptive fields (name, mode, variant) are decoded when present
// but are not required for the decision logic.
type ManifestEntry struct {
	Name          string `json:"name"`
	Mode          string `json:"mode"`
	Variant       string `json:"variant"`
	Filename      string `json:"filename"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	SyncedAt      string `json:"synced_at"`
	SourceMachine string `json:"source_machine"`
	DownloadPath  string `json:"download_path"`
	// Sidecars lists the map-exploration files that travel with a character save
	// (characters only; shared-stash entries never carry these). Both fields are
	// decoded tolerantly: absent on an entry means a zero value (no sidecars).
	Sidecars             []SidecarMeta `json:"sidecars"`
	SidecarsDownloadPath string        `json:"sidecars_download_path"`
}

// SidecarMeta describes one map-exploration sidecar file as listed in the
// manifest, so the pull can decide which sidecars differ from the local copies.
type SidecarMeta struct {
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}

// entries flattens characters and shared stashes into one list for the
// per-file decision loop.
func (m *Manifest) entries() []ManifestEntry {
	out := make([]ManifestEntry, 0, len(m.Characters)+len(m.SharedStashes))
	out = append(out, m.Characters...)
	out = append(out, m.SharedStashes...)
	return out
}

// SyncDecision is the outcome of comparing one file's local, last-synced and
// server shas.
type SyncDecision int

const (
	// DecisionInSync — local matches server (or nothing newer exists). Nothing
	// to do beyond recording the sha in sync_state.
	DecisionInSync SyncDecision = iota
	// DecisionFastForward — local still equals the last confirmed sync but the
	// server moved forward. The local bytes are already in the server's history
	// by construction, so this is a safe candidate to write directly.
	DecisionFastForward
	// DecisionNewFromServer — no local file (nothing to lose) and the server has
	// bytes newer than the last sync. Candidate to write.
	DecisionNewFromServer
	// DecisionConflict — local and server both diverge from the last sync. Needs
	// an explicit per-file choice from the user.
	DecisionConflict
	// DecisionPushLocal — local changed but the server has not moved. The normal
	// upload path (the scan loop) already handles this; the pull does nothing.
	DecisionPushLocal
	// DecisionSidecarOnly — the character save itself is in sync, but at least one
	// map-exploration sidecar differs (or is missing) locally. It is applied
	// silently on a pull (server wins, no confirmation) and never counts toward
	// the "newer saves" badge.
	DecisionSidecarOnly
)

// decide classifies one file from the sync matrix. localSHA is empty when the
// file is absent locally (localPresent false); lastSyncSHA is empty when there
// is no recorded sync; serverSHA is always set (the entry exists on the server).
func decide(localPresent bool, localSHA, lastSyncSHA, serverSHA string) SyncDecision {
	if localPresent && localSHA == serverSHA {
		return DecisionInSync
	}
	if !localPresent {
		// Nothing on disk. If the server has not moved past the last sync there
		// is nothing newer to offer (a locally deleted file is not restored
		// silently); otherwise the server bytes are a safe write.
		if serverSHA == lastSyncSHA {
			return DecisionInSync
		}
		return DecisionNewFromServer
	}
	// Local present and differs from the server.
	if localSHA == lastSyncSHA {
		// Local is untouched since the last sync; the server advanced.
		return DecisionFastForward
	}
	if serverSHA == lastSyncSHA {
		// Only the local side changed; the scan loop pushes it.
		return DecisionPushLocal
	}
	// Both sides diverged from the last sync.
	return DecisionConflict
}

// safeBasename runs the extension-independent hostile-input checks shared by the
// save and sidecar sanitizers. It returns true only when the name has no control
// characters (NUL included), no path separators, is an idempotent bare basename,
// is not a dotfile (which also rejects "." and ".."), and is not a Windows
// reserved device name (matched on the segment before the first dot, since
// Windows treats "CON.map" as the CON device, not a file).
func safeBasename(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	if name != filepath.Base(name) {
		return false
	}
	if strings.HasPrefix(name, ".") {
		return false
	}
	stem := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		stem = name[:i]
	}
	return !isReservedDeviceName(stem)
}

// sanitizeFilename treats the manifest filename as hostile input. It returns the
// safe basename and true only when it passes the shared hostile-input checks and
// carries a .d2s or .d2i extension.
func sanitizeFilename(name string) (string, bool) {
	if !safeBasename(name) {
		return "", false
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".d2s" && ext != ".d2i" {
		return "", false
	}
	return name, true
}

// sanitizeSidecarFilename treats a sidecar filename from the server as hostile.
// On top of the shared hostile-input checks it requires a map-exploration
// extension (.map or .ma<single digit>, case-insensitive) and — crucially — that
// the whole name is exactly base + that extension, i.e. the sidecar belongs to
// the character whose .d2s the pull just handled. Anything with an extra segment
// (e.g. "Hero.foo.map"), a different stem, or a two-digit .maNN is rejected.
func sanitizeSidecarFilename(name, base string) (string, bool) {
	if !safeBasename(name) {
		return "", false
	}
	ext := filepath.Ext(name)
	if !validSidecarExt(ext) {
		return "", false
	}
	if name[:len(name)-len(ext)] != base {
		return "", false
	}
	return name, true
}

// validSidecarExt reports whether ext (with the leading dot) is a map-exploration
// sidecar extension: ".map" or ".ma<one digit>", compared case-insensitively.
func validSidecarExt(ext string) bool {
	e := strings.ToLower(ext)
	if e == ".map" {
		return true
	}
	if len(e) == 4 && strings.HasPrefix(e, ".ma") {
		return e[3] >= '0' && e[3] <= '9'
	}
	return false
}

// isReservedDeviceName reports whether s is a Windows reserved device name
// (case-insensitive): CON, PRN, AUX, NUL, COM1-9, LPT1-9.
func isReservedDeviceName(s string) bool {
	switch strings.ToUpper(s) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	u := strings.ToUpper(s)
	if len(u) == 4 && (strings.HasPrefix(u, "COM") || strings.HasPrefix(u, "LPT")) {
		return u[3] >= '1' && u[3] <= '9'
	}
	return false
}

// pullWrite verifies downloaded save bytes and atomically writes them into
// savesDir, backing up any existing file first. It never deletes a local file
// and never writes outside savesDir. The sha of the downloaded bytes is checked
// against both the manifest sha and the server's X-Sha256 header BEFORE the
// rename; any mismatch aborts without touching the destination.
func pullWrite(savesDir, backupsDir, filename string, data []byte, manifestSHA, headerSHA string) error {
	clean, ok := sanitizeFilename(filename)
	if !ok {
		return fmt.Errorf("rejected unsafe filename %q", filename)
	}
	if err := verifySHA(clean, data, manifestSHA, headerSHA); err != nil {
		return err
	}
	return writeAtomic(savesDir, backupsDir, clean, data)
}

// verifySHA checks the sha256 of data against every non-empty expected digest
// (the manifest sha, the X-Sha256 header, the batch JSON sha…). An empty
// expected value is skipped, so callers can pass an optional header sha as "".
func verifySHA(clean string, data []byte, expected ...string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	for _, exp := range expected {
		if exp == "" {
			continue
		}
		if !strings.EqualFold(got, exp) {
			return fmt.Errorf("sha mismatch for %s: got %s != expected %s", clean, got, exp)
		}
	}
	return nil
}

// writeAtomic writes pre-sanitized, pre-verified bytes for cleanName into
// savesDir, backing up any existing file first, then temp-file + fsync + rename.
// cleanName MUST already be a sanitized basename and data MUST already be
// sha-verified by the caller. It never deletes and never writes outside savesDir.
func writeAtomic(savesDir, backupsDir, cleanName string, data []byte) error {
	dest := filepath.Join(savesDir, cleanName)

	// Back up any existing file before overwriting it (one backup per filename,
	// overwriting the previous one).
	if _, err := os.Stat(dest); err == nil {
		if err := backupFile(dest, backupsDir, cleanName); err != nil {
			return fmt.Errorf("backup of %s failed: %w", cleanName, err)
		}
	}

	// Atomic write: temp file in the SAME directory, fsync, then rename.
	tmp, err := os.CreateTemp(savesDir, ".grailward-*.tmp")
	if err != nil {
		return fmt.Errorf("could not create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("could not write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("could not sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("could not close temp file: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("could not rename into place: %w", err)
	}
	return nil
}

// backupFile copies src to <backupsDir>/<filename>, overwriting any previous
// backup for that filename. The backup directory lives beside the config, never
// inside the saves folder.
func backupFile(src, backupsDir, filename string) error {
	if err := os.MkdirAll(backupsDir, 0755); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(backupsDir, filename), data, 0644)
}

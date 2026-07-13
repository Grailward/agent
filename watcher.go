package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type FileStat struct {
	Mtime time.Time
	Size  int64
}

// State is the coarse run state surfaced to the tray (icon color + tooltip).
type State int

const (
	StateSyncing State = iota // actively watching/uploading (gold)
	StatePaused               // user paused the sync (grey)
	StateError                // last scan or upload failed (red)
)

// StatusFunc receives coarse state + a human-readable line for the tray/menu.
type StatusFunc func(state State, message string)

// NewerFunc reports how many server saves are newer than the local copy, so the
// tray can update the "Pull latest now" label. Two-way mode only.
type NewerFunc func(count int)

// syncCheckInterval is how often two-way mode silently re-checks the server for
// newer saves (status/label only — never a dialog or a write).
const syncCheckInterval = 5 * time.Minute

type Watcher struct {
	Config    *Config
	Client    *Client
	Pending   map[string]FileStat
	Uploaded  map[string]string // path -> sha256
	lastLine  map[string]string // path -> last line logged, to suppress repeats
	Machine   string
	syncState *SyncState

	// pendingSidecars queues characters whose map-exploration sidecar batch failed
	// to upload (base name -> server character name), retried at the top of each
	// scan. It never blocks or fails the .d2s upload; the scan goroutine owns it,
	// so it needs no lock.
	pendingSidecars map[string]string

	mu         sync.Mutex
	paused     bool
	interval   time.Duration
	newerCount int

	// errs is the persistent pull-error latch: a per-scope error message that
	// only its own successful operation clears — a routine healthy scan never
	// does. The tray derives its red state from a non-empty latch, so a failed
	// manifest fetch / download / sidecar carry stays visible until it recovers
	// instead of being painted over by the next "Watching for changes". Both the
	// scan goroutine (setErr/clearErr on pull outcomes) and the tray goroutine
	// (clearErrs/clearErrsFunc on mode & map-sync toggles) write it, so every
	// access is guarded by w.mu.
	errs map[string]string

	reset     chan struct{} // wakes the loop when interval/pause changes
	pullReq   chan struct{} // requests an interactive pull on the scan goroutine
	updateReq chan struct{} // requests an interactive self-update on the scan goroutine
	changeDir chan string   // requests a live saves-folder swap on the scan goroutine
	onStatus  StatusFunc
	onNewer   NewerFunc

	// Transfer-activity indicator. A real byte transfer — a save
	// upload, a pull download/write, a map-sidecar carry — flips the tray to a
	// distinct "transferring" icon so an active transfer reads differently from the
	// idle watching state; a routine scan that sends nothing never triggers it.
	// Every transfer runs on the single scan goroutine, bracketed by a transfer
	// scope (Scan and runPull each open one); noteTransfer flips the icon on the
	// first transfer inside a scope, and closing the outermost scope restores the
	// state-derived icon. All three are guarded by w.mu like the rest of the shared
	// tray state; the scan goroutine writes them and the tray reads onActivity.
	onActivity func(active bool)
	xferDepth  int
	xferShown  bool

	// Self-update offer state, shared with the tray and guarded by w.mu. The scan
	// goroutine sets/clears the offer from a manifest check; the tray reads onUpdate
	// to show the "Update to vX.Y.Z…" item. The check is a separate concern from the
	// sync-error latch — an update failure never turns the tray red.
	onUpdate       func(state updateUIState, version string)
	updateOffer    *UpdateFile
	updateOfferVer string
	updateState    updateUIState

	// persistMu serializes config.json writes so concurrent preference changes
	// (interval / sync mode / token, driven from different tray goroutines)
	// can't interleave or lose one another's update.
	persistMu sync.Mutex

	// Test seams for the interactive/OS-dependent pieces of the pull; default
	// to the real platform implementations in NewWatcher.
	confirmPull     func(message string) (bool, error)
	resolveConflict func(filename string) (ConflictChoice, error)
	gameRunning     func(savesDir string, before time.Time) (bool, string)

	// Test seams for the OS login item. Default to the real per-user
	// platform implementations; a nil seam falls back to them at call time so a
	// struct-literal Watcher (tests) stays safe.
	enableLoginItem  func(execPath string) error
	disableLoginItem func() error
	loginItemTarget  func() string

	// Self-update seams + config. updateURL/updateHTTP/currentVersion default to the
	// production manifest URL, the default HTTP client, and the embedded build
	// Version; confirmUpdate/applyUpdate default to the platform dialog + swap. Tests
	// point updateURL at an httptest server and inject the interactive/OS seams.
	updateURL      string
	updateHTTP     *http.Client
	currentVersion string
	confirmUpdate  func(version string) (bool, error)
	applyUpdate    func(execPath string, file *UpdateFile, data []byte) error
	showMessage    func(message string) error // defaults to ShowAbout
}

func NewWatcher(cfg *Config, client *Client) (*Watcher, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	statePath, _ := SyncStatePath()
	return &Watcher{
		Config:           cfg,
		Client:           client,
		Pending:          make(map[string]FileStat),
		Uploaded:         make(map[string]string),
		lastLine:         make(map[string]string),
		errs:             make(map[string]string),
		pendingSidecars:  make(map[string]string),
		Machine:          hostname,
		syncState:        LoadSyncState(statePath),
		interval:         time.Duration(cfg.PollInterval * float64(time.Second)),
		reset:            make(chan struct{}, 1),
		pullReq:          make(chan struct{}, 1),
		updateReq:        make(chan struct{}, 1),
		changeDir:        make(chan string, 1),
		confirmPull:      ConfirmPull,
		resolveConflict:  ResolveConflict,
		gameRunning:      GameLikelyRunning,
		enableLoginItem:  enableStartAtLogin,
		disableLoginItem: disableStartAtLogin,
		loginItemTarget:  startAtLoginTarget,
		currentVersion:   Version,
		confirmUpdate:    ConfirmUpdate,
		applyUpdate:      ApplyUpdate,
	}, nil
}

// SetStatusFunc registers the callback used to report state to the tray.
// A no-op default keeps headless/CLI runs working.
func (w *Watcher) SetStatusFunc(f StatusFunc) {
	w.mu.Lock()
	w.onStatus = f
	w.mu.Unlock()
}

func (w *Watcher) status(state State, message string) {
	w.mu.Lock()
	f := w.onStatus
	// Error latch: an active persistent error overrides a healthy "syncing"
	// report so a routine scan can't paint over it (the whole point of the
	// latch). A Paused report is explicit user intent and passes through; an
	// error report obviously passes through too.
	if state == StateSyncing && len(w.errs) > 0 {
		state = StateError
		message = w.errSummaryLocked()
	}
	w.mu.Unlock()
	if f != nil {
		f(state, message)
	}
}

// errSummaryLocked renders the tray line for the current latch. The caller holds
// w.mu. A single error shows its own message; several collapse to a count so the
// tray stays readable and the per-error detail lives in the log.
func (w *Watcher) errSummaryLocked() string {
	switch len(w.errs) {
	case 0:
		return "Watching for changes"
	case 1:
		for _, m := range w.errs {
			return m
		}
	}
	return fmt.Sprintf("%d pending sync errors — see log", len(w.errs))
}

// setErr latches a persistent error under key and paints the tray red. Only
// clearErr(key) (or clearErrs/clearErrsFunc) removes it — never a routine scan.
// setErr is always called from the scan goroutine, but the latch is shared with
// tray-goroutine clearers, so w.mu guards every touch.
func (w *Watcher) setErr(key, message string) {
	w.mu.Lock()
	if w.errs == nil {
		w.errs = make(map[string]string)
	}
	w.errs[key] = message
	f := w.onStatus
	msg := w.errSummaryLocked()
	w.mu.Unlock()
	if f != nil {
		f(StateError, msg)
	}
}

// clearErr removes one latched error (the equivalent operation succeeded). If it
// actually cleared something it re-renders: back to the remaining error summary,
// or to the normal syncing/paused line once the latch is empty. A no-op key
// (never latched) renders nothing, so healthy runs stay quiet.
func (w *Watcher) clearErr(key string) {
	w.mu.Lock()
	_, existed := w.errs[key]
	delete(w.errs, key)
	state, msg := w.deriveIdleLocked()
	f := w.onStatus
	w.mu.Unlock()
	if existed && f != nil {
		f(state, msg)
	}
}

// clearErrs drops every latched error and re-renders once. Used when the whole
// latch is meaningless for the new world — switching to push-only (never pulls)
// or swapping the saves folder — so a stale latch can't leave the tray red.
func (w *Watcher) clearErrs() {
	w.clearErrsFunc(func(string) bool { return true })
}

// clearErrsFunc drops every latched error whose key satisfies drop, re-rendering
// once if anything changed (to the remaining error summary, or the idle
// syncing/paused line). It may run on the scan or the tray goroutine; w.mu guards
// the map. Deleting during the range is allowed by the language spec.
func (w *Watcher) clearErrsFunc(drop func(key string) bool) {
	w.mu.Lock()
	changed := false
	for key := range w.errs {
		if drop(key) {
			delete(w.errs, key)
			changed = true
		}
	}
	state, msg := w.deriveIdleLocked()
	f := w.onStatus
	w.mu.Unlock()
	if changed && f != nil {
		f(state, msg)
	}
}

// deriveIdleLocked returns the state/message to show when no specific report is
// in flight: the remaining error summary if the latch is non-empty, otherwise the
// paused line or the normal watching line. Caller holds w.mu.
func (w *Watcher) deriveIdleLocked() (State, string) {
	if len(w.errs) > 0 {
		return StateError, w.errSummaryLocked()
	}
	if w.paused {
		return StatePaused, "Sync paused"
	}
	return StateSyncing, "Watching for changes"
}

// SetNewerFunc registers the callback used to report the count of newer server
// saves to the tray.
func (w *Watcher) SetNewerFunc(f NewerFunc) {
	w.mu.Lock()
	w.onNewer = f
	w.mu.Unlock()
}

// setNewer stores the newer-save count and notifies the tray if it changed.
func (w *Watcher) setNewer(count int) {
	w.mu.Lock()
	changed := w.newerCount != count
	w.newerCount = count
	f := w.onNewer
	w.mu.Unlock()
	if changed && f != nil {
		f(count)
	}
}

// SetActivityFunc registers the callback that shows/hides the transfer-activity
// icon. A no-op default keeps headless/CLI runs working.
func (w *Watcher) SetActivityFunc(f func(active bool)) {
	w.mu.Lock()
	w.onActivity = f
	w.mu.Unlock()
}

// enterTransfers opens a transfer scope. Scan and runPull each bracket their body
// with enter/leave; noteTransfer inside flips the tray to the activity icon on the
// first real transfer, and leaveTransfers restores the state-derived icon when the
// outermost scope closes. A scope with no transfer never touches the icon, so a
// routine scan stays quiet.
func (w *Watcher) enterTransfers() {
	w.mu.Lock()
	w.xferDepth++
	w.mu.Unlock()
}

// noteTransfer marks that a real byte transfer is starting. The first one inside a
// scope flips the tray to the activity icon; later ones in the same scope are
// no-ops (no flicker between files in one scan). A stray call outside any scope is
// ignored so the activity icon can never get stuck on.
func (w *Watcher) noteTransfer() {
	w.mu.Lock()
	show := w.xferDepth > 0 && !w.xferShown
	if show {
		w.xferShown = true
	}
	f := w.onActivity
	w.mu.Unlock()
	if show && f != nil {
		f(true)
	}
}

// leaveTransfers closes a transfer scope. When the outermost scope closes and the
// activity icon was shown, it hides it; the tray then restores the icon for the
// last reported state, so a batch that ended with a latched error comes back red
// (the red latch wins) rather than plain gold.
func (w *Watcher) leaveTransfers() {
	w.mu.Lock()
	if w.xferDepth > 0 {
		w.xferDepth--
	}
	restore := w.xferDepth == 0 && w.xferShown
	if restore {
		w.xferShown = false
	}
	f := w.onActivity
	w.mu.Unlock()
	if restore && f != nil {
		f(false)
	}
}

// SyncMode returns the current sync mode (push or two_way).
func (w *Watcher) SyncMode() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.Config.SyncMode == SyncModeTwoWay {
		return SyncModeTwoWay
	}
	return SyncModePush
}

// MapSyncEnabled reports whether map-exploration sidecar sync is active
// (default ON). It is read under w.mu because the tray can toggle it live while
// the scan goroutine consults it.
func (w *Watcher) MapSyncEnabled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Config.MapSyncEnabled()
}

// SetMapSync toggles map-exploration sidecar sync live and persists it. When off,
// no sidecar is sent, downloaded, or written; toggling it on lets the next scan
// flush any queued sidecars.
func (w *Watcher) SetMapSync(on bool) {
	w.mu.Lock()
	v := on
	w.Config.SyncMapFiles = &v
	w.mu.Unlock()
	if !on {
		// With map sync off, applySidecars early-returns and can no longer clear a
		// sidecar latch — drop every one now so none stays red forever.
		w.clearErrsFunc(func(key string) bool { return strings.HasPrefix(key, sidecarErrPrefix) })
	}
	if on {
		log.Print("Sync map exploration enabled")
	} else {
		log.Print("Sync map exploration disabled")
	}
	w.persistConfig()
}

// StartAtLoginEnabled reports whether the OS login item is configured. Read under
// w.mu because the tray toggles it live.
func (w *Watcher) StartAtLoginEnabled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Config.StartAtLogin
}

// SetStartAtLogin turns the per-user OS login item on or off and, only if that
// OS write succeeds, persists the preference. The OS write is attempted first so a
// failure never leaves config.json claiming a state the system doesn't have — the
// tray reads back StartAtLoginEnabled and reflects reality instead of lying. It
// runs on the tray goroutine (like the other preference writes); the platform
// calls touch only per-user paths and need no elevation.
func (w *Watcher) SetStartAtLogin(on bool) error {
	if on {
		exec, err := currentExecPath()
		if err != nil {
			return err
		}
		enable := w.enableLoginItem
		if enable == nil {
			enable = enableStartAtLogin
		}
		if err := enable(exec); err != nil {
			return err
		}
		log.Printf("Start with system enabled — login item at %s", w.loginItemTargetPath())
	} else {
		disable := w.disableLoginItem
		if disable == nil {
			disable = disableStartAtLogin
		}
		if err := disable(); err != nil {
			return err
		}
		log.Print("Start with system disabled — login item removed")
	}
	w.mu.Lock()
	w.Config.StartAtLogin = on
	w.mu.Unlock()
	w.persistConfig()
	return nil
}

// loginItemTargetPath describes where the OS login item lives (the LaunchAgent
// plist path on macOS, the HKCU Run value on Windows), used only for the enable log
// line. Nil-safe so a struct-literal Watcher in tests falls back to the real seam.
func (w *Watcher) loginItemTargetPath() string {
	target := w.loginItemTarget
	if target == nil {
		target = startAtLoginTarget
	}
	return target()
}

// SavesDir returns the folder currently being watched. It is read under w.mu
// because the tray can read it (Open folder, About, the Change folder default)
// while the scan goroutine swaps it live via applyDirChange.
func (w *Watcher) SavesDir() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Config.SavesDir
}

// SetSyncMode switches sync mode live and persists it to config. Switching to
// two-way requests an immediate pull check; switching to push clears any
// pending newer-count badge.
func (w *Watcher) SetSyncMode(mode string) {
	if mode != SyncModeTwoWay {
		mode = SyncModePush
	}
	w.mu.Lock()
	w.Config.SyncMode = mode
	w.mu.Unlock()
	if mode == SyncModeTwoWay {
		log.Print("Sync mode set to two-way")
	} else {
		log.Print("Sync mode set to push-only")
	}
	w.persistConfig()
	if mode == SyncModeTwoWay {
		w.RequestPull()
	} else {
		// Push-only never pulls, so no pull error can be produced or resolved
		// here — drop any latched ones so the tray doesn't stay red forever.
		w.clearErrs()
		w.setNewer(0)
		w.status(StateSyncing, "Watching for changes")
	}
}

// RequestPull asks the scan goroutine to run an interactive pull. It is a
// non-blocking nudge (deduplicated by the buffered channel), mirroring wake().
func (w *Watcher) RequestPull() {
	select {
	case w.pullReq <- struct{}{}:
	default:
	}
}

// ChangeSavesDir requests a live switch to a different saves folder. The actual
// swap runs on the scan goroutine (delivered via the changeDir channel, like
// pullReq) so it can reset the lock-free scan maps without racing an in-flight
// scan, and so a pull already in progress finishes writing into the old folder
// before the re-target takes effect — never into the new one. The buffered
// channel absorbs the (user-driven, one-at-a-time) request.
func (w *Watcher) ChangeSavesDir(dir string) {
	w.changeDir <- dir
}

// Paused reports whether the sync loop is currently paused.
func (w *Watcher) Paused() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paused
}

// SetPaused toggles the sync loop on/off without stopping the process.
func (w *Watcher) SetPaused(p bool) {
	w.mu.Lock()
	w.paused = p
	w.mu.Unlock()
	w.wake()
	if p {
		w.status(StatePaused, "Sync paused")
	} else {
		w.status(StateSyncing, "Watching for changes")
	}
}

// SetInterval changes the poll cadence live and persists it to config.
func (w *Watcher) SetInterval(seconds float64) {
	w.mu.Lock()
	w.interval = time.Duration(seconds * float64(time.Second))
	w.Config.PollInterval = seconds
	w.mu.Unlock()
	w.wake()
	log.Printf("Poll interval set to %gs", seconds)
	w.persistConfig()
}

// SetToken swaps the stored API token and persists it. Used by the tray "Reset
// token" action; the live HTTP client is updated separately by the caller.
func (w *Watcher) SetToken(token string) {
	w.mu.Lock()
	w.Config.Token = token
	w.mu.Unlock()
	w.persistConfig()
}

// persistConfig writes the current config to disk under a dedicated mutex so
// only one writer runs at a time. It snapshots the Config under w.mu (never
// handing the live, concurrently-mutated struct to the JSON marshaler) and
// SaveConfig itself is atomic, so the on-disk file always reflects a coherent,
// most-recent state without lost updates.
func (w *Watcher) persistConfig() {
	w.persistMu.Lock()
	defer w.persistMu.Unlock()

	path, err := GetConfigPath()
	if err != nil {
		log.Printf("Could not resolve config path: %v", err)
		return
	}
	w.mu.Lock()
	snapshot := *w.Config
	w.mu.Unlock()

	if err := SaveConfig(path, &snapshot); err != nil {
		log.Printf("Could not persist config: %v", err)
	}
}

func (w *Watcher) currentInterval() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.interval <= 0 {
		return time.Duration(DefaultPollInterval * float64(time.Second))
	}
	return w.interval
}

// wake nudges the loop to re-read pause/interval immediately.
func (w *Watcher) wake() {
	select {
	case w.reset <- struct{}{}:
	default:
	}
}

// Start runs the polling loop until the process exits. It reacts live to
// SetPaused/SetInterval via the reset channel, runs interactive pulls requested
// via pullReq, and does the periodic two-way check on its own timer.
//
// Every pull action (dialogs, downloads, writes, sync_state and map updates)
// runs here on this single goroutine, exactly like the scan, so the watcher
// maps stay lock-free. The tray only ever nudges channels; native dialogs are
// separate processes that block this goroutine while open, which is desired —
// no upload or write races a pending user confirmation.
func (w *Watcher) Start() {
	w.mu.Lock()
	poll := w.Config.PollInterval
	w.mu.Unlock()
	log.Printf("Watching directory: %s -> %s (poll interval: %.1fs, mode: %s)", w.SavesDir(), w.Config.URL, poll, w.SyncMode())
	w.status(StateSyncing, "Watching for changes")

	w.scanIfActive()

	// Kick off the startup pull check for two-way mode.
	if w.SyncMode() == SyncModeTwoWay {
		w.RequestPull()
	}

	// Startup self-update check (log-only on failure; never latches).
	w.checkForUpdate()

	syncTimer := time.NewTimer(syncCheckInterval)
	defer syncTimer.Stop()
	updateTimer := time.NewTimer(updateCheckInterval)
	defer updateTimer.Stop()

	for {
		scanTimer := time.NewTimer(w.currentInterval())
		select {
		case <-scanTimer.C:
			w.scanIfActive()
		case <-w.pullReq:
			scanTimer.Stop()
			w.runPull()
		case <-w.updateReq:
			scanTimer.Stop()
			w.runUpdate()
		case dir := <-w.changeDir:
			scanTimer.Stop()
			if w.applyDirChange(dir) {
				w.scanIfActive() // pick up the new folder right away
			}
		case <-syncTimer.C:
			scanTimer.Stop()
			w.periodicSyncCheck()
			syncTimer.Reset(syncCheckInterval)
		case <-updateTimer.C:
			scanTimer.Stop()
			w.checkForUpdate()
			updateTimer.Reset(updateCheckInterval)
		case <-w.reset:
			scanTimer.Stop()
		}
	}
}

func (w *Watcher) scanIfActive() {
	if w.Paused() {
		return
	}
	w.Scan()
}

// applyDirChange re-targets the watcher at a new saves folder. It runs on the
// scan goroutine (delivered via the changeDir channel), so the lock-free scan
// maps can be reset here without racing an in-flight scan, and a pull opened
// against the old folder finishes before the swap takes effect. The new path is
// validated (it must be an existing directory); an invalid or unchanged path is
// left as a no-op and the current folder is kept. It reports whether the folder
// actually changed, so the caller can trigger an immediate rescan.
//
// The maps are keyed by absolute path under the OLD folder — those entries will
// never be seen again, so they are cleared; a same-named file in the NEW folder
// then goes through debounce + upload from scratch. sync_state (keyed by
// basename) is deliberately left intact: the sha decision matrix will treat a
// same-named but divergent file in the new folder as a conflict, never a clobber.
func (w *Watcher) applyDirChange(dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false
	}
	old := w.SavesDir()
	if filepath.Clean(dir) == filepath.Clean(old) {
		return false // same folder, nothing to do
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		log.Printf("Ignoring saves folder change: %q is not an accessible directory: %v", dir, err)
		return false
	}

	w.mu.Lock()
	w.Config.SavesDir = dir
	w.mu.Unlock()

	clear(w.Pending)
	clear(w.Uploaded)
	clear(w.lastLine)
	// The new folder is a fresh world — old-folder errors are meaningless. Drop
	// the whole latch so it can't repaint red under the "Saves folder changed"
	// line; any real problem re-latches on the next cycle.
	w.clearErrs()

	log.Printf("Saves folder changed: %s -> %s", old, dir)
	w.persistConfig()

	w.setNewer(0) // the old folder's newer-count is meaningless for the new one
	w.status(StateSyncing, "Saves folder changed")
	return true
}

// Scan performs a single directory scan.
func (w *Watcher) Scan() {
	// Bracket the scan as a transfer scope: the first upload/sidecar carry flips
	// the tray to the activity icon, restored when the scan returns. A scan that
	// sends nothing never touches the icon.
	w.enterTransfers()
	defer w.leaveTransfers()

	// Snapshot the target once so the whole scan sees a single consistent folder
	// even if a change lands between iterations of the outer loop.
	savesDir := w.SavesDir()

	// Flush any sidecar batch that failed on a previous scan before looking at the
	// saves again. This never touches the .d2s upload flow.
	if w.MapSyncEnabled() {
		w.retryPendingSidecars()
	}

	entries, err := os.ReadDir(savesDir)
	if err != nil {
		log.Printf("Error reading saves directory %s: %v", savesDir, err)
		w.status(StateError, "Cannot read saves folder")
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".d2s" && ext != ".d2i" {
			continue
		}

		path := filepath.Join(savesDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		currentStat := FileStat{
			Mtime: info.ModTime(),
			Size:  info.Size(),
		}

		// Debounce: skip if file is new or modified since last check
		pending, exists := w.Pending[path]
		if !exists || !pending.Mtime.Equal(currentStat.Mtime) || pending.Size != currentStat.Size {
			w.Pending[path] = currentStat
			continue
		}

		bytes, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			log.Printf("Error reading file %s: %v", entry.Name(), err)
			continue
		}

		var valid bool
		if ext == ".d2i" {
			valid = validStash(bytes)
		} else {
			valid = validSave(bytes)
		}

		if !valid {
			continue
		}

		hash := sha256.Sum256(bytes)
		sha256Hex := hex.EncodeToString(hash[:])

		if w.Uploaded[path] == sha256Hex {
			continue
		}

		w.upload(entry.Name(), path, bytes, sha256Hex)
	}
}

func (w *Watcher) upload(filename, path string, bytes []byte, sha256Hex string) {
	w.noteTransfer() // a real upload is starting — show the activity icon
	resp, err := w.Client.UploadSnapshot(filename, bytes, sha256Hex, w.Machine)
	if err != nil {
		w.logOnce(path, fmt.Sprintf("%s: network failure (%v); will retry in next scan", filename, err))
		w.status(StateError, "Network failure — will retry")
		return
	}

	if resp.Error != "" {
		w.logOnce(path, fmt.Sprintf("%s: ERROR — %s", filename, resp.Error))
		w.status(StateError, filename+": "+resp.Error)
		return
	}

	w.Uploaded[path] = sha256Hex
	// Record the confirmed sync so two-way mode can tell a plain local edit
	// apart from a genuine conflict later. Harmless in push-only mode.
	w.recordSync(filename, sha256Hex)

	if resp.SharedStash != nil {
		w.logOnce(path, fmt.Sprintf("%s: %s (stash %s, %d items)", filename, resp.Status, resp.SharedStash.Mode, resp.SharedStash.ItemCount))
	} else if resp.Character != nil {
		w.logOnce(path, fmt.Sprintf("%s: %s (%s lvl %d)", filename, resp.Status, resp.Character.Name, resp.Character.Level))
	} else {
		w.logOnce(path, fmt.Sprintf("%s: %s", filename, resp.Status))
	}
	w.status(StateSyncing, "Synced "+filename)

	// A character save carries map-exploration sidecars; push them alongside it.
	// Shared stashes (resp.Character == nil) have none.
	if resp.Character != nil && w.MapSyncEnabled() {
		w.pushSidecars(sidecarBase(filename), resp.Character.Name)
	}
}

// logOnce logs a per-file line only when it differs from the last line logged
// for that file. A persistent, unchanged condition (e.g. an invalid token that
// fails every poll) is logged once instead of flooding the file every scan;
// the next distinct outcome (recovery, a new error) logs again. Runs on the
// single scan goroutine, so the map needs no lock.
func (w *Watcher) logOnce(path, line string) {
	if w.lastLine[path] == line {
		return
	}
	w.lastLine[path] = line
	log.Print(line)
}

// sidecarBase returns the character base name a set of sidecars shares — the
// filename with its extension stripped (e.g. "Mira.d2s" -> "Mira").
func sidecarBase(filename string) string {
	return strings.TrimSuffix(filename, filepath.Ext(filename))
}

// localSidecar is one map-exploration file found beside a character save.
type localSidecar struct {
	name string // basename, e.g. "Mira.map"
	sha  string
	data []byte
}

// collectSidecars returns the map-exploration sidecars in savesDir that belong to
// base: exactly "<base>.map" and "<base>.ma<one digit>" (extension compared
// case-insensitively, like the scan). These are local files we own, so no hostile
// sanitization is needed — the match is by exact stem plus a valid sidecar ext.
func collectSidecars(savesDir, base string) ([]localSidecar, error) {
	entries, err := os.ReadDir(savesDir)
	if err != nil {
		return nil, err
	}
	var out []localSidecar
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := filepath.Ext(name)
		if !validSidecarExt(ext) || name[:len(name)-len(ext)] != base {
			continue
		}
		data, err := os.ReadFile(filepath.Join(savesDir, name))
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		out = append(out, localSidecar{name: name, sha: hex.EncodeToString(sum[:]), data: data})
	}
	return out, nil
}

// pushSidecars uploads the map-exploration sidecars sitting beside a character
// save that was just synced. It sends only the files whose bytes are not already
// recorded as synced, in one batch keyed by the server's character name. On
// success each file's sha is recorded; on any failure the character is queued for
// retry on the next scan. It never affects the .d2s upload outcome.
func (w *Watcher) pushSidecars(base, charName string) {
	sidecars, err := collectSidecars(w.SavesDir(), base)
	if err != nil {
		w.markSidecarRetry(base, charName)
		return
	}
	var files []SidecarFile
	for _, sc := range sidecars {
		if strings.EqualFold(w.lastSyncSHA(sc.name), sc.sha) {
			continue // already on the server
		}
		files = append(files, SidecarFile{
			Filename:  sc.name,
			SHA256:    sc.sha,
			RawBase64: base64.StdEncoding.EncodeToString(sc.data),
		})
	}
	if len(files) == 0 {
		delete(w.pendingSidecars, base) // nothing outstanding for this character
		return
	}

	w.noteTransfer() // sidecar bytes are actually going out now
	resp, err := w.Client.UploadSidecars(charName, files, w.Machine)
	if err != nil {
		w.logOnce("__sidecar_push__"+base, base+": map sidecar upload failed ("+err.Error()+"); will retry")
		w.markSidecarRetry(base, charName)
		return
	}
	if resp.Error != "" {
		w.logOnce("__sidecar_push__"+base, base+": map sidecar upload rejected — "+resp.Error+"; will retry")
		w.markSidecarRetry(base, charName)
		return
	}

	for _, f := range files {
		w.recordSync(f.Filename, f.SHA256)
	}
	delete(w.pendingSidecars, base)
	w.logOnce("__sidecar_push__"+base, fmt.Sprintf("%s: synced %d map sidecar(s)", base, len(files)))
}

// retryPendingSidecars re-attempts every queued sidecar batch. It snapshots the
// keys first so pushSidecars can mutate the map (clear on success, re-queue on
// failure) without disturbing the range.
func (w *Watcher) retryPendingSidecars() {
	if len(w.pendingSidecars) == 0 {
		return
	}
	pending := make(map[string]string, len(w.pendingSidecars))
	for base, name := range w.pendingSidecars {
		pending[base] = name
	}
	for base, name := range pending {
		w.pushSidecars(base, name)
	}
}

// markSidecarRetry queues a character's sidecars for a later retry, lazily
// initializing the map so a struct-literal Watcher (tests) stays safe.
func (w *Watcher) markSidecarRetry(base, charName string) {
	if w.pendingSidecars == nil {
		w.pendingSidecars = make(map[string]string)
	}
	w.pendingSidecars[base] = charName
}

// validSave validates a .d2s character file using signature and size header check.
func validSave(bytes []byte) bool {
	if len(bytes) < 12 {
		return false
	}
	sig := binary.LittleEndian.Uint32(bytes[0:4])
	if sig != 0xAA55AA55 {
		return false
	}
	fileSize := binary.LittleEndian.Uint32(bytes[8:12])
	return fileSize == uint32(len(bytes))
}

// validStash validates a .d2i shared stash file checking signatures and chained tab sizes.
func validStash(bytes []byte) bool {
	offset := 0
	length := len(bytes)
	if length == 0 {
		return false
	}
	for offset < length {
		if offset+4 > length {
			return false
		}
		sig := binary.LittleEndian.Uint32(bytes[offset : offset+4])
		if sig != 0xAA55AA55 {
			return false
		}
		if offset+18 > length {
			return false
		}
		tabSize := int(binary.LittleEndian.Uint16(bytes[offset+16 : offset+18]))
		if tabSize < 64 {
			return false
		}
		offset += tabSize
	}
	return offset == length && offset > 0
}

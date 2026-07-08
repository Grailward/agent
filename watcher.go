package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
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

	mu         sync.Mutex
	paused     bool
	interval   time.Duration
	newerCount int
	reset      chan struct{} // wakes the loop when interval/pause changes
	pullReq    chan struct{} // requests an interactive pull on the scan goroutine
	changeDir  chan string   // requests a live saves-folder swap on the scan goroutine
	onStatus   StatusFunc
	onNewer    NewerFunc

	// persistMu serializes config.json writes so concurrent preference changes
	// (interval / sync mode / token, driven from different tray goroutines)
	// can't interleave or lose one another's update.
	persistMu sync.Mutex

	// Test seams for the interactive/OS-dependent pieces of the pull; default
	// to the real platform implementations in NewWatcher.
	confirmPull     func(message string) (bool, error)
	resolveConflict func(filename string) (ConflictChoice, error)
	gameRunning     func(savesDir string, before time.Time) (bool, string)
}

func NewWatcher(cfg *Config, client *Client) (*Watcher, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	statePath, _ := SyncStatePath()
	return &Watcher{
		Config:          cfg,
		Client:          client,
		Pending:         make(map[string]FileStat),
		Uploaded:        make(map[string]string),
		lastLine:        make(map[string]string),
		Machine:         hostname,
		syncState:       LoadSyncState(statePath),
		interval:        time.Duration(cfg.PollInterval * float64(time.Second)),
		reset:           make(chan struct{}, 1),
		pullReq:         make(chan struct{}, 1),
		changeDir:       make(chan string, 1),
		confirmPull:     ConfirmPull,
		resolveConflict: ResolveConflict,
		gameRunning:     GameLikelyRunning,
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
	w.mu.Unlock()
	if f != nil {
		f(state, message)
	}
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

// SyncMode returns the current sync mode (push or two_way).
func (w *Watcher) SyncMode() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.Config.SyncMode == SyncModeTwoWay {
		return SyncModeTwoWay
	}
	return SyncModePush
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
	w.persistConfig()
	if mode == SyncModeTwoWay {
		w.RequestPull()
	} else {
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

	syncTimer := time.NewTimer(syncCheckInterval)
	defer syncTimer.Stop()

	for {
		scanTimer := time.NewTimer(w.currentInterval())
		select {
		case <-scanTimer.C:
			w.scanIfActive()
		case <-w.pullReq:
			scanTimer.Stop()
			w.runPull()
		case dir := <-w.changeDir:
			scanTimer.Stop()
			if w.applyDirChange(dir) {
				w.scanIfActive() // pick up the new folder right away
			}
		case <-syncTimer.C:
			scanTimer.Stop()
			w.periodicSyncCheck()
			syncTimer.Reset(syncCheckInterval)
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

	log.Printf("Saves folder changed: %s -> %s", old, dir)
	w.persistConfig()

	w.setNewer(0) // the old folder's newer-count is meaningless for the new one
	w.status(StateSyncing, "Saves folder changed")
	return true
}

// Scan performs a single directory scan.
func (w *Watcher) Scan() {
	// Snapshot the target once so the whole scan sees a single consistent folder
	// even if a change lands between iterations of the outer loop.
	savesDir := w.SavesDir()
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

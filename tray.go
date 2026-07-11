package main

import (
	"fmt"
	"log"
	"strings"

	"fyne.io/systray"
)

// Tray icon bytes (iconSyncing, iconPaused, iconError) are defined in icon.go,
// which encodes them per platform (raw PNG on macOS, ICO on Windows).

// pollPresets are the poll intervals offered in the tray submenu.
var pollPresets = []struct {
	Label   string
	Seconds float64
}{
	{"2 seconds", 2},
	{"5 seconds", 5},
	{"15 seconds", 15},
	{"1 minute", 60},
}

// tray holds the menu items whose state changes at runtime.
type tray struct {
	watcher *Watcher
	client  *Client

	mToggle     *systray.MenuItem
	mResetTok   *systray.MenuItem
	mPolls      []*systray.MenuItem
	mPull       *systray.MenuItem
	mModePush   *systray.MenuItem
	mModeTwoWay *systray.MenuItem
	mMapSync    *systray.MenuItem
}

// RunTray takes over the main thread and runs the menu-bar UI. The watcher
// polls in a background goroutine; the tray drives pause/interval/token.
func RunTray(w *Watcher, c *Client) {
	t := &tray{watcher: w, client: c}
	systray.Run(t.onReady, func() {})
}

func (t *tray) onReady() {
	systray.SetTitle("")
	systray.SetTooltip("Grailward Agent " + Version)

	t.mToggle = systray.AddMenuItem("Pause sync", "Pause or resume watching for save changes")

	// Poll interval submenu.
	mPoll := systray.AddMenuItem("Poll interval", "How often to scan the saves folder")
	for _, p := range pollPresets {
		item := mPoll.AddSubMenuItemCheckbox(p.Label, "", p.Seconds == t.watcher.Config.PollInterval)
		t.mPolls = append(t.mPolls, item)
	}

	// Sync mode selector (radio). Push-only never writes to disk; two-way
	// additionally offers to pull newer server saves, always with confirmation.
	mMode := systray.AddMenuItem("Sync mode", "Choose how saves are reconciled")
	twoWay := t.watcher.SyncMode() == SyncModeTwoWay
	t.mModePush = mMode.AddSubMenuItemCheckbox("Push only", "Only upload local changes; never write to disk", !twoWay)
	t.mModeTwoWay = mMode.AddSubMenuItemCheckbox("Two-way", "Also offer to pull newer server saves (with confirmation)", twoWay)

	// Map-exploration sidecar sync (default ON): carry each character's
	// explored-map files (fog of war) across machines alongside the save.
	t.mMapSync = systray.AddMenuItemCheckbox("Sync map exploration",
		"Also sync each character's explored-map files across machines",
		t.watcher.MapSyncEnabled())

	// On-demand pull; only meaningful (and shown) in two-way mode.
	t.mPull = systray.AddMenuItem("Pull latest now", "Check the server for newer saves and pull them")
	if !twoWay {
		t.mPull.Hide()
	}

	systray.AddSeparator()
	// Saves folder submenu: open the watched folder, or switch to a different one.
	mSaves := systray.AddMenuItem("Saves folder", "The folder the agent watches")
	mOpenFolder := mSaves.AddSubMenuItem("Open folder", "Reveal the watched folder")
	mChangeFolder := mSaves.AddSubMenuItem("Change folder", "Watch a different saves folder")
	mLogs := systray.AddMenuItem("Open logs", "Open the agent log file")
	systray.AddSeparator()
	t.mResetTok = systray.AddMenuItem("Reset token", "Clear the stored token and enter a new one")
	mAbout := systray.AddMenuItem("About Grailward Agent", "Version and configuration details")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Stop the agent")

	// Wire the watcher's status + newer-count into the tray, then start polling.
	t.watcher.SetStatusFunc(t.render)
	t.watcher.SetNewerFunc(t.renderNewer)
	go t.watcher.Start()

	// Event loop: systray channels fire on menu clicks.
	go func() {
		for {
			select {
			case <-t.mToggle.ClickedCh:
				t.watcher.SetPaused(!t.watcher.Paused())
			case <-t.mPull.ClickedCh:
				t.watcher.RequestPull()
			case <-t.mModePush.ClickedCh:
				t.setMode(SyncModePush)
			case <-t.mModeTwoWay.ClickedCh:
				t.setMode(SyncModeTwoWay)
			case <-t.mMapSync.ClickedCh:
				t.toggleMapSync()
			case <-mOpenFolder.ClickedCh:
				if err := OpenPath(t.watcher.SavesDir()); err != nil {
					log.Printf("Could not open saves folder: %v", err)
				}
			case <-mChangeFolder.ClickedCh:
				t.changeSavesDir()
			case <-mLogs.ClickedCh:
				if path, err := LogPath(); err == nil {
					if err := OpenPath(path); err != nil {
						log.Printf("Could not open logs: %v", err)
					}
				}
			case <-mAbout.ClickedCh:
				t.showAbout()
			case <-t.mResetTok.ClickedCh:
				t.resetToken()
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()

	// Poll preset clicks — one goroutine per item.
	for i, item := range t.mPolls {
		go func(idx int, mi *systray.MenuItem) {
			for range mi.ClickedCh {
				t.watcher.SetInterval(pollPresets[idx].Seconds)
				t.syncPollChecks(idx)
			}
		}(i, item)
	}
}

// render reflects a watcher state change onto the icon + status line. It runs
// on the watcher's goroutine, but systray setters are safe to call from anywhere.
func (t *tray) render(state State, message string) {
	switch state {
	case StatePaused:
		systray.SetIcon(iconPaused)
		t.mToggle.SetTitle("Resume sync")
	case StateError:
		systray.SetIcon(iconError)
		t.mToggle.SetTitle("Pause sync")
	default: // StateSyncing
		systray.SetIcon(iconSyncing)
		t.mToggle.SetTitle("Pause sync")
	}
	systray.SetTooltip(fmt.Sprintf("Grailward Agent %s — %s", Version, message))
}

// showAbout builds the About panel text from the live config and shows it in a
// native OK dialog (osascript on macOS, a PowerShell MessageBox on Windows).
func (t *tray) showAbout() {
	configDir, err := ConfigDir()
	if err != nil {
		configDir = "(unknown)"
	}
	msg := aboutText(Version, t.watcher.Config.URL, t.watcher.SavesDir(), t.watcher.SyncMode(), configDir)
	if err := ShowAbout(msg); err != nil {
		log.Printf("Could not show About dialog: %v", err)
	}
}

// aboutText renders the About panel body. Pure (no I/O) so it is unit-testable;
// the version already carries its own "v" prefix on release builds ("dev"
// locally). The trailing line is a required unaffiliated-fan-tool disclaimer.
func aboutText(version, serverURL, savesDir, syncMode, configDir string) string {
	mode := "Push only"
	if syncMode == SyncModeTwoWay {
		mode = "Two-way"
	}
	return fmt.Sprintf(
		"Grailward Agent %s\n"+
			"Server: %s\n"+
			"Saves folder: %s\n"+
			"Sync mode: %s\n"+
			"Config & logs: %s\n\n"+
			"Unofficial fan-made tool — not affiliated with Blizzard Entertainment.",
		version, serverURL, savesDir, mode, configDir)
}

// renderNewer updates the "Pull latest now" label with the count of newer
// server saves. Runs on the watcher goroutine; systray setters are safe there.
func (t *tray) renderNewer(count int) {
	if t.mPull == nil {
		return
	}
	if count > 0 {
		t.mPull.SetTitle(fmt.Sprintf("Pull latest now (%d newer)", count))
	} else {
		t.mPull.SetTitle("Pull latest now")
	}
}

// setMode switches the sync mode, updates the radio checks and the visibility
// of the pull item.
func (t *tray) setMode(mode string) {
	t.watcher.SetSyncMode(mode)
	if mode == SyncModeTwoWay {
		t.mModeTwoWay.Check()
		t.mModePush.Uncheck()
		t.mPull.Show()
	} else {
		t.mModePush.Check()
		t.mModeTwoWay.Uncheck()
		t.mPull.Hide()
	}
}

// toggleMapSync flips the map-exploration sidecar sync, persists it via the
// watcher, and reflects the new state on the checkbox.
func (t *tray) toggleMapSync() {
	on := !t.mMapSync.Checked()
	t.watcher.SetMapSync(on)
	if on {
		t.mMapSync.Check()
	} else {
		t.mMapSync.Uncheck()
	}
}

func (t *tray) syncPollChecks(active int) {
	for i, item := range t.mPolls {
		if i == active {
			item.Check()
		} else {
			item.Uncheck()
		}
	}
}

// changeSavesDir opens the native folder picker (defaulting to the current
// folder) and hands the choice to the watcher, which validates and re-targets
// live on its own goroutine. A cancelled/empty pick is a no-op. This runs on the
// tray event goroutine, like resetToken, so the blocking picker holds up only
// the menu — never the scan loop.
func (t *tray) changeSavesDir() {
	dir, err := PromptSavesDir(t.watcher.SavesDir())
	if err != nil {
		log.Printf("Saves folder picker failed: %v", err)
		return
	}
	if strings.TrimSpace(dir) == "" {
		return // cancelled
	}
	t.watcher.ChangeSavesDir(dir)
}

// resetToken prompts for a new token, persists it (serialized with the other
// preference writes), and rewires the live client. On cancel the old token
// stays in place.
func (t *tray) resetToken() {
	token, err := PromptToken(t.watcher.Config.URL)
	if err != nil {
		log.Printf("Token reset failed: %v", err)
		return
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return // cancelled
	}
	t.watcher.SetToken(token)
	t.client.SetToken(token)
	log.Println("Token updated via tray")
	t.render(StateSyncing, "Token updated")
}

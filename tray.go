package main

import (
	_ "embed"
	"log"

	"fyne.io/systray"
)

//go:embed icons/syncing.png
var iconSyncing []byte

//go:embed icons/paused.png
var iconPaused []byte

//go:embed icons/error.png
var iconError []byte

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

	mToggle   *systray.MenuItem
	mResetTok *systray.MenuItem
	mPolls    []*systray.MenuItem
}

// RunTray takes over the main thread and runs the menu-bar UI. The watcher
// polls in a background goroutine; the tray drives pause/interval/token.
func RunTray(w *Watcher, c *Client) {
	t := &tray{watcher: w, client: c}
	systray.Run(t.onReady, func() {})
}

func (t *tray) onReady() {
	systray.SetTitle("")
	systray.SetTooltip("Grailward Agent")

	t.mToggle = systray.AddMenuItem("Pause sync", "Pause or resume watching for save changes")

	// Poll interval submenu.
	mPoll := systray.AddMenuItem("Poll interval", "How often to scan the saves folder")
	for _, p := range pollPresets {
		item := mPoll.AddSubMenuItemCheckbox(p.Label, "", p.Seconds == t.watcher.Config.PollInterval)
		t.mPolls = append(t.mPolls, item)
	}

	systray.AddSeparator()
	mOpen := systray.AddMenuItem("Open saves folder", "Reveal the watched folder")
	mLogs := systray.AddMenuItem("Open logs", "Open the agent log file")
	t.mResetTok = systray.AddMenuItem("Reset token…", "Clear the stored token and enter a new one")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Stop the agent")

	// Wire the watcher's status into the tray, then start polling.
	t.watcher.SetStatusFunc(t.render)
	go t.watcher.Start()

	// Event loop: systray channels fire on menu clicks.
	go func() {
		for {
			select {
			case <-t.mToggle.ClickedCh:
				t.watcher.SetPaused(!t.watcher.Paused())
			case <-mOpen.ClickedCh:
				if err := OpenPath(t.watcher.Config.SavesDir); err != nil {
					log.Printf("Could not open saves folder: %v", err)
				}
			case <-mLogs.ClickedCh:
				if path, err := LogPath(); err == nil {
					if err := OpenPath(path); err != nil {
						log.Printf("Could not open logs: %v", err)
					}
				}
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
	systray.SetTooltip("Grailward Agent — " + message)
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

// resetToken clears the stored token, prompts for a new one and rewires the
// client. On cancel the old token stays in place.
func (t *tray) resetToken() {
	token, err := PromptAndSaveToken(t.watcher.Config)
	if err != nil {
		log.Printf("Token reset failed: %v", err)
		return
	}
	if token == "" {
		return // cancelled
	}
	t.client.SetToken(token)
	log.Println("Token updated via tray")
	t.render(StateSyncing, "Token updated")
}

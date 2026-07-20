//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Linux platform support. One linux/amd64 build covers desktop Linux and the Steam
// Deck (SteamOS is x86_64 Arch with KDE Plasma in Desktop Mode). Native dialogs go
// through kdialog (KDE, guaranteed on the Deck) with a zenity (GNOME) fallback;
// start-at-login is an XDG autostart .desktop entry; the update is a raw-binary swap.
// This file holds only the OS-touching wrappers — the escaping, ranking, and dialog
// argument/result logic live in build-tag-free files so they are unit-testable on
// any host.

// ---------------------------------------------------------------------------
// Native dialogs (kdialog first, then zenity)
// ---------------------------------------------------------------------------

// linuxDialogTool identifies which native dialog helper is available.
type linuxDialogTool int

const (
	dialogNone  linuxDialogTool = iota // neither kdialog nor zenity on PATH
	dialogKDE                          // kdialog
	dialogGNOME                        // zenity
)

// detectDialogTool picks kdialog first (present on the Steam Deck's KDE Plasma),
// then zenity, in PATH-presence order. dialogNone means no native dialog is
// available — callers then take their conservative default.
func detectDialogTool() (linuxDialogTool, string) {
	if path, err := exec.LookPath("kdialog"); err == nil {
		return dialogKDE, path
	}
	if path, err := exec.LookPath("zenity"); err == nil {
		return dialogGNOME, path
	}
	return dialogNone, ""
}

// runDialog runs a dialog helper and reports its stdout, exit code, and whether the
// process ran at all. A clean exit is code 0; a button/cancel that exits non-zero is
// captured as its exit code (not an error); only a failure to start the process
// leaves ran=false, which every caller treats as its conservative default.
func runDialog(name string, args ...string) (stdout string, exitCode int, ran bool) {
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err == nil {
		return out.String(), 0, true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), ee.ExitCode(), true
	}
	return out.String(), -1, false
}

// linuxConfirm shows a two-button yes/no dialog and returns true only when the user
// chooses the affirmative (okLabel). A missing dialog tool or any dismissal returns
// false — the conservative default shared by all confirmation callers.
func linuxConfirm(title, message, okLabel, cancelLabel string) bool {
	switch tool, path := detectDialogTool(); tool {
	case dialogKDE:
		_, code, ran := runDialog(path, "--title", title, "--yesno", message,
			"--yes-label", okLabel, "--no-label", cancelLabel)
		return ran && code == 0
	case dialogGNOME:
		_, code, ran := runDialog(path, "--question", "--title", title, "--text", message,
			"--ok-label", okLabel, "--cancel-label", cancelLabel)
		return ran && code == 0
	default:
		return false
	}
}

// ConfirmUpdate shows a two-button confirmation before applying an update
// (Update = apply, Later = postpone). Returns true only when the user chooses
// Update; a missing dialog or dismissal is Later.
func ConfirmUpdate(version string) (bool, error) {
	msg := fmt.Sprintf("A new version of Grailward Agent (%s) is available.\n\nGrailward will download it, replace this copy, and restart. Update now?", version)
	return linuxConfirm("Grailward Agent — Update", msg, "Update", "Later"), nil
}

// ConfirmUpdateFound shows the confirmation for a user-initiated check that found a
// newer version. Its "Update now?" IS the apply confirmation, so the manual flow
// never opens a second dialog. Returns true only when the user chooses Update.
func ConfirmUpdateFound(newVer, curVer string) (bool, error) {
	msg := fmt.Sprintf("Version %s is available (you have %s). Update now?", newVer, curVer)
	return linuxConfirm("Grailward Agent — Update", msg, "Update", "Later"), nil
}

// ConfirmPull shows a two-button confirmation for a batch pull (Pull = proceed,
// Skip = do nothing). Returns true only when the user chooses Pull.
func ConfirmPull(message string) (bool, error) {
	return linuxConfirm("Grailward Agent", message, "Pull", "Skip"), nil
}

// ShowAbout displays an informational dialog with a single OK button. A missing
// dialog tool is reported as an error (there is nothing to show), consistent with
// the dialog-failure semantics on the other platforms.
func ShowAbout(message string) error {
	switch tool, path := detectDialogTool(); tool {
	case dialogKDE:
		if _, _, ran := runDialog(path, "--title", "Grailward Agent", "--msgbox", message); !ran {
			return fmt.Errorf("could not show dialog via kdialog")
		}
		return nil
	case dialogGNOME:
		if _, _, ran := runDialog(path, "--info", "--title", "Grailward Agent", "--text", message); !ran {
			return fmt.Errorf("could not show dialog via zenity")
		}
		return nil
	default:
		return fmt.Errorf("no dialog tool available (install kdialog or zenity)")
	}
}

// ResolveConflict shows a three-button choice for a conflicted file. The argument
// construction and the (exit code, stdout) → choice mapping are the pure helpers in
// dialogtools.go; here they are only wired to the detected tool. A missing tool or a
// dismissal is the conservative Skip.
func ResolveConflict(filename string) (ConflictChoice, error) {
	const title = "Grailward Agent — Conflict"
	message := conflictMessage(filename)
	switch tool, path := detectDialogTool(); tool {
	case dialogKDE:
		_, code, ran := runDialog(path, kdialogConflictArgs(title, message)...)
		if !ran {
			return ConflictSkip, nil
		}
		return interpretKdialogConflict(code), nil
	case dialogGNOME:
		out, code, ran := runDialog(path, zenityConflictArgs(title, message)...)
		if !ran {
			return ConflictSkip, nil
		}
		return interpretZenityConflict(code, out), nil
	default:
		return ConflictSkip, nil
	}
}

// PromptToken displays a text-input dialog for the API token. Cancelling (or an
// empty result the caller rejects) aborts setup like the other platforms. With no
// dialog tool at all, it returns a clear error telling the user to set the token in
// the config file instead.
func PromptToken(url string) (string, error) {
	prompt := "Please enter your Grailward API Token for: " + url
	switch tool, path := detectDialogTool(); tool {
	case dialogKDE:
		out, code, ran := runDialog(path, "--title", "Grailward Agent Setup", "--inputbox", prompt)
		if !ran || code != 0 {
			return "", fmt.Errorf("token input dialog canceled or failed")
		}
		return strings.TrimSpace(out), nil
	case dialogGNOME:
		out, code, ran := runDialog(path, "--entry", "--title", "Grailward Agent Setup", "--text", prompt)
		if !ran || code != 0 {
			return "", fmt.Errorf("token input dialog canceled or failed")
		}
		return strings.TrimSpace(out), nil
	default:
		return "", fmt.Errorf("no dialog tool available to prompt for the API token; install kdialog or zenity, or set the token in the config file")
	}
}

// PromptSavesDir displays a folder picker for the saves directory, starting at
// defaultPath (or the home directory when that path does not exist). Cancelling
// aborts setup; with no dialog tool it returns a clear error pointing the user at
// the config file.
func PromptSavesDir(defaultPath string) (string, error) {
	locPath := defaultPath
	if _, err := os.Stat(locPath); err != nil {
		if home, err := os.UserHomeDir(); err == nil {
			locPath = home
		}
	}
	const prompt = "Select Diablo II Resurrected Saved Games directory:"
	switch tool, path := detectDialogTool(); tool {
	case dialogKDE:
		out, code, ran := runDialog(path, "--title", prompt, "--getexistingdirectory", locPath)
		if !ran || code != 0 {
			return "", fmt.Errorf("folder picker canceled or failed")
		}
		return strings.TrimSpace(out), nil
	case dialogGNOME:
		start := locPath
		if start != "" && !strings.HasSuffix(start, "/") {
			start += "/" // a trailing slash tells zenity to open inside the directory
		}
		out, code, ran := runDialog(path, "--file-selection", "--directory", "--title", prompt, "--filename="+start)
		if !ran || code != 0 {
			return "", fmt.Errorf("folder picker canceled or failed")
		}
		return strings.TrimSpace(out), nil
	default:
		return "", fmt.Errorf("no dialog tool available to pick the saves folder; install kdialog or zenity, or set the saves directory in the config file")
	}
}

// OpenPath reveals a folder in the desktop's file manager via xdg-open.
func OpenPath(path string) error {
	return exec.Command("xdg-open", path).Start()
}

// ---------------------------------------------------------------------------
// Start at login (XDG autostart)
// ---------------------------------------------------------------------------

// autostartPath returns the per-user XDG autostart entry path
// (~/.config/autostart/grailward-agent.desktop). os.UserConfigDir honours
// XDG_CONFIG_HOME, so tests can redirect it under a temp directory.
func autostartPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autostart", "grailward-agent.desktop"), nil
}

// enableStartAtLogin writes the XDG autostart .desktop entry for execPath, creating
// the autostart directory if needed. Everything lives under the user's own config
// directory, so no elevated privilege is involved; the entry takes effect at the
// next login (the agent is already running, so nothing is launched now).
func enableStartAtLogin(execPath string) error {
	path, err := autostartPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(autostartDesktopEntry(execPath)), 0644)
}

// disableStartAtLogin removes the autostart entry. A missing file is not an error
// (the login item is already gone).
func disableStartAtLogin() error {
	path, err := autostartPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// startAtLoginExecPath returns the executable path currently registered in the
// autostart entry's Exec= line, or ("", false) when no entry exists or it can't be
// parsed.
func startAtLoginExecPath() (string, bool) {
	path, err := autostartPath()
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return parseAutostartExec(string(data))
}

// startAtLoginUpToDate reports whether the on-disk autostart entry is byte-for-byte
// what this build would write for execPath, mirroring the macOS plist heal. A
// missing or unreadable entry reads as not up to date so the startup self-heal
// recreates it.
func startAtLoginUpToDate(execPath string) bool {
	path, err := autostartPath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return autostartUpToDate(string(data), execPath)
}

// startAtLoginTarget describes where the login item lives — the autostart entry
// path — for the enable log line. Best effort: falls back to the bare filename when
// the config directory can't be resolved.
func startAtLoginTarget() string {
	if path, err := autostartPath(); err == nil {
		return path
	}
	return "grailward-agent.desktop"
}

// ---------------------------------------------------------------------------
// Self-update (raw binary swap)
// ---------------------------------------------------------------------------

// ApplyUpdate replaces the running Linux binary with the downloaded artifact and
// relaunches. The Linux manifest entry is the raw executable, and Linux permits
// renaming over a running file, so the new bytes are staged in the same directory
// and atomically renamed over execPath (no backup of the old binary, like the raw
// macOS path). On success this does not return — it re-execs and exits.
func ApplyUpdate(execPath string, file *UpdateFile, data []byte) error {
	dir := filepath.Dir(execPath)
	tmp, err := os.CreateTemp(dir, ".gw-update-*") // same volume => atomic rename
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0755); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, execPath); err != nil {
		os.Remove(tmpName)
		return err
	}

	log.Printf("Update applied; restarting %s", execPath)
	if err := exec.Command(execPath, os.Args[1:]...).Start(); err != nil {
		log.Printf("Relaunch failed (%v); please start the agent manually", err)
	}
	os.Exit(0)
	return nil // unreachable
}

// cleanUpdateLeftovers removes any stray staging files left beside the executable
// by a previous update (best-effort; called at startup). The Linux swap keeps no
// backup, so there is only the ".gw-update-*" staging temp to clear.
func cleanUpdateLeftovers(execPath string) {
	dir := filepath.Dir(execPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gw-update-") {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// ---------------------------------------------------------------------------
// Default saves directory + game guard
// ---------------------------------------------------------------------------

// GetDefaultSavesDir discovers the D2R saves directory by probing the paths a
// Steam/Proton, Lutris, or plain-Wine install of the Battle.net app leaves the
// "Saved Games/Diablo II Resurrected" folder at. Candidates are gathered in
// most-to-least-likely order (Steam prefix, then the Deck's SD card, then Lutris,
// then Wine) and pickSavesDir chooses the first that actually holds a save. Returns
// "" when nothing matches, in which case setup falls back to the folder picker.
func GetDefaultSavesDir() string {
	const tail = "Saved Games/Diablo II Resurrected"
	var patterns []string
	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		// 1. Battle.net run through Proton as a non-Steam game — native Steam, and
		//    Flatpak Steam (its sandbox puts XDG_DATA_HOME under ~/.var/app, common
		//    on Fedora and immutable desktops).
		patterns = append(patterns,
			filepath.Join(home, ".local/share/Steam/steamapps/compatdata/*/pfx/drive_c/users/steamuser", tail),
			filepath.Join(home, ".var/app/com.valvesoftware.Steam/data/Steam/steamapps/compatdata/*/pfx/drive_c/users/steamuser", tail))
	}
	// 2. A Proton prefix living on the Steam Deck's microSD card.
	patterns = append(patterns,
		filepath.Join("/run/media/*/steamapps/compatdata/*/pfx/drive_c/users/steamuser", tail),
		filepath.Join("/run/media/*/*/steamapps/compatdata/*/pfx/drive_c/users/steamuser", tail))
	if homeErr == nil {
		patterns = append(patterns,
			// 3. Lutris-managed Wine prefix.
			filepath.Join(home, "Games/*/drive_c/users/*", tail),
			// 4. Plain Wine prefix.
			filepath.Join(home, ".wine/drive_c/users/*", tail))
	}

	var candidates []string
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			continue // only ErrBadPattern, which these fixed patterns never are
		}
		candidates = append(candidates, matches...)
	}
	return pickSavesDir(candidates)
}

// GameLikelyRunning is the Linux reinforcement before a write (never a substitute
// for the confirmation): it looks for a D2R process — the Windows binary surfaces as
// "D2R.exe" in a Wine/Proton command line — and, failing that, for any save touched
// in the last ~90s (a likely active session). Detection being unavailable returns
// false; the dialog is the real barrier. before bounds the mtime heuristic to
// activity that predates the current pull run so the agent's own writes never
// self-trigger it. Identical in shape to the macOS guard.
func GameLikelyRunning(savesDir string, before time.Time) (bool, string) {
	if exec.Command("pgrep", "-f", gameProcessPattern).Run() == nil {
		return true, "a Diablo II: Resurrected process appears to be running"
	}
	if recentlySaved(savesDir, 90*time.Second, before) {
		return true, "a save changed in the last 90 seconds — a game session may be active"
	}
	return false, ""
}

//go:build darwin

package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// launchAgentPath returns the per-user LaunchAgent plist path
// (~/Library/LaunchAgents/com.grailward.agent.plist).
func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

// enableStartAtLogin writes the LaunchAgent plist for execPath, creating the
// LaunchAgents directory if needed. It deliberately does NOT `launchctl load` the
// plist: the agent is already running, so loading now would start a second copy.
// The login item takes effect at the next login. No elevated privilege is needed —
// everything lives under the user's own home.
func enableStartAtLogin(execPath string) error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(launchAgentPlist(execPath)), 0644)
}

// disableStartAtLogin removes the LaunchAgent plist and best-effort boots out any
// loaded instance. The bootout is only reinforcement (the removed plist is what
// stops the next login); its failure is ignored because nothing may be loaded.
func disableStartAtLogin() error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchAgentLabel)).Run()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// startAtLoginExecPath returns the executable path currently registered in the
// LaunchAgent plist, or ("", false) when no plist exists or it can't be parsed.
func startAtLoginExecPath() (string, bool) {
	path, err := launchAgentPath()
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return parseLaunchAgentExec(string(data))
}

// ConfirmUpdate shows a two-button confirmation before applying an update.
// Returns true only when the user explicitly chooses Update.
func ConfirmUpdate(version string) (bool, error) {
	msg := fmt.Sprintf("A new version of Grailward Agent (%s) is available.\n\nGrailward will download it, replace this copy, and restart. Update now?", version)
	script := fmt.Sprintf(`button returned of (display dialog "%s" with title "Grailward Agent — Update" buttons {"Later", "Update"} default button "Update" with icon note)`, escapeAppleScript(msg))
	out, err := runOsascript(script)
	if err != nil {
		return false, nil // dismissed == Later
	}
	return strings.TrimSpace(out) == "Update", nil
}

// ApplyUpdate replaces the running macOS copy with the downloaded artifact (the
// zipped .app) and relaunches. If the agent runs from inside a .app bundle the
// whole bundle is swapped and the previous bundle is kept as a one-level backup
// (Name.app.gw-backup, cleaned on the next start). A raw universal binary instead
// has its file replaced atomically, with no backup of the old binary. On success
// this does not return — it relaunches and exits.
func ApplyUpdate(execPath string, file *UpdateFile, data []byte) error {
	if bundle, ok := macUpdateTarget(execPath); ok {
		return applyMacBundleUpdate(bundle, data)
	}
	return applyMacRawUpdate(execPath, data)
}

// applyMacBundleUpdate swaps a whole .app bundle: unzip beside it (same volume),
// move the current bundle to a backup, move the new one into place, relaunch.
func applyMacBundleUpdate(bundlePath string, zipData []byte) error {
	parent := filepath.Dir(bundlePath)
	staging, err := os.MkdirTemp(parent, ".gw-update-*")
	if err != nil {
		return err
	}
	if err := unzipInto(zipData, staging); err != nil {
		os.RemoveAll(staging)
		return err
	}
	newApp, err := findDotApp(staging)
	if err != nil {
		os.RemoveAll(staging)
		return err
	}
	backup := bundlePath + ".gw-backup"
	_ = os.RemoveAll(backup)
	if err := os.Rename(bundlePath, backup); err != nil {
		os.RemoveAll(staging)
		return err
	}
	if err := os.Rename(newApp, bundlePath); err != nil {
		_ = os.Rename(backup, bundlePath) // best-effort restore
		os.RemoveAll(staging)
		return err
	}
	os.RemoveAll(staging)

	log.Printf("Update applied; relaunching %s", bundlePath)
	if err := exec.Command("open", bundlePath).Start(); err != nil {
		log.Printf("Relaunch failed (%v); please start Grailward Agent manually", err)
	}
	os.Exit(0)
	return nil // unreachable
}

// applyMacRawUpdate swaps a raw universal binary: extract the inner executable
// from the .app zip, atomically replace the running file, re-exec.
func applyMacRawUpdate(execPath string, zipData []byte) error {
	bin, err := extractBundleBinary(zipData)
	if err != nil {
		return err
	}
	dir := filepath.Dir(execPath)
	tmp, err := os.CreateTemp(dir, ".gw-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(bin); err != nil {
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

// unzipInto extracts a zip archive into dest, preserving file modes (notably the
// executable bit on the inner binary) and rejecting path-traversal entries.
func unzipInto(data []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	root := filepath.Clean(dest)
	for _, f := range zr.File {
		target := filepath.Join(dest, f.Name)
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path in archive: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := writeZipFile(f, target); err != nil {
			return err
		}
	}
	return nil
}

func writeZipFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(target, f.Mode())
}

// findDotApp returns the single .app directory at the top level of dir.
func findDotApp(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no .app bundle found in update archive")
}

// extractBundleBinary returns the bytes of the executable inside the .app zip
// (the first regular file under Contents/MacOS/).
func extractBundleBinary(zipData []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if strings.Contains(f.Name, "/Contents/MacOS/") {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("no Contents/MacOS/ binary found in update archive")
}

// cleanUpdateLeftovers removes the previous update's backup and any stray staging
// directories left beside the executable (best-effort; called at startup).
func cleanUpdateLeftovers(execPath string) {
	if bundle, ok := macUpdateTarget(execPath); ok {
		_ = os.RemoveAll(bundle + ".gw-backup")
		removeStagingDirs(filepath.Dir(bundle))
		return
	}
	removeStagingDirs(filepath.Dir(execPath))
}

func removeStagingDirs(dir string) {
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

// GetDefaultSavesDir returns the standard macOS D2R saves path via CrossOver.
func GetDefaultSavesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library/Application Support/CrossOver/Bottles/Battle.net Desktop App/drive_c/users/crossover/Saved Games/Diablo II Resurrected")
}

// OpenPath reveals a folder in Finder.
func OpenPath(path string) error {
	return exec.Command("open", path).Start()
}

// PromptToken displays an AppleScript text input dialog for the token.
func PromptToken(url string) (string, error) {
	escapedURL := strings.ReplaceAll(strings.ReplaceAll(url, "\\", "\\\\"), "\"", "\\\"")
	script := fmt.Sprintf(`text returned of (display dialog "Please enter your Grailward API Token for: %s" default answer "" with title "Grailward Agent Setup" with icon note buttons {"Cancel", "OK"} default button "OK")`, escapedURL)

	cmd := exec.Command("osascript", "-e", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("dialog canceled or failed: %w", err)
	}

	return strings.TrimSpace(out.String()), nil
}

// PromptSavesDir displays an AppleScript folder picker dialog.
func PromptSavesDir(defaultPath string) (string, error) {
	// If default path does not exist, use user home
	locPath := defaultPath
	if _, err := os.Stat(locPath); err != nil {
		if home, err := os.UserHomeDir(); err == nil {
			locPath = home
		}
	}

	escapedPath := strings.ReplaceAll(strings.ReplaceAll(locPath, "\\", "\\\\"), "\"", "\\\"")
	script := fmt.Sprintf(`POSIX path of (choose folder with prompt "Select Diablo II Resurrected Saved Games directory:" default location POSIX file "%s")`, escapedPath)

	cmd := exec.Command("osascript", "-e", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("folder picker canceled or failed: %w", err)
	}

	return strings.TrimSpace(out.String()), nil
}

// escapeAppleScript escapes a string for embedding in a double-quoted
// AppleScript literal, turning newlines into AppleScript line breaks so the
// dialog renders multi-line text without breaking the script.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\" & return & \"")
	return s
}

// runOsascript runs a one-line AppleScript and returns its stdout.
func runOsascript(script string) (string, error) {
	cmd := exec.Command("osascript", "-e", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// ShowAbout displays a simple informational dialog with a single OK button.
func ShowAbout(message string) error {
	script := fmt.Sprintf(`display dialog "%s" with title "Grailward Agent" buttons {"OK"} default button "OK" with icon note`, escapeAppleScript(message))
	_, err := runOsascript(script)
	return err
}

// ConfirmPull shows a two-button confirmation for a batch pull. It returns true
// only when the user explicitly chooses Pull; a dismissed dialog is a Skip.
func ConfirmPull(message string) (bool, error) {
	script := fmt.Sprintf(`button returned of (display dialog "%s" with title "Grailward Agent" buttons {"Skip", "Pull"} default button "Pull" with icon note)`, escapeAppleScript(message))
	out, err := runOsascript(script)
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(out) == "Pull", nil
}

// ResolveConflict shows a three-button choice for a conflicted file.
func ResolveConflict(filename string) (ConflictChoice, error) {
	script := fmt.Sprintf(`button returned of (display dialog "%s" with title "Grailward Agent — Conflict" buttons {"Skip", "Use server", "Keep local"} default button "Skip" with icon caution)`, escapeAppleScript(conflictMessage(filename)))
	out, err := runOsascript(script)
	if err != nil {
		return ConflictSkip, nil
	}
	switch strings.TrimSpace(out) {
	case "Keep local":
		return ConflictKeepLocal, nil
	case "Use server":
		return ConflictUseServer, nil
	default:
		return ConflictSkip, nil
	}
}

// gameProcessPattern matches the running D2R game binary in the CrossOver/Wine
// command line. It targets the real executable "D2R.exe" (an extended-regexp
// literal dot) and deliberately does NOT match the CrossOver engine helper
// "D2R-CXEngine", which can linger as a wine leftover after the game exits — a
// bare "D2R" match would take that as an open game and refuse writes forever.
// Passed verbatim to pgrep -f (POSIX ERE) and to gameProcessRe below (RE2); the
// pattern is simple enough to mean the same in both.
const gameProcessPattern = `D2R\.exe`

var gameProcessRe = regexp.MustCompile(gameProcessPattern)

// matchesGameProcess reports whether a process command line looks like the real
// D2R game binary. Kept pure and package-level so the exact pattern handed to
// pgrep is unit-testable without a running game.
func matchesGameProcess(cmdline string) bool {
	return gameProcessRe.MatchString(cmdline)
}

// GameLikelyRunning is a best-effort, macOS reinforcement before a write (never
// a substitute for the confirmation): it looks for a D2R process and, failing
// that, for any save touched in the last ~90s (a likely active session).
// Detection being unavailable returns false — the dialog is the real barrier.
//
// before bounds the mtime heuristic to activity that predates the current pull
// run, so the agent's own in-run writes (mtime >= before) never self-trigger it.
func GameLikelyRunning(savesDir string, before time.Time) (bool, string) {
	if exec.Command("pgrep", "-f", gameProcessPattern).Run() == nil {
		return true, "a Diablo II: Resurrected process appears to be running"
	}
	if recentlySaved(savesDir, 90*time.Second, before) {
		return true, "a save changed in the last 90 seconds — a game session may be active"
	}
	return false, ""
}

// recentlySaved reports whether any .d2s/.d2i in dir was modified within the
// given window but strictly before the given bound (which excludes the agent's
// own writes during the current pull run).
func recentlySaved(dir string, within time.Duration, before time.Time) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	cutoff := time.Now().Add(-within)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".d2s" && ext != ".d2i" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mt := info.ModTime()
		if mt.After(cutoff) && mt.Before(before) {
			return true
		}
	}
	return false
}

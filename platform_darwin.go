//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

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

// GameLikelyRunning is a best-effort, macOS reinforcement before a write (never
// a substitute for the confirmation): it looks for a D2R process and, failing
// that, for any save touched in the last ~90s (a likely active session).
// Detection being unavailable returns false — the dialog is the real barrier.
//
// before bounds the mtime heuristic to activity that predates the current pull
// run, so the agent's own in-run writes (mtime >= before) never self-trigger it.
func GameLikelyRunning(savesDir string, before time.Time) (bool, string) {
	if exec.Command("pgrep", "-f", "D2R").Run() == nil {
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

//go:build darwin
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

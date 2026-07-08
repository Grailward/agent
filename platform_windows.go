//go:build windows

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

// GetDefaultSavesDir returns the standard Windows D2R saves path.
func GetDefaultSavesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Saved Games", "Diablo II Resurrected")
}

// OpenPath reveals a folder in Explorer.
func OpenPath(path string) error {
	return exec.Command("explorer", path).Start()
}

// PromptToken displays a PowerShell input dialog for the token.
func PromptToken(url string) (string, error) {
	script := `
Add-Type -AssemblyName Microsoft.VisualBasic
$url = $env:GRAILWARD_URL
$token = [Microsoft.VisualBasic.Interaction]::InputBox("Please enter your Grailward API Token for: $url", "Grailward Agent Setup", "")
Write-Output $token
`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "GRAILWARD_URL="+url)

	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("powershell input dialog failed: %w", err)
	}

	return strings.TrimSpace(out.String()), nil
}

// PromptSavesDir displays a PowerShell folder selection dialog.
func PromptSavesDir(defaultPath string) (string, error) {
	script := `
Add-Type -AssemblyName System.Windows.Forms
$f = New-Object System.Windows.Forms.FolderBrowserDialog
$f.Description = "Select Diablo II Resurrected Saved Games directory:"
$defaultPath = $env:DEFAULT_PATH
if ($defaultPath -and (Test-Path $defaultPath)) {
    $f.SelectedPath = $defaultPath
}
if ($f.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) {
    Write-Output $f.SelectedPath
} else {
    exit 1
}
`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "DEFAULT_PATH="+defaultPath)

	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("powershell folder picker dialog canceled or failed: %w", err)
	}

	return strings.TrimSpace(out.String()), nil
}

// runPowerShell runs a PowerShell snippet with extra environment entries and
// returns its stdout. The message text is passed via env to avoid quoting
// issues in the script body.
func runPowerShell(script string, env ...string) (string, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// ShowAbout displays a simple informational MessageBox with a single OK button.
// The message text is passed via env to avoid quoting issues in the script body.
func ShowAbout(message string) error {
	script := `
Add-Type -AssemblyName System.Windows.Forms
[void][System.Windows.Forms.MessageBox]::Show($env:GW_MSG, "Grailward Agent", [System.Windows.Forms.MessageBoxButtons]::OK, [System.Windows.Forms.MessageBoxIcon]::Information)
`
	_, err := runPowerShell(script, "GW_MSG="+message)
	return err
}

// ConfirmPull shows a two-button confirmation for a batch pull (OK = Pull,
// Cancel = Skip). Returns true only when the user chooses Pull.
func ConfirmPull(message string) (bool, error) {
	script := `
Add-Type -AssemblyName System.Windows.Forms
$r = [System.Windows.Forms.MessageBox]::Show($env:GW_MSG, "Grailward Agent", [System.Windows.Forms.MessageBoxButtons]::OKCancel, [System.Windows.Forms.MessageBoxIcon]::Question)
if ($r -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output "1" } else { Write-Output "0" }
`
	out, err := runPowerShell(script, "GW_MSG="+message+"\r\n\r\nOK = Pull      Cancel = Skip")
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(out) == "1", nil
}

// ResolveConflict shows a three-button choice for a conflicted file
// (Yes = Keep local, No = Use server, Cancel = Skip).
func ResolveConflict(filename string) (ConflictChoice, error) {
	script := `
Add-Type -AssemblyName System.Windows.Forms
$r = [System.Windows.Forms.MessageBox]::Show($env:GW_MSG, "Grailward Agent - Conflict", [System.Windows.Forms.MessageBoxButtons]::YesNoCancel, [System.Windows.Forms.MessageBoxIcon]::Warning)
switch ($r) {
  ([System.Windows.Forms.DialogResult]::Yes) { Write-Output "keep" }
  ([System.Windows.Forms.DialogResult]::No)  { Write-Output "server" }
  default                                     { Write-Output "skip" }
}
`
	msg := conflictMessage(filename) + "\r\n\r\nYes = Keep local      No = Use server      Cancel = Skip"
	out, err := runPowerShell(script, "GW_MSG="+msg)
	if err != nil {
		return ConflictSkip, nil
	}
	switch strings.TrimSpace(out) {
	case "keep":
		return ConflictKeepLocal, nil
	case "server":
		return ConflictUseServer, nil
	default:
		return ConflictSkip, nil
	}
}

// GameLikelyRunning is the Windows reinforcement before a write: a hard refusal
// if D2R.exe is in the task list. Detection being unavailable returns false —
// the confirmation dialog remains the real barrier. The before bound (used by
// the macOS mtime heuristic) is not needed here.
func GameLikelyRunning(savesDir string, before time.Time) (bool, string) {
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq D2R.exe", "/NH").Output()
	if err != nil {
		return false, ""
	}
	if strings.Contains(strings.ToLower(string(out)), "d2r.exe") {
		return true, "Diablo II: Resurrected (D2R.exe) appears to be running"
	}
	return false, ""
}

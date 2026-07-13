//go:build windows

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

	"golang.org/x/sys/windows/registry"
)

// ConfirmUpdate shows an OK/Cancel confirmation before applying an update
// (OK = Update, Cancel = Later). Returns true only when the user chooses Update.
func ConfirmUpdate(version string) (bool, error) {
	script := `
Add-Type -AssemblyName System.Windows.Forms
$r = [System.Windows.Forms.MessageBox]::Show($env:GW_MSG, "Grailward Agent - Update", [System.Windows.Forms.MessageBoxButtons]::OKCancel, [System.Windows.Forms.MessageBoxIcon]::Question)
if ($r -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output "1" } else { Write-Output "0" }
`
	msg := fmt.Sprintf("A new version of Grailward Agent (%s) is available.\r\n\r\nGrailward will download it, replace this copy, and restart. Update now?", version)
	out, err := runPowerShell(script, "GW_MSG="+msg)
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(out) == "1", nil
}

// ApplyUpdate replaces the running .exe and relaunches. A running Windows binary
// can't be overwritten but can be renamed, so the current exe is moved aside to
// ".old" (removed on the next start) and the new bytes take its path. On success
// this does not return — it spawns the new exe and exits.
func ApplyUpdate(execPath string, file *UpdateFile, data []byte) error {
	dir := filepath.Dir(execPath)
	tmp, err := os.CreateTemp(dir, ".gw-update-*.exe") // same volume => atomic rename
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

	old := execPath + ".old"
	_ = os.Remove(old)
	if err := os.Rename(execPath, old); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, execPath); err != nil {
		_ = os.Rename(old, execPath) // best-effort restore
		os.Remove(tmpName)
		return err
	}

	log.Printf("Update applied; restarting %s", execPath)
	if err := exec.Command(execPath, os.Args[1:]...).Start(); err != nil {
		log.Printf("Relaunch failed (%v); please start Grailward Agent manually", err)
	}
	os.Exit(0)
	return nil // unreachable
}

// cleanUpdateLeftovers removes the previous exe (".old") and any stray staging
// files beside the executable (best-effort; called at startup).
func cleanUpdateLeftovers(execPath string) {
	_ = os.Remove(execPath + ".old")
	dir := filepath.Dir(execPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gw-update-") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// runKeyPath is the per-user (HKCU) autostart key; runKeyName is this agent's
// value under it. HKCU needs no elevation, and a distinct value name keeps the
// login item independent of any other program's entries.
const (
	runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	runKeyName = "GrailwardAgent"
)

// enableStartAtLogin writes the HKCU Run value pointing (quoted) at execPath.
func enableStartAtLogin(execPath string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(runKeyName, runKeyValue(execPath))
}

// disableStartAtLogin deletes the HKCU Run value. A missing value is not an error
// (the login item is already gone).
func disableStartAtLogin() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(runKeyName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// startAtLoginExecPath returns the executable path currently registered in the
// HKCU Run value (quotes stripped), or ("", false) when there is no such value.
func startAtLoginExecPath() (string, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runKeyName)
	if err != nil {
		return "", false
	}
	return unquoteRunValue(v), true
}

// startAtLoginUpToDate reports whether the registered login item is current for
// execPath. On Windows the HKCU Run value is fully determined by the executable path
// (runKeyValue), which the exec-path heal check already covers, so there is no extra
// content to refresh.
func startAtLoginUpToDate(string) bool {
	return true
}

// startAtLoginTarget describes where the login item lives — the HKCU Run value — for
// the enable log line.
func startAtLoginTarget() string {
	return `HKCU\` + runKeyPath + `\` + runKeyName
}

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

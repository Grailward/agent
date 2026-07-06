//go:build windows
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

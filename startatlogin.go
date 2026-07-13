package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// This file holds the platform-independent parts of the "Start with system"
// feature: the login-item content generators, which are pure functions so their
// output is unit-testable, and the startup self-heal decision.
// The side-effecting pieces (writing the macOS LaunchAgent plist or the Windows
// HKCU Run value, reading the registered path back) live in the platform files.

// launchAgentLabel is the reverse-DNS label of the per-user macOS LaunchAgent; it
// matches the .app bundle identifier so the login item is unambiguous.
const launchAgentLabel = "com.grailward.agent"

// launchAgentPlist renders the per-user LaunchAgent property list that starts the
// agent at login. RunAtLoad launches it once at login and there is no KeepAlive,
// so a user Quit stays quit until the next login. execPath is the absolute path to
// the executable to run and is XML-escaped for safe embedding.
func launchAgentPlist(execPath string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchAgentLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + plistEscape(execPath) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
`
}

// parseLaunchAgentExec extracts the first ProgramArguments string from a
// LaunchAgent plist body — the executable path the login item points at. It is
// tolerant enough for the plists this agent writes and returns ("", false) when it
// can't find one. Kept pure (no file I/O) so it is unit-testable.
func parseLaunchAgentExec(plist string) (string, bool) {
	i := strings.Index(plist, "<key>ProgramArguments</key>")
	if i < 0 {
		return "", false
	}
	rest := plist[i:]
	open := strings.Index(rest, "<string>")
	if open < 0 {
		return "", false
	}
	rest = rest[open+len("<string>"):]
	end := strings.Index(rest, "</string>")
	if end < 0 {
		return "", false
	}
	return plistUnescape(rest[:end]), true
}

// plistEscape escapes the XML metacharacters that can appear in a filesystem path
// so it embeds cleanly in a plist <string>. The ampersand is replaced first so it
// does not double-escape the entities introduced for < and >.
func plistEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// plistUnescape reverses plistEscape. The ampersand entity is decoded last so a
// literal "&lt;" written as "&amp;lt;" round-trips.
func plistUnescape(s string) string {
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
}

// runKeyValue renders the HKCU\...\Run value for Windows: the executable path
// wrapped in double quotes so a path containing spaces parses as a single
// argument. Pure, so it is unit-testable on any platform.
func runKeyValue(execPath string) string {
	return `"` + execPath + `"`
}

// unquoteRunValue strips the surrounding double quotes runKeyValue adds, so a
// registered Run value can be compared against a bare executable path.
func unquoteRunValue(v string) string {
	return strings.Trim(v, `"`)
}

// startAtLoginNeedsHeal reports whether an enabled login item must be rewritten:
// when it is missing entirely, or when it points at a different executable than
// the one now running (the app was moved, or replaced by an auto-update). Pure, so
// the self-heal decision is unit-testable without touching the real login item.
func startAtLoginNeedsHeal(registered string, present bool, current string) bool {
	return !present || registered != current
}

// currentExecPath returns the absolute, symlink-resolved path of the running
// executable — used both to register the login item and to self-heal a stale one,
// so both sides compare the same canonical form.
func currentExecPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p), nil
}

// maybeHealStartAtLogin rewrites the OS login item when it is enabled in config
// but no longer points at this executable. A disabled preference or an
// unresolvable executable path is a silent no-op. Best-effort: a write failure is
// logged, never fatal. Called once at startup.
func maybeHealStartAtLogin(cfg *Config) {
	if cfg == nil || !cfg.StartAtLogin {
		return
	}
	current, err := currentExecPath()
	if err != nil {
		log.Printf("Start-at-login self-heal skipped: %v", err)
		return
	}
	registered, present := startAtLoginExecPath()
	if !startAtLoginNeedsHeal(registered, present, current) {
		return
	}
	if err := enableStartAtLogin(current); err != nil {
		log.Printf("Could not refresh start-at-login item: %v", err)
		return
	}
	log.Printf("Start-at-login item refreshed to point at %s", current)
}

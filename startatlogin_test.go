package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLaunchAgentPlistRoundTrips proves the generated plist is well-formed enough
// that parseLaunchAgentExec reads the exact executable path back, including one
// that needs XML escaping.
func TestLaunchAgentPlistRoundTrips(t *testing.T) {
	for _, exec := range []string{
		"/Applications/Grailward Agent.app/Contents/MacOS/grailward-agent",
		"/home/a&b/<weird>/grailward-agent",
	} {
		plist := launchAgentPlist(exec)
		if !strings.Contains(plist, "<key>Label</key>") || !strings.Contains(plist, launchAgentLabel) {
			t.Fatalf("plist missing label:\n%s", plist)
		}
		if !strings.Contains(plist, "<key>RunAtLoad</key>") {
			t.Fatalf("plist missing RunAtLoad:\n%s", plist)
		}
		// A raw & or < in the path would make the plist malformed; the metachars
		// must appear only in escaped form inside the <string> value.
		if strings.Contains(exec, "<") && strings.Contains(plist, "<weird>") {
			t.Fatalf("angle brackets not escaped in plist:\n%s", plist)
		}
		if strings.Contains(exec, "&") && !strings.Contains(plist, "&amp;") {
			t.Fatalf("ampersand not escaped in plist:\n%s", plist)
		}
		got, ok := parseLaunchAgentExec(plist)
		if !ok || got != exec {
			t.Fatalf("parseLaunchAgentExec round-trip = (%q, %v), want (%q, true)", got, ok, exec)
		}
	}
}

// TestParseLaunchAgentExecMissing returns not-ok for a plist without
// ProgramArguments.
func TestParseLaunchAgentExecMissing(t *testing.T) {
	if got, ok := parseLaunchAgentExec("<plist><dict></dict></plist>"); ok {
		t.Fatalf("expected not-ok for a plist with no ProgramArguments, got %q", got)
	}
}

// TestRunKeyValueRoundTrips proves the Windows Run value is quoted and strips back
// to the bare path (so a self-heal comparison lines up).
func TestRunKeyValueRoundTrips(t *testing.T) {
	exec := `C:\Program Files\Grailward\grailward-agent.exe`
	v := runKeyValue(exec)
	if !strings.HasPrefix(v, `"`) || !strings.HasSuffix(v, `"`) {
		t.Fatalf("Run value must be double-quoted, got %q", v)
	}
	if got := unquoteRunValue(v); got != exec {
		t.Fatalf("unquoteRunValue(%q) = %q, want %q", v, got, exec)
	}
}

// TestStartAtLoginNeedsHeal covers the self-heal decision matrix.
func TestStartAtLoginNeedsHeal(t *testing.T) {
	cur := "/opt/app/grailward-agent"
	cases := []struct {
		name       string
		registered string
		present    bool
		want       bool
	}{
		{"missing item needs heal", "", false, true},
		{"stale path needs heal", "/old/path/grailward-agent", true, true},
		{"matching path is fine", cur, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := startAtLoginNeedsHeal(c.registered, c.present, cur); got != c.want {
				t.Fatalf("startAtLoginNeedsHeal(%q, %v, %q) = %v, want %v", c.registered, c.present, cur, got, c.want)
			}
		})
	}
}

// TestConfigStartAtLoginRoundTrip: default off is omitted from the file; an
// explicit on round-trips.
func TestConfigStartAtLoginRoundTrip(t *testing.T) {
	offPath := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(offPath, &Config{URL: "u"}); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}
	raw, err := os.ReadFile(offPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "start_at_login") {
		t.Fatalf("default-off start_at_login should be omitted, got:\n%s", raw)
	}

	onPath := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(onPath, &Config{URL: "u", StartAtLogin: true}); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}
	got, err := LoadConfig(onPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if !got.StartAtLogin {
		t.Fatal("explicit start_at_login=true did not round-trip")
	}
}

// TestSetStartAtLoginPersistsOnSuccess: a successful OS write flips the config and
// persists it, and the enable seam is handed the running executable path.
func TestSetStartAtLoginPersistsOnSuccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	var enabledWith string
	w := &Watcher{
		Config:          &Config{},
		enableLoginItem: func(p string) error { enabledWith = p; return nil },
	}
	if err := w.SetStartAtLogin(true); err != nil {
		t.Fatalf("SetStartAtLogin(true): %v", err)
	}
	if !w.StartAtLoginEnabled() {
		t.Fatal("should read enabled after a successful toggle")
	}
	if enabledWith == "" {
		t.Fatal("enable seam was not handed an executable path")
	}
	path, _ := GetConfigPath()
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("config not persisted: %v", err)
	}
	if !got.StartAtLogin {
		t.Fatal("start_at_login was not persisted to config.json")
	}
}

// TestSetStartAtLoginFailureDoesNotPersist: when the OS write fails the preference
// must NOT be persisted and the watcher must keep reading disabled, so the tray
// checkbox reflects reality instead of lying.
func TestSetStartAtLoginFailureDoesNotPersist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	w := &Watcher{
		Config:          &Config{},
		enableLoginItem: func(string) error { return errors.New("write refused") },
	}
	if err := w.SetStartAtLogin(true); err == nil {
		t.Fatal("expected an error when the OS write fails")
	}
	if w.StartAtLoginEnabled() {
		t.Fatal("a failed OS write must not mark start-at-login enabled")
	}
	path, _ := GetConfigPath()
	if got, err := LoadConfig(path); err == nil && got.StartAtLogin {
		t.Fatal("a failed OS write must not persist start_at_login=true")
	}
}

// TestSetStartAtLoginDisablePersists: turning it off runs the disable seam and
// persists the off state.
func TestSetStartAtLoginDisablePersists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	disabled := false
	w := &Watcher{
		Config:           &Config{StartAtLogin: true},
		disableLoginItem: func() error { disabled = true; return nil },
	}
	if err := w.SetStartAtLogin(false); err != nil {
		t.Fatalf("SetStartAtLogin(false): %v", err)
	}
	if !disabled {
		t.Fatal("disable seam was not invoked")
	}
	if w.StartAtLoginEnabled() {
		t.Fatal("should read disabled after turning it off")
	}
	path, _ := GetConfigPath()
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("config not persisted: %v", err)
	}
	if got.StartAtLogin {
		t.Fatal("start_at_login=false was not persisted")
	}
}

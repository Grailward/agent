package main

import (
	"bytes"
	"errors"
	"log"
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

// TestIsTranslocated distinguishes a real App Translocation path (a read-only,
// per-launch-ephemeral mount) from a normal install path.
func TestIsTranslocated(t *testing.T) {
	translocated := []string{
		"/private/var/folders/62/abc/T/AppTranslocation/A887B1/d/Grailward Agent.app/Contents/MacOS/grailward-agent",
		"/var/folders/xy/z/AppTranslocation/UUID/d/Grailward Agent.app/Contents/MacOS/grailward-agent",
	}
	for _, p := range translocated {
		if !isTranslocated(p) {
			t.Fatalf("isTranslocated(%q) = false, want true", p)
		}
	}
	normal := []string{
		"/Applications/Grailward Agent.app/Contents/MacOS/grailward-agent",
		"/Users/me/Applications/Grailward Agent.app/Contents/MacOS/grailward-agent",
		"/usr/local/bin/grailward-agent",
		`C:\Program Files\Grailward\grailward-agent.exe`,
	}
	for _, p := range normal {
		if isTranslocated(p) {
			t.Fatalf("isTranslocated(%q) = true, want false", p)
		}
	}
}

// TestStartAtLoginHealDecision covers the startup self-heal decision, including the
// two translocation cases: a translocated current path is skipped (never overwrite a
// good registration with an ephemeral one), while a plist already pointing at a
// translocated path is rewritten once the app runs from a stable location.
func TestStartAtLoginHealDecision(t *testing.T) {
	const good = "/Applications/Grailward Agent.app/Contents/MacOS/grailward-agent"
	const transloc = "/private/var/folders/62/x/T/AppTranslocation/ABC/d/Grailward Agent.app/Contents/MacOS/grailward-agent"

	cases := []struct {
		name       string
		current    string
		registered string
		present    bool
		upToDate   bool
		want       healAction
	}{
		{"translocated current is skipped", transloc, good, true, true, healSkipTranslocated},
		{"translocated current skipped even if unregistered", transloc, "", false, true, healSkipTranslocated},
		{"already correct is a no-op", good, good, true, true, healNone},
		{"missing item is rewritten", good, "", false, true, healRewrite},
		{"stale content is rewritten", good, good, true, false, healRewrite},
		{"repairs a plist pointing at a translocated path", good, transloc, true, true, healRewrite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := startAtLoginHealDecision(c.current, c.registered, c.present, c.upToDate); got != c.want {
				t.Fatalf("startAtLoginHealDecision = %v, want %v", got, c.want)
			}
		})
	}
}

// TestSetStartAtLoginTranslocatedRefuses: turning on Start with system from a
// translocated app must refuse — no login item written, the preference stays off,
// and the move-to-Applications dialog is shown.
func TestSetStartAtLoginTranslocatedRefuses(t *testing.T) {
	enableCalled := false
	var shown string
	w := &Watcher{
		Config:   &Config{},
		lastLine: map[string]string{},
		errs:     map[string]string{},
		execPath: func() (string, error) {
			return "/private/var/folders/62/x/T/AppTranslocation/ABC/d/Grailward Agent.app/Contents/MacOS/grailward-agent", nil
		},
		enableLoginItem: func(string) error { enableCalled = true; return nil },
		showMessage:     func(m string) error { shown = m; return nil },
	}

	if err := w.SetStartAtLogin(true); err == nil {
		t.Fatal("SetStartAtLogin must fail for a translocated app")
	}
	if enableCalled {
		t.Fatal("a translocated app must not write a login item")
	}
	if w.StartAtLoginEnabled() {
		t.Fatal("the preference must stay off when the app is translocated")
	}
	if !strings.Contains(shown, "Applications folder") {
		t.Fatalf("translocation dialog missing move-to-Applications guidance: %q", shown)
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

// TestLaunchAgentPlistBundleAssociation: an executable inside a .app gets the
// AssociatedBundleIdentifiers key (naming the real bundle id) so the macOS Login
// Items panel can attribute the item to the app; a raw binary omits the key
// because it has no bundle to point at.
func TestLaunchAgentPlistBundleAssociation(t *testing.T) {
	bundleExec := "/Applications/Grailward Agent.app/Contents/MacOS/grailward-agent"
	p := launchAgentPlist(bundleExec)
	if !strings.Contains(p, "<key>AssociatedBundleIdentifiers</key>") {
		t.Fatalf("bundle exec should get AssociatedBundleIdentifiers:\n%s", p)
	}
	if !strings.Contains(p, "<string>"+bundleIdentifier+"</string>") {
		t.Fatalf("association must name the bundle id %q:\n%s", bundleIdentifier, p)
	}

	rawExec := "/usr/local/bin/grailward-agent"
	if r := launchAgentPlist(rawExec); strings.Contains(r, "AssociatedBundleIdentifiers") {
		t.Fatalf("raw binary must not get AssociatedBundleIdentifiers:\n%s", r)
	}
}

// TestLaunchAgentUpToDate drives the self-heal freshness decision: a plist written
// by an older version (missing AssociatedBundleIdentifiers) reads as out of date
// for a bundle executable, so startup rewrites it; the freshly generated plist
// reads as up to date.
func TestLaunchAgentUpToDate(t *testing.T) {
	bundleExec := "/Applications/Grailward Agent.app/Contents/MacOS/grailward-agent"
	current := launchAgentPlist(bundleExec)

	// A v0.5.0-style plist: identical but for the missing association block.
	old := strings.Replace(current,
		"\t<key>AssociatedBundleIdentifiers</key>\n\t<array>\n\t\t<string>"+bundleIdentifier+"</string>\n\t</array>\n",
		"", 1)
	if old == current {
		t.Fatal("test setup: association block was not present to strip")
	}
	if launchAgentUpToDate(old, bundleExec) {
		t.Fatal("a plist missing AssociatedBundleIdentifiers must read as out of date")
	}
	if !launchAgentUpToDate(current, bundleExec) {
		t.Fatal("the freshly generated plist must read as up to date")
	}
}

// TestSetStartAtLoginLogsTarget: a successful toggle logs the outcome in both
// directions, and the enable line names the real login-item target.
func TestSetStartAtLoginLogsTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	var buf bytes.Buffer
	orig, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(orig); log.SetFlags(flags) }()

	const target = "/Users/tester/Library/LaunchAgents/com.grailward.agent.plist"
	w := &Watcher{
		Config:           &Config{},
		enableLoginItem:  func(string) error { return nil },
		disableLoginItem: func() error { return nil },
		loginItemTarget:  func() string { return target },
	}

	if err := w.SetStartAtLogin(true); err != nil {
		t.Fatalf("SetStartAtLogin(true): %v", err)
	}
	if want := "Start with system enabled — login item at " + target; !strings.Contains(buf.String(), want) {
		t.Fatalf("enable log missing %q in:\n%s", want, buf.String())
	}

	buf.Reset()
	if err := w.SetStartAtLogin(false); err != nil {
		t.Fatalf("SetStartAtLogin(false): %v", err)
	}
	if want := "Start with system disabled — login item removed"; !strings.Contains(buf.String(), want) {
		t.Fatalf("disable log missing %q in:\n%s", want, buf.String())
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

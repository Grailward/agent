package main

import (
	"strings"
	"testing"
)

// TestAutostartExecRoundTrips proves an execPath survives the render → parse round
// trip through the desktop-entry quoting, including the awkward cases a real install
// path can hit: a space (the common case, which forces the surrounding quotes), a
// double quote, a dollar sign, a backtick, and a literal backslash.
func TestAutostartExecRoundTrips(t *testing.T) {
	paths := []string{
		"/usr/local/bin/grailward-agent",
		"/home/deck/Applications/Grailward Agent/grailward-agent",
		`/home/o"d/grailward-agent`,
		`/home/a$b/grailward-agent`,
		"/home/back`tick/grailward-agent",
		`/home/a\b/grailward-agent`,
		`/home/mix "a$b"\c/grailward-agent`,
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			entry := autostartDesktopEntry(p)
			got, ok := parseAutostartExec(entry)
			if !ok {
				t.Fatalf("parseAutostartExec found no Exec= line in:\n%s", entry)
			}
			if got != p {
				t.Fatalf("round trip = %q, want %q\nentry:\n%s", got, p, entry)
			}
		})
	}
}

// TestAutostartDesktopEntryShape pins the entry's determinism and required keys: the
// same path renders the same bytes, and the standard autostart keys are present.
func TestAutostartDesktopEntryShape(t *testing.T) {
	const exe = "/opt/grailward/grailward-agent"
	entry := autostartDesktopEntry(exe)

	if entry != autostartDesktopEntry(exe) {
		t.Fatal("autostartDesktopEntry is not deterministic for the same path")
	}
	for _, want := range []string{
		"[Desktop Entry]",
		"Type=Application",
		"Name=Grailward Agent",
		"X-GNOME-Autostart-enabled=true",
		`Exec="/opt/grailward/grailward-agent"`,
	} {
		if !strings.Contains(entry, want) {
			t.Fatalf("entry missing %q:\n%s", want, entry)
		}
	}
}

// TestAutostartUpToDate proves the byte-for-byte heal check: the freshly rendered
// body is up to date, a body for a different path is not, and a body missing a key
// (an older build's entry) is not — so the startup self-heal rewrites it.
func TestAutostartUpToDate(t *testing.T) {
	const exe = "/opt/grailward/grailward-agent"
	fresh := autostartDesktopEntry(exe)

	if !autostartUpToDate(fresh, exe) {
		t.Fatal("freshly rendered entry should be up to date")
	}
	if autostartUpToDate(fresh, "/somewhere/else/grailward-agent") {
		t.Fatal("entry for a different path must not read as up to date")
	}
	stale := strings.Replace(fresh, "X-GNOME-Autostart-enabled=true\n", "", 1)
	if autostartUpToDate(stale, exe) {
		t.Fatal("an entry missing a key must read as out of date")
	}
}

// TestParseAutostartExecMissing proves a body with no Exec= line reports not found
// rather than an empty match.
func TestParseAutostartExecMissing(t *testing.T) {
	if got, ok := parseAutostartExec("[Desktop Entry]\nType=Application\n"); ok {
		t.Fatalf("parseAutostartExec on a body without Exec= = (%q, true), want (\"\", false)", got)
	}
}

// TestDesktopExecQuoteEscaping pins the two-layer escaping for the tricky
// characters, so a regression in either layer is caught directly rather than only
// through the round trip: a space stays literal inside the quotes, a double quote
// becomes \\", a dollar becomes \\$, and a literal backslash becomes four.
func TestDesktopExecQuoteEscaping(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"/a b/x", `"/a b/x"`},
		{`/a"b/x`, `"/a\\"b/x"`},
		{`/a$b/x`, `"/a\\$b/x"`},
		{`/a\b/x`, `"/a\\\\b/x"`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := desktopExecQuote(tt.in); got != tt.want {
				t.Fatalf("desktopExecQuote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

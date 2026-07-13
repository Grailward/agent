//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEscapeAppleScript proves the escaping keeps the osascript string literal
// well-formed: quotes and backslashes are escaped, and a real newline becomes an
// AppleScript concatenation rather than an unterminated literal.
func TestEscapeAppleScript(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"double quote", `"`, `\"`},
		{"backslash", `\`, `\\`},
		{"literal backslash-n is not a newline", `\n`, `\\n`},
		{"newline becomes concatenation", "a\nb", `a" & return & "b`},
		{"quote and newline combined", "a\"b\nc", `a\"b" & return & "c`},
		{"backslash then quote then newline", "x\\\"\ny", `x\\\"" & return & "y`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := escapeAppleScript(tt.in)
			if got != tt.want {
				t.Fatalf("escapeAppleScript(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// A raw newline in the source would make an unterminated AppleScript
			// string literal; the escaper must have removed every one.
			if strings.ContainsRune(got, '\n') {
				t.Fatalf("escaped output still contains a raw newline: %q", got)
			}
		})
	}
}

// TestMatchesGameProcess pins the hardened game-process pattern: it must match
// the real "D2R.exe" binary (however the CrossOver/Wine command line spells its
// path) but never the "D2R-CXEngine" helper, which lingers as a wine leftover and
// would otherwise keep the write guard tripped forever.
func TestMatchesGameProcess(t *testing.T) {
	tests := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{"bare exe", "D2R.exe", true},
		{"windows path with args", `C:\Program Files (x86)\Diablo II Resurrected\D2R.exe -launch`, true},
		{"crossover unix path to exe", "/Users/x/Library/Application Support/CrossOver/Bottles/Battle.net/drive_c/Program Files (x86)/Diablo II Resurrected/D2R.exe", true},
		{"crossover engine leftover", "D2R-CXEngine", false},
		{"crossover engine leftover with path", "/Applications/CrossOver.app/Contents/SharedSupport/CrossOver/bin/D2R-CXEngine", false},
		{"unrelated process", "SomeOtherApp --flag", false},
		{"bare D2R without exe", "D2R", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesGameProcess(tt.cmdline); got != tt.want {
				t.Fatalf("matchesGameProcess(%q) = %v, want %v", tt.cmdline, got, tt.want)
			}
		})
	}
}

// TestRecentlySavedExcludesInRunWrites guards the fix for the batch self-trigger:
// the mtime heuristic must count a save modified before the pull run began (real
// game activity) but ignore one modified after it began (the agent's own write).
func TestRecentlySavedExcludesInRunWrites(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name   string
		mtime  time.Time
		before time.Time
		want   bool
	}{
		{"recent, before the run start (game activity)", now.Add(-30 * time.Second), now.Add(-10 * time.Second), true},
		{"recent, after the run start (agent's own write)", now.Add(-30 * time.Second), now.Add(-60 * time.Second), false},
		{"too old to matter", now.Add(-200 * time.Second), now, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := t.TempDir()
			p := filepath.Join(d, "Hero.d2s")
			if err := os.WriteFile(p, syntheticSave(64, 0x01), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(p, tt.mtime, tt.mtime); err != nil {
				t.Fatal(err)
			}
			if got := recentlySaved(d, 90*time.Second, tt.before); got != tt.want {
				t.Fatalf("recentlySaved = %v, want %v", got, tt.want)
			}
		})
	}

	// A non-save file is never counted, even when freshly modified.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if recentlySaved(dir, 90*time.Second, now.Add(time.Hour)) {
		t.Fatal("a .txt must not count as save activity")
	}
}

//go:build darwin

package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// syntheticAppZip builds an in-memory zip mirroring the real macOS download: a
// "Grailward Agent.app" bundle with an executable inner binary and an Info.plist.
// Entirely fabricated bytes — never a real artifact.
func syntheticAppZip(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, mode os.FileMode, data []byte) {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	add("Grailward Agent.app/Contents/Info.plist", 0644, []byte("<plist/>"))
	add("Grailward Agent.app/Contents/MacOS/grailward-agent", 0755, binary)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestUnzipAppPipeline exercises the macOS apply extraction primitives against a
// synthetic .app zip: the bundle is found, the inner binary keeps its exec bit,
// and extractBundleBinary returns exactly those bytes.
func TestUnzipAppPipeline(t *testing.T) {
	binary := []byte("\xcf\xfa\xed\xfe fake mach-o body")
	zipData := syntheticAppZip(t, binary)

	dest := t.TempDir()
	if err := unzipInto(zipData, dest); err != nil {
		t.Fatalf("unzipInto: %v", err)
	}
	app, err := findDotApp(dest)
	if err != nil {
		t.Fatalf("findDotApp: %v", err)
	}
	if filepath.Base(app) != "Grailward Agent.app" {
		t.Fatalf("findDotApp = %q, want the bundle", app)
	}
	innerPath := filepath.Join(app, "Contents", "MacOS", "grailward-agent")
	info, err := os.Stat(innerPath)
	if err != nil {
		t.Fatalf("inner binary not extracted: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Fatalf("inner binary lost its executable bit: mode %v", info.Mode())
	}
	got, err := os.ReadFile(innerPath)
	if err != nil || !bytes.Equal(got, binary) {
		t.Fatal("inner binary bytes differ from source")
	}

	// extractBundleBinary (used for the raw-binary swap) returns the same bytes.
	extracted, err := extractBundleBinary(zipData)
	if err != nil {
		t.Fatalf("extractBundleBinary: %v", err)
	}
	if !bytes.Equal(extracted, binary) {
		t.Fatal("extractBundleBinary returned the wrong bytes")
	}
}

// TestUnzipRejectsTraversal proves the extractor refuses a zip-slip path.
func TestUnzipRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../escape.txt")
	w.Write([]byte("x"))
	zw.Close()

	if err := unzipInto(buf.Bytes(), t.TempDir()); err == nil {
		t.Fatal("unzipInto must reject a path-traversal entry")
	}
}

// TestStartAtLoginWriteReadHeal exercises the darwin login-item file round trip
// under a temp HOME (never launchctl, never the real ~/Library): enable writes the
// plist, startAtLoginExecPath reads the path back, and re-enabling with a moved
// path rewrites it — the self-heal case.
func TestStartAtLoginWriteReadHeal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Nothing registered yet.
	if _, ok := startAtLoginExecPath(); ok {
		t.Fatal("no login item should be registered on a fresh HOME")
	}

	exec1 := "/Applications/Grailward Agent.app/Contents/MacOS/grailward-agent"
	if err := enableStartAtLogin(exec1); err != nil {
		t.Fatalf("enableStartAtLogin: %v", err)
	}
	// The plist lands under the temp HOME, not the real one.
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("plist not written to the expected path: %v", err)
	}
	got, ok := startAtLoginExecPath()
	if !ok || got != exec1 {
		t.Fatalf("startAtLoginExecPath = (%q, %v), want (%q, true)", got, ok, exec1)
	}
	if startAtLoginNeedsHeal(got, ok, exec1) {
		t.Fatal("a matching registered path should not need heal")
	}

	// The app moved: re-enabling with the new path rewrites the plist (self-heal).
	exec2 := "/Users/someone/Downloads/Grailward Agent.app/Contents/MacOS/grailward-agent"
	if !startAtLoginNeedsHeal(got, ok, exec2) {
		t.Fatal("a moved executable should need heal")
	}
	if err := enableStartAtLogin(exec2); err != nil {
		t.Fatalf("enableStartAtLogin (heal): %v", err)
	}
	if got, ok := startAtLoginExecPath(); !ok || got != exec2 {
		t.Fatalf("after heal, startAtLoginExecPath = (%q, %v), want (%q, true)", got, ok, exec2)
	}
}

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

// TestDialogIconClause proves the AppleScript icon fragment points at the bundle's
// AppIcon.icns only when the agent runs from a .app whose icon exists, and that a
// special character in the path is escaped for the double-quoted literal.
func TestDialogIconClause(t *testing.T) {
	// A raw binary outside any bundle yields no clause.
	if got := dialogIconClauseFor("/usr/local/bin/grailward-agent"); got != "" {
		t.Fatalf("raw binary must yield no icon clause, got %q", got)
	}

	// A bundle layout: no clause until AppIcon.icns actually exists, then the escaped
	// POSIX-file clause pointing at it.
	root := t.TempDir()
	res := filepath.Join(root, "Grailward Agent.app", "Contents", "Resources")
	macos := filepath.Join(root, "Grailward Agent.app", "Contents", "MacOS")
	if err := os.MkdirAll(res, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(macos, 0755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(macos, "grailward-agent")
	if got := dialogIconClauseFor(exe); got != "" {
		t.Fatalf("a bundle without AppIcon.icns must yield no clause, got %q", got)
	}
	icns := filepath.Join(res, "AppIcon.icns")
	if err := os.WriteFile(icns, []byte("icns"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, want := dialogIconClauseFor(exe), ` with icon POSIX file "`+icns+`"`; got != want {
		t.Fatalf("dialogIconClauseFor = %q, want %q", got, want)
	}

	// A quote in the bundle path must be escaped inside the clause.
	qContents := filepath.Join(root, `od"d.app`, "Contents")
	if err := os.MkdirAll(filepath.Join(qContents, "Resources"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(qContents, "MacOS"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(qContents, "Resources", "AppIcon.icns"), []byte("i"), 0644); err != nil {
		t.Fatal(err)
	}
	qClause := dialogIconClauseFor(filepath.Join(qContents, "MacOS", "grailward-agent"))
	if !strings.Contains(qClause, `od\"d.app`) {
		t.Fatalf("quote in bundle path not escaped: %q", qClause)
	}
}

// TestEscapeAppleScriptPath proves a quote/backslash in an icns path is escaped so
// the POSIX file literal stays well-formed.
func TestEscapeAppleScriptPath(t *testing.T) {
	if got, want := escapeAppleScriptPath(`/A "B"\c`), `/A \"B\"\\c`; got != want {
		t.Fatalf("escapeAppleScriptPath = %q, want %q", got, want)
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

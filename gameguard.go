package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Shared, platform-independent parts of the "is a game session active?" write
// guard. GameLikelyRunning itself is per-platform (pgrep on macOS/Linux, tasklist
// on Windows), but the process pattern and the recent-save heuristic are identical
// wherever a Wine/Proton layer runs the Windows binary, so they live here in a
// build-tag-free file. That makes them compile — and their tests run — on every OS;
// the pieces a given OS doesn't call are simply unused there, which Go allows.

// gameProcessPattern matches the running D2R game binary in a Wine/Proton/CrossOver
// command line. It targets the real executable "D2R.exe" (an extended-regexp literal
// dot) and deliberately does NOT match the CrossOver engine helper "D2R-CXEngine",
// which can linger as a wine leftover after the game exits — a bare "D2R" match would
// take that as an open game and refuse writes forever. Passed verbatim to pgrep -f
// (POSIX ERE) and to gameProcessRe below (RE2); the pattern is simple enough to mean
// the same in both.
const gameProcessPattern = `D2R\.exe`

var gameProcessRe = regexp.MustCompile(gameProcessPattern)

// matchesGameProcess reports whether a process command line looks like the real
// D2R game binary. Kept pure and package-level so the exact pattern handed to
// pgrep is unit-testable without a running game.
func matchesGameProcess(cmdline string) bool {
	return gameProcessRe.MatchString(cmdline)
}

// recentlySaved reports whether any .d2s/.d2i in dir was modified within the
// given window but strictly before the given bound (which excludes the agent's
// own writes during the current pull run).
func recentlySaved(dir string, within time.Duration, before time.Time) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	cutoff := time.Now().Add(-within)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".d2s" && ext != ".d2i" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mt := info.ModTime()
		if mt.After(cutoff) && mt.Before(before) {
			return true
		}
	}
	return false
}

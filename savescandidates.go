package main

import (
	"os"
	"path/filepath"
	"strings"
)

// pickSavesDir chooses the best saves directory from an ordered list of candidate
// paths (already glob-expanded by the platform layer). The rule: the first
// candidate that actually holds a .d2s character save wins; failing that, the first
// candidate that merely exists; failing that, "". Order is significant — callers
// pass candidates from most to least likely — and ties never occur because the scan
// stops at the first match. It reads the filesystem but is otherwise OS-agnostic, so
// it is unit-testable on any host with temp directories.
func pickSavesDir(candidates []string) string {
	firstExisting := ""
	for _, c := range candidates {
		info, err := os.Stat(c)
		if err != nil || !info.IsDir() {
			continue
		}
		if firstExisting == "" {
			firstExisting = c
		}
		if dirHasD2S(c) {
			return c
		}
	}
	return firstExisting
}

// dirHasD2S reports whether dir directly contains at least one .d2s character save
// (case-insensitive). A save's presence is the strongest signal that a candidate
// Wine/Proton prefix is the real, played one rather than an empty leftover.
func dirHasD2S(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".d2s") {
			return true
		}
	}
	return false
}

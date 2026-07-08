package main

import (
	"strings"
	"testing"
)

func TestAboutText(t *testing.T) {
	got := aboutText("v1.2.3", "https://grailward.example", "/saves/d2r", SyncModePush, "/cfg/grailward-agent")

	wantLines := []string{
		"Grailward Agent v1.2.3",
		"Server: https://grailward.example",
		"Saves folder: /saves/d2r",
		"Sync mode: Push only",
		"Config & logs: /cfg/grailward-agent",
		"",
		"Unofficial fan-made tool — not affiliated with Blizzard Entertainment.",
	}
	if got != strings.Join(wantLines, "\n") {
		t.Fatalf("aboutText mismatch:\n got:\n%s\nwant:\n%s", got, strings.Join(wantLines, "\n"))
	}
}

// The sync mode is rendered as a human label, not the stored config token.
func TestAboutTextSyncModeLabels(t *testing.T) {
	if got := aboutText("dev", "u", "s", SyncModeTwoWay, "c"); !strings.Contains(got, "Sync mode: Two-way") {
		t.Errorf("two-way mode not labelled: %q", got)
	}
	if got := aboutText("dev", "u", "s", SyncModePush, "c"); !strings.Contains(got, "Sync mode: Push only") {
		t.Errorf("push mode not labelled: %q", got)
	}
	// A dev build shows the raw version verbatim (no synthetic "v" prefix).
	if got := aboutText("dev", "u", "s", SyncModePush, "c"); !strings.HasPrefix(got, "Grailward Agent dev\n") {
		t.Errorf("dev version not shown verbatim: %q", got)
	}
}

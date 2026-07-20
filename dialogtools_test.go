package main

import (
	"slices"
	"testing"
)

// TestKdialogConflictArgs pins the flags handed to kdialog: a --yesnocancel prompt
// carrying the title, message, and the three choice labels in the expected slots.
func TestKdialogConflictArgs(t *testing.T) {
	args := kdialogConflictArgs("A title", "A message")
	want := []string{
		"--title", "A title",
		"--yesnocancel", "A message",
		"--yes-label", "Keep local",
		"--no-label", "Use server",
		"--cancel-label", "Skip",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("kdialogConflictArgs = %v, want %v", args, want)
	}
}

// TestInterpretKdialogConflict maps kdialog's exit codes to choices: 0 = Keep local,
// 1 = Use server, 2 = Skip, and any unexpected status is the conservative Skip.
func TestInterpretKdialogConflict(t *testing.T) {
	tests := []struct {
		code int
		want ConflictChoice
	}{
		{0, ConflictKeepLocal},
		{1, ConflictUseServer},
		{2, ConflictSkip},
		{-1, ConflictSkip},
		{255, ConflictSkip},
	}
	for _, tt := range tests {
		if got := interpretKdialogConflict(tt.code); got != tt.want {
			t.Fatalf("interpretKdialogConflict(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
}

// TestZenityConflictArgs pins the flags handed to zenity: a --question with the
// extra button that carries the third choice, plus the ok/cancel label overrides.
func TestZenityConflictArgs(t *testing.T) {
	args := zenityConflictArgs("A title", "A message")
	want := []string{
		"--question",
		"--title", "A title",
		"--text", "A message",
		"--ok-label", "Keep local",
		"--cancel-label", "Skip",
		"--extra-button", "Use server",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("zenityConflictArgs = %v, want %v", args, want)
	}
}

// TestInterpretZenityConflict maps zenity's (exit code, stdout) to choices: OK exits
// 0 with empty stdout (Keep local); the extra button exits non-zero but echoes its
// label (Use server); Cancel and a closed window exit non-zero with empty stdout
// (Skip). The extra-button label is matched even with surrounding whitespace/newline
// as zenity prints it.
func TestInterpretZenityConflict(t *testing.T) {
	tests := []struct {
		name   string
		code   int
		stdout string
		want   ConflictChoice
	}{
		{"ok clicked", 0, "", ConflictKeepLocal},
		{"extra button", 1, "Use server\n", ConflictUseServer},
		{"extra button no newline", 1, "Use server", ConflictUseServer},
		{"cancel", 1, "", ConflictSkip},
		{"closed window", 255, "", ConflictSkip},
		{"unexpected stdout non-zero", 1, "something else", ConflictSkip},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := interpretZenityConflict(tt.code, tt.stdout); got != tt.want {
				t.Fatalf("interpretZenityConflict(%d, %q) = %v, want %v", tt.code, tt.stdout, got, tt.want)
			}
		})
	}
}

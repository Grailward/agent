package main

import (
	"os"
	"path/filepath"
	"testing"
)

// mkDir creates a directory under t.TempDir and returns its path.
func mkDir(t *testing.T, withD2S bool) string {
	t.Helper()
	d := t.TempDir()
	if withD2S {
		if err := os.WriteFile(filepath.Join(d, "Hero.d2s"), syntheticSave(64, 0x01), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// TestPickSavesDirPrefersD2S proves a candidate holding a .d2s wins over a
// merely-existing one, regardless of order — the save is the strongest signal.
func TestPickSavesDirPrefersD2S(t *testing.T) {
	empty := mkDir(t, false)
	withSave := mkDir(t, true)

	if got := pickSavesDir([]string{empty, withSave}); got != withSave {
		t.Fatalf("pickSavesDir = %q, want the dir with a save %q", got, withSave)
	}
	// The first .d2s-bearing candidate wins when several qualify.
	withSave2 := mkDir(t, true)
	if got := pickSavesDir([]string{empty, withSave, withSave2}); got != withSave {
		t.Fatalf("pickSavesDir = %q, want the first dir with a save %q", got, withSave)
	}
}

// TestPickSavesDirFallsBackToFirstExisting proves that with no save anywhere, the
// first existing directory is chosen; a missing path ahead of it is skipped.
func TestPickSavesDirFallsBackToFirstExisting(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	first := mkDir(t, false)
	second := mkDir(t, false)

	if got := pickSavesDir([]string{missing, first, second}); got != first {
		t.Fatalf("pickSavesDir = %q, want the first existing dir %q", got, first)
	}
}

// TestPickSavesDirEmptyAndNonDir proves an empty list and a plain file both yield
// "" (nothing usable), so setup falls back to the folder picker.
func TestPickSavesDirEmptyAndNonDir(t *testing.T) {
	if got := pickSavesDir(nil); got != "" {
		t.Fatalf("pickSavesDir(nil) = %q, want \"\"", got)
	}

	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := pickSavesDir([]string{file}); got != "" {
		t.Fatalf("pickSavesDir([file]) = %q, want \"\" (a file is not a saves dir)", got)
	}
}

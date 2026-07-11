package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// syntheticSave builds a header-valid .d2s payload of the given size (>=12).
func syntheticSave(size int, fill byte) []byte {
	if size < 12 {
		size = 12
	}
	b := make([]byte, size)
	for i := range b {
		b[i] = fill
	}
	binary.LittleEndian.PutUint32(b[0:4], 0xAA55AA55)
	binary.LittleEndian.PutUint32(b[8:12], uint32(size))
	return b
}

func TestDecideMatrix(t *testing.T) {
	tests := []struct {
		name         string
		localPresent bool
		local        string
		lastSync     string
		server       string
		want         SyncDecision
	}{
		{"in sync, state matches", true, "A", "A", "A", DecisionInSync},
		{"in sync, state stale", true, "A", "B", "A", DecisionInSync},
		{"in sync, state missing", true, "A", "", "A", DecisionInSync},
		{"fast-forward", true, "A", "A", "B", DecisionFastForward},
		{"new from server, no state", false, "", "", "S", DecisionNewFromServer},
		{"new from server, absent but server moved", false, "", "OLD", "NEW", DecisionNewFromServer},
		{"absent, server equals last sync", false, "", "S", "S", DecisionInSync},
		{"absent, no state, no server change is impossible; server always set", false, "", "", "S", DecisionNewFromServer},
		{"conflict, both diverge", true, "L", "BASE", "S", DecisionConflict},
		{"conflict, no state, local != server", true, "L", "", "S", DecisionConflict},
		{"push local, server unchanged", true, "L", "BASE", "BASE", DecisionPushLocal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decide(tt.localPresent, tt.local, tt.lastSync, tt.server)
			if got != tt.want {
				t.Fatalf("decide(present=%v, local=%q, last=%q, server=%q) = %d, want %d",
					tt.localPresent, tt.local, tt.lastSync, tt.server, got, tt.want)
			}
		})
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"Kristan.d2s", "Kristan.d2s", true},
		{"SharedStashSoftCoreV2.d2i", "SharedStashSoftCoreV2.d2i", true},
		{"HERO.D2S", "HERO.D2S", true}, // extension check is case-insensitive
		{"../evil.d2s", "", false},
		{"..\\evil.d2s", "", false},
		{"foo/bar.d2s", "", false},
		{"sub\\bar.d2i", "", false},
		{".hidden.d2s", "", false},
		{"..", "", false},
		{".", "", false},
		{"", "", false},
		{"Kristan.txt", "", false},
		{"Kristan", "", false},
		{"Kristan.d2s.exe", "", false},
		// Control characters (NUL, tab, newline, DEL) are rejected.
		{"nul\x00.d2s", "", false},
		{"He\x01ro.d2s", "", false},
		{"Hero\t.d2s", "", false},
		{"Hero\n.d2s", "", false},
		{"Hero\x7f.d2s", "", false},
		// Windows reserved device names, with and without extension, any case.
		{"CON.d2s", "", false},
		{"con.d2s", "", false},
		{"NUL.d2s", "", false},
		{"PRN.d2i", "", false},
		{"AUX.d2s", "", false},
		{"COM1.d2s", "", false},
		{"lpt9.d2i", "", false},
		{"COM1.foo.d2s", "", false}, // reserved segment before the first dot
		// Names that merely resemble device names remain valid.
		{"CONSOLE.d2s", "CONSOLE.d2s", true},
		{"COM0.d2s", "COM0.d2s", true},   // COM0 is not reserved
		{"COM10.d2s", "COM10.d2s", true}, // only COM1-9 are reserved
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := sanitizeFilename(tt.in)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("sanitizeFilename(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestSanitizeSidecarFilename(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		base   string
		want   string
		wantOK bool
	}{
		{"map", "Mira.map", "Mira", "Mira.map", true},
		{"ma0", "Mira.ma0", "Mira", "Mira.ma0", true},
		{"ma4", "Mira.ma4", "Mira", "Mira.ma4", true},
		{"ma9", "Mira.ma9", "Mira", "Mira.ma9", true},
		{"uppercase map ext", "Mira.MAP", "Mira", "Mira.MAP", true},
		{"uppercase ma ext", "Mira.MA0", "Mira", "Mira.MA0", true},
		// Two-digit map index is not a valid sidecar extension.
		{"two digit index", "Mira.ma10", "Mira", "", false},
		// .key and .ctl are explicitly out of scope.
		{"key excluded", "Mira.key", "Mira", "", false},
		{"ctl excluded", "Mira.ctl", "Mira", "", false},
		{"d2s is not a sidecar", "Mira.d2s", "Mira", "", false},
		{"no extension", "Mira", "Mira", "", false},
		// Stem must match the character base exactly.
		{"other char stem", "Other.map", "Mira", "", false},
		{"extra middle segment", "Mira.foo.map", "Mira", "", false},
		// Hostile-input rejections shared with the save sanitizer.
		{"traversal", "../Mira.map", "Mira", "", false},
		{"backslash sep", "sub\\Mira.map", "Mira", "", false},
		{"forward sep", "sub/Mira.map", "Mira", "", false},
		{"dotfile", ".Mira.map", "Mira", "", false},
		{"nul byte", "Mira\x00.map", "Mira", "", false},
		{"control byte", "Mi\x01ra.map", "Mira", "", false},
		{"empty", "", "Mira", "", false},
		// Reserved device name as the base is rejected even with a valid ext.
		{"reserved base", "CON.map", "CON", "", false},
		{"reserved base lower", "nul.map", "nul", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sanitizeSidecarFilename(tt.in, tt.base)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("sanitizeSidecarFilename(%q, %q) = (%q, %v), want (%q, %v)",
					tt.in, tt.base, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestPullWriteNewFile(t *testing.T) {
	saves := t.TempDir()
	backups := t.TempDir()
	data := syntheticSave(64, 0x11)
	sha := sha256hex(data)

	if err := pullWrite(saves, backups, "New.d2s", data, sha, sha); err != nil {
		t.Fatalf("pullWrite failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(saves, "New.d2s"))
	if err != nil {
		t.Fatalf("destination not written: %v", err)
	}
	if sha256hex(got) != sha {
		t.Fatalf("written bytes differ from source")
	}
	// No pre-existing file means no backup.
	if _, err := os.Stat(filepath.Join(backups, "New.d2s")); !os.IsNotExist(err) {
		t.Fatalf("expected no backup for a fresh file, got err=%v", err)
	}
}

func TestPullWriteOverwriteBacksUp(t *testing.T) {
	saves := t.TempDir()
	backups := t.TempDir()
	old := syntheticSave(64, 0x22)
	dest := filepath.Join(saves, "Hero.d2s")
	if err := os.WriteFile(dest, old, 0644); err != nil {
		t.Fatal(err)
	}

	newData := syntheticSave(80, 0x33)
	sha := sha256hex(newData)
	if err := pullWrite(saves, backups, "Hero.d2s", newData, sha, sha); err != nil {
		t.Fatalf("pullWrite failed: %v", err)
	}

	got, _ := os.ReadFile(dest)
	if sha256hex(got) != sha {
		t.Fatalf("destination not overwritten with new bytes")
	}
	bak, err := os.ReadFile(filepath.Join(backups, "Hero.d2s"))
	if err != nil {
		t.Fatalf("expected a backup of the old file: %v", err)
	}
	if sha256hex(bak) != sha256hex(old) {
		t.Fatalf("backup does not match the original bytes")
	}
}

func TestPullWriteShaMismatchAborts(t *testing.T) {
	saves := t.TempDir()
	backups := t.TempDir()
	old := syntheticSave(64, 0x44)
	dest := filepath.Join(saves, "Hero.d2s")
	if err := os.WriteFile(dest, old, 0644); err != nil {
		t.Fatal(err)
	}

	newData := syntheticSave(80, 0x55)
	wrongSHA := sha256hex(syntheticSave(80, 0x66)) // does not match newData

	// Manifest sha mismatch.
	if err := pullWrite(saves, backups, "Hero.d2s", newData, wrongSHA, sha256hex(newData)); err == nil {
		t.Fatal("expected error on manifest sha mismatch")
	}
	// X-Sha256 mismatch.
	if err := pullWrite(saves, backups, "Hero.d2s", newData, sha256hex(newData), wrongSHA); err == nil {
		t.Fatal("expected error on X-Sha256 mismatch")
	}

	// The destination must be untouched by either aborted write.
	got, _ := os.ReadFile(dest)
	if sha256hex(got) != sha256hex(old) {
		t.Fatalf("destination was modified despite an aborted write")
	}
}

func TestPullWriteRejectsUnsafeFilename(t *testing.T) {
	saves := t.TempDir()
	backups := t.TempDir()
	data := syntheticSave(64, 0x77)
	sha := sha256hex(data)

	if err := pullWrite(saves, backups, "../escape.d2s", data, sha, sha); err == nil {
		t.Fatal("expected error for a traversal filename")
	}
	// Nothing must have been written anywhere under saves.
	entries, _ := os.ReadDir(saves)
	if len(entries) != 0 {
		t.Fatalf("unsafe filename produced %d files in saves dir", len(entries))
	}
}

func TestPullWriteEmptyHeaderSkipsHeaderCheck(t *testing.T) {
	saves := t.TempDir()
	backups := t.TempDir()
	data := syntheticSave(64, 0x88)
	sha := sha256hex(data)

	// Empty X-Sha256 must still succeed as long as the manifest sha matches.
	if err := pullWrite(saves, backups, "Hero.d2s", data, sha, ""); err != nil {
		t.Fatalf("expected success with empty header sha, got %v", err)
	}
}

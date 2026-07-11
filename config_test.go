package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigMapSyncDefault: an unset (nil) SyncMapFiles defaults ON; an explicit
// value is honored.
func TestConfigMapSyncDefault(t *testing.T) {
	on, off := true, false
	tests := []struct {
		name string
		val  *bool
		want bool
	}{
		{"nil defaults on", nil, true},
		{"explicit on", &on, true},
		{"explicit off", &off, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{SyncMapFiles: tt.val}
			if got := c.MapSyncEnabled(); got != tt.want {
				t.Fatalf("MapSyncEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestConfigMapSyncRoundTrip: an explicit OFF survives SaveConfig/LoadConfig, and
// a config written by an older agent (no field at all) loads as nil -> ON.
func TestConfigMapSyncRoundTrip(t *testing.T) {
	// Explicit OFF round-trips.
	path := filepath.Join(t.TempDir(), "config.json")
	off := false
	if err := SaveConfig(path, &Config{URL: "u", SyncMapFiles: &off}); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if got.SyncMapFiles == nil || *got.SyncMapFiles {
		t.Fatalf("explicit OFF not round-tripped: %v", got.SyncMapFiles)
	}
	if got.MapSyncEnabled() {
		t.Fatal("explicit OFF must read as disabled")
	}

	// A nil value is omitted from the file (omitempty), so an old config stays
	// valid — decoding it yields nil, which defaults ON.
	onPath := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(onPath, &Config{URL: "u"}); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}
	raw, err := os.ReadFile(onPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sync_map_files") {
		t.Fatalf("nil SyncMapFiles should be omitted from config.json, got:\n%s", raw)
	}

	legacy := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(legacy, []byte(`{"url":"u","token":"t","saves_dir":"/s","poll_interval":2,"sync_mode":"push"}`), 0644); err != nil {
		t.Fatal(err)
	}
	old, err := LoadConfig(legacy)
	if err != nil {
		t.Fatalf("LoadConfig(legacy) failed: %v", err)
	}
	if old.SyncMapFiles != nil {
		t.Fatal("legacy config should decode SyncMapFiles as nil")
	}
	if !old.MapSyncEnabled() {
		t.Fatal("legacy config must default map sync ON")
	}
}

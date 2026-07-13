package main

import (
	"bytes"
	"encoding/binary"
	"log"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetMapSyncLogsChange: toggling map-exploration sync logs the new state on one
// discrete line each way, with no logOnce dedup — a preference change is an event,
// not a persistent per-file condition.
func TestSetMapSyncLogsChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	var buf bytes.Buffer
	orig, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(orig); log.SetFlags(flags) }()

	on := true
	w := &Watcher{
		Config:   &Config{SyncMapFiles: &on},
		lastLine: map[string]string{},
		errs:     map[string]string{},
	}

	w.SetMapSync(false)
	w.SetMapSync(true)

	out := buf.String()
	if !strings.Contains(out, "Sync map exploration disabled") {
		t.Fatalf("missing disable line in:\n%s", out)
	}
	if !strings.Contains(out, "Sync map exploration enabled") {
		t.Fatalf("missing enable line in:\n%s", out)
	}
}

// TestLogOnceSuppressesRepeats guards the fix for the log-spam bug: a
// persistent, unchanged per-file condition (e.g. HTTP 401 every poll) must be
// logged once, while a distinct next outcome logs again.
func TestLogOnceSuppressesRepeats(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(orig); log.SetFlags(flags) }()

	w := &Watcher{lastLine: make(map[string]string)}
	path := "/saves/Birmuth.d2s"

	for i := 0; i < 5; i++ {
		w.logOnce(path, "Birmuth.d2s: ERROR — HTTP 401 - Invalid or missing token")
	}
	w.logOnce(path, "Birmuth.d2s: created (Birmuth lvl 21)") // recovery logs again
	w.logOnce("/saves/Mira.d2s", "Mira.d2s: ERROR — HTTP 401 - Invalid or missing token")

	lines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1
	if lines != 3 {
		t.Fatalf("expected 3 log lines (one 401 + recovery + other file), got %d:\n%s", lines, buf.String())
	}
}

func TestValidSave(t *testing.T) {
	tests := []struct {
		name     string
		setup    func() []byte
		expected bool
	}{
		{
			name: "Valid save file",
			setup: func() []byte {
				data := make([]byte, 20)
				binary.LittleEndian.PutUint32(data[0:4], 0xAA55AA55)
				binary.LittleEndian.PutUint32(data[8:12], 20)
				return data
			},
			expected: true,
		},
		{
			name: "Invalid signature",
			setup: func() []byte {
				data := make([]byte, 20)
				binary.LittleEndian.PutUint32(data[0:4], 0x11223344)
				binary.LittleEndian.PutUint32(data[8:12], 20)
				return data
			},
			expected: false,
		},
		{
			name: "Incorrect size in header",
			setup: func() []byte {
				data := make([]byte, 20)
				binary.LittleEndian.PutUint32(data[0:4], 0xAA55AA55)
				binary.LittleEndian.PutUint32(data[8:12], 15) // says 15, actual 20
				return data
			},
			expected: false,
		},
		{
			name: "Too short slice",
			setup: func() []byte {
				return []byte{0x55, 0xAA, 0x55, 0xAA} // only 4 bytes
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.setup()
			result := validSave(data)
			if result != tt.expected {
				t.Errorf("expected validSave to be %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestValidStash(t *testing.T) {
	tests := []struct {
		name     string
		setup    func() []byte
		expected bool
	}{
		{
			name: "Valid single tab stash",
			setup: func() []byte {
				tab := make([]byte, 64)
				binary.LittleEndian.PutUint32(tab[0:4], 0xAA55AA55)
				binary.LittleEndian.PutUint16(tab[16:18], 64)
				return tab
			},
			expected: true,
		},
		{
			name: "Valid multi-tab stash",
			setup: func() []byte {
				tab1 := make([]byte, 64)
				binary.LittleEndian.PutUint32(tab1[0:4], 0xAA55AA55)
				binary.LittleEndian.PutUint16(tab1[16:18], 64)

				tab2 := make([]byte, 80)
				binary.LittleEndian.PutUint32(tab2[0:4], 0xAA55AA55)
				binary.LittleEndian.PutUint16(tab2[16:18], 80)

				return append(tab1, tab2...)
			},
			expected: true,
		},
		{
			name: "Stash tab size too small",
			setup: func() []byte {
				tab := make([]byte, 64)
				binary.LittleEndian.PutUint32(tab[0:4], 0xAA55AA55)
				binary.LittleEndian.PutUint16(tab[16:18], 50) // less than 64
				return tab
			},
			expected: false,
		},
		{
			name: "Stash second tab bad signature",
			setup: func() []byte {
				tab1 := make([]byte, 64)
				binary.LittleEndian.PutUint32(tab1[0:4], 0xAA55AA55)
				binary.LittleEndian.PutUint16(tab1[16:18], 64)

				tab2 := make([]byte, 80)
				binary.LittleEndian.PutUint32(tab2[0:4], 0x11223344) // bad sig
				binary.LittleEndian.PutUint16(tab2[16:18], 80)

				return append(tab1, tab2...)
			},
			expected: false,
		},
		{
			name: "Stash mismatched length",
			setup: func() []byte {
				tab1 := make([]byte, 64)
				binary.LittleEndian.PutUint32(tab1[0:4], 0xAA55AA55)
				binary.LittleEndian.PutUint16(tab1[16:18], 64)

				tab2 := make([]byte, 80)
				binary.LittleEndian.PutUint32(tab2[0:4], 0xAA55AA55)
				binary.LittleEndian.PutUint16(tab2[16:18], 90) // claims 90, actual bytes 80

				return append(tab1, tab2...)
			},
			expected: false,
		},
		{
			name: "Empty stash slice",
			setup: func() []byte {
				return []byte{}
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.setup()
			result := validStash(data)
			if result != tt.expected {
				t.Errorf("expected validStash to be %v, got %v", tt.expected, result)
			}
		})
	}
}

package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

func main() {
	// Configure logging to show only time.
	log.SetFlags(log.Ltime)

	// Arm the file log before anything can fail. As a menu-bar / background GUI
	// app there is no terminal to read stdout, so a log.Fatalf raised while
	// loading configuration would otherwise vanish. LogPath only needs the config
	// directory, not the loaded config, so it is safe to set up first.
	setupFileLog()

	cfg, err := SetupConfig()
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	// Check if saves directory exists and is a directory.
	info, err := os.Stat(cfg.SavesDir)
	if err != nil {
		log.Fatalf("Saves directory does not exist: %s (error: %v)", cfg.SavesDir, err)
	}
	if !info.IsDir() {
		log.Fatalf("Saves directory path is not a directory: %s", cfg.SavesDir)
	}

	client := NewClient(cfg.URL, cfg.Token)

	watcher, err := NewWatcher(cfg, client)
	if err != nil {
		log.Fatalf("Failed to initialize watcher: %v", err)
	}

	// RunTray owns the main thread; the watcher polls in a goroutine.
	RunTray(watcher, client)
}

// LogPath returns the agent log file path (next to config.json).
func LogPath() (string, error) {
	path, err := GetConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "agent.log"), nil
}

// swallowingWriter forwards writes to an inner writer but always reports full
// success. It shields an io.MultiWriter fan-out from a dead output handle: a
// Windows GUI binary (built with -H windowsgui) has no attached console, so every
// write to os.Stderr fails with "The handle is invalid". io.MultiWriter stops at
// the first writer that errors, which would starve any writer behind the dead
// one — swallowing the error keeps the destinations independent no matter their
// order.
type swallowingWriter struct{ w io.Writer }

func (s swallowingWriter) Write(p []byte) (int, error) {
	_, _ = s.w.Write(p)
	return len(p), nil
}

// setupFileLog tees log output to a truncated-on-start file in the config dir,
// plus stderr (best-effort) for shell runs. The file is listed first, and stderr
// is wrapped so a failed write can never abort the fan-out and leave the log file
// empty — the exact bug that produced blank Windows logs.
func setupFileLog() {
	logPath, err := LogPath()
	if err != nil {
		return
	}
	// First run: the config directory may not exist yet (it is normally created
	// later, when the config is first saved) — without it the log file cannot open.
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(f, swallowingWriter{os.Stderr}))
}

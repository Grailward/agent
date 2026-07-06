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

	cfg, err := SetupConfig()
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	// As a menu-bar app there is no terminal to read stdout; mirror logs to a
	// file next to the config for diagnostics (still echoed to stderr when run
	// from a shell).
	setupFileLog()

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

// setupFileLog tees log output to a rotating-by-truncation file in the config dir.
func setupFileLog() {
	logPath, err := LogPath()
	if err != nil {
		return
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
}

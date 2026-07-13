package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const (
	DefaultURL          = "https://grailward.com"
	DefaultPollInterval = 2.0
)

// Sync modes select how the agent reconciles local and server saves.
//   - SyncModePush (default): the agent only uploads local changes; it never
//     writes to the saves folder.
//   - SyncModeTwoWay: the agent additionally offers to pull newer server saves
//     onto disk. Every write is opt-in and requires explicit user confirmation.
const (
	SyncModePush   = "push"
	SyncModeTwoWay = "two_way"
)

// Version is the build version, injected at build time via
// -ldflags "-X main.Version=vX.Y.Z". Defaults to "dev" for local builds.
var Version = "dev"

type Config struct {
	URL          string  `json:"url"`
	Token        string  `json:"token"`
	SavesDir     string  `json:"saves_dir"`
	PollInterval float64 `json:"poll_interval"`
	SyncMode     string  `json:"sync_mode"`
	// SyncMapFiles toggles syncing the map-exploration sidecar files (.map /
	// .ma0…) that sit beside each character save. A nil pointer (a config written
	// before this option existed) is treated as ON, so upgrading users keep the
	// feature without editing config.json. Read it through MapSyncEnabled.
	SyncMapFiles *bool `json:"sync_map_files,omitempty"`
	// StartAtLogin mirrors whether the per-user OS login item is installed, so the
	// tray checkbox reflects the setting across restarts and startup self-heal can
	// tell whether a login item is expected. Default off (opt-in); omitted from the
	// file when false so older configs stay clean.
	StartAtLogin bool `json:"start_at_login,omitempty"`
}

// MapSyncEnabled reports whether map-exploration sidecar files should be synced.
// It defaults to ON: an unset (nil) value means enabled.
func (c *Config) MapSyncEnabled() bool {
	return c.SyncMapFiles == nil || *c.SyncMapFiles
}

// ConfigDir returns the directory holding config.json (and the sync state and
// backups that live beside it).
func ConfigDir() (string, error) {
	path, err := GetConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

// GetConfigPath returns the path to config.json in the OS-specific user config directory.
func GetConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "grailward-agent", "config.json"), nil
}

// LoadConfig loads configuration from the file.
func LoadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var cfg Config
	dec := json.NewDecoder(file)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SaveConfig saves the configuration atomically: it marshals to a temp file in
// the same directory, then renames over the target. A crash or a concurrent
// writer can never leave a truncated/half-streamed config.json behind.
func SaveConfig(path string, cfg *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n') // match the previous encoder's trailing newline

	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// ClearToken erases the stored token (keeping url/saves_dir/interval) and
// persists the change. Used by the tray "Reset token" action.
func ClearToken(cfg *Config) error {
	cfg.Token = ""
	path, err := GetConfigPath()
	if err != nil {
		return err
	}
	return SaveConfig(path, cfg)
}

// SetupConfig parses CLI flags and returns the final configuration.
// If config is missing, incomplete, or --config is set, it triggers interactive dialogs.
func SetupConfig() (*Config, error) {
	showVersion := flag.Bool("version", false, "Print the agent version and exit")
	clearToken := flag.Bool("clear-token", false, "Erase the local configuration/token and exit")
	reconfigure := flag.Bool("config", false, "Re-trigger dialog inputs to update configuration")
	urlFlag := flag.String("url", "", "Override target URL (defaults to https://grailward.com)")
	savesDirFlag := flag.String("saves-dir", "", "Override Saves Directory path")
	pollFlag := flag.Float64("poll", 0, "Override polling interval in seconds")

	flag.Parse()

	if *showVersion {
		fmt.Printf("grailward-agent %s\n", Version)
		os.Exit(0)
	}

	configPath, err := GetConfigPath()
	if err != nil {
		return nil, fmt.Errorf("failed to get config path: %w", err)
	}

	if *clearToken {
		if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to clear token/config: %w", err)
		}
		fmt.Println("Configuration and token cleared successfully.")
		os.Exit(0)
	}

	var cfg *Config
	loaded := false

	if !*reconfigure {
		if c, err := LoadConfig(configPath); err == nil {
			cfg = c
			loaded = true
		}
	}

	if cfg == nil {
		cfg = &Config{
			URL:          DefaultURL,
			PollInterval: DefaultPollInterval,
		}
	}

	// Apply URL CLI override if provided, else keep loaded or default
	if *urlFlag != "" {
		cfg.URL = *urlFlag
	} else if cfg.URL == "" {
		cfg.URL = DefaultURL
	}

	// Apply Poll CLI override if provided, else keep loaded or default
	if *pollFlag > 0 {
		cfg.PollInterval = *pollFlag
	} else if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}

	// Override SavesDir CLI override if provided
	if *savesDirFlag != "" {
		cfg.SavesDir = *savesDirFlag
	}

	// Default to push-only sync (never writes to disk) unless the config picks
	// two-way explicitly.
	if cfg.SyncMode != SyncModeTwoWay {
		cfg.SyncMode = SyncModePush
	}

	// Check if Token or SavesDir is missing and needs prompt
	needsSave := !loaded || *reconfigure

	if cfg.Token == "" {
		token, err := PromptToken(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("failed to prompt for token: %w", err)
		}
		if token == "" {
			return nil, fmt.Errorf("token cannot be empty")
		}
		cfg.Token = token
		needsSave = true
	}

	if cfg.SavesDir == "" {
		defaultSaves := GetDefaultSavesDir()
		savesDir, err := PromptSavesDir(defaultSaves)
		if err != nil {
			return nil, fmt.Errorf("failed to prompt for saves directory: %w", err)
		}
		if savesDir == "" {
			return nil, fmt.Errorf("saves directory cannot be empty")
		}
		cfg.SavesDir = savesDir
		needsSave = true
	}

	if needsSave {
		if err := SaveConfig(configPath, cfg); err != nil {
			return nil, fmt.Errorf("failed to save configuration: %w", err)
		}
		fmt.Printf("Configuration saved to %s\n", configPath)
	}

	return cfg, nil
}

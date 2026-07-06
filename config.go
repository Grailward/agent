package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultURL          = "https://grailward.com"
	DefaultPollInterval = 2.0
)

// Version is the build version, injected at build time via
// -ldflags "-X main.Version=vX.Y.Z". Defaults to "dev" for local builds.
var Version = "dev"

type Config struct {
	URL          string  `json:"url"`
	Token        string  `json:"token"`
	SavesDir     string  `json:"saves_dir"`
	PollInterval float64 `json:"poll_interval"`
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

// SaveConfig saves the configuration to the file.
func SaveConfig(path string, cfg *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	return enc.Encode(cfg)
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

// PromptAndSaveToken opens the native token dialog, stores the result and
// persists it. Returns the new token (empty if the user cancelled).
func PromptAndSaveToken(cfg *Config) (string, error) {
	token, err := PromptToken(cfg.URL)
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", nil
	}
	cfg.Token = token
	path, err := GetConfigPath()
	if err != nil {
		return "", err
	}
	if err := SaveConfig(path, cfg); err != nil {
		return "", err
	}
	return token, nil
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

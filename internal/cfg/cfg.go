// Package cfg persists user preferences to ~/.config/taildrives/config.json.
package cfg

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Config is the on-disk shape.
type Config struct {
	Category      string   `json:"category"`
	Endpoint      string   `json:"endpoint"`
	UserNamespace string   `json:"user_namespace"`
	Concurrency   int      `json:"concurrency"`
	Theme         string   `json:"theme"`
	Favorites     []string `json:"favorites"`
}

// Default returns a sensible empty config.
func Default() Config {
	return Config{
		Category:    "No Active Content Filtering",
		Endpoint:    "http://100.100.100.100:8080",
		Concurrency: 4,
		Theme:       "cyberpunk",
		Favorites:   []string{},
	}
}

// Dir is the on-disk config directory (XDG_CONFIG_HOME-aware).
func Dir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "taildrives")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "taildrives")
}

// File returns the absolute path to the config file.
func File() string { return filepath.Join(Dir(), "config.json") }

// Load reads config. Returns defaults on any error.
func Load() Config {
	def := Default()
	b, err := os.ReadFile(File())
	if err != nil {
		return def
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return def
	}
	// Backfill missing fields with defaults so older configs upgrade cleanly.
	// Migrate legacy "no-filter" category names to the current default.
	switch c.Category {
	case "", "all", "all devices", "all content":
		c.Category = def.Category
	}
	if c.Endpoint == "" {
		c.Endpoint = def.Endpoint
	}
	if c.Concurrency == 0 {
		c.Concurrency = def.Concurrency
	}
	if c.Theme == "" {
		c.Theme = def.Theme
	}
	if c.Favorites == nil {
		c.Favorites = []string{}
	}
	return c
}

// Save atomically writes config to disk.
func Save(c Config) error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(Dir(), "config-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		// Best-effort cleanup if rename failed.
		if _, statErr := os.Stat(tmp.Name()); !errors.Is(statErr, fs.ErrNotExist) {
			_ = os.Remove(tmp.Name())
		}
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), File())
}

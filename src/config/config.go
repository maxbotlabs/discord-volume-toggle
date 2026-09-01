// Package config persists the app's settings to a JSON file in %APPDATA%.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds the persisted settings.
type Config struct {
	KeybindVK   uint32    `json:"keybind_vk"`
	KeybindMods uint32    `json:"keybind_mods"`
	Levels      []float32 `json:"levels"`      // volume levels to cycle through, e.g. [1.0, 0.5, 0.25, 0.0]
	ProcessName string    `json:"process_name"` // e.g. "Discord.exe"
}

// Log is a hook for debug logging (set by the caller).
var Log func(format string, args ...interface{})

func logf(format string, args ...interface{}) {
	if Log != nil {
		Log(format, args...)
	}
}

// Default returns the default configuration.
func Default() Config {
	return Config{
		KeybindVK:   0x56, // 'V'
		KeybindMods: 0x0002 | 0x0001, // Ctrl+Alt
		Levels:      []float32{1.0, 0.5, 0.25, 0.0},
		ProcessName: "Discord.exe",
	}
}

// Path returns the config file path in %APPDATA%\DiscordVolumeToggle\config.json.
func Path() (string, error) {
	appData, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(appData, "DiscordVolumeToggle")
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the config, falling back to defaults if the file is missing or
// corrupt.
func Load() (Config, error) {
	cfg := Default()
	p, err := Path()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Default(), nil // corrupt -> reset to defaults
	}
	// Defensive: if Levels is empty (e.g. from an old config format), restore
	// the default levels.
	if len(cfg.Levels) == 0 {
		cfg.Levels = Default().Levels
	}
	logf("loaded config from %s: vk=0x%X mods=0x%X levels=%v", p, cfg.KeybindVK, cfg.KeybindMods, cfg.Levels)
	return cfg, nil
}

// Save writes the config to disk, creating directories as needed.
func Save(cfg Config) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	logf("saving config to %s: %s", p, string(data))
	return os.WriteFile(p, data, 0o644)
}

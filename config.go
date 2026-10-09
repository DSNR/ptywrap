package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config holds user-tunable settings loaded from
// $XDG_CONFIG_HOME/ptywrap/config.json (or ~/.config/ptywrap/config.json).
// Any field left at its zero value falls back to the built-in default.
type Config struct {
	Shell          string `json:"shell"`
	ScrollbackSize int    `json:"scrollbackSize"`
	LogDir         string `json:"logDir"`
	HideStatusBar  bool   `json:"hideStatusBar"`
	CursorStyle    string `json:"cursorStyle"` // "block" (default) or "bar"
	Theme          Theme  `json:"theme"`
}

type Theme struct {
	Preset         string    `json:"preset"`
	BorderColor    string    `json:"borderColor"`
	BellColor      string    `json:"bellColor"`
	SelectionColor string    `json:"selectionColor"`
	SearchColor    string    `json:"searchColor"`
	SSHColor       string    `json:"sshColor"`
	Palette        [16]string `json:"palette"` // ANSI 0-15 overrides; empty entries keep the terminal default
}

// presets are built-in named themes selectable via Theme.Preset. Palette
// order is the standard ANSI 0-15: black, red, green, yellow, blue,
// magenta, cyan, white, then their bright variants.
var presets = map[string]Theme{
	"dracula": {
		BorderColor: "141", BellColor: "203", SelectionColor: "61", SearchColor: "212", SSHColor: "228",
		Palette: [16]string{
			"#21222c", "#ff5555", "#50fa7b", "#f1fa8c", "#bd93f9", "#ff79c6", "#8be9fd", "#f8f8f2",
			"#6272a4", "#ff6e6e", "#69ff94", "#ffffa5", "#d6acff", "#ff92df", "#a4ffff", "#ffffff",
		},
	},
	"nord": {
		BorderColor: "109", BellColor: "167", SelectionColor: "60", SearchColor: "108", SSHColor: "179",
		Palette: [16]string{
			"#3b4252", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#88c0d0", "#e5e9f0",
			"#4c566a", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#8fbcbb", "#eceff4",
		},
	},
	"gruvbox-dark": {
		BorderColor: "142", BellColor: "167", SelectionColor: "237", SearchColor: "214", SSHColor: "208",
		Palette: [16]string{
			"#282828", "#cc241d", "#98971a", "#d79921", "#458588", "#b16286", "#689d6a", "#a89984",
			"#928374", "#fb4934", "#b8bb26", "#fabd2f", "#83a598", "#d3869b", "#8ec07c", "#ebdbb2",
		},
	},
	"solarized-dark": {
		BorderColor: "37", BellColor: "160", SelectionColor: "240", SearchColor: "136", SSHColor: "136",
		Palette: [16]string{
			"#073642", "#dc322f", "#859900", "#b58900", "#268bd2", "#d33682", "#2aa198", "#eee8d5",
			"#002b36", "#cb4b16", "#586e75", "#657b83", "#839496", "#6c71c4", "#93a1a1", "#fdf6e3",
		},
	},
}

func defaultConfig() Config {
	return Config{
		ScrollbackSize: 5000,
		CursorStyle:    "block",
		Theme: Theme{
			BorderColor:    "2",
			BellColor:      "196",
			SelectionColor: "237",
			SearchColor:    "58",
			SSHColor:       "3",
		},
	}
}

func configPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "ptywrap", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "ptywrap", "config.json")
}

// writeDefaultConfig writes the built-in default config as JSON to
// configPath(), creating parent directories as needed. It refuses to
// overwrite an existing file.
func writeDefaultConfig() error {
	path := configPath()
	if path == "" {
		return errors.New("could not determine config path")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config already exists at %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(defaultConfig(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// loadConfig reads the config file if present, overlaying it on the
// defaults. A missing file is not an error.
func loadConfig() Config {
	cfg := defaultConfig()
	path := configPath()
	if path == "" {
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	var fileCfg Config
	if err := json.Unmarshal(data, &fileCfg); err != nil {
		return cfg
	}
	if fileCfg.Shell != "" {
		cfg.Shell = fileCfg.Shell
	}
	if fileCfg.ScrollbackSize != 0 {
		cfg.ScrollbackSize = fileCfg.ScrollbackSize
	}
	if fileCfg.LogDir != "" {
		cfg.LogDir = fileCfg.LogDir
	}
	cfg.HideStatusBar = fileCfg.HideStatusBar
	if fileCfg.CursorStyle != "" {
		cfg.CursorStyle = fileCfg.CursorStyle
	}
	if fileCfg.Theme.Preset != "" {
		if preset, ok := presets[fileCfg.Theme.Preset]; ok {
			cfg.Theme = preset
		}
	}
	if fileCfg.Theme.BorderColor != "" {
		cfg.Theme.BorderColor = fileCfg.Theme.BorderColor
	}
	if fileCfg.Theme.BellColor != "" {
		cfg.Theme.BellColor = fileCfg.Theme.BellColor
	}
	if fileCfg.Theme.SelectionColor != "" {
		cfg.Theme.SelectionColor = fileCfg.Theme.SelectionColor
	}
	if fileCfg.Theme.SearchColor != "" {
		cfg.Theme.SearchColor = fileCfg.Theme.SearchColor
	}
	if fileCfg.Theme.SSHColor != "" {
		cfg.Theme.SSHColor = fileCfg.Theme.SSHColor
	}
	for i, c := range fileCfg.Theme.Palette {
		if c != "" {
			cfg.Theme.Palette[i] = c
		}
	}
	return cfg
}

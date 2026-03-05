package tools

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// ShellConfig holds the parsed .config/shell.toml configuration.
type ShellConfig struct {
	Defaults ShellDefaults              `toml:"defaults"`
	Commands map[string]ShellCommandCfg `toml:"commands"`
}

// ShellDefaults holds default values for all commands.
type ShellDefaults struct {
	Timeout       string `toml:"timeout"`
	MaxOutput     int    `toml:"max_output"`
	WorkspaceOnly bool   `toml:"workspace_only"`
}

// ShellCommandCfg holds per-command overrides.
type ShellCommandCfg struct {
	Description   string `toml:"description"`
	Timeout       string `toml:"timeout"`
	WorkspaceOnly *bool  `toml:"workspace_only"`
}

// loadShellConfig reads and parses a TOML shell config file. Returns sensible
// defaults if the file doesn't exist.
func loadShellConfig(path string) (*ShellConfig, error) {
	cfg := &ShellConfig{
		Defaults: ShellDefaults{
			Timeout:       "30s",
			MaxOutput:     10000,
			WorkspaceOnly: true,
		},
		Commands: make(map[string]ShellCommandCfg),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read shell config: %w", err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse shell config: %w", err)
	}

	if cfg.Commands == nil {
		cfg.Commands = make(map[string]ShellCommandCfg)
	}
	if cfg.Defaults.Timeout == "" {
		cfg.Defaults.Timeout = "30s"
	}
	if cfg.Defaults.MaxOutput <= 0 {
		cfg.Defaults.MaxOutput = 10000
	}

	return cfg, nil
}

// IsAllowed returns true if the command is in the allowlist.
func (cfg *ShellConfig) IsAllowed(cmd string) bool {
	_, ok := cfg.Commands[cmd]
	return ok
}

// ResolvedTimeout returns the effective timeout for a command.
func (cfg *ShellConfig) ResolvedTimeout(cmd string) time.Duration {
	if cc, ok := cfg.Commands[cmd]; ok && cc.Timeout != "" {
		if d, err := time.ParseDuration(cc.Timeout); err == nil {
			return d
		}
	}
	if d, err := time.ParseDuration(cfg.Defaults.Timeout); err == nil {
		return d
	}
	return 30 * time.Second
}

// ResolvedWorkspaceOnly returns the effective workspace_only setting for a command.
func (cfg *ShellConfig) ResolvedWorkspaceOnly(cmd string) bool {
	if cc, ok := cfg.Commands[cmd]; ok && cc.WorkspaceOnly != nil {
		return *cc.WorkspaceOnly
	}
	return cfg.Defaults.WorkspaceOnly
}

// ResolvedMaxOutput returns the effective max output size in characters.
func (cfg *ShellConfig) ResolvedMaxOutput() int {
	if cfg.Defaults.MaxOutput > 0 {
		return cfg.Defaults.MaxOutput
	}
	return 10000
}

// CommandDescriptions returns a one-line-per-command summary for prompt injection.
func (cfg *ShellConfig) CommandDescriptions() string {
	var parts []string
	for name, cc := range cfg.Commands {
		entry := name
		if cc.Description != "" {
			entry += " (" + cc.Description + ")"
		}
		if cc.Timeout != "" {
			entry += " [" + cc.Timeout + "]"
		}
		if cc.WorkspaceOnly != nil && *cc.WorkspaceOnly {
			entry += " [workspace only]"
		} else if cc.WorkspaceOnly == nil && cfg.Defaults.WorkspaceOnly {
			entry += " [workspace only]"
		}
		parts = append(parts, entry)
	}
	if len(parts) == 0 {
		return "No commands configured."
	}
	return "Available commands: " + strings.Join(parts, ", ")
}

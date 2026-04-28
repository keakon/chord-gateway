// Package config defines the gateway configuration model and path resolution.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Paths holds the resolved file system paths for the gateway.
// All paths follow the priority: CLI flags > env vars > XDG defaults.
type Paths struct {
	// ConfigHome is the directory containing config files (e.g. config.yaml).
	ConfigHome string
	// ConfigFile is the resolved path to the gateway config file.
	ConfigFile string
	// StateDir is the base directory for persistent runtime state.
	StateDir string
	// LogFile is the resolved path to the gateway log file.
	LogFile string
	// DedupeDir is the directory for Feishu deduplication persistence.
	DedupeDir string
}

// XDG default values.
const (
	defaultConfigHome = ".config/chord-gateway"
	defaultStateDir   = ".local/state/chord-gateway"
	defaultLogFile    = "gateway.log"
)

// Environment variable names.
const (
	envConfigHome = "CHORD_GATEWAY_CONFIG_HOME"
	envStateDir   = "CHORD_GATEWAY_STATE_DIR"
	envConfigFile = "CHORD_GATEWAY_CONFIG"
	envLogFile    = "CHORD_GATEWAY_LOG_FILE"
)

// Resolve returns a Paths instance based on the provided overrides and
// XDG base directories. Pass empty strings for all override arguments to get
// the defaults.
//
// configHomeOverride and configFileOverride are mutually exclusive;
// if configFileOverride is non-empty, configHomeOverride is ignored.
func Resolve(configHomeOverride, configFileOverride, stateDirOverride, logFileOverride, xdgConfigHome, xdgStateHome, home string) (*Paths, error) {
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}

	// --- ConfigHome / ConfigFile ---
	configHome := configHomeOverride
	if configHome == "" {
		if xdgConfigHome != "" {
			configHome = filepath.Join(xdgConfigHome, "chord-gateway")
		} else {
			configHome = filepath.Join(home, defaultConfigHome)
		}
	}

	configFile := configFileOverride
	if configFile == "" {
		if configHomeOverride != "" {
			// When user explicitly sets config home, use config.yaml inside it.
			configFile = filepath.Join(configHome, "config.yaml")
		} else if xdgConfigHome != "" {
			// With XDG_CONFIG_HOME set, use <xdg>/chord-gateway/config.yaml.
			configFile = filepath.Join(xdgConfigHome, "chord-gateway", "config.yaml")
		} else {
			// Default: <home>/.config/chord-gateway/config.yaml.
			configFile = filepath.Join(home, defaultConfigHome, "config.yaml")
		}
	}

	// --- StateDir ---
	stateDir := stateDirOverride
	if stateDir == "" {
		if xdgStateHome != "" {
			stateDir = filepath.Join(xdgStateHome, "chord-gateway")
		} else {
			stateDir = filepath.Join(home, defaultStateDir)
		}
	}

	// --- LogFile ---
	logFile := logFileOverride
	if logFile == "" {
		logFile = filepath.Join(stateDir, defaultLogFile)
	}

	// --- DedupeDir (feishu deduplication state) ---
	dedupeDir := stateDir

	return &Paths{
		ConfigHome: configHome,
		ConfigFile: configFile,
		StateDir:   stateDir,
		LogFile:    logFile,
		DedupeDir:  dedupeDir,
	}, nil
}

// ResolveFromEnv reads XDG environment variables and home dir, then calls Resolve.
// Priority: explicit overrides (typically CLI flags) > env vars > XDG defaults.
func ResolveFromEnv(configHomeOverride, configFileOverride, stateDirOverride, logFileOverride string) (*Paths, error) {
	xdgConfigHome := os.Getenv("XDG_CONFIG_HOME")
	xdgStateHome := os.Getenv("XDG_STATE_HOME")
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	if configFileOverride == "" {
		if v := os.Getenv(envConfigFile); v != "" {
			configFileOverride = v
		}
	}
	if configHomeOverride == "" {
		if v := os.Getenv(envConfigHome); v != "" {
			configHomeOverride = v
		}
	}
	if stateDirOverride == "" {
		if v := os.Getenv(envStateDir); v != "" {
			stateDirOverride = v
		}
	}
	if logFileOverride == "" {
		if v := os.Getenv(envLogFile); v != "" {
			logFileOverride = v
		}
	}

	return Resolve(configHomeOverride, configFileOverride, stateDirOverride, logFileOverride, xdgConfigHome, xdgStateHome, home)
}

// Expand expands ~ at the start of p to the user's home directory.
func Expand(p string) string {
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

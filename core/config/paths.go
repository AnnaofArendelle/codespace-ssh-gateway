package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AppName is used for config/state directory names.
const AppName = "ssh-gateway"

// Env vars that override the default locations.
const (
	EnvConfigPath = "GATEWAY_CONFIG"
	EnvStateDir   = "GATEWAY_STATE_DIR"
)

// DefaultConfigPath resolves the config file location:
// $GATEWAY_CONFIG, else $XDG_CONFIG_HOME/ssh-gateway/config.yaml,
// else ~/.config/ssh-gateway/config.yaml.
func DefaultConfigPath() string {
	if p := os.Getenv(EnvConfigPath); p != "" {
		return ExpandPath(p)
	}
	return filepath.Join(DefaultConfigDir(), "config.yaml")
}

// DefaultConfigDir is the directory holding the config file.
func DefaultConfigDir() string {
	if p := os.Getenv(EnvConfigPath); p != "" {
		return filepath.Dir(ExpandPath(p))
	}
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(ExpandPath(base), AppName)
	}
	return filepath.Join(homeDir(), ".config", AppName)
}

// DefaultStateDir is where host keys, the codespace key pair, known hosts and
// the control socket live.
func DefaultStateDir() string {
	if p := os.Getenv(EnvStateDir); p != "" {
		return ExpandPath(p)
	}
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(ExpandPath(base), AppName)
	}
	return filepath.Join(homeDir(), ".local", "state", AppName)
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}

// ExpandPath expands a leading ~ and makes the path absolute where possible.
func ExpandPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(homeDir(), p[2:])
	}
	return p
}

// EnsureDir creates dir (and parents) with the given mode.
func EnsureDir(dir string, mode os.FileMode) error {
	if dir == "" {
		return fmt.Errorf("empty directory path")
	}
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	// MkdirAll honours umask; tighten explicitly for private dirs.
	if mode&0o077 == 0 {
		if err := os.Chmod(dir, mode); err != nil {
			return err
		}
	}
	return nil
}

// PermWarning returns a non-empty warning when a file holding secrets is
// readable by anyone but its owner.
func PermWarning(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if mode := st.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Sprintf("%s is mode %#o; it may contain a token. Run: chmod 600 %s", path, mode, path)
	}
	return ""
}

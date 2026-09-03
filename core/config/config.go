// Package config loads the gateway's configuration file.
//
// Core keys (provider, ssh, lifecycle, log, control, state_dir) are validated
// strictly. Every other top-level key is treated as an opaque provider section
// and handed to that provider verbatim, so provider-specific settings never
// need a struct field in the core.
package config

import (
	"path/filepath"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/secret"

	"gopkg.in/yaml.v3"
)

// Config is the whole configuration file.
type Config struct {
	Provider  string
	StateDir  string
	SSH       SSHConfig
	Lifecycle LifecycleConfig
	Log       LogConfig
	Control   ControlConfig

	path     string
	sections map[string]*yaml.Node
	warnings []string
}

// SSHConfig is the gateway's own SSH front door. These credentials are entirely
// separate from whatever the provider uses to reach an environment.
type SSHConfig struct {
	Listen               string        `yaml:"listen"`
	HostKey              string        `yaml:"host_key"`
	AuthorizedKeys       string        `yaml:"authorized_keys"`
	AuthorizedKeysInline []string      `yaml:"authorized_keys_inline"`
	PasswordAuth         bool          `yaml:"password_auth"`
	Password             secret.Value  `yaml:"password"`
	AllowedUsers         []string      `yaml:"allowed_users"`
	MaxSessions          int           `yaml:"max_sessions"`
	MaxSessionsPerEnv    int           `yaml:"max_sessions_per_environment"`
	HandshakeTimeout     time.Duration `yaml:"handshake_timeout"`
	Banner               string        `yaml:"banner"`
	ShutdownGrace        time.Duration `yaml:"shutdown_grace"`
	// InstallAlias keeps a `Host codespace` block in ~/.ssh/config up to date on
	// every start, so `ssh root@codespace` works without a separate step. Set it
	// to false to manage ~/.ssh/config yourself.
	InstallAlias *bool `yaml:"install_alias"`
}

// LifecycleConfig bounds the orchestration the gateway performs. There is
// deliberately no idle or stop timer here: idling is the provider's job.
type LifecycleConfig struct {
	AutoCreate         bool          `yaml:"auto_create"`
	StartTimeout       time.Duration `yaml:"start_timeout"`
	CreateTimeout      time.Duration `yaml:"create_timeout"`
	ConnectTimeout     time.Duration `yaml:"connect_timeout"`
	StatusPollInterval time.Duration `yaml:"status_poll_interval"`
	ConnectRetries     int           `yaml:"connect_retries"`

	// StopOnLastDisconnect is opt-in and off by default. When enabled the
	// gateway calls Provider.Stop as soon as its last session for an
	// environment closes. This is an explicit operator-requested stop, not an
	// activity detector: there is no timer, and nothing pretends to be the
	// provider's own idle mechanism. Leave it off to let the provider's
	// official idle handling own the lifecycle.
	StopOnLastDisconnect bool `yaml:"stop_on_last_disconnect"`
}

// LogConfig mirrors logging.Options.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	File   string `yaml:"file"`
}

// ControlConfig is the local unix socket used by `gateway status` / `stop`.
type ControlConfig struct {
	Enabled bool   `yaml:"enabled"`
	Socket  string `yaml:"socket"`
}

// AliasEnabled reports whether the gateway may keep the ~/.ssh/config entry.
func (c *Config) AliasEnabled() bool {
	return c.SSH.InstallAlias == nil || *c.SSH.InstallAlias
}

// Defaults returns a config with every default filled in.
func Defaults() *Config {
	return &Config{
		Provider: "github",
		StateDir: DefaultStateDir(),
		SSH: SSHConfig{
			// Loopback by default: that is what makes the zero-configuration
			// local setup safe to leave without a key or password.
			Listen:            "127.0.0.1:2222",
			HandshakeTimeout:  30 * time.Second,
			ShutdownGrace:     5 * time.Second,
			MaxSessions:       0,
			MaxSessionsPerEnv: 0,
		},
		Lifecycle: LifecycleConfig{
			AutoCreate:         true,
			StartTimeout:       5 * time.Minute,
			CreateTimeout:      20 * time.Minute,
			ConnectTimeout:     2 * time.Minute,
			StatusPollInterval: 2 * time.Second,
			ConnectRetries:     10,
		},
		Log:      LogConfig{Level: "info", Format: "text"},
		Control:  ControlConfig{Enabled: true},
		sections: map[string]*yaml.Node{},
	}
}

// Path is the file this config was loaded from ("" if defaults only).
func (c *Config) Path() string { return c.path }

// Warnings are non-fatal problems found while loading.
func (c *Config) Warnings() []string { return c.warnings }

// ProviderSection returns the raw config section for a provider, or nil.
func (c *Config) ProviderSection(key string) providersSection {
	if n, ok := c.sections[key]; ok && n != nil && !n.IsZero() {
		return providersSection{node: n}
	}
	return providersSection{}
}

// SectionKeys lists the provider sections present in the file.
func (c *Config) SectionKeys() []string {
	out := make([]string, 0, len(c.sections))
	for k := range c.sections {
		out = append(out, k)
	}
	return out
}

// HostKeyPath is the resolved host key location.
func (c *Config) HostKeyPath() string {
	if c.SSH.HostKey != "" {
		return ExpandPath(c.SSH.HostKey)
	}
	return filepath.Join(c.StateDir, "host_ed25519")
}

// AuthorizedKeysPath is the resolved authorized_keys location.
func (c *Config) AuthorizedKeysPath() string {
	if c.SSH.AuthorizedKeys != "" {
		return ExpandPath(c.SSH.AuthorizedKeys)
	}
	dir := DefaultConfigDir()
	if c.path != "" {
		dir = filepath.Dir(c.path)
	}
	return filepath.Join(dir, "authorized_keys")
}

// ControlSocketPath is the resolved control socket location.
func (c *Config) ControlSocketPath() string {
	if c.Control.Socket != "" {
		return ExpandPath(c.Control.Socket)
	}
	return filepath.Join(c.StateDir, "control.sock")
}

// ProviderStateDir is a private directory for one provider's own files.
func (c *Config) ProviderStateDir(name string) string {
	return filepath.Join(c.StateDir, "providers", name)
}

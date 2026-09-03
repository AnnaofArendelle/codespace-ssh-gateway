package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// coreKeys are the top-level keys owned by the core; anything else is a
// provider section.
var coreKeys = map[string]bool{
	"provider":  true,
	"state_dir": true,
	"ssh":       true,
	"lifecycle": true,
	"log":       true,
	"control":   true,
}

// providersSection adapts a raw YAML node to providers.ConfigDecoder.
type providersSection struct{ node *yaml.Node }

// Present reports whether the config file actually had this section.
func (s providersSection) Present() bool { return s.node != nil }

// Decode unmarshals the section into v, rejecting unknown fields. An absent
// section decodes to nothing, leaving v at its defaults.
func (s providersSection) Decode(v any) error {
	if s.node == nil {
		return nil
	}
	return decodeStrict(s.node, v)
}

func decodeStrict(n *yaml.Node, out any) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(n); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(buf.Bytes()))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

// minPollInterval is the fastest the gateway will poll a provider for state.
const minPollInterval = 100 * time.Millisecond

// ErrNoConfig is returned by Load when the file does not exist.
var ErrNoConfig = errors.New("config file not found")

// Load reads and validates the config file. path may be empty for the default
// location.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}
	path = ExpandPath(path)

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w at %s (run `gateway config init`)", ErrNoConfig, path)
		}
		return nil, err
	}

	cfg := Defaults()
	cfg.path = path

	var top map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	for key, node := range top {
		n := node
		if !coreKeys[key] {
			cfg.sections[key] = &n
			continue
		}
		var err error
		switch key {
		case "provider":
			err = n.Decode(&cfg.Provider)
		case "state_dir":
			err = n.Decode(&cfg.StateDir)
		case "ssh":
			err = decodeStrict(&n, &cfg.SSH)
		case "lifecycle":
			err = decodeStrict(&n, &cfg.Lifecycle)
		case "log":
			err = decodeStrict(&n, &cfg.Log)
		case "control":
			err = decodeStrict(&n, &cfg.Control)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, key, err)
		}
	}

	cfg.StateDir = ExpandPath(cfg.StateDir)
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir()
	}
	if w := PermWarning(path); w != "" {
		cfg.warnings = append(cfg.warnings, w)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Provider == "" {
		return errors.New("provider must be set")
	}
	if c.SSH.Listen == "" {
		c.SSH.Listen = Defaults().SSH.Listen
	}
	if _, _, err := net.SplitHostPort(c.SSH.Listen); err != nil {
		return fmt.Errorf("ssh.listen %q is not host:port: %w", c.SSH.Listen, err)
	}
	if c.SSH.HandshakeTimeout <= 0 {
		c.SSH.HandshakeTimeout = 30 * time.Second
	}
	if c.SSH.ShutdownGrace < 0 {
		return errors.New("ssh.shutdown_grace must not be negative")
	}
	if c.SSH.MaxSessions < 0 || c.SSH.MaxSessionsPerEnv < 0 {
		return errors.New("ssh session limits must not be negative")
	}
	d := Defaults()
	if c.Lifecycle.StartTimeout <= 0 {
		c.Lifecycle.StartTimeout = d.Lifecycle.StartTimeout
	}
	if c.Lifecycle.CreateTimeout <= 0 {
		c.Lifecycle.CreateTimeout = d.Lifecycle.CreateTimeout
	}
	if c.Lifecycle.ConnectTimeout <= 0 {
		c.Lifecycle.ConnectTimeout = d.Lifecycle.ConnectTimeout
	}
	if c.Lifecycle.StatusPollInterval <= 0 {
		c.Lifecycle.StatusPollInterval = d.Lifecycle.StatusPollInterval
	} else if c.Lifecycle.StatusPollInterval < minPollInterval {
		// A floor, not a correction: polling a provider API faster than this
		// only earns rate limits.
		c.Lifecycle.StatusPollInterval = minPollInterval
	}
	if c.Lifecycle.ConnectRetries < 0 {
		return errors.New("lifecycle.connect_retries must not be negative")
	}
	if _, err := parseLogLevel(c.Log.Level); err != nil {
		return err
	}
	return nil
}

func parseLogLevel(s string) (string, error) {
	switch s {
	case "", "debug", "info", "warn", "warning", "error":
		return s, nil
	}
	return "", fmt.Errorf("log.level %q is not one of debug|info|warn|error", s)
}

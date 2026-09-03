package github

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// envSource is what a codespace was made from. Remembering it lets the gateway
// rebuild a codespace the operator deleted, without asking them to declare a
// repository they already told GitHub about once.
type envSource struct {
	Repository       string `json:"repository"`
	Branch           string `json:"branch,omitempty"`
	Machine          string `json:"machine,omitempty"`
	Location         string `json:"location,omitempty"`
	DevcontainerPath string `json:"devcontainer_path,omitempty"`
}

// sourceCache persists envSource per handle in the provider state directory.
type sourceCache struct {
	path string
	log  *slog.Logger

	mu      sync.Mutex
	loaded  bool
	entries map[string]envSource
}

func newSourceCache(dir string, log *slog.Logger) *sourceCache {
	return &sourceCache{
		path:    filepath.Join(dir, "environments.json"),
		log:     log,
		entries: map[string]envSource{},
	}
}

func (c *sourceCache) load() {
	if c.loaded {
		return
	}
	c.loaded = true
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var entries map[string]envSource
	if err := json.Unmarshal(raw, &entries); err != nil {
		c.log.Warn("ignoring unreadable environment cache", slog.String("path", c.path))
		return
	}
	for k, v := range entries {
		c.entries[k] = v
	}
}

// remember records how a codespace was built, under every name it answers to.
func (c *sourceCache) remember(src envSource, names ...string) {
	if src.Repository == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	changed := false
	for _, name := range names {
		if name == "" {
			continue
		}
		if c.entries[name] != src {
			c.entries[name] = src
			changed = true
		}
	}
	if changed {
		c.flush()
	}
}

// lookup returns what is known about a handle.
func (c *sourceCache) lookup(name string) (envSource, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	src, ok := c.entries[name]
	return src, ok && src.Repository != ""
}

// any returns a remembered source when exactly one repository is known, which
// covers "the only codespace I ever had was deleted".
func (c *sourceCache) any() (envSource, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	var found envSource
	for _, src := range c.entries {
		if src.Repository == "" {
			continue
		}
		if found.Repository != "" && found.Repository != src.Repository {
			return envSource{}, false // ambiguous
		}
		found = src
	}
	return found, found.Repository != ""
}

// flush persists the cache; callers hold the mutex.
func (c *sourceCache) flush() {
	raw, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(c.path, raw, 0o600); err != nil {
		c.log.Debug("could not persist environment cache", slog.Any("error", err))
	}
}

// merge fills empty create options from a remembered source. Explicit config
// always wins.
func (s envSource) merge(cfg CreateConfig) CreateConfig {
	if cfg.Repository == "" {
		cfg.Repository = s.Repository
	}
	if cfg.Branch == "" {
		cfg.Branch = s.Branch
	}
	if cfg.Machine == "" {
		cfg.Machine = s.Machine
	}
	if cfg.Location == "" {
		cfg.Location = s.Location
	}
	if cfg.DevcontainerPath == "" {
		cfg.DevcontainerPath = s.DevcontainerPath
	}
	return cfg
}

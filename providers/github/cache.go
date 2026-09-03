package github

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	gossh "golang.org/x/crypto/ssh"
)

// sshFingerprint renders a signer's public key fingerprint.
func sshFingerprint(s gossh.Signer) string {
	if s == nil {
		return ""
	}
	return gossh.FingerprintSHA256(s.PublicKey())
}

// userCache remembers the remote login name for each codespace so the gateway
// does not have to ask the GitHub CLI on every connection. It is a cache of a
// fact GitHub owns, not a source of truth: a rejected key invalidates it.
type userCache struct {
	path string
	log  *slog.Logger

	mu      sync.Mutex
	loaded  bool
	entries map[string]string
}

func newUserCache(dir string, log *slog.Logger) *userCache {
	return &userCache{path: filepath.Join(dir, "ssh_users.json"), log: log, entries: map[string]string{}}
}

func (c *userCache) load() {
	if c.loaded {
		return
	}
	c.loaded = true
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var entries map[string]string
	if err := json.Unmarshal(raw, &entries); err != nil {
		c.log.Warn("ignoring unreadable ssh user cache", slog.String("path", c.path))
		return
	}
	for k, v := range entries {
		c.entries[k] = v
	}
}

func (c *userCache) get(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	user, ok := c.entries[name]
	return user, ok && user != ""
}

func (c *userCache) set(name, user string) {
	if name == "" || user == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	if c.entries[name] == user {
		return
	}
	c.entries[name] = user
	c.flush()
}

func (c *userCache) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	if _, ok := c.entries[name]; !ok {
		return
	}
	delete(c.entries, name)
	c.flush()
}

// flush persists the cache; callers hold the mutex.
func (c *userCache) flush() {
	raw, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(c.path, raw, 0o600); err != nil {
		c.log.Debug("could not persist ssh user cache", slog.Any("error", err))
	}
}

package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Template renders a starter config file. The token is written only if the
// operator supplied one; the file is always created mode 0600.
func Template(provider, token, environment, listen string) string {
	if provider == "" {
		provider = "github"
	}
	if listen == "" {
		listen = ":2222"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `# ssh-gateway configuration.
# This file can hold a credential: keep it mode 0600.

provider: %s

`, provider)
	if provider == "github" {
		fmt.Fprintf(&b, `github:
  # Personal access token with the "codespace" scope. Leave empty to fall back
  # to $GITHUB_TOKEN, $GH_TOKEN, or the GitHub CLI's own auth (`+"`gh auth token`"+`).
  token: %q
  # Default codespace served when a client does not name one.
  codespace: %q
  # Connection backend, all of which run the official `+"`gh codespace ssh`"+`:
  #   auto  - prefer stdio, fall back to exec
  #   stdio - `+"`gh codespace ssh --stdio`"+` + native SSH client (recommended)
  #   exec  - `+"`gh codespace ssh`"+` on a local pty
  connector: auto
  # Only used when the codespace above does not exist yet.
  create:
    repository: ""            # owner/name (required for auto-create)
    branch: ""                # defaults to the repository default branch
    machine: ""               # e.g. basicLinux32gb; empty = GitHub default
    devcontainer_path: ""
    idle_timeout_minutes: 30  # GitHub's own idle stop window
    retention_period_minutes: 0

`, token, environment)
	}
	fmt.Fprintf(&b, `ssh:
  listen: %q
  # Keys allowed to use the gateway. An "authorized_keys" file next to this
  # config is also read when present.
  authorized_keys_inline: []
  # Password auth is off by default. If enabled without a password, the
  # gateway generates a random one at startup and prints it once.
  password_auth: false
  # Empty means any username is accepted (ssh root@host works).
  allowed_users: []

lifecycle:
  auto_create: true
  start_timeout: 5m
  create_timeout: 20m
  connect_timeout: 2m
  # Keep this false to let the provider's official idle mechanism own the
  # lifecycle. Setting it true makes the gateway stop the environment as soon
  # as its last session closes.
  stop_on_last_disconnect: false

log:
  level: info
  format: text
`, listen)
	return b.String()
}

// WriteFile writes content to path with mode 0600, creating parents.
func WriteFile(path, content string) error {
	if err := EnsureDir(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, []byte(content), 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Patch sets a nested string key in an existing config file, preserving
// comments, formatting and every other value (notably: the token).
func Patch(path string, keyPath []string, value string) error {
	return patch(path, keyPath, value, "!!str")
}

// PatchBool sets a nested boolean key.
func PatchBool(path string, keyPath []string, value bool) error {
	return patch(path, keyPath, strconv.FormatBool(value), "!!bool")
}

func patch(path string, keyPath []string, value, tag string) error {
	if len(keyPath) == 0 {
		return fmt.Errorf("empty key path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			root.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: top level is not a mapping", path)
	}

	cur := root
	for _, key := range keyPath[:len(keyPath)-1] {
		next := mappingValue(cur, key)
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			appendMapping(cur, key, next)
		}
		if next.Kind != yaml.MappingNode {
			next.Kind, next.Tag, next.Value, next.Content = yaml.MappingNode, "!!map", "", nil
		}
		cur = next
	}

	last := keyPath[len(keyPath)-1]
	if v := mappingValue(cur, last); v != nil {
		v.Kind, v.Tag, v.Value, v.Content, v.Style = yaml.ScalarNode, tag, value, nil, 0
	} else {
		appendMapping(cur, last, &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return writeAtomic(path, buf.Bytes(), st.Mode().Perm())
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func appendMapping(m *yaml.Node, key string, value *yaml.Node) {
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

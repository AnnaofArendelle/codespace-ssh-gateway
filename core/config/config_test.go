package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAppliesDefaultsAndOverrides(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "config.yaml", `provider: github
state_dir: `+dir+`/state

github:
  token: secret-token-value
  codespace: my-box

ssh:
  listen: 127.0.0.1:2299
  handshake_timeout: 45s

lifecycle:
  auto_create: false
  start_timeout: 90s
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Provider != "github" {
		t.Errorf("provider %q", cfg.Provider)
	}
	if cfg.SSH.Listen != "127.0.0.1:2299" {
		t.Errorf("listen %q", cfg.SSH.Listen)
	}
	if cfg.SSH.HandshakeTimeout != 45*time.Second {
		t.Errorf("handshake timeout %v", cfg.SSH.HandshakeTimeout)
	}
	if cfg.Lifecycle.AutoCreate {
		t.Error("auto_create should be false")
	}
	if cfg.Lifecycle.StartTimeout != 90*time.Second {
		t.Errorf("start timeout %v", cfg.Lifecycle.StartTimeout)
	}
	// Untouched values keep their defaults.
	if cfg.Lifecycle.ConnectTimeout != 2*time.Minute {
		t.Errorf("connect timeout %v, want the 2m default", cfg.Lifecycle.ConnectTimeout)
	}
	if cfg.HostKeyPath() != filepath.Join(dir, "state", "host_ed25519") {
		t.Errorf("host key path %q", cfg.HostKeyPath())
	}
}

func TestLoadRejectsUnknownCoreField(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "config.yaml", "provider: github\nssh:\n  listne: \":22\"\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("a misspelled ssh field was accepted")
	} else if !strings.Contains(err.Error(), "listne") {
		t.Errorf("error does not name the bad field: %v", err)
	}
}

func TestProviderSectionStaysOpaque(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "config.yaml", `provider: demo
demo:
  endpoint: https://example.test
  count: 3
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var section struct {
		Endpoint string `yaml:"endpoint"`
		Count    int    `yaml:"count"`
	}
	if err := cfg.ProviderSection("demo").Decode(&section); err != nil {
		t.Fatalf("decode section: %v", err)
	}
	if section.Endpoint != "https://example.test" || section.Count != 3 {
		t.Errorf("section decoded as %+v", section)
	}
	// An absent section decodes to nothing rather than failing.
	if err := cfg.ProviderSection("missing").Decode(&section); err != nil {
		t.Errorf("absent section: %v", err)
	}
}

func TestPatchKeepsEverythingElse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := config.WriteFile(path, config.Template("github", "gho_thetoken12345", "old-box", ":2222")); err != nil {
		t.Fatal(err)
	}
	if err := config.Patch(path, []string{"github", "codespace"}, "new-box"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "new-box") {
		t.Error("patched value missing")
	}
	if !strings.Contains(text, "gho_thetoken12345") {
		t.Error("patching the config dropped the token")
	}
	if !strings.Contains(text, "# ssh-gateway 配置文件") {
		t.Error("patching the config dropped its comments")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	section := map[string]any{}
	if err := cfg.ProviderSection("github").Decode(&section); err != nil {
		t.Fatal(err)
	}
	if section["codespace"] != "new-box" || section["token"] != "gho_thetoken12345" {
		t.Errorf("after patch: codespace=%v token set=%v", section["codespace"], section["token"] != "")
	}
}

func TestRedactedFileHidesCredentials(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "config.yaml", `provider: github
github:
  token: gho_supersecretvalue
ssh:
  password: hunter2hunter2
  listen: ":2222"
`)
	out, err := config.RedactedFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "gho_supersecretvalue") || strings.Contains(out, "hunter2hunter2") {
		t.Fatalf("redacted output still contains a secret:\n%s", out)
	}
	if !strings.Contains(out, ":2222") {
		t.Errorf("redaction removed non-secret values:\n%s", out)
	}
	// The redacted output must still be valid YAML, so it can be inspected or
	// pasted into a bug report.
	redactedPath := write(t, dir, "redacted.yaml", out)
	if _, err := config.Load(redactedPath); err != nil {
		t.Errorf("redacted config does not parse: %v\n%s", err, out)
	}
}

func TestRedactionKeepsNonSecretSettings(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "config.yaml", `provider: github
github:
  token: gho_realsecret123456
  token_file: /etc/gateway/token
  host_key_policy: tofu
ssh:
  password_auth: false
  password: hunter2hunter2
`)
	out, err := config.RedactedFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"gho_realsecret123456", "hunter2hunter2"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q survived redaction:\n%s", secret, out)
		}
	}
	// A path to a credential, a policy name and a boolean switch are not secrets.
	for _, keep := range []string{"/etc/gateway/token", "tofu", "password_auth: false"} {
		if !strings.Contains(out, keep) {
			t.Errorf("redaction hid the non-secret %q:\n%s", keep, out)
		}
	}
	if _, err := config.Load(write(t, dir, "redacted.yaml", out)); err != nil {
		t.Errorf("redacted output does not reload: %v\n%s", err, out)
	}
}

func TestPermWarningOnWorldReadableConfig(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "config.yaml", "provider: github\n")
	if w := config.PermWarning(path); w != "" {
		t.Errorf("0600 config warned: %s", w)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if w := config.PermWarning(path); w == "" {
		t.Error("a world-readable config did not warn")
	}
}

func TestPollIntervalFloor(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "config.yaml", "provider: github\nlifecycle:\n  status_poll_interval: 1ms\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Lifecycle.StatusPollInterval < 100*time.Millisecond {
		t.Errorf("poll interval %v is below the floor", cfg.Lifecycle.StatusPollInterval)
	}
}

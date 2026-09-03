package logging_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/logging"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/secret"

	"gopkg.in/yaml.v3"
)

const token = "gho_supersecrettoken12345"

func TestRedactorHidesSecretsEverywhere(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.log")

	red := logging.NewRedactor()
	red.Add(token)
	log, closer, err := logging.New(logging.Options{Level: "debug", Format: "json", File: path}, red)
	if err != nil {
		t.Fatal(err)
	}

	log.Info("token loaded "+token,
		slog.String("token", token),
		slog.Any("err", fmt.Errorf("request failed with %s", token)),
		slog.Group("nested", slog.String("inner", token)))
	log.Debug("plain", slog.Int("count", 1))
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatalf("the log file contains the secret:\n%s", raw)
	}
	if !strings.Contains(string(raw), "***") {
		t.Errorf("nothing was redacted:\n%s", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file mode %#o, want 0600", perm)
	}
}

func TestRedactorIgnoresShortStrings(t *testing.T) {
	red := logging.NewRedactor()
	red.Add("abc") // too short to search and replace safely
	if got := red.Redact("abc def"); got != "abc def" {
		t.Errorf("short secret was replaced: %q", got)
	}
}

func TestSecretValueRefusesToRender(t *testing.T) {
	s := secret.New(token)

	if got := fmt.Sprintf("%s|%v|%q", s, s, s); strings.Contains(got, token) {
		t.Errorf("fmt printed the secret: %s", got)
	}
	asJSON, err := json.Marshal(map[string]any{"token": s})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(asJSON), token) {
		t.Errorf("json printed the secret: %s", asJSON)
	}
	asYAML, err := yaml.Marshal(map[string]any{"token": s})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(asYAML), token) {
		t.Errorf("yaml printed the secret: %s", asYAML)
	}
	if s.Reveal() != token {
		t.Error("Reveal did not return the plaintext")
	}
	if !s.Equal(token) || s.Equal("other") {
		t.Error("Equal is broken")
	}
	if secret.New("").String() != "" {
		t.Error("an empty secret should render as empty")
	}
}

func TestSecretUnmarshalsFromYAMLAndIgnoresPlaceholder(t *testing.T) {
	var cfg struct {
		Token secret.Value `yaml:"token"`
	}
	if err := yaml.Unmarshal([]byte("token: "+token+"\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Token.Reveal() != token {
		t.Errorf("decoded %q", cfg.Token.Reveal())
	}
	// A config that was overwritten with redacted output must not yield "***".
	if err := yaml.Unmarshal([]byte("token: '"+secret.Redacted+"'\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Token.Empty() {
		t.Errorf("the placeholder was taken as a real token: %q", cfg.Token.Reveal())
	}
}

func TestNewRejectsBadFormat(t *testing.T) {
	if _, _, err := logging.New(logging.Options{Format: "yaml"}, nil); err == nil {
		t.Error("an unknown log format was accepted")
	}
	var buf bytes.Buffer
	_ = buf
	if _, err := logging.ParseLevel("shout"); err == nil {
		t.Error("an unknown level was accepted")
	}
}

package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/secret"
)

// Config is the `github:` section of the gateway config file. It is decoded by
// this package only; the core never looks inside it.
type Config struct {
	// Token is a personal access token with the "codespace" scope.
	Token secret.Value `yaml:"token"`
	// TokenFile reads the token from a file instead (useful with systemd
	// credentials or a secrets manager).
	TokenFile string `yaml:"token_file"`
	// Codespace is the default target. It matches either a codespace name or a
	// display name, so a stable handle survives re-creation.
	Codespace string `yaml:"codespace"`

	APIURL string `yaml:"api_url"`
	GHPath string `yaml:"gh_path"`
	// Connector selects the connection backend: auto, stdio or exec.
	Connector string `yaml:"connector"`
	// HostKeyPolicy controls verification of the codespace's SSH host key on
	// the gateway->codespace hop: tofu (default), strict or insecure.
	HostKeyPolicy string `yaml:"host_key_policy"`
	// SSHUser overrides the remote login name. Empty means ask the GitHub CLI.
	SSHUser        string        `yaml:"ssh_user"`
	RequestTimeout time.Duration `yaml:"request_timeout"`

	Create CreateConfig `yaml:"create"`
}

// CreateConfig holds the parameters used when a codespace has to be created.
// These are GitHub-specific by design and never appear in the core model.
type CreateConfig struct {
	Repository             string `yaml:"repository"` // owner/name
	Branch                 string `yaml:"branch"`
	Machine                string `yaml:"machine"`
	Location               string `yaml:"location"`
	DevcontainerPath       string `yaml:"devcontainer_path"`
	IdleTimeoutMinutes     int    `yaml:"idle_timeout_minutes"`
	RetentionPeriodMinutes int    `yaml:"retention_period_minutes"`
	DisplayName            string `yaml:"display_name"`
}

// Connector backends.
const (
	ConnectorAuto  = "auto"
	ConnectorStdio = "stdio"
	ConnectorExec  = "exec"
)

// Host key policies for the gateway -> codespace hop.
const (
	PolicyTOFU     = "tofu"
	PolicyStrict   = "strict"
	PolicyInsecure = "insecure"
)

const defaultAPIURL = "https://api.github.com"

func (c *Config) applyDefaults() error {
	if c.APIURL == "" {
		c.APIURL = defaultAPIURL
	}
	c.APIURL = strings.TrimSuffix(strings.TrimSpace(c.APIURL), "/")
	if !strings.HasPrefix(c.APIURL, "https://") && !strings.HasPrefix(c.APIURL, "http://") {
		return fmt.Errorf("github.api_url %q must be an http(s) URL", c.APIURL)
	}
	switch c.Connector {
	case "":
		c.Connector = ConnectorAuto
	case ConnectorAuto, ConnectorStdio, ConnectorExec:
	default:
		return fmt.Errorf("github.connector %q must be auto, stdio or exec", c.Connector)
	}
	switch c.HostKeyPolicy {
	case "":
		c.HostKeyPolicy = PolicyTOFU
	case PolicyTOFU, PolicyStrict, PolicyInsecure:
	default:
		return fmt.Errorf("github.host_key_policy %q must be tofu, strict or insecure", c.HostKeyPolicy)
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 30 * time.Second
	}
	if c.Create.IdleTimeoutMinutes != 0 &&
		(c.Create.IdleTimeoutMinutes < 5 || c.Create.IdleTimeoutMinutes > 240) {
		return fmt.Errorf("github.create.idle_timeout_minutes must be between 5 and 240 (GitHub's own limits)")
	}
	if repo := c.Create.Repository; repo != "" && strings.Count(repo, "/") != 1 {
		return fmt.Errorf("github.create.repository %q must be owner/name", repo)
	}
	return nil
}

// tokenSource says where a token came from, for logs and `gateway status`.
type tokenSource string

const (
	tokenFromConfig tokenSource = "config file"
	tokenFromFile   tokenSource = "token_file"
	tokenFromEnv    tokenSource = "environment"
	tokenFromGH     tokenSource = "gh auth token"
)

var errNoToken = errors.New("no GitHub token available: set github.token, github.token_file, " +
	"$GITHUB_TOKEN/$GH_TOKEN, or sign in with `gh auth login`")

// resolveToken finds a token without ever copying the GitHub CLI's auth
// implementation: if nothing is configured it asks `gh` for its own token.
func (c *Config) resolveToken(ctx context.Context, gh *ghCLI) (secret.Value, tokenSource, error) {
	if !c.Token.Empty() {
		return c.Token, tokenFromConfig, nil
	}
	if c.TokenFile != "" {
		raw, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return secret.Value{}, "", fmt.Errorf("read github.token_file: %w", err)
		}
		tok := secret.New(string(raw))
		if tok.Empty() {
			return secret.Value{}, "", fmt.Errorf("github.token_file %s is empty", c.TokenFile)
		}
		return tok, tokenFromFile, nil
	}
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return secret.New(v), tokenFromEnv, nil
		}
	}
	if gh != nil && gh.Available() {
		if tok, err := gh.AuthToken(ctx); err == nil && !tok.Empty() {
			return tok, tokenFromGH, nil
		}
	}
	return secret.Value{}, "", errNoToken
}

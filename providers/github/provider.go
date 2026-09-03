// Package github implements the Provider contract on top of GitHub Codespaces.
//
// Division of labour, on purpose:
//   - lifecycle (list/get/create/start/stop) uses the documented Codespaces
//     REST API, so it works even when the GitHub CLI is absent;
//   - connections reuse the official `gh codespace ssh`, so the gateway never
//     reimplements Dev Tunnels or the internal RPC that starts sshd inside the
//     container.
//
// Nothing here knows about the gateway's SSH server.
package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// ProviderName is the registry name used in the config file.
const ProviderName = "github"

var errGHMissing = errors.New("the GitHub CLI (gh) is required to open a codespace connection; " +
	"install it from https://cli.github.com or set github.gh_path")

func init() {
	providers.Register(providers.Registration{
		Name:                  ProviderName,
		Summary:               "GitHub Codespaces (REST API for lifecycle, `gh codespace ssh` for connections)",
		ConfigKey:             "github",
		DefaultEnvironmentKey: "codespace",
		Factory:               New,
	})
}

// Provider is the GitHub Codespaces provider.
type Provider struct {
	cfg      Config
	api      *apiClient
	gh       *ghCLI
	keys     *keyPair
	hostKeys *hostKeyStore
	users    *userCache
	sources  *sourceCache
	log      *slog.Logger

	tokenSrc tokenSource
	ghErr    error // why gh is unavailable, if it is

	mu            sync.Mutex
	connectorName string
}

// New builds the provider from its config section. It is the registered factory.
func New(ctx context.Context, deps providers.Deps) (providers.Provider, error) {
	var cfg Config
	if deps.Config != nil {
		if err := deps.Config.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("github config section: %w", err)
		}
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}

	log := deps.Logger.With(slog.String("provider", ProviderName))

	ghPath, ghErr := findGH(cfg.GHPath)
	gh := &ghCLI{path: ghPath, log: log, apiURL: cfg.APIURL}
	if ghErr != nil {
		// Not fatal: `gateway codespace list` and friends only need the API.
		log.Warn("github cli unavailable", slog.Any("error", ghErr))
	}

	token, src, err := cfg.resolveToken(ctx, gh)
	if err != nil {
		return nil, err
	}
	deps.Redact(token.Reveal())
	gh.token = token.Reveal
	log.Info("github token loaded",
		slog.String("source", string(src)), slog.Int("length", token.Len()))

	if err := os.MkdirAll(deps.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create provider state dir: %w", err)
	}
	keys, err := ensureKeyPair(deps.StateDir)
	if err != nil {
		return nil, fmt.Errorf("prepare codespace key pair: %w", err)
	}
	log.Info("gateway to codespace key ready",
		slog.String("public_key", keys.PublicPath),
		slog.String("fingerprint", fingerprintOf(keys)))

	return &Provider{
		cfg: cfg,
		api: &apiClient{
			http:    &http.Client{Timeout: cfg.RequestTimeout + 5*time.Second},
			base:    cfg.APIURL,
			token:   token.Reveal,
			timeout: cfg.RequestTimeout,
			agent:   "ssh-gateway",
		},
		gh:            gh,
		keys:          keys,
		hostKeys:      newHostKeyStore(deps.StateDir, cfg.HostKeyPolicy, log),
		users:         newUserCache(deps.StateDir, log),
		sources:       newSourceCache(deps.StateDir, log),
		log:           log,
		tokenSrc:      src,
		ghErr:         ghErr,
		connectorName: cfg.Connector,
	}, nil
}

func fingerprintOf(kp *keyPair) string {
	if kp == nil || kp.Signer == nil {
		return ""
	}
	return sshFingerprint(kp.Signer)
}

// Name implements providers.Provider.
func (p *Provider) Name() string { return ProviderName }

// DefaultEnvironment is the configured codespace handle.
func (p *Provider) DefaultEnvironment() string { return p.cfg.Codespace }

// Close releases nothing today; kept for interface completeness.
func (p *Provider) Close() error { return nil }

func (p *Provider) connector() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.connectorName
}

func (p *Provider) setConnector(name string) {
	p.mu.Lock()
	p.connectorName = name
	p.mu.Unlock()
}

// TokenSource reports where the token came from (never the token itself).
func (p *Provider) TokenSource() string { return string(p.tokenSrc) }

// Info describes the provider's effective configuration for `gateway status`.
// It never contains the token.
func (p *Provider) Info() map[string]string {
	info := map[string]string{
		"token_source":    string(p.tokenSrc),
		"api_url":         p.cfg.APIURL,
		"connector":       p.connector(),
		"host_key_policy": p.cfg.HostKeyPolicy,
		"gateway_key":     fingerprintOf(p.keys),
		"gh":              p.gh.Path(),
	}
	if info["gh"] == "" {
		info["gh"] = "not found"
	}
	if p.cfg.Create.Repository != "" {
		info["create_repository"] = p.cfg.Create.Repository
	}
	if p.cfg.SSHUser != "" {
		info["ssh_user"] = p.cfg.SSHUser
	}
	return info
}

// Capabilities reports what this provider can really do, including the honest
// answer about idle handling.
func (p *Provider) Capabilities() providers.Capabilities {
	caps := providers.Capabilities{
		Create:                 p.cfg.Create.Repository != "",
		Stop:                   true,
		Subsystems:             true,
		Signals:                true,
		Resize:                 true,
		ProviderManagedIdle:    true,
		IdleMechanism:          "GitHub Codespaces idle timeout (idle_timeout_minutes per codespace)",
		SSHActivityAttribution: providers.IdleAttributionUnverified,
	}
	caps.Notes = append(caps.Notes,
		"The gateway runs no idle timer and never reports synthetic activity: a codespace stops "+
			"when GitHub's own idle mechanism decides it should.",
		"GitHub documents the idle timeout but publishes no activity/session API, so whether an SSH "+
			"session through the tunnel postpones that timeout is unverified. A codespace may stop "+
			"under an open but idle session; the next connection starts it again.")
	if p.cfg.Create.Repository == "" {
		caps.Notes = append(caps.Notes,
			"Auto-create is unavailable until github.create.repository is set.")
	}
	if p.connector() == ConnectorExec {
		caps.Signals = false
		caps.Notes = append(caps.Notes,
			"The exec connector reports gh's exit status rather than the remote command's, and cannot "+
				"forward SSH signals.")
	}
	return caps
}

// sshUserFor resolves the remote login name for a codespace, asking the GitHub
// CLI once and caching the answer. The second return value reports whether the
// answer came from the cache, so a rejected key can invalidate it.
func (p *Provider) sshUserFor(ctx context.Context, id string, req providers.ConnectRequest) (string, bool, error) {
	if p.cfg.SSHUser != "" {
		return p.cfg.SSHUser, false, nil
	}
	if user, ok := p.users.get(id); ok {
		return user, true, nil
	}
	if !p.gh.Available() {
		return "", false, errGHMissing
	}
	req.Notify("正在向 gh 查询 codespace 的 ssh 信息…")
	target, err := p.gh.probeSSHTarget(ctx, id)
	if err != nil {
		if hint := tunnelNetworkHint(err); hint != "" {
			return "", false, classifyConnectError(fmt.Errorf("%s；原始错误：%w", hint, err))
		}
		return "", false, classifyConnectError(
			fmt.Errorf("determine ssh user for codespace %s: %w", id, err))
	}
	p.users.set(id, target.User)
	p.log.Info("resolved codespace ssh user",
		slog.String("codespace", id), slog.String("user", target.User))
	return target.User, false, nil
}

// ForgetHostKey drops pinned host keys; used by the CLI.
func (p *Provider) ForgetHostKey(name string) (int, error) { return p.hostKeys.Forget(name) }

// KeyPaths exposes the gateway->codespace key locations for status output.
func (p *Provider) KeyPaths() (private, public string) {
	return p.keys.PrivatePath, p.keys.PublicPath
}

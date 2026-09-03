// Package gateway wires the pieces together: it owns the provider, the
// lifecycle state machine, the session registry and the SSH front door.
//
// This package is the only place where those parts meet, and it is deliberately
// provider-agnostic: it talks to whatever providers.Provider the config selected.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/lifecycle"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/logging"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/session"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
	sshsrv "github.com/AnnaofArendelle/codespace-ssh-gateway/ssh"
)

// Gateway is a running gateway instance.
type Gateway struct {
	cfg    *config.Config
	prov   providers.Provider
	life   *lifecycle.Manager
	sess   *session.Manager
	srv    *sshsrv.Server
	log    *slog.Logger
	redact *logging.Redactor

	startedAt time.Time
	control   *controlServer
	stop      context.CancelFunc
}

// New builds a gateway from configuration. It creates the state directory,
// instantiates the configured provider and prepares the SSH server (including
// binding-time validation of the auth configuration).
func New(ctx context.Context, cfg *config.Config, log *slog.Logger, red *logging.Redactor) (*Gateway, error) {
	if err := config.EnsureDir(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare state dir: %w", err)
	}
	prov, err := BuildProvider(ctx, cfg, log, red)
	if err != nil {
		return nil, err
	}

	life := lifecycle.New(prov, lifecycle.Options{
		AutoCreate:           cfg.Lifecycle.AutoCreate,
		StartTimeout:         cfg.Lifecycle.StartTimeout,
		CreateTimeout:        cfg.Lifecycle.CreateTimeout,
		ConnectTimeout:       cfg.Lifecycle.ConnectTimeout,
		PollInterval:         cfg.Lifecycle.StatusPollInterval,
		ConnectRetries:       cfg.Lifecycle.ConnectRetries,
		StopOnLastDisconnect: cfg.Lifecycle.StopOnLastDisconnect,
	}, log)

	sessions := session.NewManager(cfg.SSH.MaxSessions, cfg.SSH.MaxSessionsPerEnv)
	g := &Gateway{
		cfg: cfg, prov: prov, life: life, sess: sessions,
		log: log, redact: red, startedAt: time.Now(),
	}
	sessions.SetHooks(func(env string) {
		log.Debug("first session for environment", slog.String("environment", env))
	}, nil)

	srv, err := sshsrv.New(sshsrv.Config{
		Listen:           cfg.SSH.Listen,
		HostKeyPath:      cfg.HostKeyPath(),
		HandshakeTimeout: cfg.SSH.HandshakeTimeout,
		ShutdownGrace:    cfg.SSH.ShutdownGrace,
		Banner:           cfg.SSH.Banner,
		Auth: sshsrv.AuthConfig{
			AuthorizedKeysFile:   cfg.AuthorizedKeysPath(),
			AuthorizedKeysInline: cfg.SSH.AuthorizedKeysInline,
			PasswordAuth:         cfg.SSH.PasswordAuth,
			Password:             cfg.SSH.Password,
			AllowedUsers:         cfg.SSH.AllowedUsers,
		},
	}, g, log)
	if err != nil {
		prov.Close()
		return nil, err
	}
	g.srv = srv
	if pw, ok := srv.Authorizer().GeneratedPassword(); ok {
		red.Add(pw)
	}
	return g, nil
}

// BuildProvider instantiates the configured provider. The CLI uses it for
// commands that need a provider but no SSH server.
func BuildProvider(ctx context.Context, cfg *config.Config, log *slog.Logger, red *logging.Redactor) (providers.Provider, error) {
	reg, ok := providers.Lookup(cfg.Provider)
	if !ok {
		known := make([]string, 0)
		for _, r := range providers.Registrations() {
			known = append(known, r.Name)
		}
		return nil, fmt.Errorf("unknown provider %q in %s (available: %v)", cfg.Provider, cfg.Path(), known)
	}
	stateDir := cfg.ProviderStateDir(reg.Name)
	if err := config.EnsureDir(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare provider state dir: %w", err)
	}
	section := cfg.ProviderSection(reg.ConfigKey)
	prov, err := providers.New(ctx, cfg.Provider, providers.Deps{
		Config:   section,
		Logger:   log,
		StateDir: stateDir,
		Redact:   red.Add,
	})
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", cfg.Provider, err)
	}
	return prov, nil
}

// Provider exposes the live provider (used by CLI commands over the gateway).
func (g *Gateway) Provider() providers.Provider { return g.prov }

// Lifecycle exposes the lifecycle manager.
func (g *Gateway) Lifecycle() *lifecycle.Manager { return g.life }

// Addr is the bound SSH address (available after Listen).
func (g *Gateway) Addr() string {
	if a := g.srv.Addr(); a != nil {
		return a.String()
	}
	return g.cfg.SSH.Listen
}

// GeneratedPassword returns the password generated for password auth, if the
// operator enabled password auth without configuring one.
func (g *Gateway) GeneratedPassword() (string, bool) {
	return g.srv.Authorizer().GeneratedPassword()
}

// Listen binds the SSH port without serving, so startup errors surface early.
func (g *Gateway) Listen() error { return g.srv.Listen() }

// Run serves until ctx is cancelled or `gateway stop` asks it to exit.
func (g *Gateway) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g.stop = cancel

	if g.srv.Addr() == nil {
		if err := g.Listen(); err != nil {
			return err
		}
	}
	if g.cfg.Control.Enabled {
		ctrl, err := newControlServer(g, g.cfg.ControlSocketPath())
		if err != nil {
			g.log.Warn("control socket unavailable", slog.Any("error", err))
		} else {
			g.control = ctrl
			go ctrl.serve()
			defer ctrl.close()
		}
	}
	if remove, err := writePIDFile(g.cfg.StateDir); err != nil {
		g.log.Warn("could not write pid file", slog.Any("error", err))
	} else {
		defer remove()
	}
	g.log.Info("gateway ready",
		slog.String("listen", g.Addr()),
		slog.String("provider", g.prov.Name()),
		slog.String("default_environment", g.prov.DefaultEnvironment()),
		slog.String("host_key", g.srv.HostKeyFingerprint()))

	err := g.srv.Serve(ctx)
	g.log.Info("gateway stopped")
	return err
}

// Shutdown asks a running gateway to exit.
func (g *Gateway) Shutdown() {
	if g.stop != nil {
		g.stop()
	}
}

// Close releases provider resources.
func (g *Gateway) Close() error {
	var errs []error
	if g.control != nil {
		g.control.close()
	}
	if err := g.prov.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

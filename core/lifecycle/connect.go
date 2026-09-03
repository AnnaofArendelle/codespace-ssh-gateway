package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// Connect ensures the environment is running and opens a session on it.
//
// A freshly started environment can answer API calls a moment before its SSH
// server accepts connections, so transient failures are retried with backoff.
// The context bounds establishment only: on success the returned Conn owns its
// own transport and lives until Close.
func (m *Manager) Connect(ctx context.Context, handle string, req providers.ConnectRequest) (providers.Conn, providers.Environment, error) {
	env, err := m.Ensure(ctx, handle, req.Progress)
	if err != nil {
		return nil, env, err
	}

	m.setPhase(handle, PhaseConnecting, "opening session")
	backoff := time.Second
	var lastErr error

	for attempt := 0; attempt <= m.opts.ConnectRetries; attempt++ {
		if attempt > 0 {
			req.Notify(fmt.Sprintf("connection not ready yet; retrying (%d/%d)", attempt, m.opts.ConnectRetries))
			select {
			case <-ctx.Done():
				return nil, env, fmt.Errorf("connecting to %s: %w", env.ID, ctx.Err())
			case <-time.After(backoff):
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
			// The environment may have gone away again between attempts.
			if refreshed, rerr := m.Ensure(ctx, handle, req.Progress); rerr == nil {
				env = refreshed
			}
		}

		cctx, cancel := context.WithTimeout(ctx, m.opts.ConnectTimeout)
		conn, cerr := m.prov.Connect(cctx, env.ID, req)
		cancel()
		if cerr == nil {
			m.bump(handle, func(s *envState) { s.connects++ })
			m.log.Info("session established",
				slog.String("environment", env.ID),
				slog.String("transport", conn.Describe()),
				slog.Int("attempt", attempt+1))
			return conn, env, nil
		}

		lastErr = cerr
		if !providers.IsTemporary(cerr) || errors.Is(cerr, providers.ErrAuth) {
			break
		}
		m.log.Warn("connect attempt failed",
			slog.String("environment", env.ID),
			slog.Int("attempt", attempt+1),
			slog.Any("error", cerr))
	}

	m.recordErr(handle, lastErr)
	m.setPhase(handle, PhaseFailed, "connect failed")
	return nil, env, fmt.Errorf("connect to %s: %w", env.ID, lastErr)
}

// OnSessionOpened marks the environment as carrying a live client session.
func (m *Manager) OnSessionOpened(handle string) {
	m.setPhase(handle, PhaseConnected, "client session attached")
}

// OnSessionClosed is called when a client session ends; remaining is how many
// sessions are still attached to that environment.
//
// With the default configuration this only records a phase change: the gateway
// does not stop anything, and the provider's own idle mechanism decides when
// the environment shuts down.
func (m *Manager) OnSessionClosed(handle string, remaining int) {
	if remaining > 0 {
		return
	}
	caps := m.prov.Capabilities()

	if m.opts.StopOnLastDisconnect {
		if !caps.Stop {
			m.log.Warn("stop_on_last_disconnect is set but the provider cannot stop environments",
				slog.String("provider", m.prov.Name()))
		} else {
			m.setPhase(handle, PhaseStopping, "stop_on_last_disconnect: last session closed")
			go func() {
				if err := m.Stop(context.Background(), handle); err != nil {
					m.log.Error("stop after last disconnect failed",
						slog.String("environment", handle), slog.Any("error", err))
				}
			}()
			return
		}
	}

	reason := "last session closed; no gateway-side idle handling"
	if caps.ProviderManagedIdle {
		reason = "last session closed; idle handling owned by " + caps.IdleMechanism
	}
	m.setPhase(handle, PhaseProviderManaged, reason)
}

// Stop asks the provider to stop the environment now. Used by the CLI and by
// the opt-in stop_on_last_disconnect behaviour.
func (m *Manager) Stop(ctx context.Context, handle string) error {
	id := m.ResolvedID(handle)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	m.setPhase(handle, PhaseStopping, "stop requested")
	if err := m.prov.Stop(ctx, id); err != nil {
		m.recordErr(handle, err)
		m.setPhase(handle, PhaseFailed, "stop failed")
		return fmt.Errorf("stop environment %s: %w", id, err)
	}
	m.setPhase(handle, PhaseStopped, "stopped on request")
	return nil
}

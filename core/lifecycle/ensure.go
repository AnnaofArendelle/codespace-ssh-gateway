package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// Ensure brings envID to a connectable state, creating or starting it when
// needed, and returns the resolved environment.
//
// Concurrent callers that want the same environment share a single operation:
// the first one starts the work, the others wait for it. The work keeps running
// even if the caller that triggered it goes away, as long as someone is still
// waiting; when the last waiter leaves, the operation is cancelled.
//
// notify, if non-nil, receives short progress lines for this caller. Every
// waiter gets them, including those that joined an operation in progress.
func (m *Manager) Ensure(ctx context.Context, envID string, notify func(string)) (providers.Environment, error) {
	if envID == "" {
		return providers.Environment{}, errors.New("no environment selected (run `gateway codespace select <name>`)")
	}

	var opCtx context.Context
	m.mu.Lock()
	op, joined := m.ops[envID]
	if joined {
		op.waiters++
	} else {
		budget := m.opts.StartTimeout + 30*time.Second
		if m.opts.AutoCreate {
			budget += m.opts.CreateTimeout
		}
		var cancel context.CancelFunc
		opCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), budget)
		op = &operation{
			done: make(chan struct{}), cancel: cancel, waiters: 1,
			startedAt: m.now(), notifiers: map[int64]func(string){},
		}
		m.ops[envID] = op
	}
	waiters := op.waiters
	token := op.addNotifier(notify)
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		op.removeNotifier(token)
		m.mu.Unlock()
	}()

	if joined {
		m.log.Info("joined in-flight lifecycle operation",
			slog.String("environment", envID), slog.Int("waiters", waiters))
		if notify != nil {
			notify(fmt.Sprintf("已有会话正在准备 %s，等它完成…", envID))
		}
	} else {
		go m.runEnsure(opCtx, envID, op)
	}

	select {
	case <-op.done:
		m.release(envID, op, false)
		return op.env, op.err
	case <-ctx.Done():
		m.release(envID, op, true)
		return providers.Environment{}, fmt.Errorf("waiting for environment %s: %w", envID, ctx.Err())
	}
}

func (m *Manager) release(envID string, op *operation, abandoned bool) {
	m.mu.Lock()
	op.waiters--
	orphaned := abandoned && op.waiters == 0
	if orphaned && m.ops[envID] == op {
		delete(m.ops, envID)
	}
	m.mu.Unlock()
	if orphaned {
		m.log.Info("no clients left waiting, cancelling lifecycle operation",
			slog.String("environment", envID))
		op.cancel()
	}
}

func (m *Manager) runEnsure(ctx context.Context, envID string, op *operation) {
	env, err := m.ensure(ctx, envID)

	m.mu.Lock()
	op.env, op.err = env, err
	if m.ops[envID] == op {
		delete(m.ops, envID)
	}
	m.mu.Unlock()

	op.cancel()
	close(op.done)
}

// ensure is the actual state machine: Get -> (Create) -> (Start) -> wait.
func (m *Manager) ensure(ctx context.Context, handle string) (providers.Environment, error) {
	env, err := m.get(ctx, handle)
	if err != nil {
		if !errors.Is(err, providers.ErrNotFound) {
			m.recordErr(handle, err)
			m.setPhase(handle, PhaseFailed, "lookup failed")
			return providers.Environment{}, err
		}
		if !m.opts.AutoCreate {
			m.recordErr(handle, err)
			return providers.Environment{}, fmt.Errorf(
				"auto-create is disabled (lifecycle.auto_create: false): %w", err)
		}
		if env, err = m.create(ctx, handle); err != nil {
			return providers.Environment{}, err
		}
	}
	m.resolve(handle, env)
	m.observe(env)

	if env.State.Connectable() {
		m.setPhase(handle, PhaseRunning, "environment already running")
		return env, nil
	}

	sctx, cancel := context.WithTimeout(ctx, m.opts.StartTimeout)
	defer cancel()

	// A codespace that is still shutting down cannot be started yet: wait for
	// the transition to finish instead of polling for a state it cannot reach.
	if env.State == providers.StateStopping {
		m.setPhase(handle, PhaseStopping, "environment is shutting down; waiting before starting it")
		if env, err = m.waitFor(sctx, handle, env.ID, "stop to finish",
			func(s providers.State) bool { return s != providers.StateStopping }); err != nil {
			m.recordErr(handle, err)
			m.setPhase(handle, PhaseFailed, "waiting for shutdown failed")
			return providers.Environment{}, err
		}
		if env.State.Connectable() {
			m.setPhase(handle, PhaseRunning, "environment came back up on its own")
			return env, nil
		}
	}

	if env.State.Startable() {
		m.setPhase(handle, PhaseStarting, "start requested for incoming ssh session")
		if err := m.prov.Start(sctx, env.ID); err != nil {
			m.recordErr(handle, err)
			m.setPhase(handle, PhaseFailed, "start failed")
			return providers.Environment{}, fmt.Errorf("start environment %s: %w", env.ID, err)
		}
		m.bump(handle, func(s *envState) { s.starts++ })
	} else {
		m.setPhase(handle, PhaseForState(env.State),
			fmt.Sprintf("environment already transitioning (%s)", env.State))
	}

	env, err = m.waitFor(sctx, handle, env.ID, "environment to become RUNNING",
		func(s providers.State) bool { return s.Connectable() })
	if err != nil {
		m.recordErr(handle, err)
		m.setPhase(handle, PhaseFailed, "did not become ready")
		return providers.Environment{}, err
	}
	m.setPhase(handle, PhaseRunning, "environment ready")
	return env, nil
}

// get reads the environment, tolerating a transient provider hiccup. The
// provider's own client already retries; this covers the case where it gave up.
func (m *Manager) get(ctx context.Context, id string) (providers.Environment, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return providers.Environment{}, ctx.Err()
			case <-time.After(m.opts.PollInterval):
			}
		}
		env, err := m.prov.Get(ctx, id)
		if err == nil {
			return env, nil
		}
		if !providers.IsTemporary(err) {
			return providers.Environment{}, err
		}
		lastErr = err
		m.log.Warn("transient provider error, retrying",
			slog.String("environment", id), slog.Int("attempt", attempt+1), slog.Any("error", err))
	}
	return providers.Environment{}, lastErr
}

func (m *Manager) create(ctx context.Context, handle string) (providers.Environment, error) {
	m.setPhase(handle, PhaseProvisioning, "environment does not exist, creating it")
	cctx, cancel := context.WithTimeout(ctx, m.opts.CreateTimeout)
	defer cancel()

	env, err := m.prov.Create(cctx, providers.CreateSpec{Name: handle})
	if err != nil {
		m.recordErr(handle, err)
		m.setPhase(handle, PhaseFailed, "create failed")
		return providers.Environment{}, fmt.Errorf("create environment %q: %w", handle, err)
	}
	m.bump(handle, func(s *envState) { s.creates++ })
	m.log.Info("environment created",
		slog.String("handle", handle),
		slog.String("environment", env.ID),
		slog.String("state", string(env.State)))
	return env, nil
}

// waitFor polls until done(state) holds, tolerating transient provider errors
// but not authentication failures or a terminal provider state.
func (m *Manager) waitFor(ctx context.Context, handle, id, what string, done func(providers.State) bool) (providers.Environment, error) {
	ticker := time.NewTicker(m.opts.PollInterval)
	defer ticker.Stop()

	var (
		last       = providers.StateUnknown
		fails      int
		startedAt  = m.now()
		lastNotify = m.now()
	)
	for {
		select {
		case <-ctx.Done():
			return providers.Environment{}, fmt.Errorf(
				"timed out waiting for %s on %s (last state %s): %w", what, id, last, ctx.Err())
		case <-ticker.C:
		}

		env, err := m.prov.Get(ctx, id)
		if err != nil {
			if errors.Is(err, providers.ErrAuth) {
				return providers.Environment{}, err
			}
			if providers.IsTemporary(err) && fails < 5 {
				fails++
				m.log.Warn("transient error while waiting for environment",
					slog.String("environment", id), slog.Int("attempt", fails), slog.Any("error", err))
				continue
			}
			return providers.Environment{}, err
		}
		fails = 0
		m.observe(env)
		last = env.State

		if done(env.State) {
			return env, nil
		}
		if since := m.now().Sub(lastNotify); since >= 5*time.Second {
			lastNotify = m.now()
			m.notify(handle, fmt.Sprintf("仍在等待（provider 状态 %s，已等待 %s）",
				env.NativeState, m.now().Sub(startedAt).Round(time.Second)))
		}
		switch env.State {
		case providers.StateFailed:
			return providers.Environment{}, fmt.Errorf(
				"environment %s failed to start (provider state %q)", id, env.NativeState)
		case providers.StateNotFound:
			return providers.Environment{}, &providers.NotFoundError{Provider: m.prov.Name(), ID: id}
		}
		m.log.Debug("waiting for environment",
			slog.String("environment", id),
			slog.String("state", string(env.State)),
			slog.String("waiting_for", what))
	}
}

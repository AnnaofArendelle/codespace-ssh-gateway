package lifecycle

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// Options bounds the orchestration work. None of these are idle timers: they
// only limit how long the gateway waits for the provider to do something it was
// explicitly asked to do.
type Options struct {
	AutoCreate           bool
	StartTimeout         time.Duration
	CreateTimeout        time.Duration
	ConnectTimeout       time.Duration
	PollInterval         time.Duration
	ConnectRetries       int
	StopOnLastDisconnect bool
}

func (o Options) withDefaults() Options {
	if o.StartTimeout <= 0 {
		o.StartTimeout = 5 * time.Minute
	}
	if o.CreateTimeout <= 0 {
		o.CreateTimeout = 20 * time.Minute
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = 2 * time.Minute
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 2 * time.Second
	}
	if o.ConnectRetries < 0 {
		o.ConnectRetries = 0
	}
	return o
}

type envState struct {
	phase Phase
	since time.Time
	// resolved is the provider-side id the handle currently maps to. They
	// differ when a provider names environments itself (GitHub picks the
	// codespace name; the gateway keeps addressing it by the configured handle).
	resolved string
	native   string
	lastErr  string
	starts   int
	creates  int
	connects int
	history  []Transition
}

// operation is one in-flight Ensure shared by every caller that wants the same
// environment (single flight).
type operation struct {
	done      chan struct{}
	env       providers.Environment
	err       error
	waiters   int
	cancel    context.CancelFunc
	startedAt time.Time

	// notifiers receive progress lines, one entry per waiting caller.
	notifiers map[int64]func(string)
	nextToken int64
}

// addNotifier registers a progress callback; callers hold Manager.mu.
func (op *operation) addNotifier(fn func(string)) int64 {
	if fn == nil {
		return 0
	}
	op.nextToken++
	if op.notifiers == nil {
		op.notifiers = map[int64]func(string){}
	}
	op.notifiers[op.nextToken] = fn
	return op.nextToken
}

// removeNotifier drops a callback; callers hold Manager.mu.
func (op *operation) removeNotifier(token int64) {
	if token != 0 {
		delete(op.notifiers, token)
	}
}

// Manager drives environments towards a connectable state.
type Manager struct {
	prov providers.Provider
	opts Options
	log  *slog.Logger
	now  func() time.Time

	mu     sync.Mutex
	states map[string]*envState
	ops    map[string]*operation
}

// New builds a Manager.
func New(prov providers.Provider, opts Options, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		prov:   prov,
		opts:   opts.withDefaults(),
		log:    log,
		now:    time.Now,
		states: map[string]*envState{},
		ops:    map[string]*operation{},
	}
}

// Provider exposes the underlying provider (for status reporting).
func (m *Manager) Provider() providers.Provider { return m.prov }

// Options returns the effective options.
func (m *Manager) Options() Options { return m.opts }

func (m *Manager) stateLocked(env string) *envState {
	st, ok := m.states[env]
	if !ok {
		st = &envState{phase: PhaseUnknown, since: m.now()}
		m.states[env] = st
	}
	return st
}

// notify sends a progress line to every caller waiting on this environment, so
// a client that has been waiting 40 seconds knows what is happening.
func (m *Manager) notify(env, msg string) {
	m.mu.Lock()
	op, ok := m.ops[env]
	if !ok || len(op.notifiers) == 0 {
		m.mu.Unlock()
		return
	}
	fns := make([]func(string), 0, len(op.notifiers))
	for _, fn := range op.notifiers {
		fns = append(fns, fn)
	}
	m.mu.Unlock()
	for _, fn := range fns {
		fn(msg)
	}
}

// setPhase records a transition. Repeat transitions to the same phase are
// collapsed so the history stays readable.
func (m *Manager) setPhase(env string, to Phase, reason string) {
	m.mu.Lock()
	st := m.stateLocked(env)
	from := st.phase
	if from == to {
		m.mu.Unlock()
		return
	}
	now := m.now()
	st.phase, st.since = to, now
	if to != PhaseFailed {
		st.lastErr = ""
	}
	st.history = append(st.history, Transition{From: from, To: to, At: now, Reason: reason})
	if len(st.history) > maxHistory {
		st.history = st.history[len(st.history)-maxHistory:]
	}
	m.mu.Unlock()

	m.log.Info("lifecycle phase",
		slog.String("environment", env),
		slog.String("from", string(from)),
		slog.String("to", string(to)),
		slog.String("reason", reason))
	if notice := to.Notice(); notice != "" {
		m.notify(env, notice)
	}
}

// Phase returns the current phase of an environment.
func (m *Manager) Phase(env string) Phase {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked(env).phase
}

func (m *Manager) observe(env providers.Environment) {
	if env.ID == "" {
		return
	}
	m.mu.Lock()
	st := m.stateLocked(env.ID)
	if env.NativeState != "" {
		st.native = env.NativeState
	}
	m.mu.Unlock()
}

// resolve records which provider-side id a handle maps to.
func (m *Manager) resolve(handle string, env providers.Environment) {
	if env.ID == "" {
		return
	}
	m.mu.Lock()
	st := m.stateLocked(handle)
	changed := st.resolved != env.ID
	st.resolved = env.ID
	m.mu.Unlock()
	if changed && handle != env.ID {
		m.log.Info("environment handle resolved",
			slog.String("handle", handle), slog.String("environment", env.ID))
	}
}

// ResolvedID returns the provider-side id for a handle, falling back to the
// handle itself.
func (m *Manager) ResolvedID(handle string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.states[handle]; ok && st.resolved != "" {
		return st.resolved
	}
	return handle
}

func (m *Manager) recordErr(env string, err error) {
	m.mu.Lock()
	st := m.stateLocked(env)
	st.lastErr = err.Error()
	m.mu.Unlock()
}

func (m *Manager) bump(env string, f func(*envState)) {
	m.mu.Lock()
	f(m.stateLocked(env))
	m.mu.Unlock()
}

// Status returns a snapshot for one environment.
func (m *Manager) Status(env string) EnvStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.stateLocked(env)
	out := EnvStatus{
		Environment: env,
		Resolved:    st.resolved,
		Phase:       st.phase,
		Since:       st.since,
		NativeState: st.native,
		LastError:   st.lastErr,
		Starts:      st.starts,
		Creates:     st.creates,
		Connects:    st.connects,
		History:     append([]Transition(nil), st.history...),
	}
	if op, ok := m.ops[env]; ok {
		out.InFlight, out.Waiters = true, op.waiters
	}
	return out
}

// All returns snapshots for every environment the gateway has touched.
func (m *Manager) All() []EnvStatus {
	m.mu.Lock()
	names := make([]string, 0, len(m.states))
	for name := range m.states {
		names = append(names, name)
	}
	m.mu.Unlock()

	sort.Strings(names)
	out := make([]EnvStatus, 0, len(names))
	for _, name := range names {
		out = append(out, m.Status(name))
	}
	return out
}

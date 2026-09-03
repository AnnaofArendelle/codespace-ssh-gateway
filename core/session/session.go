// Package session tracks live client sessions.
//
// It exists so the gateway can enforce limits, report status, and know when the
// last client for an environment went away. It deliberately does not implement
// activity detection, keepalives or idle timers: deciding when an idle
// environment stops is the provider's job.
package session

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Errors returned by Open.
var (
	ErrTooManySessions       = errors.New("too many sessions")
	ErrTooManySessionsForEnv = errors.New("too many sessions for environment")
)

// Info describes one client session.
type Info struct {
	ID            string    `json:"id"`
	User          string    `json:"user"`
	Environment   string    `json:"environment"`
	RemoteAddr    string    `json:"remote_addr"`
	ClientVersion string    `json:"client_version,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	PTY           bool      `json:"pty"`
	Kind          string    `json:"kind"` // shell | exec | subsystem
	Phase         string    `json:"phase"`
}

// Session is a live session handle.
type Session struct {
	info   Info
	mgr    *Manager
	closed atomic.Bool
	mu     sync.Mutex
}

// ID returns the session id.
func (s *Session) ID() string { return s.info.ID }

// Environment returns the target environment id.
func (s *Session) Environment() string { return s.info.Environment }

// Info returns a snapshot.
func (s *Session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// SetPhase records what the session is currently doing.
func (s *Session) SetPhase(phase string) {
	s.mu.Lock()
	s.info.Phase = phase
	s.mu.Unlock()
}

// SetKind records shell/exec/subsystem once known.
func (s *Session) SetKind(kind string, pty bool) {
	s.mu.Lock()
	s.info.Kind, s.info.PTY = kind, pty
	s.mu.Unlock()
}

// Manager owns the set of live sessions.
type Manager struct {
	mu        sync.Mutex
	byID      map[string]*Session
	perEnv    map[string]int
	seq       uint64
	maxTotal  int
	maxPerEnv int
	onLast    func(env string)
	onFirst   func(env string)
}

// NewManager returns a manager. Zero limits mean unlimited.
func NewManager(maxTotal, maxPerEnv int) *Manager {
	return &Manager{
		byID:      map[string]*Session{},
		perEnv:    map[string]int{},
		maxTotal:  maxTotal,
		maxPerEnv: maxPerEnv,
	}
}

// SetHooks registers callbacks fired when an environment gains its first
// session and loses its last one. Both run on the caller's goroutine.
func (m *Manager) SetHooks(onFirst, onLast func(env string)) {
	m.mu.Lock()
	m.onFirst, m.onLast = onFirst, onLast
	m.mu.Unlock()
}

// Open registers a session, enforcing the configured limits.
func (m *Manager) Open(info Info) (*Session, error) {
	m.mu.Lock()
	if m.maxTotal > 0 && len(m.byID) >= m.maxTotal {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w (limit %d)", ErrTooManySessions, m.maxTotal)
	}
	if m.maxPerEnv > 0 && m.perEnv[info.Environment] >= m.maxPerEnv {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w %s (limit %d)", ErrTooManySessionsForEnv, info.Environment, m.maxPerEnv)
	}
	m.seq++
	if info.StartedAt.IsZero() {
		info.StartedAt = time.Now()
	}
	info.ID = fmt.Sprintf("s%d", m.seq)
	if info.Phase == "" {
		info.Phase = "opening"
	}
	s := &Session{info: info, mgr: m}
	m.byID[info.ID] = s
	m.perEnv[info.Environment]++
	first := m.perEnv[info.Environment] == 1
	onFirst := m.onFirst
	m.mu.Unlock()

	if first && onFirst != nil {
		onFirst(info.Environment)
	}
	return s, nil
}

// Close deregisters a session. It is safe to call more than once.
func (m *Manager) Close(s *Session) {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return
	}
	env := s.Environment()
	m.mu.Lock()
	delete(m.byID, s.ID())
	if n := m.perEnv[env]; n <= 1 {
		delete(m.perEnv, env)
	} else {
		m.perEnv[env] = n - 1
	}
	last := m.perEnv[env] == 0
	onLast := m.onLast
	m.mu.Unlock()

	if last && onLast != nil {
		onLast(env)
	}
}

// CountFor returns the number of live sessions for an environment.
func (m *Manager) CountFor(env string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.perEnv[env]
}

// Total returns the number of live sessions.
func (m *Manager) Total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byID)
}

// List returns a snapshot of live sessions, oldest first.
func (m *Manager) List() []Info {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.byID))
	for _, s := range m.byID {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	out := make([]Info, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.Info())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

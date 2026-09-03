package lifecycle_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/lifecycle"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/logging"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// stubProvider is a controllable provider used to test the state machine
// itself. The real provider is exercised end to end in core/gateway.
type stubProvider struct {
	mu    sync.Mutex
	state providers.State
	// counters
	gets, starts, creates, stops, connects int32
	// behaviour
	getErr        error
	startErr      error
	createErr     error
	connectErrs   []error // consumed one per Connect call
	startDelay    time.Duration
	pollsToRun    int
	stoppingPolls int  // polls before a stopping environment reports stopped
	getFails      int  // return temporary errors this many times
	getAuthFail   bool // return an auth error from Get
	missing       bool
	createdID     string
	capabilities  providers.Capabilities
	connectCtxErr chan struct{}
}

func newStub(state providers.State) *stubProvider {
	return &stubProvider{
		state:        state,
		createdID:    "created-1",
		capabilities: providers.Capabilities{Stop: true, ProviderManagedIdle: true, IdleMechanism: "stub idle"},
	}
}

func (s *stubProvider) Name() string                                          { return "stub" }
func (s *stubProvider) Capabilities() providers.Capabilities                  { return s.capabilities }
func (s *stubProvider) DefaultEnvironment() string                            { return "env" }
func (s *stubProvider) Close() error                                          { return nil }
func (s *stubProvider) List(context.Context) ([]providers.Environment, error) { return nil, nil }

func (s *stubProvider) Get(ctx context.Context, id string) (providers.Environment, error) {
	atomic.AddInt32(&s.gets, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return providers.Environment{}, s.getErr
	}
	if s.getAuthFail {
		return providers.Environment{}, providers.ErrAuth
	}
	if s.getFails > 0 {
		s.getFails--
		return providers.Environment{}, providers.Temporary(errors.New("stub api hiccup"))
	}
	if s.missing {
		return providers.Environment{}, &providers.NotFoundError{Provider: "stub", ID: id}
	}
	if s.state == providers.StateStopping {
		if s.stoppingPolls > 0 {
			s.stoppingPolls--
		} else {
			s.state = providers.StateStopped
		}
	}
	if s.state == providers.StateStarting && s.pollsToRun > 0 {
		s.pollsToRun--
		if s.pollsToRun == 0 {
			s.state = providers.StateRunning
		}
	}
	return providers.Environment{ID: id, Provider: "stub", State: s.state, NativeState: string(s.state)}, nil
}

func (s *stubProvider) Create(ctx context.Context, spec providers.CreateSpec) (providers.Environment, error) {
	atomic.AddInt32(&s.creates, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return providers.Environment{}, s.createErr
	}
	s.missing = false
	s.state = providers.StateStarting
	s.pollsToRun = 1
	return providers.Environment{ID: s.createdID, Provider: "stub", State: s.state}, nil
}

func (s *stubProvider) Start(ctx context.Context, id string) error {
	atomic.AddInt32(&s.starts, 1)
	if s.startDelay > 0 {
		select {
		case <-time.After(s.startDelay):
		case <-ctx.Done():
			if s.connectCtxErr != nil {
				close(s.connectCtxErr)
				s.connectCtxErr = nil
			}
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startErr != nil {
		return s.startErr
	}
	s.state = providers.StateStarting
	if s.pollsToRun == 0 {
		s.pollsToRun = 1
	}
	return nil
}

func (s *stubProvider) Stop(ctx context.Context, id string) error {
	atomic.AddInt32(&s.stops, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = providers.StateStopped
	return nil
}

func (s *stubProvider) Status(ctx context.Context, id string) (providers.State, error) {
	env, err := s.Get(ctx, id)
	return env.State, err
}

func (s *stubProvider) Connect(ctx context.Context, id string, req providers.ConnectRequest) (providers.Conn, error) {
	n := atomic.AddInt32(&s.connects, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if int(n) <= len(s.connectErrs) {
		if err := s.connectErrs[n-1]; err != nil {
			return nil, err
		}
	}
	return stubConn{}, nil
}

type stubConn struct{}

func (stubConn) Stdin() io.WriteCloser               { return nopWriteCloser{} }
func (stubConn) Stdout() io.Reader                   { return strings.NewReader("") }
func (stubConn) Stderr() io.Reader                   { return nil }
func (stubConn) Resize(uint16, uint16) error         { return nil }
func (stubConn) Signal(string) error                 { return nil }
func (stubConn) Wait() (providers.ExitStatus, error) { return providers.ExitStatus{}, nil }
func (stubConn) Close() error                        { return nil }
func (stubConn) Describe() string                    { return "stub" }

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func newManager(t *testing.T, p providers.Provider, opts lifecycle.Options) *lifecycle.Manager {
	t.Helper()
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Millisecond
	}
	if opts.StartTimeout == 0 {
		opts.StartTimeout = 2 * time.Second
	}
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = time.Second
	}
	return lifecycle.New(p, opts, logging.Discard())
}

func TestEnsureStartsStoppedEnvironment(t *testing.T) {
	stub := newStub(providers.StateStopped)
	stub.pollsToRun = 2
	m := newManager(t, stub, lifecycle.Options{})

	env, err := m.Ensure(context.Background(), "env", nil)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if env.State != providers.StateRunning {
		t.Errorf("state %s, want RUNNING", env.State)
	}
	if got := atomic.LoadInt32(&stub.starts); got != 1 {
		t.Errorf("start called %d times, want 1", got)
	}
	if m.Phase("env") != lifecycle.PhaseRunning {
		t.Errorf("phase %s, want RUNNING", m.Phase("env"))
	}
}

func TestEnsureRunningEnvironmentDoesNotStart(t *testing.T) {
	stub := newStub(providers.StateRunning)
	m := newManager(t, stub, lifecycle.Options{})
	if _, err := m.Ensure(context.Background(), "env", nil); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&stub.starts); got != 0 {
		t.Errorf("start called %d times for a running environment", got)
	}
}

func TestEnsureCreatesWhenMissing(t *testing.T) {
	stub := newStub(providers.StateStopped)
	stub.missing = true
	m := newManager(t, stub, lifecycle.Options{AutoCreate: true})

	env, err := m.Ensure(context.Background(), "env", nil)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if env.ID != "created-1" {
		t.Errorf("environment id %q, want the created one", env.ID)
	}
	if got := atomic.LoadInt32(&stub.creates); got != 1 {
		t.Errorf("create called %d times, want 1", got)
	}
	if id := m.ResolvedID("env"); id != "created-1" {
		t.Errorf("handle resolved to %q", id)
	}
}

func TestEnsureRefusesToCreateWhenDisabled(t *testing.T) {
	stub := newStub(providers.StateStopped)
	stub.missing = true
	m := newManager(t, stub, lifecycle.Options{AutoCreate: false})

	_, err := m.Ensure(context.Background(), "env", nil)
	if err == nil {
		t.Fatal("ensure succeeded with auto-create disabled")
	}
	if !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("error does not wrap ErrNotFound: %v", err)
	}
	if got := atomic.LoadInt32(&stub.creates); got != 0 {
		t.Errorf("create was called %d times", got)
	}
}

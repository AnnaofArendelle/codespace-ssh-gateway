package lifecycle_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/lifecycle"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// Concurrent callers for the same environment must share one start.
func TestEnsureSharesOneOperation(t *testing.T) {
	stub := newStub(providers.StateStopped)
	stub.startDelay = 150 * time.Millisecond
	stub.pollsToRun = 2
	m := newManager(t, stub, lifecycle.Options{})

	const clients = 5
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Ensure(context.Background(), "env", nil); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("ensure: %v", err)
	}
	if got := atomic.LoadInt32(&stub.starts); got != 1 {
		t.Errorf("start called %d times for %d concurrent callers, want 1", got, clients)
	}
}

// When every waiter goes away, the shared operation is cancelled instead of
// running on forever.
func TestAbandonedOperationIsCancelled(t *testing.T) {
	stub := newStub(providers.StateStopped)
	stub.startDelay = 5 * time.Second
	cancelled := make(chan struct{})
	stub.connectCtxErr = cancelled
	m := newManager(t, stub, lifecycle.Options{StartTimeout: 10 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Ensure(ctx, "env", nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ensure returned %v, want a cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ensure did not return after its only caller left")
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the provider operation was not cancelled")
	}
}

// A codespace often accepts API calls before its SSH server does, so a
// temporary connect failure must be retried.
func TestConnectRetriesTemporaryErrors(t *testing.T) {
	stub := newStub(providers.StateRunning)
	stub.connectErrs = []error{providers.Temporary(errors.New("connection refused")), nil}
	m := newManager(t, stub, lifecycle.Options{ConnectRetries: 3})

	conn, _, err := m.Connect(context.Background(), "env", providers.ConnectRequest{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	if got := atomic.LoadInt32(&stub.connects); got != 2 {
		t.Errorf("connect attempted %d times, want 2", got)
	}
}

// A permanent failure must not be retried.
func TestConnectDoesNotRetryPermanentErrors(t *testing.T) {
	stub := newStub(providers.StateRunning)
	stub.connectErrs = []error{errors.New("no such subsystem")}
	m := newManager(t, stub, lifecycle.Options{ConnectRetries: 3})

	if _, _, err := m.Connect(context.Background(), "env", providers.ConnectRequest{}); err == nil {
		t.Fatal("connect succeeded unexpectedly")
	}
	if got := atomic.LoadInt32(&stub.connects); got != 1 {
		t.Errorf("connect attempted %d times, want 1", got)
	}
}

// By default the last disconnect only records a phase: the provider owns idle.
func TestLastDisconnectLeavesTheEnvironmentAlone(t *testing.T) {
	stub := newStub(providers.StateRunning)
	m := newManager(t, stub, lifecycle.Options{})
	if _, err := m.Ensure(context.Background(), "env", nil); err != nil {
		t.Fatal(err)
	}
	m.OnSessionOpened("env")
	m.OnSessionClosed("env", 0)

	if phase := m.Phase("env"); phase != lifecycle.PhaseProviderManaged {
		t.Errorf("phase %s, want %s", phase, lifecycle.PhaseProviderManaged)
	}
	if got := atomic.LoadInt32(&stub.stops); got != 0 {
		t.Errorf("stop called %d times; the provider should decide", got)
	}
}

// A session that is not the last one must not change the phase.
func TestRemainingSessionsKeepTheEnvironmentConnected(t *testing.T) {
	stub := newStub(providers.StateRunning)
	m := newManager(t, stub, lifecycle.Options{})
	if _, err := m.Ensure(context.Background(), "env", nil); err != nil {
		t.Fatal(err)
	}
	m.OnSessionOpened("env")
	m.OnSessionClosed("env", 1)
	if phase := m.Phase("env"); phase != lifecycle.PhaseConnected {
		t.Errorf("phase %s, want CONNECTED while a session remains", phase)
	}
}

// The opt-in setting stops the environment explicitly.
func TestStopOnLastDisconnectCallsStop(t *testing.T) {
	stub := newStub(providers.StateRunning)
	m := newManager(t, stub, lifecycle.Options{StopOnLastDisconnect: true})
	if _, err := m.Ensure(context.Background(), "env", nil); err != nil {
		t.Fatal(err)
	}
	m.OnSessionOpened("env")
	m.OnSessionClosed("env", 0)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&stub.stops) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stop was called %d times, want 1", atomic.LoadInt32(&stub.stops))
}

// A codespace that is still shutting down must be waited out, then started,
// instead of being polled for a state it cannot reach.
func TestEnsureWaitsForShutdownThenStarts(t *testing.T) {
	stub := newStub(providers.StateStopping)
	stub.stoppingPolls = 2
	m := newManager(t, stub, lifecycle.Options{StartTimeout: 3 * time.Second})

	env, err := m.Ensure(context.Background(), "env", nil)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if env.State != providers.StateRunning {
		t.Errorf("state %s, want RUNNING", env.State)
	}
	if got := atomic.LoadInt32(&stub.starts); got != 1 {
		t.Errorf("start called %d times, want 1 after the shutdown finished", got)
	}
	var sawStopping bool
	for _, tr := range m.Status("env").History {
		if tr.To == lifecycle.PhaseStopping {
			sawStopping = true
		}
	}
	if !sawStopping {
		t.Error("the STOPPING phase was not reported while waiting")
	}
}

// Transient provider errors while waiting must not abort the start.
func TestWaitReadyToleratesTemporaryErrors(t *testing.T) {
	stub := newStub(providers.StateStopped)
	stub.pollsToRun = 3
	stub.getFails = 2
	m := newManager(t, stub, lifecycle.Options{})

	if _, err := m.Ensure(context.Background(), "env", nil); err != nil {
		t.Fatalf("ensure: %v", err)
	}
}

// An authentication failure while waiting must abort immediately.
func TestWaitReadyAbortsOnAuthError(t *testing.T) {
	stub := newStub(providers.StateStopped)
	stub.pollsToRun = 5
	stub.getAuthFail = true
	m := newManager(t, stub, lifecycle.Options{})

	_, err := m.Ensure(context.Background(), "env", nil)
	if !errors.Is(err, providers.ErrAuth) {
		t.Fatalf("ensure returned %v, want an auth error", err)
	}
}

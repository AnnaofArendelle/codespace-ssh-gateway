package gateway_test

import (
	"strings"
	"testing"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/lifecycle"

	gossh "golang.org/x/crypto/ssh"
)

// An invalid token must be reported on the client's terminal, and must not take
// the gateway down.
func TestInvalidTokenIsReported(t *testing.T) {
	h := newHarness(t, options{state: "Available", token: "gho_wrongtoken0000000"})
	h.start()

	client := h.dial("root")
	defer client.Close()

	_, stderr, code, err := h.exec(client, "echo nope")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code == 0 {
		t.Fatal("exec succeeded with an invalid token")
	}
	if !strings.Contains(stderr, "认证") && !strings.Contains(stderr, "401") {
		t.Errorf("stderr does not explain the auth failure: %q", stderr)
	}
	if strings.Contains(stderr, "gho_wrongtoken") {
		t.Errorf("stderr leaked the token: %q", stderr)
	}

	// The gateway is still serving.
	if _, _, _, err := h.exec(client, "echo again"); err != nil {
		t.Fatalf("second exec on a live connection failed: %v", err)
	}
}

// A start call that GitHub refuses must surface as a failed session.
func TestStartAPIFailureIsReported(t *testing.T) {
	h := newHarness(t, options{state: "Shutdown"})
	h.gh.StartError = 500
	h.start()

	client := h.dial("root")
	defer client.Close()
	_, stderr, code, err := h.exec(client, "echo nope")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code == 0 {
		t.Fatal("session succeeded although start failed")
	}
	if !strings.Contains(stderr, "start") && !strings.Contains(stderr, "500") {
		t.Errorf("stderr does not mention the failed start: %q", stderr)
	}
	if phase := h.envStatus(h.handle).Phase; phase != lifecycle.PhaseFailed {
		t.Errorf("phase is %s, want FAILED", phase)
	}
}

// A codespace that goes to Failed instead of Available must be reported, not
// waited on forever.
func TestCodespaceFailsToStart(t *testing.T) {
	h := newHarness(t, options{state: "Shutdown"})
	h.gh.FailStartTransition = true
	h.start()

	client := h.dial("root")
	defer client.Close()
	_, stderr, code, err := h.exec(client, "echo nope")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code == 0 {
		t.Fatal("session succeeded although the codespace failed to start")
	}
	if !strings.Contains(stderr, "failed to start") {
		t.Errorf("stderr does not mention the failure: %q", stderr)
	}
}

// A codespace whose SSH server is not accepting connections yet must be retried.
func TestRetriesUntilSSHServerAccepts(t *testing.T) {
	h := newHarness(t, options{state: "Available"})
	h.sshd.RefuseNext(2)
	h.start()

	client := h.dial("root")
	defer client.Close()
	stdout, stderr, code, err := h.exec(client, "echo ready")
	if err != nil {
		t.Fatalf("exec: %v (stderr %q)", err, stderr)
	}
	if stdout != "ready\n" || code != 0 {
		t.Fatalf("got %q/%d after retries, want \"ready\\n\"/0 (stderr %q)", stdout, code, stderr)
	}
	if !strings.Contains(stderr, "重试") {
		t.Errorf("the client was not told about the retries: %q", stderr)
	}
}

// A provider API that does not answer in time must fail cleanly.
func TestProviderAPITimeout(t *testing.T) {
	h := newHarness(t, options{state: "Available", requestTimeout: "100ms"})
	h.gh.Latency = 400 * time.Millisecond
	h.start()

	client := h.dial("root")
	defer client.Close()
	_, stderr, code, err := h.exec(client, "echo nope")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code == 0 {
		t.Fatal("session succeeded although the API timed out")
	}
	lower := strings.ToLower(stderr)
	if !strings.Contains(stderr, "超时") && !strings.Contains(lower, "deadline") &&
		!strings.Contains(lower, "timeout") {
		t.Errorf("stderr does not mention a timeout: %q", stderr)
	}
}

// Without auto-create, a missing codespace is a clear error rather than a hang.
func TestMissingCodespaceWithoutAutoCreate(t *testing.T) {
	h := newHarness(t, options{state: "", autoCreate: false})
	h.start()

	client := h.dial("root")
	defer client.Close()
	_, stderr, code, err := h.exec(client, "echo nope")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code == 0 {
		t.Fatal("session succeeded although the codespace does not exist")
	}
	if !strings.Contains(stderr, "不存在") {
		t.Errorf("stderr does not explain the missing codespace: %q", stderr)
	}
}

// A client key that is not authorized must not get in.
func TestUnauthorizedClientKeyRejected(t *testing.T) {
	h := newHarness(t, options{state: "Available"})
	h.start()

	other, _ := newKeyPair(t)
	cfg := &gossh.ClientConfig{
		User:            "root",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(other)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	if _, err := gossh.Dial("tcp", h.addr, cfg); err == nil {
		t.Fatal("an unauthorized key was accepted")
	}
	// The authorized key still works.
	client := h.dial("root")
	client.Close()
}

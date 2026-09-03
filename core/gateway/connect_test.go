package gateway_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/lifecycle"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/internal/testenv"
)

// A codespace that is already running should be reached without any lifecycle
// calls, and the gateway must authenticate with its own generated key.
func TestExecOnRunningCodespace(t *testing.T) {
	h := newHarness(t, options{state: "Available"})
	h.start()

	client := h.dial("root")
	defer client.Close()

	stdout, _, code, err := h.exec(client, "echo hello")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if stdout != "hello\n" || code != 0 {
		t.Fatalf("got stdout %q code %d, want \"hello\\n\" and 0", stdout, code)
	}
	if n := h.gh.Calls("/start"); n != 0 {
		t.Errorf("start was called %d times for an already running codespace", n)
	}

	offered := h.sshd.OfferedKeys()
	if len(offered) == 0 {
		t.Fatal("codespace saw no public key")
	}
	if want := h.gatewayKeyFingerprint(); offered[0] != want {
		t.Errorf("codespace saw key %s, want the gateway key %s", offered[0], want)
	}
	if sess := h.sshd.LastSession(); sess == nil || sess.Command != "echo hello" {
		t.Errorf("codespace recorded %+v, want exec of \"echo hello\"", sess)
	}
	if phase := h.envStatus(h.handle).Phase; phase != lifecycle.PhaseConnected {
		t.Errorf("phase is %s, want %s", phase, lifecycle.PhaseConnected)
	}
}

// A stopped codespace must be started on demand, and the phase history must
// show the documented path.
func TestStartsStoppedCodespace(t *testing.T) {
	h := newHarness(t, options{state: "Shutdown", polls: 2})
	h.start()

	client := h.dial("root")
	defer client.Close()

	stdout, stderr, code, err := h.exec(client, "whoami")
	if err != nil {
		t.Fatalf("exec: %v (stderr %q)", err, stderr)
	}
	if stdout != "vscode\n" || code != 0 {
		t.Fatalf("got %q/%d, want \"vscode\\n\"/0 (stderr %q)", stdout, code, stderr)
	}
	if n := h.gh.Calls("/start"); n != 1 {
		t.Errorf("start called %d times, want exactly 1", n)
	}

	st := h.envStatus(h.handle)
	if st.Starts != 1 {
		t.Errorf("recorded %d starts, want 1", st.Starts)
	}
	var phases []string
	for _, tr := range st.History {
		phases = append(phases, string(tr.To))
	}
	joined := strings.Join(phases, ",")
	for _, want := range []string{string(lifecycle.PhaseStarting), string(lifecycle.PhaseRunning),
		string(lifecycle.PhaseConnecting), string(lifecycle.PhaseConnected)} {
		if !strings.Contains(joined, want) {
			t.Errorf("phase history %q is missing %s", joined, want)
		}
	}
}

// A missing codespace is created once, and the configured handle keeps working
// afterwards even though GitHub named the codespace itself.
func TestCreatesMissingCodespace(t *testing.T) {
	h := newHarness(t, options{state: "", autoCreate: true, createRepo: "octo/demo"})
	h.start()

	client := h.dial("root")
	if stdout, stderr, _, err := h.exec(client, "echo created"); err != nil || stdout != "created\n" {
		t.Fatalf("exec after create: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	client.Close()

	if n := h.gh.Created(); n != 1 {
		t.Fatalf("created %d codespaces, want 1", n)
	}
	st := h.envStatus(h.handle)
	if st.Resolved == "" || st.Resolved == h.handle {
		t.Errorf("handle %q resolved to %q, want the generated codespace name", h.handle, st.Resolved)
	}

	// A second connection must reuse it: the handle now matches by display name.
	client2 := h.dial("root")
	defer client2.Close()
	if _, _, _, err := h.exec(client2, "echo again"); err != nil {
		t.Fatalf("second exec: %v", err)
	}
	if n := h.gh.Created(); n != 1 {
		t.Errorf("created %d codespaces after reconnect, want 1", n)
	}
}

// Two clients arriving together must share one start operation.
func TestConcurrentClientsShareOneStart(t *testing.T) {
	h := newHarness(t, options{state: "Shutdown", polls: 4})
	h.start()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := h.tryDial("root")
			if err != nil {
				errs <- err
				return
			}
			defer client.Close()
			sess, err := client.NewSession()
			if err != nil {
				errs <- err
				return
			}
			defer sess.Close()
			out, err := sess.Output("echo shared")
			if err != nil {
				errs <- err
				return
			}
			if string(out) != "shared\n" {
				errs <- errUnexpected(string(out))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent client failed: %v", err)
	}

	if n := h.gh.Calls("/start"); n != 1 {
		t.Errorf("start called %d times for two concurrent clients, want 1", n)
	}
	if st := h.envStatus(h.handle); st.Connects != 2 {
		t.Errorf("recorded %d connects, want 2", st.Connects)
	}
}

// The login name may name the target: ssh root+other-box@gateway.
func TestEnvironmentSelectionByLogin(t *testing.T) {
	h := newHarness(t, options{state: "Available"})
	h.gh.Add(testCodespace("other-box", "Available"))
	h.start()

	client := h.dial("root+other-box")
	defer client.Close()
	if _, _, _, err := h.exec(client, "echo routed"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if st := h.envStatus("other-box"); st.Phase != lifecycle.PhaseConnected {
		t.Errorf("other-box phase is %q, want CONNECTED", st.Phase)
	}
	if st := h.envStatus(h.handle); st.Connects != 0 {
		t.Errorf("default environment was used despite an explicit target")
	}
}

// The environment can also be selected with an SSH env request.
func TestEnvironmentSelectionByEnvVar(t *testing.T) {
	h := newHarness(t, options{state: "Available"})
	h.gh.Add(testCodespace("env-box", "Available"))
	h.start()

	client := h.dial("root")
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Setenv("GATEWAY_ENV", "env-box"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	out, err := sess.Output("echo picked")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if string(out) != "picked\n" {
		t.Fatalf("got %q", out)
	}
	waitFor(t, "env-box to be connected", 5*time.Second, func() bool {
		return h.envStatus("env-box").Connects == 1
	})
}

// The gateway must reuse the documented GitHub CLI invocations rather than
// reimplementing the tunnel, and must hand gh its own key.
func TestUsesDocumentedGHCommands(t *testing.T) {
	h := newHarness(t, options{state: "Available"})
	argsLog := filepath.Join(h.dir, "gh-args.log")
	t.Setenv(testenv.EnvArgsLogPath, argsLog)
	h.start()

	client := h.dial("root")
	defer client.Close()
	if _, _, _, err := h.exec(client, "echo args"); err != nil {
		t.Fatalf("exec: %v", err)
	}

	raw, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("gh was never invoked: %v", err)
	}
	log := string(raw)
	keyPath := filepath.Join(h.dir, "state", "providers", "github", "codespace_ed25519")
	for _, want := range []string{
		"codespace ssh -c my-box --config",
		"codespace ssh -c my-box --stdio -- -i " + keyPath,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("gh was not called with %q; calls were:\n%s", want, log)
		}
	}
}

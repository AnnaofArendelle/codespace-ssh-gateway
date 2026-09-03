package gateway_test

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/lifecycle"

	gossh "golang.org/x/crypto/ssh"
)

// An interactive session must carry the pty request, the terminal type and
// later window changes all the way into the codespace.
func TestPTYShellAndResize(t *testing.T) {
	h := newHarness(t, options{state: "Available"})
	h.start()

	client := h.dial("root")
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{gossh.ECHO: 1}); err != nil {
		t.Fatalf("pty request: %v", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}

	readUntil(t, stdout, "codespace ready", 10*time.Second)
	if err := sess.WindowChange(40, 100); err != nil {
		t.Fatalf("window change: %v", err)
	}
	if _, err := stdin.Write([]byte("hello there\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, stdout, "echo: hello there", 10*time.Second)
	if _, err := stdin.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	remote := h.sshd.LastSession()
	if remote == nil || !remote.PTY {
		t.Fatalf("codespace did not see a pty: %+v", remote)
	}
	if remote.Term != "xterm-256color" {
		t.Errorf("codespace saw TERM %q, want xterm-256color", remote.Term)
	}
	if remote.Rows != 24 || remote.Cols != 80 {
		t.Errorf("codespace saw %dx%d, want 24x80", remote.Rows, remote.Cols)
	}
	waitFor(t, "window change to reach the codespace", 5*time.Second, func() bool {
		for _, wc := range h.sshd.LastSession().WindowChanges {
			if wc == [2]uint32{40, 100} {
				return true
			}
		}
		return false
	})
}

// Exit status and stderr must come from the remote command, not from the
// transport.
func TestExitStatusAndStderr(t *testing.T) {
	h := newHarness(t, options{state: "Available"})
	h.start()
	client := h.dial("root")
	defer client.Close()

	if _, _, code, err := h.exec(client, "exit 7"); err != nil || code != 7 {
		t.Fatalf("exit 7 gave code %d err %v, want 7", code, err)
	}
	stdout, stderr, code, err := h.exec(client, "to-stderr")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code != 0 {
		t.Errorf("code %d, want 0", code)
	}
	if stdout != "" {
		t.Errorf("stdout %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "this is stderr") {
		t.Errorf("stderr %q does not contain the remote stderr", stderr)
	}
	if _, _, code, _ := h.exec(client, "definitely-not-a-command"); code != 127 {
		t.Errorf("unknown command gave %d, want 127", code)
	}
}

// A subsystem request (sftp, used by scp/sftp clients) must be forwarded.
func TestSubsystemForwarded(t *testing.T) {
	h := newHarness(t, options{state: "Available"})
	h.start()
	client := h.dial("root")
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatalf("subsystem: %v", err)
	}
	readUntil(t, stdout, "subsystem:sftp", 10*time.Second)
	if remote := h.sshd.LastSession(); remote == nil || remote.Kind != "subsystem" {
		t.Errorf("codespace saw %+v, want a subsystem request", remote)
	}
}

// By default the gateway must not stop anything when the last client leaves:
// that decision belongs to the provider's idle mechanism.
func TestProviderOwnsIdleByDefault(t *testing.T) {
	h := newHarness(t, options{state: "Available"})
	h.start()

	client := h.dial("root")
	if _, _, _, err := h.exec(client, "echo bye"); err != nil {
		t.Fatal(err)
	}
	client.Close()

	waitFor(t, "the session to be released", 5*time.Second, func() bool {
		return h.envStatus(h.handle).Phase == lifecycle.PhaseProviderManaged
	})
	if n := h.gh.Calls("/stop"); n != 0 {
		t.Errorf("the gateway called stop %d times; idle handling must be left to the provider", n)
	}
	if state := h.gh.State(h.handle); state != "Available" {
		t.Errorf("codespace state is %q, want it left Available for GitHub to decide", state)
	}
}

// With the opt-in setting, the last disconnect stops the environment through
// the provider API.
func TestStopOnLastDisconnect(t *testing.T) {
	h := newHarness(t, options{state: "Available", stopOnLastDisconnect: true})
	h.start()

	client := h.dial("root")
	if _, _, _, err := h.exec(client, "echo bye"); err != nil {
		t.Fatal(err)
	}
	client.Close()

	waitFor(t, "the codespace to be stopped", 10*time.Second, func() bool {
		return h.gh.State(h.handle) == "Shutdown"
	})
	if n := h.gh.Calls("/stop"); n != 1 {
		t.Errorf("stop was called %d times, want 1", n)
	}
}

// Restarting the gateway must keep the host key (so clients do not see a
// changed fingerprint) and must reconnect without further setup.
func TestGatewayRestartKeepsHostKey(t *testing.T) {
	h := newHarness(t, options{state: "Available"})
	h.start()
	first := h.hostKeyFingerprint()
	client := h.dial("root")
	if _, _, _, err := h.exec(client, "echo one"); err != nil {
		t.Fatal(err)
	}
	client.Close()
	h.stop()

	h.start()
	if second := h.hostKeyFingerprint(); second != first {
		t.Errorf("host key changed across restart: %s -> %s", first, second)
	}
	client2 := h.dial("root")
	defer client2.Close()
	if out, _, _, err := h.exec(client2, "echo two"); err != nil || out != "two\n" {
		t.Fatalf("exec after restart: %q %v", out, err)
	}
}

// The exec connector runs the plain `gh codespace ssh`, which delegates to the
// system ssh client. This exercises the local pty path.
func TestExecConnectorInteractive(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh binary available for the exec connector")
	}
	h := newHarness(t, options{state: "Available", connector: "exec"})
	h.start()

	client := h.dial("root")
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm", 30, 90, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	readUntil(t, stdout, "codespace ready", 20*time.Second)
	if _, err := stdin.Write([]byte("via-exec\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, stdout, "echo: via-exec", 20*time.Second)
	_, _ = stdin.Write([]byte("exit\n"))
	_ = sess.Wait()

	if remote := h.sshd.LastSession(); remote == nil || !remote.PTY {
		t.Errorf("the codespace did not see a pty through the exec connector: %+v", remote)
	}
}

// A client that waits for a cold start must be told what is happening instead
// of staring at a blank terminal.
func TestClientSeesStartupProgress(t *testing.T) {
	h := newHarness(t, options{state: "Shutdown", polls: 3})
	h.start()

	client := h.dial("root")
	defer client.Close()
	_, stderr, code, err := h.exec(client, "echo progress")
	if err != nil || code != 0 {
		t.Fatalf("exec: %v code=%d stderr=%q", err, code, stderr)
	}
	for _, want := range []string{"正在启动", "已就绪"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("client was not told about %s; stderr was:\n%s", want, stderr)
		}
	}
}

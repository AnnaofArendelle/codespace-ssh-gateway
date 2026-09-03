package gateway_test

import (
	"net"
	"os/exec"
	"strings"
	"testing"
)

// TestSystemSSHClient uses the real OpenSSH client, which is what an actual user
// runs: `ssh root@gateway <command>`.
func TestSystemSSHClient(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh binary available")
	}
	h := newHarness(t, options{state: "Shutdown", polls: 1})
	h.start()

	host, port, err := net.SplitHostPort(h.addr)
	if err != nil {
		t.Fatal(err)
	}
	sshArgs := []string{
		"-i", h.clientKeyPath,
		"-p", port,
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "LogLevel=ERROR",
		"root@" + host,
	}

	out, err := exec.Command("ssh", append(append([]string{}, sshArgs...), "echo", "from-openssh")...).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "from-openssh") {
		t.Errorf("unexpected output: %q", out)
	}
	if n := h.gh.Calls("/start"); n != 1 {
		t.Errorf("start called %d times, want 1", n)
	}

	// Exit codes must survive both hops.
	cmd := exec.Command("ssh", append(append([]string{}, sshArgs...), "exit", "7")...)
	if err := cmd.Run(); err == nil {
		t.Error("expected a non-zero exit status")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		t.Errorf("openssh reported %v, want exit status 7", err)
	}

	// A second connection reuses the running codespace.
	out, err = exec.Command("ssh", append(append([]string{}, sshArgs...), "whoami")...).CombinedOutput()
	if err != nil {
		t.Fatalf("second ssh failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "vscode") {
		t.Errorf("unexpected output: %q", out)
	}
	if n := h.gh.Calls("/start"); n != 1 {
		t.Errorf("start called %d times overall, want 1", n)
	}
}

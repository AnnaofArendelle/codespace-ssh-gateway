package gateway_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/lifecycle"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/internal/testenv"

	gossh "golang.org/x/crypto/ssh"
)

// gatewayKeyFingerprint is the fingerprint of the key the gateway generated for
// the gateway -> codespace hop.
func (h *harness) gatewayKeyFingerprint() string {
	h.t.Helper()
	path := filepath.Join(h.dir, "state", "providers", "github", "codespace_ed25519.pub")
	raw, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("read gateway key: %v", err)
	}
	key, _, _, _, err := gossh.ParseAuthorizedKey(raw)
	if err != nil {
		h.t.Fatalf("parse gateway key: %v", err)
	}
	return gossh.FingerprintSHA256(key)
}

// envStatus returns the lifecycle view of one handle.
func (h *harness) envStatus(handle string) lifecycle.EnvStatus {
	h.t.Helper()
	for _, e := range h.gw.Status().Environments {
		if e.Environment == handle {
			return e
		}
	}
	return lifecycle.EnvStatus{}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// readUntil reads from r until want appears, or the timeout expires.
func readUntil(t *testing.T, r io.Reader, want string, timeout time.Duration) string {
	t.Helper()
	type result struct {
		data string
		err  error
	}
	found := make(chan result, 1)
	go func() {
		var buf bytes.Buffer
		chunk := make([]byte, 256)
		for {
			n, err := r.Read(chunk)
			if n > 0 {
				buf.Write(chunk[:n])
				if bytes.Contains(buf.Bytes(), []byte(want)) {
					found <- result{buf.String(), nil}
					return
				}
			}
			if err != nil {
				found <- result{buf.String(), err}
				return
			}
		}
	}()
	select {
	case res := <-found:
		if !bytes.Contains([]byte(res.data), []byte(want)) {
			t.Fatalf("did not see %q in output (err %v); got: %q", want, res.err, res.data)
		}
		return res.data
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %q", want)
		return ""
	}
}

// testCodespace is a shorthand for adding another codespace to the fake API.
func testCodespace(name, state string) testenv.Codespace {
	return testenv.Codespace{Name: name, DisplayName: name, State: state, IdleTimeoutMinutes: 30}
}

// errUnexpected reports unexpected output from a concurrent client.
func errUnexpected(got string) error {
	return fmt.Errorf("unexpected output %q", got)
}

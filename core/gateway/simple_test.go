package gateway_test

import (
	"strings"
	"testing"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/gateway"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/logging"
)

// The minimal setup is "a token in the config file": no keys, no password, and
// no codespace named. A local client must still get a shell.
func TestTokenOnlyConfigJustWorks(t *testing.T) {
	h := newHarness(t, options{state: "Available", noClientKeys: true, noDefaultEnv: true})
	h.start()

	client, err := h.dialAnonymous("root")
	if err != nil {
		t.Fatalf("connecting without any credential failed: %v", err)
	}
	defer client.Close()

	stdout, stderr, code, err := h.exec(client, "echo simple")
	if err != nil || code != 0 {
		t.Fatalf("exec: %v code=%d stderr=%q", err, code, stderr)
	}
	if stdout != "simple\n" {
		t.Fatalf("got %q, want \"simple\\n\"", stdout)
	}
	// The gateway picked the only codespace the account has.
	if st := h.envStatus(h.handle); st.Connects != 1 {
		t.Errorf("expected the single codespace to be used, status: %+v", st)
	}
}

// Binding to a public interface without a credential must refuse to start
// rather than silently exposing the codespace.
func TestPublicListenerNeedsACredential(t *testing.T) {
	h := newHarness(t, options{state: "Available", noClientKeys: true, listen: "0.0.0.0:0"})
	cfg, err := config.Load(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gateway.New(t.Context(), cfg, logging.Discard(), logging.NewRedactor())
	if err == nil {
		t.Fatal("a public listener with no credential was accepted")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error does not explain the fix: %v", err)
	}
}

func TestLoopbackListenDetection(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:2222": true,
		"localhost:2222": true,
		"[::1]:2222":     true,
		":2222":          false,
		"0.0.0.0:2222":   false,
		"192.168.1.5:22": false,
		"garbage":        false,
	}
	for addr, want := range cases {
		if got := gateway.LoopbackListen(addr); got != want {
			t.Errorf("LoopbackListen(%q) = %v, want %v", addr, got, want)
		}
	}
}

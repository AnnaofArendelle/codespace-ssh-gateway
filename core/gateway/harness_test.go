package gateway_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/gateway"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/logging"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/internal/testenv"

	_ "github.com/AnnaofArendelle/codespace-ssh-gateway/providers/github"

	gossh "golang.org/x/crypto/ssh"
)

// The test binary doubles as the `gh` executable the gateway shells out to, so
// the real subprocess and transport code runs unmodified.
func TestMain(m *testing.M) {
	testenv.FakeGHMain()
	os.Exit(m.Run())
}

const testToken = "gho_testtoken0123456789"

type options struct {
	handle               string
	state                string // GitHub state; "" means the codespace does not exist
	polls                int    // polls before a starting codespace reports Available
	connector            string
	autoCreate           bool
	createRepo           string
	stopOnLastDisconnect bool
	token                string
	requestTimeout       string
	extraGitHub          string
	// noClientKeys leaves the gateway without any authorized key, which on a
	// loopback listener means "no credential required".
	noClientKeys bool
	// noDefaultEnv leaves github.codespace empty so the gateway has to work out
	// the target itself.
	noDefaultEnv bool
	// listen overrides the listen address (default 127.0.0.1:0).
	listen string
}

type harness struct {
	t       *testing.T
	gh      *testenv.GitHub
	sshd    *testenv.SSHD
	dir     string
	cfgPath string
	handle  string

	clientKey     gossh.Signer
	clientKeyPath string
	gw            *gateway.Gateway
	addr          string
	cancel        context.CancelFunc
	served        chan error
}

func newHarness(t *testing.T, opt options) *harness {
	t.Helper()
	if opt.handle == "" {
		opt.handle = "my-box"
	}
	if opt.token == "" {
		opt.token = testToken
	}
	if opt.connector == "" {
		opt.connector = "stdio"
	}
	if opt.requestTimeout == "" {
		opt.requestTimeout = "10s"
	}

	dir := t.TempDir()
	gh := testenv.NewGitHub(t, testToken)
	sshd := testenv.NewSSHD(t)

	if opt.state != "" {
		gh.Add(testenv.Codespace{
			Name:                 opt.handle,
			DisplayName:          opt.handle,
			State:                opt.state,
			IdleTimeoutMinutes:   30,
			PollsBeforeAvailable: opt.polls,
		})
	}

	clientSigner, clientAuthorized, clientKeyPath := newKeyFile(t, dir)

	handleValue := opt.handle
	if opt.noDefaultEnv {
		handleValue = ""
	}
	authorizedKeys := fmt.Sprintf("\n  authorized_keys_inline:\n    - %q", clientAuthorized)
	if opt.noClientKeys {
		authorizedKeys = ""
	}
	listen := opt.listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`provider: github
state_dir: %q

github:
  token: %q
  codespace: %q
  api_url: %q
  gh_path: %q
  connector: %s
  request_timeout: %s
%s

ssh:
  listen: %s%s
  handshake_timeout: 15s
  shutdown_grace: 2s

lifecycle:
  auto_create: %v
  start_timeout: 30s
  create_timeout: 60s
  connect_timeout: 20s
  status_poll_interval: 100ms
  connect_retries: 4
  stop_on_last_disconnect: %v

log:
  level: error

control:
  enabled: false
`,
		filepath.Join(dir, "state"),
		opt.token, handleValue, gh.URL(), os.Args[0], opt.connector, opt.requestTimeout,
		indentBlock(opt.extraGitHub),
		listen, authorizedKeys,
		opt.autoCreate, opt.stopOnLastDisconnect)

	if opt.createRepo != "" {
		cfg = strings.Replace(cfg, "  connector: ",
			fmt.Sprintf("  create:\n    repository: %q\n    branch: main\n    idle_timeout_minutes: 30\n  connector: ",
				opt.createRepo), 1)
	}
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(testenv.EnvFakeGH, "1")
	t.Setenv(testenv.EnvSSHDAddr, sshd.Addr)
	t.Setenv(testenv.EnvSSHUser, "vscode")
	t.Setenv(testenv.EnvGHToken, testToken)

	return &harness{t: t, gh: gh, sshd: sshd, dir: dir, cfgPath: cfgPath,
		handle: opt.handle, clientKey: clientSigner, clientKeyPath: clientKeyPath}
}

func indentBlock(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

func newKeyPair(t *testing.T) (gossh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(signer.PublicKey())))
	return signer, line
}

// newKeyFile also writes the private key in OpenSSH format, so the system ssh
// client can use it.
func newKeyFile(t *testing.T, dir string) (gossh.Signer, string, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block, err := gossh.MarshalPrivateKey(priv, "gateway test client")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "client_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(signer.PublicKey())))
	return signer, line, path
}

// start boots a gateway from the harness config and waits until it listens.
func (h *harness) start() {
	h.t.Helper()
	cfg, err := config.Load(h.cfgPath)
	if err != nil {
		h.t.Fatalf("load config: %v", err)
	}
	red := logging.NewRedactor()
	logger := testLogger(h.t)

	ctx, cancel := context.WithCancel(context.Background())
	gw, err := gateway.New(ctx, cfg, logger, red)
	if err != nil {
		cancel()
		h.t.Fatalf("gateway.New: %v", err)
	}
	if err := gw.Listen(); err != nil {
		cancel()
		h.t.Fatalf("listen: %v", err)
	}
	h.gw, h.cancel, h.addr = gw, cancel, gw.Addr()
	h.served = make(chan error, 1)
	go func() { h.served <- gw.Run(ctx) }()
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case <-h.served:
	case <-time.After(10 * time.Second):
		h.t.Error("gateway did not shut down")
	}
	_ = h.gw.Close()
	h.cancel = nil
}

// dial opens an SSH connection to the gateway as the given login.
func (h *harness) dial(login string) *gossh.Client {
	h.t.Helper()
	client, err := h.tryDial(login)
	if err != nil {
		h.t.Fatalf("dial gateway: %v", err)
	}
	return client
}

// dialAnonymous connects with no credential at all, the way a local user does
// against a loopback gateway.
func (h *harness) dialAnonymous(login string) (*gossh.Client, error) {
	cfg := &gossh.ClientConfig{
		User:            login,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	return gossh.Dial("tcp", h.addr, cfg)
}

func (h *harness) tryDial(login string) (*gossh.Client, error) {
	cfg := &gossh.ClientConfig{
		User:            login,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(h.clientKey)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	return gossh.Dial("tcp", h.addr, cfg)
}

// exec runs one command over a fresh session and returns its streams.
func (h *harness) exec(client *gossh.Client, cmd string) (string, string, int, error) {
	h.t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		return "", "", 0, err
	}
	defer sess.Close()
	var stdout, stderr strings.Builder
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	err = sess.Run(cmd)
	code := 0
	var exitErr *gossh.ExitError
	if errors.As(err, &exitErr) {
		code, err = exitErr.ExitStatus(), nil
	}
	return stdout.String(), stderr.String(), code, err
}

// testLogger routes gateway logs into the test output, which makes a failing
// end-to-end test readable. Set GATEWAY_TEST_LOG=debug for more.
func testLogger(t *testing.T) *slog.Logger {
	level := slog.LevelWarn
	if v := os.Getenv("GATEWAY_TEST_LOG"); v != "" {
		if parsed, err := logging.ParseLevel(v); err == nil {
			level = parsed
		}
	}
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: level}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// hostKeyFingerprint records what a client would pin for this gateway.
func (h *harness) hostKeyFingerprint() string {
	h.t.Helper()
	var fp string
	cfg := &gossh.ClientConfig{
		User: "root",
		Auth: []gossh.AuthMethod{gossh.PublicKeys(h.clientKey)},
		HostKeyCallback: func(_ string, _ net.Addr, key gossh.PublicKey) error {
			fp = gossh.FingerprintSHA256(key)
			return nil
		},
		Timeout: 10 * time.Second,
	}
	client, err := gossh.Dial("tcp", h.addr, cfg)
	if err != nil {
		h.t.Fatalf("dial for host key: %v", err)
	}
	client.Close()
	return fp
}

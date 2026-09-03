package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/cli"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"

	_ "github.com/AnnaofArendelle/codespace-ssh-gateway/providers/github"
)

// run executes one CLI invocation against a temporary config location.
func run(t *testing.T, cfgPath string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	full := append([]string{"-config", cfgPath}, args...)
	code := cli.Run(full, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestConfigInitWritesAPrivateFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	t.Setenv("HOME", dir) // keep key import away from the real ~/.ssh

	code, stdout, stderr := run(t, cfgPath, "config", "init", "-codespace", "my-box", "-listen", ":2299")
	if code != 0 {
		t.Fatalf("config init exited %d: %s%s", code, stdout, stderr)
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode %#o, want 0600", perm)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if cfg.Provider != "github" {
		t.Errorf("provider %q", cfg.Provider)
	}
	if cfg.SSH.Listen != ":2299" {
		t.Errorf("listen %q, want :2299", cfg.SSH.Listen)
	}
	if !cfg.Lifecycle.AutoCreate {
		t.Error("auto_create should default to true in the template")
	}
	if cfg.Lifecycle.StopOnLastDisconnect {
		t.Error("stop_on_last_disconnect must default to false: the provider owns idle")
	}

	// A second init must not clobber the file.
	if code, _, _ := run(t, cfgPath, "config", "init"); code == 0 {
		t.Error("config init overwrote an existing config without -force")
	}
}

func TestConfigShowRedactsTheToken(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := config.WriteFile(cfgPath, config.Template("github", "gho_shouldnotappear1234", "box", ":2222")); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run(t, cfgPath, "config", "show")
	if code != 0 {
		t.Fatalf("config show exited %d: %s", code, stderr)
	}
	if strings.Contains(stdout, "gho_shouldnotappear1234") {
		t.Errorf("config show leaked the token:\n%s", stdout)
	}
	if !strings.Contains(stdout, "provider: github") {
		t.Errorf("config show did not print the configuration:\n%s", stdout)
	}
}

func TestProviderListShowsTheSelectedProvider(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := config.WriteFile(cfgPath, config.Template("github", "", "box", ":2222")); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run(t, cfgPath, "provider", "list")
	if code != 0 {
		t.Fatalf("provider list exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "github") || !strings.Contains(stdout, "gh codespace ssh") {
		t.Errorf("unexpected provider list:\n%s", stdout)
	}
}

func TestStatusWithoutARunningGateway(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := config.WriteFile(cfgPath, config.Template("github", "", "box", ":2222")); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := run(t, cfgPath, "status")
	if code != 0 {
		t.Fatalf("status exited %d", code)
	}
	if !strings.Contains(stdout, "not running") {
		t.Errorf("status output does not say the gateway is down:\n%s", stdout)
	}
}

func TestMissingConfigIsExplained(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := run(t, filepath.Join(dir, "absent.yaml"), "status")
	if code == 0 {
		t.Error("status succeeded without a config file")
	}
	if !strings.Contains(stderr, "config init") {
		t.Errorf("error does not point at `config init`: %s", stderr)
	}
}

func TestSetTokenPatchesConfigFromStdin(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := config.WriteFile(cfgPath, config.Template("github", "", "box", ":2222")); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("gho_freshtoken987654\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	saved := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = saved }()

	code, stdout, stderr := run(t, cfgPath, "config", "set-token")
	if code != 0 {
		t.Fatalf("set-token exited %d: %s%s", code, stdout, stderr)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "gho_freshtoken987654") {
		t.Error("the token was not stored")
	}
	if strings.Contains(stdout, "gho_freshtoken987654") {
		t.Errorf("set-token echoed the token: %s", stdout)
	}
}

func TestVersionAndHelp(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if code, stdout, _ := run(t, cfgPath, "version"); code != 0 || !strings.Contains(stdout, "ssh-gateway") {
		t.Errorf("version output %q (code %d)", stdout, code)
	}
	if code, stdout, _ := run(t, cfgPath, "help"); code != 0 || !strings.Contains(stdout, "codespace select") {
		t.Errorf("help output %q (code %d)", stdout, code)
	}
	if code, _, stderr := run(t, cfgPath, "nonsense"); code != 2 || !strings.Contains(stderr, "unknown command") {
		t.Errorf("unknown command handling: code %d stderr %q", code, stderr)
	}
}

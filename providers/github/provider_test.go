package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/logging"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/internal/testenv"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"

	"gopkg.in/yaml.v3"
)

const testToken = "gho_providertesttoken1"

// yamlSection feeds a literal config section to the provider factory.
type yamlSection string

func (y yamlSection) Decode(v any) error { return yaml.Unmarshal([]byte(y), v) }

func newTestProvider(t *testing.T, gh *testenv.GitHub, extra string) *Provider {
	t.Helper()
	section := yamlSection(fmt.Sprintf("token: %q\napi_url: %q\ncodespace: my-box\n%s",
		testToken, gh.URL(), extra))
	p, err := New(context.Background(), providers.Deps{
		Config:   section,
		Logger:   logging.Discard(),
		StateDir: t.TempDir(),
		Redact:   func(...string) {},
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p.(*Provider)
}

func TestStateMapping(t *testing.T) {
	cases := map[string]providers.State{
		"Available":    providers.StateRunning,
		"Shutdown":     providers.StateStopped,
		"Archived":     providers.StateStopped,
		"Starting":     providers.StateStarting,
		"Queued":       providers.StateProvisioning,
		"Provisioning": providers.StateProvisioning,
		"ShuttingDown": providers.StateStopping,
		"Failed":       providers.StateFailed,
		"Unavailable":  providers.StateUnavailable,
		"Deleted":      providers.StateNotFound,
		"Something":    providers.StateUnknown,
		"":             providers.StateUnknown,
	}
	for native, want := range cases {
		if got := stateFor(native); got != want {
			t.Errorf("stateFor(%q) = %s, want %s", native, got, want)
		}
	}
}

func TestGetResolvesDisplayName(t *testing.T) {
	gh := testenv.NewGitHub(t, testToken)
	gh.Add(testenv.Codespace{Name: "octo-demo-abc123", DisplayName: "my-box", State: "Available"})
	p := newTestProvider(t, gh, "")

	env, err := p.Get(context.Background(), "my-box")
	if err != nil {
		t.Fatalf("get by display name: %v", err)
	}
	if env.ID != "octo-demo-abc123" {
		t.Errorf("resolved to %q, want the codespace name", env.ID)
	}
	if env.State != providers.StateRunning {
		t.Errorf("state %s", env.State)
	}

	if _, err := p.Get(context.Background(), "nope"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("missing codespace gave %v, want ErrNotFound", err)
	}
}

func TestAPIErrorClassification(t *testing.T) {
	gh := testenv.NewGitHub(t, testToken)
	gh.Add(testenv.Codespace{Name: "my-box", State: "Available"})
	p := newTestProvider(t, gh, "")
	ctx := context.Background()

	// A wrong token is an auth error, and the token never appears in it.
	bad := newTestProvider(t, gh, "")
	bad.api.token = func() string { return "not-the-token" }
	_, err := bad.Get(ctx, "my-box")
	if !errors.Is(err, providers.ErrAuth) {
		t.Errorf("401 mapped to %v, want ErrAuth", err)
	}

	// A rate-limited response is temporary, not fatal.
	gh.Inject("/user/codespaces/my-box", http.StatusForbidden, `{"message":"API rate limit exceeded"}`, 3)
	_, err = p.Get(ctx, "my-box")
	if !providers.IsTemporary(err) {
		t.Errorf("rate limit mapped to %v, want a temporary error", err)
	}

	// Server errors are retried, so a single 500 is invisible.
	gh.Inject("/user/codespaces/my-box", http.StatusInternalServerError, "", 1)
	if _, err := p.Get(ctx, "my-box"); err != nil {
		t.Errorf("a single 500 was not retried away: %v", err)
	}
}

func TestStartToleratesAlreadyRunning(t *testing.T) {
	gh := testenv.NewGitHub(t, testToken)
	gh.Add(testenv.Codespace{Name: "my-box", State: "Available"})
	p := newTestProvider(t, gh, "")

	if err := p.Start(context.Background(), "my-box"); err != nil {
		t.Fatalf("starting a running codespace failed: %v", err)
	}
}

func TestStopToleratesStopped(t *testing.T) {
	gh := testenv.NewGitHub(t, testToken)
	gh.Add(testenv.Codespace{Name: "my-box", State: "Shutdown"})
	p := newTestProvider(t, gh, "")

	if err := p.Stop(context.Background(), "my-box"); err != nil {
		t.Fatalf("stopping a stopped codespace failed: %v", err)
	}
}

func TestCreateUsesHandleAsDisplayName(t *testing.T) {
	gh := testenv.NewGitHub(t, testToken)
	p := newTestProvider(t, gh, "create:\n  repository: octo/demo\n  branch: main\n  idle_timeout_minutes: 30\n")

	env, err := p.Create(context.Background(), providers.CreateSpec{Name: "my-box"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.DisplayName != "my-box" {
		t.Errorf("display name %q, want the handle so it stays addressable", env.DisplayName)
	}
	if env.ID == "my-box" {
		t.Errorf("expected GitHub to name the codespace itself, got %q", env.ID)
	}
	// The handle keeps resolving afterwards.
	if got, err := p.Get(context.Background(), "my-box"); err != nil || got.ID != env.ID {
		t.Errorf("handle did not resolve after create: %v (%q vs %q)", err, got.ID, env.ID)
	}
}

func TestCreateNeedsARepository(t *testing.T) {
	gh := testenv.NewGitHub(t, testToken)
	p := newTestProvider(t, gh, "")
	_, err := p.Create(context.Background(), providers.CreateSpec{Name: "my-box"})
	if err == nil {
		t.Fatal("create without a repository succeeded")
	}
	if !strings.Contains(err.Error(), "github.create.repository") {
		t.Errorf("error does not say what to configure: %v", err)
	}
}

func TestCapabilitiesAreHonestAboutIdle(t *testing.T) {
	gh := testenv.NewGitHub(t, testToken)
	p := newTestProvider(t, gh, "")
	caps := p.Capabilities()

	if !caps.ProviderManagedIdle {
		t.Error("the GitHub provider should report provider-managed idle")
	}
	if caps.SSHActivityAttribution != providers.IdleAttributionUnverified {
		t.Errorf("attribution %q: it must not claim more than GitHub documents",
			caps.SSHActivityAttribution)
	}
	if len(caps.Notes) == 0 {
		t.Error("no notes explaining the idle caveats")
	}
	if caps.Create {
		t.Error("create should be off until a repository is configured")
	}
}

func TestCreateSourcesListsRepositories(t *testing.T) {
	gh := testenv.NewGitHub(t, testToken)
	p := newTestProvider(t, gh, "")

	sources, err := p.CreateSources(context.Background(), 20)
	if err != nil {
		t.Fatalf("create sources: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("got %d sources, want 2: %+v", len(sources), sources)
	}
	if sources[0].Name != "octo/demo" {
		t.Errorf("first source is %q, want the most recently pushed repository", sources[0].Name)
	}
	if !strings.Contains(sources[1].Detail, "私有") {
		t.Errorf("private repository not marked: %q", sources[1].Detail)
	}
	if !strings.Contains(sources[0].Detail, "main") {
		t.Errorf("default branch missing: %q", sources[0].Detail)
	}
}

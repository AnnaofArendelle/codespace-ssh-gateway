// Package providers defines the contract between the gateway core and a
// backing environment provider (GitHub Codespaces today; CodeSandbox, Cloud
// Shell, Pterobox and friends later).
//
// Nothing in this package may reference a specific provider, and no provider
// implementation may reference the SSH server or gateway core.
package providers

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// Provider is one backend that can host remote environments.
type Provider interface {
	// Name is the registry name, e.g. "github".
	Name() string
	// Capabilities reports what this provider supports, without guessing.
	Capabilities() Capabilities
	// DefaultEnvironment is the configured target used when a client does not
	// name one. Empty means the operator has not selected one yet.
	DefaultEnvironment() string

	List(ctx context.Context) ([]Environment, error)
	// Get returns one environment, or an error wrapping ErrNotFound.
	Get(ctx context.Context, id string) (Environment, error)
	// Create provisions a new environment. Providers fill in their own
	// configured defaults for anything spec leaves out.
	Create(ctx context.Context, spec CreateSpec) (Environment, error)
	// Start asks the provider to move the environment towards RUNNING. It
	// should be idempotent: starting a running environment is not an error.
	Start(ctx context.Context, id string) error
	// Stop asks the provider to stop the environment now.
	Stop(ctx context.Context, id string) error
	Status(ctx context.Context, id string) (State, error)
	// Connect opens a session. The environment is expected to be RUNNING;
	// transient "not ready yet" failures should be wrapped with Temporary.
	//
	// ctx bounds establishment only. Once Connect returns successfully the
	// session must survive cancellation of ctx and live until Conn.Close, so
	// implementations that spawn a child process must give it its own context.
	Connect(ctx context.Context, id string, req ConnectRequest) (Conn, error)

	// Close releases provider-held resources.
	Close() error
}

// ConfigDecoder hands a provider its own configuration section without
// coupling this package to a config format.
type ConfigDecoder interface {
	Decode(v any) error
}

// Deps is what the core gives a provider at construction time.
type Deps struct {
	// Config is the provider's section of the config file; nil when absent.
	Config ConfigDecoder
	Logger *slog.Logger
	// StateDir is a private, already-created directory (0700) the provider may
	// use for keys and caches.
	StateDir string
	// Redact registers a secret with the log redactor.
	Redact func(...string)
}

// Factory builds a provider from its config section.
type Factory func(ctx context.Context, deps Deps) (Provider, error)

// Registration describes a provider to the CLI before it is instantiated.
type Registration struct {
	Name    string
	Summary string
	// ConfigKey is the top-level config file key holding its settings.
	ConfigKey string
	// DefaultEnvironmentKey is the field inside ConfigKey that names the
	// default environment ("codespace" for GitHub). It is what
	// `gateway codespace select` writes.
	DefaultEnvironmentKey string
	Factory               Factory
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Registration{}
)

// Register adds a provider to the registry. It panics on duplicate names,
// which can only be a programming error.
func Register(r Registration) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if r.Name == "" || r.Factory == nil {
		panic("providers: registration needs a name and a factory")
	}
	if _, dup := registry[r.Name]; dup {
		panic("providers: duplicate registration for " + r.Name)
	}
	if r.ConfigKey == "" {
		r.ConfigKey = r.Name
	}
	if r.DefaultEnvironmentKey == "" {
		r.DefaultEnvironmentKey = "environment"
	}
	registry[r.Name] = r
}

// Registrations lists known providers, sorted by name.
func Registrations() []Registration {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Registration, 0, len(registry))
	for _, r := range registry {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup finds a registration by name.
func Lookup(name string) (Registration, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	r, ok := registry[name]
	return r, ok
}

// New instantiates a registered provider.
func New(ctx context.Context, name string, deps Deps) (Provider, error) {
	r, ok := Lookup(name)
	if !ok {
		known := make([]string, 0)
		for _, reg := range Registrations() {
			known = append(known, reg.Name)
		}
		return nil, fmt.Errorf("unknown provider %q (known: %v)", name, known)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Redact == nil {
		deps.Redact = func(...string) {}
	}
	return r.Factory(ctx, deps)
}

package providers

import "context"

// IdleAttribution records how much the gateway actually knows about whether an
// SSH session is recognised as activity by the provider's own idle timer.
//
// The gateway never synthesises activity and never runs its own idle detector,
// so this value is reported verbatim to operators instead of being assumed.
type IdleAttribution string

const (
	// IdleAttributionUnverified: the provider has an idle mechanism, but no
	// public API documents whether a gateway SSH session resets it.
	IdleAttributionUnverified IdleAttribution = "unverified"
	// IdleAttributionDocumented: the provider documents SSH sessions as activity.
	IdleAttributionDocumented IdleAttribution = "documented"
	// IdleAttributionNone: sessions are known not to count as activity.
	IdleAttributionNone IdleAttribution = "none"
)

// Capabilities is what a provider can actually do. It is printed by
// `gateway status` so nothing has to be inferred from behaviour.
type Capabilities struct {
	Create     bool `json:"create"`
	Stop       bool `json:"stop"`
	Subsystems bool `json:"subsystems"`
	Signals    bool `json:"signals"`
	Resize     bool `json:"resize"`

	// ProviderManagedIdle means the provider stops or suspends idle
	// environments on its own schedule. When true the gateway does nothing on
	// disconnect beyond releasing its session.
	ProviderManagedIdle bool `json:"provider_managed_idle"`
	// IdleMechanism names that mechanism, e.g. an API field.
	IdleMechanism string `json:"idle_mechanism,omitempty"`
	// SSHActivityAttribution is the honest answer to "does my SSH session keep
	// the environment alive?".
	SSHActivityAttribution IdleAttribution `json:"ssh_activity_attribution"`
	// Notes carry caveats an operator should read.
	Notes []string `json:"notes,omitempty"`
}

// Diagnostic is the result of one preflight check against the real backend.
type Diagnostic struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

// Diagnoser is implemented by providers that can verify their own configuration
// against the live service. `gateway doctor` uses it.
type Diagnoser interface {
	Diagnose(ctx context.Context) []Diagnostic
}

// MachineType is one size an environment can be created with.
type MachineType struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name,omitempty"`
	CPUs        int     `json:"cpus,omitempty"`
	MemoryGB    float64 `json:"memory_gb,omitempty"`
	StorageGB   float64 `json:"storage_gb,omitempty"`
	Note        string  `json:"note,omitempty"`
}

// MachineLister is implemented by providers whose environments come in sizes,
// so `gateway codespace machines` can show the real options instead of guesses.
// An empty target means "whatever the provider is configured to create from".
type MachineLister interface {
	Machines(ctx context.Context, target string) ([]MachineType, error)
}

// CreateSource is something a new environment can be created from (for GitHub
// Codespaces: a repository).
type CreateSource struct {
	// Name is what goes into the provider's create configuration.
	Name string `json:"name"`
	// Detail is a short human hint, e.g. the default branch or last push time.
	Detail string `json:"detail,omitempty"`
}

// CreateSourceLister is implemented by providers that can enumerate what an
// environment may be created from, so setup can offer a menu instead of asking
// the operator to type an identifier from memory.
type CreateSourceLister interface {
	CreateSources(ctx context.Context, limit int) ([]CreateSource, error)
}

// Introspector is implemented by providers that can describe their own
// configuration for status output. Values must never contain a secret.
type Introspector interface {
	Info() map[string]string
}

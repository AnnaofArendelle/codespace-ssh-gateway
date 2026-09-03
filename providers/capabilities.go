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

// Introspector is implemented by providers that can describe their own
// configuration for status output. Values must never contain a secret.
type Introspector interface {
	Info() map[string]string
}

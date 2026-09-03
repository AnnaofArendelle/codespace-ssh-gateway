package providers

import (
	"sort"
	"strings"
	"time"
)

// State is the provider-neutral runtime state of a remote environment. Every
// provider maps its own vocabulary onto these values; nothing outside a
// provider package should look at a provider's native state string except for
// display and diagnostics.
type State string

const (
	StateUnknown      State = "UNKNOWN"
	StateNotFound     State = "NOT_FOUND"
	StateProvisioning State = "PROVISIONING"
	StateStarting     State = "STARTING"
	StateRunning      State = "RUNNING"
	StateStopping     State = "STOPPING"
	StateStopped      State = "STOPPED"
	StateFailed       State = "FAILED"
	StateUnavailable  State = "UNAVAILABLE"
)

// Transitional reports whether the state is expected to change on its own.
func (s State) Transitional() bool {
	switch s {
	case StateProvisioning, StateStarting, StateStopping:
		return true
	}
	return false
}

// Connectable reports whether a connection can be attempted.
func (s State) Connectable() bool { return s == StateRunning }

// Startable reports whether Start is the right move from this state.
func (s State) Startable() bool {
	switch s {
	case StateStopped, StateUnknown, StateFailed:
		return true
	}
	return false
}

// Environment is the provider-neutral model of one remote development
// environment. Provider-specific detail belongs in Attributes, which the core
// only ever displays.
type Environment struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"display_name,omitempty"`
	Provider    string            `json:"provider"`
	State       State             `json:"state"`
	NativeState string            `json:"native_state,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
	// IdleTimeout is the provider's own idle-stop window, if it reports one.
	// The gateway never enforces this itself; it is surfaced so operators can
	// see which mechanism owns the environment's lifecycle.
	IdleTimeout time.Duration `json:"idle_timeout,omitempty"`
	LastUsedAt  time.Time     `json:"last_used_at,omitzero"`
	WebURL      string        `json:"web_url,omitempty"`
}

// AttrLine renders attributes deterministically for CLI output.
func (e Environment) AttrLine() string {
	keys := make([]string, 0, len(e.Attributes))
	for k := range e.Attributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+e.Attributes[k])
	}
	return strings.Join(parts, " ")
}

// CreateSpec asks a provider to create an environment. Options carries
// provider-specific knobs; the core passes it through untouched and a provider
// must reject keys it does not understand.
type CreateSpec struct {
	Name    string
	Options map[string]any
}

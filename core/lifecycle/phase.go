// Package lifecycle owns the gateway's view of an environment's state machine:
//
//	UNKNOWN -> STOPPED -> STARTING -> RUNNING -> CONNECTING -> CONNECTED
//	                                                       |
//	                            (last ssh session closes)  v
//	                                          IDLE/PROVIDER_MANAGED -> STOPPED
//
// The final transition is performed by the provider, not by this package: there
// is no idle timer here, and the gateway never reports synthetic activity.
package lifecycle

import (
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// Phase is the gateway-observed phase of one environment.
type Phase string

const (
	PhaseUnknown         Phase = "UNKNOWN"
	PhaseStopped         Phase = "STOPPED"
	PhaseProvisioning    Phase = "PROVISIONING"
	PhaseStarting        Phase = "STARTING"
	PhaseRunning         Phase = "RUNNING"
	PhaseConnecting      Phase = "CONNECTING"
	PhaseConnected       Phase = "CONNECTED"
	PhaseStopping        Phase = "STOPPING"
	PhaseProviderManaged Phase = "IDLE/PROVIDER_MANAGED"
	PhaseFailed          Phase = "FAILED"
)

// PhaseForState maps a provider state onto the phase the gateway reports when
// it is not driving a transition itself.
func PhaseForState(s providers.State) Phase {
	switch s {
	case providers.StateRunning:
		return PhaseRunning
	case providers.StateStopped:
		return PhaseStopped
	case providers.StateStarting:
		return PhaseStarting
	case providers.StateProvisioning:
		return PhaseProvisioning
	case providers.StateStopping:
		return PhaseStopping
	case providers.StateFailed:
		return PhaseFailed
	default:
		return PhaseUnknown
	}
}

// Notice is the short line a waiting SSH client is shown for this phase. An
// empty string means the phase is not worth interrupting the terminal for.
func (p Phase) Notice() string {
	switch p {
	case PhaseProvisioning:
		return "codespace 不存在，正在创建…"
	case PhaseStarting:
		return "codespace 已停止，正在启动…"
	case PhaseStopping:
		return "codespace 正在关机，等它结束后再启动…"
	case PhaseRunning:
		return "codespace 已就绪"
	case PhaseConnecting:
		return "正在建立连接…"
	case PhaseFailed:
		return "准备 codespace 失败"
	}
	return ""
}

// Transition is one recorded phase change.
type Transition struct {
	From   Phase     `json:"from"`
	To     Phase     `json:"to"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// EnvStatus is a snapshot of one environment for `gateway status`.
type EnvStatus struct {
	Environment string       `json:"environment"`
	Resolved    string       `json:"resolved_id,omitempty"`
	Phase       Phase        `json:"phase"`
	Since       time.Time    `json:"since"`
	NativeState string       `json:"native_state,omitempty"`
	LastError   string       `json:"last_error,omitempty"`
	Starts      int          `json:"starts"`
	Creates     int          `json:"creates"`
	Connects    int          `json:"connects"`
	InFlight    bool         `json:"in_flight"`
	Waiters     int          `json:"waiters,omitempty"`
	History     []Transition `json:"history,omitempty"`
}

// maxHistory caps the per-environment transition log.
const maxHistory = 32

package ssh

import (
	"context"
	"strings"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// EnvVarEnvironment lets a client pick the target environment without changing
// the login name: `ssh -o SetEnv=GATEWAY_ENV=my-box root@gateway`.
const EnvVarEnvironment = "GATEWAY_ENV"

// Login is a parsed SSH login name. The gateway accepts any user name (root
// included) as its own identity for the session; it says nothing about which
// account exists inside the environment.
type Login struct {
	// User is the gateway-side login name.
	User string
	// EnvironmentHint is the optional target after a '+', as in root+my-box.
	EnvironmentHint string
}

// ParseLogin splits "user+environment" into its parts.
func ParseLogin(raw string) Login {
	if i := strings.IndexByte(raw, '+'); i >= 0 {
		return Login{User: raw[:i], EnvironmentHint: raw[i+1:]}
	}
	return Login{User: raw}
}

// OpenRequest is everything the backend needs to open one session.
type OpenRequest struct {
	User string
	// EnvironmentHint is empty when the client did not name a target.
	EnvironmentHint string
	RemoteAddr      string
	ClientVersion   string
	KeyFingerprint  string
	Connect         providers.ConnectRequest
}

// OpenResult is a live session plus its cleanup hook.
type OpenResult struct {
	Conn providers.Conn
	// Environment is the resolved environment id, for logging.
	Environment string
	// Release is called exactly once when the client session ends.
	Release func()
}

// Backend turns an authenticated SSH request into a live environment session.
// It is implemented by the gateway core; this package must not know how an
// environment is started or which provider hosts it.
type Backend interface {
	OpenSession(ctx context.Context, req OpenRequest) (*OpenResult, error)
}

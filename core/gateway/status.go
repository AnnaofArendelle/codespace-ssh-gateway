package gateway

import (
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/lifecycle"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/session"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// Status is the machine-readable state of a running gateway. It is served on
// the control socket and rendered by `gateway status`. It never contains a
// credential.
type Status struct {
	Provider             string                 `json:"provider"`
	ProviderSummary      string                 `json:"provider_summary,omitempty"`
	ProviderInfo         map[string]string      `json:"provider_info,omitempty"`
	Capabilities         providers.Capabilities `json:"capabilities"`
	Listen               string                 `json:"listen"`
	HostKeyFingerprint   string                 `json:"host_key_fingerprint"`
	DefaultEnvironment   string                 `json:"default_environment,omitempty"`
	ConfigPath           string                 `json:"config_path,omitempty"`
	StateDir             string                 `json:"state_dir"`
	StartedAt            time.Time              `json:"started_at"`
	Uptime               string                 `json:"uptime"`
	AuthorizedKeys       int                    `json:"authorized_keys"`
	PasswordAuth         bool                   `json:"password_auth"`
	AutoCreate           bool                   `json:"auto_create"`
	StopOnLastDisconnect bool                   `json:"stop_on_last_disconnect"`
	Sessions             []session.Info         `json:"sessions"`
	Environments         []lifecycle.EnvStatus  `json:"environments"`
}

// Status snapshots the gateway.
func (g *Gateway) Status() Status {
	st := Status{
		Provider:             g.prov.Name(),
		Capabilities:         g.prov.Capabilities(),
		Listen:               g.Addr(),
		HostKeyFingerprint:   g.srv.HostKeyFingerprint(),
		DefaultEnvironment:   g.prov.DefaultEnvironment(),
		ConfigPath:           g.cfg.Path(),
		StateDir:             g.cfg.StateDir,
		StartedAt:            g.startedAt,
		Uptime:               time.Since(g.startedAt).Round(time.Second).String(),
		AuthorizedKeys:       g.srv.Authorizer().KeyCount(),
		PasswordAuth:         g.srv.Authorizer().PasswordEnabled(),
		AutoCreate:           g.cfg.Lifecycle.AutoCreate,
		StopOnLastDisconnect: g.cfg.Lifecycle.StopOnLastDisconnect,
		Sessions:             g.sess.List(),
		Environments:         g.life.All(),
	}
	if reg, ok := providers.Lookup(g.prov.Name()); ok {
		st.ProviderSummary = reg.Summary
	}
	if in, ok := g.prov.(providers.Introspector); ok {
		st.ProviderInfo = in.Info()
	}
	// Make sure the configured default shows up even before its first use.
	if st.DefaultEnvironment != "" {
		found := false
		for _, e := range st.Environments {
			if e.Environment == st.DefaultEnvironment {
				found = true
				break
			}
		}
		if !found {
			st.Environments = append(st.Environments, g.life.Status(st.DefaultEnvironment))
		}
	}
	return st
}

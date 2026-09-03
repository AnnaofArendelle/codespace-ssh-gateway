package cli

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/gateway"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
	sshsrv "github.com/AnnaofArendelle/codespace-ssh-gateway/ssh"
)

// cmdDoctor checks the configuration against reality: the provider's API, the
// gh binary, the host key, the client keys and the listen address.
func (a *app) cmdDoctor(args []string) error {
	fs := a.flagSet("doctor")
	if err := fs.Parse(args); err != nil {
		return err
	}

	type check struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail,omitempty"`
		Hint   string `json:"hint,omitempty"`
	}
	var checks []check
	add := func(name string, ok bool, detail, hint string) {
		checks = append(checks, check{name, ok, detail, hint})
	}

	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	add("config file", true, cfg.Path(), "")
	if w := config.PermWarning(cfg.Path()); w != "" {
		add("config permissions", false, w, "chmod 600 the config file")
	} else {
		add("config permissions", true, "owner-only", "")
	}

	// Listen address: bind it briefly to prove the port is free.
	if ln, err := net.Listen("tcp", cfg.SSH.Listen); err != nil {
		add("listen address", false, fmt.Sprintf("%s: %v", cfg.SSH.Listen, err),
			"stop the other process or change ssh.listen")
	} else {
		ln.Close()
		add("listen address", true, cfg.SSH.Listen+" is free", "")
	}

	// Client authentication.
	auth, authErr := sshsrv.NewAuthorizer(sshsrv.AuthConfig{
		AuthorizedKeysFile:   cfg.AuthorizedKeysPath(),
		AuthorizedKeysInline: cfg.SSH.AuthorizedKeysInline,
		PasswordAuth:         cfg.SSH.PasswordAuth,
		Password:             cfg.SSH.Password,
		AllowedUsers:         cfg.SSH.AllowedUsers,
	}, a.log)
	if authErr != nil {
		add("client auth", false, authErr.Error(),
			"gateway config authorized-key add ~/.ssh/id_ed25519.pub")
	} else {
		detail := fmt.Sprintf("%d authorized key(s)", auth.KeyCount())
		if auth.PasswordEnabled() {
			detail += ", password auth enabled"
		}
		add("client auth", true, detail, "")
	}

	// Host key: report the fingerprint clients will see, creating it if needed.
	if signer, created, err := sshsrv.LoadOrCreateHostKey(cfg.HostKeyPath()); err != nil {
		add("host key", false, err.Error(), "")
	} else {
		note := "existing"
		if created {
			note = "generated now"
		}
		add("host key", true, fmt.Sprintf("%s (%s, %s)",
			sshsrv.Fingerprint(signer.PublicKey()), cfg.HostKeyPath(), note), "")
	}

	// Provider checks talk to the real service.
	_, closeLog, err := a.logger(cfg, true)
	if err != nil {
		return err
	}
	defer closeLog()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	prov, err := gateway.BuildProvider(ctx, cfg, a.log, a.redact)
	if err != nil {
		add("provider "+cfg.Provider, false, err.Error(), "")
	} else {
		defer prov.Close()
		if d, ok := prov.(providers.Diagnoser); ok {
			for _, diag := range d.Diagnose(ctx) {
				add(diag.Name, diag.OK, diag.Detail, diag.Hint)
			}
		} else {
			add("provider "+cfg.Provider, true, "instantiated (no self-checks available)", "")
		}
		caps := prov.Capabilities()
		add("idle handling", true, fmt.Sprintf("%s; ssh-as-activity: %s",
			caps.IdleMechanism, caps.SSHActivityAttribution),
			"the gateway never runs its own idle timer")
	}

	if a.jsonOut {
		failed := false
		for _, c := range checks {
			if !c.OK {
				failed = true
			}
		}
		if err := a.writeJSON(map[string]any{"ok": !failed, "checks": checks}); err != nil {
			return err
		}
		if failed {
			return errNotHealthy
		}
		return nil
	}

	failures := 0
	for _, c := range checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
			failures++
		}
		fmt.Fprintf(a.stdout, "[%s] %-20s %s\n", mark, c.Name, c.Detail)
		if c.Hint != "" && !c.OK {
			fmt.Fprintf(a.stdout, "       hint: %s\n", c.Hint)
		}
	}
	fmt.Fprintln(a.stdout)
	if failures > 0 {
		return fmt.Errorf("%d check(s) failed", failures)
	}
	fmt.Fprintf(a.stdout, "all checks passed; run `gateway start`\n")
	return nil
}

var errNotHealthy = fmt.Errorf("one or more checks failed")

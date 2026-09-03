package github

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// Diagnose runs the checks that need the real service: the token against the
// GitHub API, the gh binary, and the configured codespace. It is what
// `gateway doctor` reports.
func (p *Provider) Diagnose(ctx context.Context) []providers.Diagnostic {
	var out []providers.Diagnostic

	// 1. GitHub CLI.
	if !p.gh.Available() {
		out = append(out, providers.Diagnostic{
			Name:   "gh cli",
			OK:     false,
			Detail: fmt.Sprintf("not found: %v", p.ghErr),
			Hint:   "install https://cli.github.com or set github.gh_path; lifecycle commands work without it, connections do not",
		})
	} else {
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		version, err := p.gh.Version(vctx)
		cancel()
		d := providers.Diagnostic{Name: "gh cli", OK: err == nil, Detail: p.gh.Path()}
		if err != nil {
			d.Detail = fmt.Sprintf("%s: %v", p.gh.Path(), err)
		} else if version != "" {
			d.Detail = fmt.Sprintf("%s (%s)", p.gh.Path(), version)
		}
		out = append(out, d)
	}

	// 2. Token: a real API call, so an invalid token is caught here.
	uctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	login, err := p.api.currentUser(uctx)
	cancel()
	tokenDiag := providers.Diagnostic{
		Name:   "github token",
		OK:     err == nil,
		Detail: fmt.Sprintf("source: %s", p.tokenSrc),
	}
	switch {
	case err == nil:
		tokenDiag.Detail = fmt.Sprintf("source: %s, authenticated as %s", p.tokenSrc, login)
	case errors.Is(err, providers.ErrAuth):
		tokenDiag.Detail = fmt.Sprintf("source: %s, rejected by GitHub: %v", p.tokenSrc, err)
		tokenDiag.Hint = "create a token with the \"codespace\" scope, or run `gh auth login`"
	default:
		tokenDiag.Detail = fmt.Sprintf("source: %s, could not be verified: %v", p.tokenSrc, err)
	}
	out = append(out, tokenDiag)

	// 3. Scope check: listing codespaces needs the codespace scope.
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	list, listErr := p.api.listCodespaces(lctx)
	cancel()
	scopeDiag := providers.Diagnostic{Name: "codespace scope", OK: listErr == nil}
	if listErr == nil {
		scopeDiag.Detail = fmt.Sprintf("%d codespace(s) visible", len(list))
	} else {
		scopeDiag.Detail = listErr.Error()
		if errors.Is(listErr, providers.ErrAuth) {
			scopeDiag.Hint = "the token is missing the \"codespace\" scope"
		}
	}
	out = append(out, scopeDiag)

	// 4. The configured default codespace.
	if handle := p.cfg.Codespace; handle == "" {
		hint := "run `gateway codespace select <name>`"
		if p.cfg.Create.Repository != "" {
			hint = "not set; the gateway would create one from " + p.cfg.Create.Repository
		}
		out = append(out, providers.Diagnostic{
			Name: "default codespace", OK: false, Detail: "not configured", Hint: hint,
		})
	} else {
		gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		env, err := p.Get(gctx, handle)
		cancel()
		d := providers.Diagnostic{Name: "default codespace", OK: err == nil}
		switch {
		case err == nil:
			d.Detail = fmt.Sprintf("%s resolves to %s (state %s, github state %s)",
				handle, env.ID, env.State, env.NativeState)
			if env.IdleTimeout > 0 {
				d.Detail += fmt.Sprintf(", idle timeout %s", env.IdleTimeout)
			}
		case errors.Is(err, providers.ErrNotFound):
			d.Detail = fmt.Sprintf("%s 不存在", handle)
			switch {
			case len(list) == 1:
				d.OK = true
				d.Detail += fmt.Sprintf("；会自动改用账号里唯一的 %s", list[0].Name)
				d.Hint = fmt.Sprintf("想固定下来： gateway codespace select %s", list[0].Name)
			case p.cfg.Create.Repository != "":
				d.OK = true
				d.Detail += fmt.Sprintf("；首次连接时会从 %s 创建", p.cfg.Create.Repository)
			case len(list) > 1:
				d.Hint = "账号里有多个 codespace：gateway codespace select <name>"
			default:
				d.Hint = "设置 github.create.repository 允许自动创建，或先建一个 codespace"
			}
		default:
			d.Detail = err.Error()
		}
		out = append(out, d)
	}

	// 5. Connection plumbing.
	out = append(out, providers.Diagnostic{
		Name:   "connector",
		OK:     true,
		Detail: fmt.Sprintf("%s (host key policy: %s)", p.connector(), p.cfg.HostKeyPolicy),
	})
	out = append(out, providers.Diagnostic{
		Name:   "gateway key",
		OK:     p.keys != nil,
		Detail: fmt.Sprintf("%s (%s)", p.keys.PublicPath, fingerprintOf(p.keys)),
		Hint:   "registered with the codespace by `gh codespace ssh -- -i`, separate from the gateway host key",
	})
	return out
}

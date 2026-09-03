package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/session"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
	sshsrv "github.com/AnnaofArendelle/codespace-ssh-gateway/ssh"
)

// OpenSession implements ssh.Backend: it is the whole "ssh in, environment out"
// path. The SSH layer knows nothing about how this happens.
func (g *Gateway) OpenSession(ctx context.Context, req sshsrv.OpenRequest) (*sshsrv.OpenResult, error) {
	// Say something before the first network call: on a slow link, resolving the
	// target can take a while and silence looks like a hang.
	req.Connect.Notify("正在准备 codespace…")

	handle, err := g.resolveEnvironment(ctx, req.EnvironmentHint, req.Connect.Progress)
	if err != nil {
		return nil, err
	}

	kind := "shell"
	switch {
	case req.Connect.Subsystem != "":
		kind = "subsystem:" + req.Connect.Subsystem
	case req.Connect.Command != "":
		kind = "exec"
	}

	sess, err := g.sess.Open(session.Info{
		User:          req.User,
		Environment:   handle,
		RemoteAddr:    req.RemoteAddr,
		ClientVersion: req.ClientVersion,
		PTY:           req.Connect.PTY,
		Kind:          kind,
		Phase:         "preparing",
	})
	if err != nil {
		return nil, err
	}

	log := g.log.With(
		slog.String("session", sess.ID()),
		slog.String("environment", handle),
		slog.String("user", req.User))
	log.Info("preparing environment",
		slog.String("kind", kind),
		slog.String("key", req.KeyFingerprint))

	conn, env, err := g.life.Connect(ctx, handle, req.Connect)
	if err != nil {
		g.sess.Close(sess)
		return nil, g.userFacing(handle, err)
	}

	sess.SetPhase("connected")
	g.life.OnSessionOpened(handle)

	released := false
	return &sshsrv.OpenResult{
		Conn:        conn,
		Environment: env.ID,
		Release: func() {
			if released {
				return
			}
			released = true
			g.sess.Close(sess)
			remaining := g.sess.CountFor(handle)
			log.Info("session released", slog.Int("remaining_sessions", remaining))
			g.life.OnSessionClosed(handle, remaining)
		},
	}, nil
}

func (g *Gateway) resolveEnvironment(ctx context.Context, hint string, notify func(string)) (string, error) {
	handle, err := ResolveEnvironment(ctx, g.prov, hint, notify)
	if err != nil {
		return "", err
	}
	if hint == "" && g.prov.DefaultEnvironment() == "" {
		g.log.Info("using the only environment this account has",
			slog.String("environment", handle))
	}
	return handle, nil
}

// DefaultEnvironmentName is the handle used when nothing is configured and the
// provider has to create the environment. It doubles as the display name, so
// `ssh root@codespace` keeps reaching the same one afterwards.
const DefaultEnvironmentName = "codespace"

// ResolveEnvironment picks the target: what the caller asked for, else the
// configured default, else the only environment the account has. That last step
// is what lets a config with nothing but a token work.
func ResolveEnvironment(ctx context.Context, prov providers.Provider, hint string, notify func(string)) (string, error) {
	note := func(msg string) {
		if notify != nil {
			notify(msg)
		}
	}
	hint = strings.TrimSpace(hint)
	if hint != "" {
		if err := validEnvironmentName(hint); err != nil {
			return "", err
		}
		return hint, nil
	}
	if def := prov.DefaultEnvironment(); def != "" {
		return def, nil
	}

	note("正在查询你账号下的 codespace…")
	envs, err := prov.List(ctx)
	if err != nil {
		return "", fmt.Errorf("没有配置目标 codespace，列举也失败了：%w", err)
	}
	switch len(envs) {
	case 1:
		return envs[0].ID, nil
	case 0:
		// Nothing exists yet. If the provider can create environments, hand back
		// a stable handle and let the lifecycle create it on the way in.
		if prov.Capabilities().Create {
			return DefaultEnvironmentName, nil
		}
		return "", errors.New("这个账号还没有 codespace：先去 github.com/codespaces 建一个，" +
			"或者设置 github.create.repository 让 gateway 自动创建")
	default:
		names := make([]string, 0, len(envs))
		for _, e := range envs {
			names = append(names, e.ID)
		}
		return "", fmt.Errorf("有多个 codespace（%s）：请设置 github.codespace，"+
			"或者用 root+<名字>@gateway 连接", strings.Join(names, ", "))
	}
}

// validEnvironmentName keeps obviously bogus handles out of provider calls.
func validEnvironmentName(name string) error {
	if len(name) > 200 {
		return errors.New("environment name is too long")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ' ':
		default:
			return fmt.Errorf("environment name %q contains unsupported characters", name)
		}
	}
	return nil
}

// userFacing turns an internal error into something worth printing on a client's
// terminal, without leaking anything sensitive.
func (g *Gateway) userFacing(handle string, err error) error {
	msg := g.redact.Redact(err.Error())
	switch {
	case errors.Is(err, providers.ErrAuth):
		return fmt.Errorf("gateway 无法通过 %s 认证：%s", g.prov.Name(), msg)
	case errors.Is(err, providers.ErrNotFound):
		return fmt.Errorf("codespace %q 不存在，也无法创建：%s", handle, msg)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("准备 codespace %q 超时：%s", handle, msg)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("准备 codespace %q 被取消：%s", handle, msg)
	case errors.Is(err, session.ErrTooManySessions), errors.Is(err, session.ErrTooManySessionsForEnv):
		return err
	default:
		return errors.New(msg)
	}
}

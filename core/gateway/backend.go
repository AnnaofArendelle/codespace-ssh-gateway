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
	handle, err := g.resolveEnvironment(req.EnvironmentHint)
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

// resolveEnvironment picks the target: what the client asked for, else the
// provider's configured default.
func (g *Gateway) resolveEnvironment(hint string) (string, error) {
	hint = strings.TrimSpace(hint)
	if hint != "" {
		if err := validEnvironmentName(hint); err != nil {
			return "", err
		}
		return hint, nil
	}
	if def := g.prov.DefaultEnvironment(); def != "" {
		return def, nil
	}
	return "", fmt.Errorf("this gateway has no default environment configured; " +
		"run `gateway codespace select <name>` or connect as user+environment")
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
		return fmt.Errorf("the gateway could not authenticate to %s: %s", g.prov.Name(), msg)
	case errors.Is(err, providers.ErrNotFound):
		return fmt.Errorf("environment %q does not exist and could not be created: %s", handle, msg)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("timed out preparing environment %q: %s", handle, msg)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("preparing environment %q was cancelled: %s", handle, msg)
	case errors.Is(err, session.ErrTooManySessions), errors.Is(err, session.ErrTooManySessionsForEnv):
		return err
	default:
		return errors.New(msg)
	}
}

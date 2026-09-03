package github

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"

	gossh "golang.org/x/crypto/ssh"
)

// connectStdio runs `gh codespace ssh --stdio`, which proxies the codespace's
// SSH server to the child's stdin/stdout, and speaks SSH over it. This is the
// same transport gh's own `--config` output hands to OpenSSH as a ProxyCommand.
func (p *Provider) connectStdio(ctx context.Context, id string, req providers.ConnectRequest) (providers.Conn, error) {
	user, cached, err := p.sshUserFor(ctx, id, req)
	if err != nil {
		return nil, err
	}

	req.Notify("正在打开 codespace 隧道（gh codespace ssh --stdio）")
	procCtx, cancelProc := context.WithCancel(context.WithoutCancel(ctx))
	established := false
	defer func() {
		if !established {
			cancelProc()
		}
	}()

	cmd := p.gh.command(procCtx, "codespace", "ssh", "-c", id, "--stdio", "--", "-i", p.keys.PrivatePath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	ghErr := newSafeBuffer(32 << 10)
	cmd.Stderr = ghErr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start gh codespace ssh: %w", err)
	}
	proc := watchProcess(cmd)

	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(p.keys.Signer)},
		HostKeyCallback: p.hostKeys.callback(id),
		Timeout:         30 * time.Second,
	}
	type handshake struct {
		client *gossh.Client
		err    error
	}
	hs := make(chan handshake, 1)
	go func() {
		conn := &pipeConn{r: stdout, w: stdin, name: id}
		sc, chans, reqs, err := gossh.NewClientConn(conn, "codespace:"+id, cfg)
		if err != nil {
			hs <- handshake{err: err}
			return
		}
		hs <- handshake{client: gossh.NewClient(sc, chans, reqs)}
	}()

	var client *gossh.Client
	// Opening the tunnel is the slowest step of a cold start (gh has to reach
	// the codespace and start sshd inside it), so keep the client informed.
	tunnelDone := make(chan struct{})
	defer close(tunnelDone)
	go func() {
		started := time.Now()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-tunnelDone:
				return
			case <-ticker.C:
				req.Notify(fmt.Sprintf("隧道还在建立中（已用 %s）",
					time.Since(started).Round(time.Second)))
			}
		}
	}()

	select {
	case r := <-hs:
		if r.err != nil {
			return nil, p.stdioFailure(id, user, r.err, ghErr, cached)
		}
		client = r.client
	case <-proc.done:
		return nil, p.stdioFailure(id, user,
			fmt.Errorf("gh exited before the tunnel was ready: %v", proc.err), ghErr, cached)
	case <-ctx.Done():
		return nil, providers.Temporaryf("opening tunnel to codespace %s: %w", id, ctx.Err())
	}

	sess, err := p.startSession(client, req)
	if err != nil {
		client.Close()
		return nil, p.stdioFailure(id, user, err, ghErr, cached)
	}

	established = true
	conn := &stdioConn{
		name:       id,
		user:       user,
		cmd:        cmd,
		cancelProc: cancelProc,
		proc:       proc,
		client:     client,
		sess:       sess.sess,
		stdin:      sess.stdin,
		stdout:     sess.stdout,
		stderr:     sess.stderr,
		ghErr:      ghErr,
		log:        p.log,
	}
	return conn, nil
}

// stdioFailure turns a failed attempt into a well-classified error, including
// whatever gh reported on stderr.
func (p *Provider) stdioFailure(id, user string, err error, ghErr *safeBuffer, usedCachedUser bool) error {
	detail := p.gh.scrub(ghErr.Tail(3))
	if strings.Contains(detail, "unknown flag: --stdio") ||
		strings.Contains(detail, "unknown flag: `--stdio`") {
		return fmt.Errorf("%w: %s", errStdioUnsupported, detail)
	}

	wrapped := err
	if detail != "" {
		wrapped = fmt.Errorf("%w (gh: %s)", err, detail)
	}

	// A rejected key usually means the cached remote username is stale (the
	// codespace was rebuilt with a different remoteUser). Drop the cache and let
	// the caller retry, which re-probes gh for the right user.
	if usedCachedUser && isAuthFailure(err) {
		p.users.forget(id)
		p.log.Warn("codespace rejected the gateway key; forgetting cached ssh user",
			slog.String("codespace", id))
		return providers.Temporaryf("authenticating to codespace %s: %w", id, wrapped)
	}
	if isAuthFailure(err) {
		return fmt.Errorf("codespace %s rejected the gateway key for remote user %q "+
			"(set github.ssh_user if gh reports the wrong user): %w", id, user, wrapped)
	}
	return classifyConnectError(fmt.Errorf("connect to codespace %s: %w", id, wrapped))
}

func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(msg, "no supported methods remain") ||
		strings.Contains(msg, "handshake failed: ssh: unable to authenticate")
}

type startedSession struct {
	sess   *gossh.Session
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

// startSession applies the client's pty/env requests and starts the right kind
// of remote session.
func (p *Provider) startSession(client *gossh.Client, req providers.ConnectRequest) (*startedSession, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open ssh session: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			sess.Close()
		}
	}()

	// Client-requested environment goes to the remote session only. It is never
	// applied to the gateway's own process environment.
	for k, v := range req.Env {
		if err := sess.Setenv(k, v); err != nil {
			p.log.Debug("remote refused env var", slog.String("name", k))
		}
	}
	if req.PTY {
		if err := sess.RequestPty(termOr(req.Term), dimOr(req.Rows, 24), dimOr(req.Cols, 80),
			parseTerminalModes(req.Modes)); err != nil {
			return nil, fmt.Errorf("request pty: %w", err)
		}
	}

	stdin, err := sess.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr io.Reader
	if !req.PTY {
		// With a pty the remote merges stderr into stdout.
		if stderr, err = sess.StderrPipe(); err != nil {
			return nil, err
		}
	}

	switch {
	case req.Subsystem != "":
		err = sess.RequestSubsystem(req.Subsystem)
	case req.Command != "":
		err = sess.Start(req.Command)
	default:
		err = sess.Shell()
	}
	if err != nil {
		return nil, fmt.Errorf("start remote session: %w", err)
	}
	ok = true
	return &startedSession{sess: sess, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

// stdioConn is a live session over the gh stdio tunnel.
type stdioConn struct {
	name       string
	user       string
	cmd        *exec.Cmd
	cancelProc context.CancelFunc
	proc       *procWatch
	client     *gossh.Client
	sess       *gossh.Session
	stdin      io.WriteCloser
	stdout     io.Reader
	stderr     io.Reader
	ghErr      *safeBuffer
	log        *slog.Logger

	waitOnce  sync.Once
	status    providers.ExitStatus
	waitErr   error
	closeOnce sync.Once
}

func (c *stdioConn) Stdin() io.WriteCloser { return c.stdin }
func (c *stdioConn) Stdout() io.Reader     { return c.stdout }
func (c *stdioConn) Stderr() io.Reader     { return c.stderr }

func (c *stdioConn) Resize(rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return nil
	}
	return c.sess.WindowChange(int(rows), int(cols))
}

func (c *stdioConn) Signal(name string) error {
	sig := gossh.Signal(strings.TrimPrefix(strings.ToUpper(name), "SIG"))
	return c.sess.Signal(sig)
}

func (c *stdioConn) Wait() (providers.ExitStatus, error) {
	c.waitOnce.Do(func() {
		err := c.sess.Wait()
		switch e := err.(type) {
		case nil:
			c.status = providers.ExitStatus{Code: 0}
		case *gossh.ExitError:
			c.status = providers.ExitStatus{Code: e.ExitStatus(), Signal: e.Signal()}
		case *gossh.ExitMissingError:
			c.status = providers.ExitStatus{Code: 255}
			c.waitErr = fmt.Errorf("remote session ended without an exit status")
		default:
			c.status = providers.ExitStatus{Code: 255}
			c.waitErr = err
		}
	})
	return c.status, c.waitErr
}

func (c *stdioConn) Close() error {
	c.closeOnce.Do(func() {
		if c.sess != nil {
			_ = c.sess.Close()
		}
		if c.client != nil {
			_ = c.client.Close()
		}
		c.cancelProc()
		select {
		case <-c.proc.done:
		case <-time.After(3 * time.Second):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
		}
		if detail := strings.TrimSpace(c.ghErr.Tail(2)); detail != "" {
			c.log.Debug("gh stderr", slog.String("codespace", c.name), slog.String("detail", detail))
		}
	})
	return nil
}

func (c *stdioConn) Describe() string {
	return fmt.Sprintf("gh codespace ssh --stdio (ssh %s@%s)", c.user, c.name)
}

package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// errNoPTY is returned when the exec connector is asked for an interactive
// session on a platform without pty support in this build.
var errNoPTY = errors.New("the exec connector needs a local pty, which this build does not provide; " +
	"set github.connector: stdio")

// connectExec runs the plain, documented `gh codespace ssh -c <name>` and wires
// the gateway session to it. When the client asked for a terminal the child runs
// on a local pty, because gh delegates to the real ssh client.
//
// Compared with the stdio connector this path cannot report the remote exit
// status (gh reports its own) and cannot forward SSH signals, so it is a
// fallback rather than the default.
func (p *Provider) connectExec(ctx context.Context, id string, req providers.ConnectRequest) (providers.Conn, error) {
	if req.PTY && !ptySupported() {
		return nil, errNoPTY
	}

	args := []string{"codespace", "ssh", "-c", id, "--", "-i", p.keys.PrivatePath}
	if req.PTY {
		args = append(args, "-tt")
	} else {
		args = append(args, "-T")
	}
	// Client-requested environment is passed as literal command-line values, so
	// nothing the client sends can enter the gateway's own process environment.
	for _, kv := range safeSetEnv(req.Env) {
		args = append(args, "-o", "SetEnv="+kv)
	}
	switch {
	case req.Subsystem != "":
		args = append(args, "-s", req.Subsystem)
	case req.Command != "":
		args = append(args, req.Command)
	}

	req.Notify("正在打开 codespace 会话（gh codespace ssh）")
	procCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	established := false
	defer func() {
		if !established {
			cancel()
		}
	}()

	cmd := p.gh.command(procCtx, args...)
	conn := &execConn{name: id, cmd: cmd, cancel: cancel, log: p.log, pty: req.PTY}

	if req.PTY {
		master, slave, err := openPTY()
		if err != nil {
			return nil, err
		}
		conn.master, conn.slave = master, slave
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.SysProcAttr = ptyProcAttr()
		if req.Rows > 0 && req.Cols > 0 {
			_ = setWinsize(master, req.Rows, req.Cols)
		}
		conn.stdin, conn.stdout = master, master
	} else {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, err
		}
		conn.stdin, conn.stdout, conn.stderr = stdin, stdout, stderr
	}

	if err := cmd.Start(); err != nil {
		conn.closeFiles()
		return nil, fmt.Errorf("start gh codespace ssh: %w", err)
	}
	if conn.slave != nil {
		// The child owns the slave end now.
		conn.slave.Close()
		conn.slave = nil
	}
	conn.proc = watchProcess(cmd)

	// Fail fast if gh dies immediately (bad codespace, missing gh auth, ...).
	select {
	case <-conn.proc.done:
		return nil, classifyConnectError(fmt.Errorf("gh codespace ssh exited immediately: %v", conn.proc.err))
	case <-time.After(300 * time.Millisecond):
	}

	established = true
	return conn, nil
}

var safeEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// safeSetEnv renders client environment requests as NAME=VALUE strings, keeping
// out anything that could confuse the ssh config parser.
func safeSetEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		if !safeEnvName.MatchString(k) {
			continue
		}
		if strings.ContainsAny(v, "\n\r\"'\\ \t") {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}

// execConn is a session backed by a `gh codespace ssh` child process.
type execConn struct {
	name   string
	cmd    *exec.Cmd
	cancel context.CancelFunc
	proc   *procWatch
	log    *slog.Logger
	pty    bool

	master *os.File
	slave  *os.File
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader

	closeOnce sync.Once
}

func (c *execConn) Stdin() io.WriteCloser { return c.stdin }
func (c *execConn) Stdout() io.Reader     { return c.stdout }
func (c *execConn) Stderr() io.Reader     { return c.stderr }

func (c *execConn) Resize(rows, cols uint16) error {
	if c.master == nil {
		return providers.ErrNotSupported
	}
	if rows == 0 || cols == 0 {
		return nil
	}
	return setWinsize(c.master, rows, cols)
}

// Signal is not supported: the local ssh client owns the connection, and a pty
// session carries control characters through the data stream instead.
func (c *execConn) Signal(string) error { return providers.ErrNotSupported }

func (c *execConn) Wait() (providers.ExitStatus, error) {
	<-c.proc.done
	code := 0
	if st := c.cmd.ProcessState; st != nil {
		code = st.ExitCode()
	}
	if code < 0 {
		code = 255
	}
	return providers.ExitStatus{Code: code}, nil
}

func (c *execConn) closeFiles() {
	if c.master != nil {
		c.master.Close()
		c.master = nil
	}
	if c.slave != nil {
		c.slave.Close()
		c.slave = nil
	}
}

func (c *execConn) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		if c.proc != nil {
			select {
			case <-c.proc.done:
			case <-time.After(3 * time.Second):
				if c.cmd.Process != nil {
					_ = c.cmd.Process.Kill()
				}
			}
		}
		c.closeFiles()
	})
	return nil
}

func (c *execConn) Describe() string {
	if c.pty {
		return fmt.Sprintf("gh codespace ssh -c %s (local pty)", c.name)
	}
	return fmt.Sprintf("gh codespace ssh -c %s (pipes)", c.name)
}

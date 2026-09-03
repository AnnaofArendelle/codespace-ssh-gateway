package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"

	gossh "golang.org/x/crypto/ssh"
)

// errStdioUnsupported means this gh build has no `--stdio`, so the exec
// connector must be used instead.
var errStdioUnsupported = errors.New("gh codespace ssh --stdio is not supported by this gh version")

// Connect opens a session inside the codespace using the official GitHub CLI as
// the transport. Two backends are available and both run `gh codespace ssh`:
//
//   - stdio: `gh codespace ssh --stdio` proxies the codespace's sshd to a pipe,
//     over which the gateway speaks SSH directly. This is the mechanism gh's own
//     `--config` output uses for OpenSSH, and it gives exact exit codes, window
//     resizing and subsystems.
//   - exec: `gh codespace ssh` is run on a local pty, which is the plain
//     documented invocation, used when --stdio is unavailable.
func (p *Provider) Connect(ctx context.Context, id string, req providers.ConnectRequest) (providers.Conn, error) {
	if !p.gh.Available() {
		return nil, fmt.Errorf("cannot connect to codespace %s: %w", id, errGHMissing)
	}
	switch p.connector() {
	case ConnectorStdio:
		return p.connectStdio(ctx, id, req)
	case ConnectorExec:
		return p.connectExec(ctx, id, req)
	default:
		conn, err := p.connectStdio(ctx, id, req)
		if err == nil || !errors.Is(err, errStdioUnsupported) {
			return conn, err
		}
		p.log.Warn("gh does not support --stdio; falling back to the exec connector")
		p.setConnector(ConnectorExec)
		return p.connectExec(ctx, id, req)
	}
}

// pipeConn adapts a child process's stdin/stdout to net.Conn so that the
// standard SSH client can run over it, exactly as OpenSSH does with a
// ProxyCommand. Deadlines are no-ops: establishment is bounded by context.
type pipeConn struct {
	r    io.Reader
	w    io.WriteCloser
	name string
}

func (c *pipeConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *pipeConn) Write(b []byte) (int, error) { return c.w.Write(b) }
func (c *pipeConn) Close() error                { return c.w.Close() }

type pipeAddr string

func (a pipeAddr) Network() string { return "gh-stdio" }
func (a pipeAddr) String() string  { return string(a) }

func (c *pipeConn) LocalAddr() net.Addr              { return pipeAddr("gateway") }
func (c *pipeConn) RemoteAddr() net.Addr             { return pipeAddr(c.name) }
func (c *pipeConn) SetDeadline(time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(time.Time) error { return nil }

// parseTerminalModes decodes the encoded terminal modes from an SSH pty-req so
// they can be replayed on the inner session. The wire format is a sequence of
// (opcode byte, uint32 argument) pairs terminated by opcode 0.
func parseTerminalModes(raw []byte) gossh.TerminalModes {
	modes := gossh.TerminalModes{}
	for len(raw) >= 5 {
		op := raw[0]
		if op == 0 {
			break
		}
		val := uint32(raw[1])<<24 | uint32(raw[2])<<16 | uint32(raw[3])<<8 | uint32(raw[4])
		modes[op] = val
		raw = raw[5:]
	}
	if len(modes) == 0 {
		modes = gossh.TerminalModes{
			gossh.ECHO:          1,
			gossh.TTY_OP_ISPEED: 14400,
			gossh.TTY_OP_OSPEED: 14400,
		}
	}
	return modes
}

// temporaryConnectError decides whether a failed connection attempt is worth
// retrying. A codespace routinely accepts API calls a few seconds before its
// SSH server is reachable.
func temporaryConnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errStdioUnsupported) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection refused",
		"connection reset",
		"broken pipe",
		"unexpected eof",
		"eof",
		"tunnel",
		"error connecting to codespace",
		"failed to connect to forwarded port",
		"error getting ssh server details",
		"could not start ssh server",
		"deadline exceeded",
		"timeout",
		"handshake failed",
		"no route to host",
		"i/o timeout",
		"502 bad gateway",
		"503 service unavailable",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func classifyConnectError(err error) error {
	if err == nil {
		return nil
	}
	if temporaryConnectError(err) {
		return providers.Temporary(err)
	}
	return err
}

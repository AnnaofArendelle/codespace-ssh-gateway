package providers

import "io"

// ConnectRequest describes the session the caller wants inside the environment.
// It is filled in from the client's SSH requests and must never be applied to
// the gateway's own process environment.
type ConnectRequest struct {
	PTY       bool
	Term      string
	Rows      uint16
	Cols      uint16
	Modes     []byte            // raw SSH terminal modes, opaque
	Command   string            // as delivered by the SSH exec request; "" means login shell
	Subsystem string            // e.g. "sftp"; mutually exclusive with Command
	Env       map[string]string // client-requested environment, best effort
	// Progress receives short status lines that are safe to show the connecting
	// user ("starting environment ..."). Optional; never given secrets.
	Progress func(string)
}

func (r ConnectRequest) note(msg string) {
	if r.Progress != nil {
		r.Progress(msg)
	}
}

// Notify reports progress to the client, if it asked for it.
func (r ConnectRequest) Notify(msg string) { r.note(msg) }

// ExitStatus is how a remote session ended.
type ExitStatus struct {
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
}

// Conn is a live duplex session inside an environment. Implementations own the
// transport (for GitHub Codespaces: a `gh codespace ssh` child process) and are
// responsible for tearing it down in Close.
type Conn interface {
	// Stdin is the write side of the remote session.
	Stdin() io.WriteCloser
	// Stdout is the read side. With a PTY this carries both streams.
	Stdout() io.Reader
	// Stderr is the remote stderr, or nil when a PTY merges it into Stdout.
	Stderr() io.Reader
	// Resize forwards a window change. Returns ErrNotSupported if the
	// transport cannot express one.
	Resize(rows, cols uint16) error
	// Signal forwards an SSH signal name such as "INT".
	Signal(name string) error
	// Wait blocks until the remote session ends.
	Wait() (ExitStatus, error)
	// Close tears the session and its transport down.
	Close() error
	// Describe names the transport, for logs and `gateway status`.
	Describe() string
}

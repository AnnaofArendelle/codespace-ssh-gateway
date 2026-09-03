package ssh

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"

	gossh "golang.org/x/crypto/ssh"
)

// run opens the environment session and pumps bytes until either side stops.
func (st *sessionState) run(ctx context.Context, kind, command, subsystem string) {
	defer close(st.done)

	pty, term, rows, cols, modes, env := st.snapshot()
	openReq := OpenRequest{
		User:            st.login.User,
		EnvironmentHint: st.environmentHint(),
		RemoteAddr:      st.sc.RemoteAddr().String(),
		ClientVersion:   string(st.sc.ClientVersion()),
		KeyFingerprint:  st.permissionValue("gateway-key"),
		Connect: providers.ConnectRequest{
			PTY:       pty,
			Term:      term,
			Rows:      rows,
			Cols:      cols,
			Modes:     modes,
			Command:   command,
			Subsystem: subsystem,
			Env:       env,
			Progress:  st.notify,
		},
	}

	st.log.Info("session requested",
		slog.String("kind", kind),
		slog.Bool("pty", pty),
		slog.String("environment_hint", openReq.EnvironmentHint))

	res, err := st.srv.backend.OpenSession(ctx, openReq)
	if err != nil {
		st.log.Warn("session could not be opened", slog.Any("error", err))
		st.fail(err)
		return
	}
	st.setConn(res.Conn, res.Release)
	st.log.Info("session open",
		slog.String("environment", res.Environment),
		slog.String("transport", res.Conn.Describe()))

	st.pump(res.Conn)
}

// pump copies the three streams and reports the remote exit status.
func (st *sessionState) pump(conn providers.Conn) {
	var stderrDone sync.WaitGroup

	go func() {
		// The client may keep stdin open for the whole session; this copier
		// simply ends when the channel closes.
		_, _ = io.Copy(conn.Stdin(), st.ch)
		_ = conn.Stdin().Close()
	}()

	if remoteErr := conn.Stderr(); remoteErr != nil {
		stderrDone.Add(1)
		go func() {
			defer stderrDone.Done()
			_, _ = io.Copy(st.ch.Stderr(), remoteErr)
		}()
	}

	_, copyErr := io.Copy(st.ch, conn.Stdout())
	if copyErr != nil {
		st.log.Debug("stdout copy ended", slog.Any("error", copyErr))
	}

	// Give stderr a moment to flush after stdout hits EOF.
	flushed := make(chan struct{})
	go func() {
		stderrDone.Wait()
		close(flushed)
	}()
	select {
	case <-flushed:
	case <-time.After(2 * time.Second):
	}

	status, err := conn.Wait()
	if err != nil {
		st.log.Info("remote session ended with error", slog.Any("error", err))
		if status.Code == 0 && status.Signal == "" {
			status.Code = 255
		}
	}
	st.sendExit(status)
}

func (st *sessionState) permissionValue(key string) string {
	if st.sc.Permissions == nil {
		return ""
	}
	return st.sc.Permissions.Extensions[key]
}

// notify shows a progress line to the connecting user.
func (st *sessionState) notify(msg string) {
	st.mu.Lock()
	quiet, pty := st.quiet, st.pty
	st.mu.Unlock()
	if quiet || msg == "" {
		return
	}
	line := "[gateway] " + msg
	if pty {
		_, _ = st.ch.Write([]byte(line + "\r\n"))
		return
	}
	_, _ = st.ch.Stderr().Write([]byte(line + "\n"))
}

// fail tells the client why it did not get a shell, the same way a real SSH
// server reports a failed exec.
func (st *sessionState) fail(err error) {
	msg := strings.TrimSpace(err.Error())
	st.mu.Lock()
	pty := st.pty
	st.mu.Unlock()
	text := "gateway: " + msg
	if pty {
		_, _ = st.ch.Write([]byte(text + "\r\n"))
	} else {
		_, _ = st.ch.Stderr().Write([]byte(text + "\n"))
	}
	st.sendExit(providers.ExitStatus{Code: 254})
}

func (st *sessionState) sendExit(status providers.ExitStatus) {
	if status.Signal != "" {
		_, _ = st.ch.SendRequest("exit-signal", false, gossh.Marshal(exitSignalMsg{
			Signal: strings.TrimPrefix(status.Signal, "SIG"),
		}))
	} else {
		code := status.Code
		if code < 0 {
			code = 255
		}
		_, _ = st.ch.SendRequest("exit-status", false, gossh.Marshal(exitStatusMsg{Status: uint32(code)}))
	}
	_ = st.ch.CloseWrite()
	_ = st.ch.Close()
}

// teardown closes the environment session and releases the gateway session
// record. It is safe to call once per channel, from either goroutine.
func (st *sessionState) teardown() {
	st.closeOnce.Do(func() {
		st.mu.Lock()
		conn, release, started, cancel := st.conn, st.release, st.started, st.cancel
		st.mu.Unlock()

		// Stop any lifecycle work started for this session. Established
		// sessions are unaffected: providers detach them from the open context.
		if cancel != nil {
			cancel()
		}
		if conn != nil {
			if err := conn.Close(); err != nil {
				st.log.Debug("closing environment session", slog.Any("error", err))
			}
		}
		if started {
			// Let run() finish sending the exit status before we drop the channel.
			select {
			case <-st.done:
			case <-time.After(5 * time.Second):
				st.log.Warn("session teardown timed out waiting for forwarder")
			}
		}
		if release != nil {
			release()
		}
		_ = st.ch.Close()
		st.log.Info("session closed")
	})
}

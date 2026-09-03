package ssh

import (
	"context"
	"log/slog"
	"sync"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"

	gossh "golang.org/x/crypto/ssh"
)

// sessionState is one SSH session channel and the environment session behind it.
type sessionState struct {
	srv   *Server
	sc    *gossh.ServerConn
	ch    gossh.Channel
	log   *slog.Logger
	login Login

	mu         sync.Mutex
	pty        bool
	term       string
	rows, cols uint16
	modes      []byte
	env        map[string]string
	quiet      bool
	started    bool
	conn       providers.Conn
	release    func()

	closeOnce sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

func (s *Server) handleSession(ctx context.Context, sc *gossh.ServerConn, newCh gossh.NewChannel, login Login, log *slog.Logger) {
	ch, reqs, err := newCh.Accept()
	if err != nil {
		log.Warn("could not accept session channel", slog.Any("error", err))
		return
	}

	// A per-session context so that a client hanging up stops the lifecycle
	// work it triggered (the last waiter leaving cancels a shared operation).
	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	st := &sessionState{
		srv: s, sc: sc, ch: ch, log: log, login: login,
		env:    map[string]string{},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	defer st.teardown()

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var m ptyRequestMsg
			if err := gossh.Unmarshal(req.Payload, &m); err != nil {
				st.reply(req, false)
				continue
			}
			st.mu.Lock()
			ok := !st.started
			if ok {
				st.pty, st.term = true, m.Term
				st.rows, st.cols = clampDimension(m.Rows), clampDimension(m.Columns)
				st.modes = []byte(m.Modes)
			}
			st.mu.Unlock()
			st.reply(req, ok)

		case "env":
			var m envMsg
			if err := gossh.Unmarshal(req.Payload, &m); err != nil {
				st.reply(req, false)
				continue
			}
			st.mu.Lock()
			if !st.started {
				st.env[m.Name] = m.Value
			}
			st.mu.Unlock()
			st.reply(req, true)

		case "window-change":
			var m windowChangeMsg
			if err := gossh.Unmarshal(req.Payload, &m); err != nil {
				st.reply(req, false)
				continue
			}
			st.resize(clampDimension(m.Rows), clampDimension(m.Columns))
			st.reply(req, true)

		case "signal":
			var m signalMsg
			if err := gossh.Unmarshal(req.Payload, &m); err == nil {
				st.forwardSignal(m.Signal)
			}
			st.reply(req, true)

		case "shell":
			st.begin(sessCtx, req, "shell", "", "")

		case "exec":
			var m execMsg
			if err := gossh.Unmarshal(req.Payload, &m); err != nil {
				st.reply(req, false)
				continue
			}
			st.begin(sessCtx, req, "exec", m.Command, "")

		case "subsystem":
			var m subsystemMsg
			if err := gossh.Unmarshal(req.Payload, &m); err != nil {
				st.reply(req, false)
				continue
			}
			st.begin(sessCtx, req, "subsystem", "", m.Subsystem)

		default:
			st.reply(req, false)
		}
	}
}

func (st *sessionState) reply(req *gossh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

// begin starts the environment session exactly once per channel.
func (st *sessionState) begin(ctx context.Context, req *gossh.Request, kind, command, subsystem string) {
	st.mu.Lock()
	if st.started {
		st.mu.Unlock()
		st.reply(req, false)
		return
	}
	st.started = true
	st.quiet = subsystem != ""
	st.mu.Unlock()

	// Reply now: opening the environment can take minutes, and OpenSSH expects
	// a prompt answer. A failure is reported the same way a failed exec is: a
	// message on stderr and a non-zero exit status.
	st.reply(req, true)
	go st.run(ctx, kind, command, subsystem)
}

func (st *sessionState) snapshot() (pty bool, term string, rows, cols uint16, modes []byte, env map[string]string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	env = make(map[string]string, len(st.env))
	for k, v := range st.env {
		if k == EnvVarEnvironment {
			continue // gateway control variable, not for the remote shell
		}
		env[k] = v
	}
	return st.pty, st.term, st.rows, st.cols, st.modes, env
}

func (st *sessionState) environmentHint() string {
	if st.login.EnvironmentHint != "" {
		return st.login.EnvironmentHint
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.env[EnvVarEnvironment]
}

func (st *sessionState) setConn(conn providers.Conn, release func()) {
	st.mu.Lock()
	st.conn, st.release = conn, release
	rows, cols := st.rows, st.cols
	st.mu.Unlock()
	if rows > 0 && cols > 0 {
		_ = conn.Resize(rows, cols)
	}
}

func (st *sessionState) currentConn() providers.Conn {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.conn
}

func (st *sessionState) resize(rows, cols uint16) {
	st.mu.Lock()
	st.rows, st.cols = rows, cols
	conn := st.conn
	st.mu.Unlock()
	if conn != nil && rows > 0 && cols > 0 {
		if err := conn.Resize(rows, cols); err != nil {
			st.log.Debug("resize failed", slog.Any("error", err))
		}
	}
}

func (st *sessionState) forwardSignal(name string) {
	if conn := st.currentConn(); conn != nil {
		if err := conn.Signal(name); err != nil {
			st.log.Debug("signal forwarding failed",
				slog.String("signal", name), slog.Any("error", err))
		}
	}
}

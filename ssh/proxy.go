package ssh

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func (s *Server) handleConn(ctx context.Context, nc net.Conn) {
	defer nc.Close()

	if s.cfg.HandshakeTimeout > 0 {
		_ = nc.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	}
	sc, chans, globalReqs, err := gossh.NewServerConn(nc, s.sshCfg)
	if err != nil {
		s.log.Warn("ssh handshake failed",
			slog.String("remote", nc.RemoteAddr().String()), slog.Any("error", err))
		return
	}
	defer sc.Close()

	// No gateway-side session timer: once authenticated the session lives as
	// long as the client keeps it.
	_ = nc.SetDeadline(time.Time{})
	if tc, ok := nc.(*net.TCPConn); ok {
		// Keepalives on the client link only. Nothing is ever sent towards the
		// environment to keep it awake.
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	go gossh.DiscardRequests(globalReqs)

	login := ParseLogin(sc.User())
	log := s.log.With(
		slog.String("remote", sc.RemoteAddr().String()),
		slog.String("user", login.User),
		slog.String("client", string(sc.ClientVersion())))
	log.Info("client connected")
	defer log.Info("client disconnected")

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(gossh.UnknownChannelType,
				fmt.Sprintf("channel type %q is not supported by this gateway", newCh.ChannelType()))
			continue
		}
		s.sessions.Add(1)
		go func(nch gossh.NewChannel) {
			defer s.sessions.Done()
			s.handleSession(ctx, sc, nch, login, log)
		}(newCh)
	}
}

// SSH wire messages we care about (RFC 4254).
type ptyRequestMsg struct {
	Term     string
	Columns  uint32
	Rows     uint32
	WidthPx  uint32
	HeightPx uint32
	Modes    string
}

type windowChangeMsg struct {
	Columns  uint32
	Rows     uint32
	WidthPx  uint32
	HeightPx uint32
}

type envMsg struct {
	Name  string
	Value string
}

type execMsg struct {
	Command string
}

type subsystemMsg struct {
	Subsystem string
}

type signalMsg struct {
	Signal string
}

type exitStatusMsg struct {
	Status uint32
}

type exitSignalMsg struct {
	Signal     string
	CoreDumped bool
	Error      string
	Lang       string
}

func clampDimension(v uint32) uint16 {
	if v > 0xffff {
		return 0xffff
	}
	return uint16(v)
}

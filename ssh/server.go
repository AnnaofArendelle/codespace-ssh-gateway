// Package ssh is the gateway's SSH front door: it authenticates clients and
// forwards their session to whatever the backend hands back. It contains no
// provider-specific logic.
package ssh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// Config configures the SSH server.
type Config struct {
	Listen           string
	HostKeyPath      string
	HandshakeTimeout time.Duration
	ShutdownGrace    time.Duration
	Banner           string
	Auth             AuthConfig
}

// Server serves the gateway's SSH endpoint.
type Server struct {
	cfg     Config
	log     *slog.Logger
	backend Backend
	auth    *Authorizer
	sshCfg  *gossh.ServerConfig
	hostKey gossh.Signer
	newHost bool

	mu       sync.Mutex
	ln       net.Listener
	conns    map[net.Conn]struct{}
	closing  bool
	sessions sync.WaitGroup
}

// New builds a server, loading or creating the host key.
func New(cfg Config, backend Backend, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if backend == nil {
		return nil, errors.New("ssh: backend is required")
	}
	auth, err := NewAuthorizer(cfg.Auth, log)
	if err != nil {
		return nil, err
	}
	signer, created, err := LoadOrCreateHostKey(cfg.HostKeyPath)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg: cfg, log: log, backend: backend, auth: auth,
		hostKey: signer, newHost: created,
		conns: map[net.Conn]struct{}{},
	}

	s.sshCfg = &gossh.ServerConfig{
		// With nothing configured on a loopback listener the gateway accepts any
		// client: no key, no password, no prompt. Anything else authenticates.
		NoClientAuth: auth.Anonymous(),
		PublicKeyCallback: func(c gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			login := ParseLogin(c.User())
			perms, err := auth.AuthenticateKey(login.User, key)
			if err != nil {
				return nil, err
			}
			return perms, nil
		},
		NoClientAuthCallback: func(c gossh.ConnMetadata) (*gossh.Permissions, error) {
			login := ParseLogin(c.User())
			if err := auth.CheckUser(login.User); err != nil {
				return nil, err
			}
			return &gossh.Permissions{Extensions: map[string]string{"gateway-auth": "none"}}, nil
		},
		AuthLogCallback: func(c gossh.ConnMetadata, method string, err error) {
			if err == nil {
				s.log.Info("client authenticated",
					slog.String("user", c.User()),
					slog.String("method", method),
					slog.String("remote", c.RemoteAddr().String()),
					slog.String("client", string(c.ClientVersion())))
				return
			}
			if method == "none" {
				return // every client probes with "none" first
			}
			s.log.Warn("authentication failed",
				slog.String("user", c.User()),
				slog.String("method", method),
				slog.String("remote", c.RemoteAddr().String()),
				slog.Any("error", err))
		},
		ServerVersion: "SSH-2.0-ssh_gateway",
	}
	if auth.PasswordEnabled() {
		s.sshCfg.PasswordCallback = func(c gossh.ConnMetadata, pw []byte) (*gossh.Permissions, error) {
			login := ParseLogin(c.User())
			return auth.AuthenticatePassword(login.User, pw)
		}
	}
	if cfg.Banner != "" {
		banner := cfg.Banner
		if !hasTrailingNewline(banner) {
			banner += "\r\n"
		}
		s.sshCfg.BannerCallback = func(gossh.ConnMetadata) string { return banner }
	}
	s.sshCfg.AddHostKey(signer)
	return s, nil
}

func hasTrailingNewline(s string) bool {
	return len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r')
}

// Authorizer exposes the authentication state for status output.
func (s *Server) Authorizer() *Authorizer { return s.auth }

// HostKeyFingerprint is the SHA256 fingerprint clients will pin.
func (s *Server) HostKeyFingerprint() string { return Fingerprint(s.hostKey.PublicKey()) }

// HostKeyCreated reports whether the host key was generated on this start.
func (s *Server) HostKeyCreated() bool { return s.newHost }

// Listen binds the socket. Doing this before Serve lets `gateway start` fail
// fast on a port clash.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Listen, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	return nil
}

// Addr is the bound address, or nil before Listen.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Serve accepts connections until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		if err := s.Listen(); err != nil {
			return err
		}
		s.mu.Lock()
		ln = s.ln
		s.mu.Unlock()
	}

	go func() {
		<-ctx.Done()
		s.shutdown()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.isClosing() {
				s.drain()
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return fmt.Errorf("accept: %w", err)
		}
		s.track(conn, true)
		go func() {
			defer s.track(conn, false)
			s.handleConn(ctx, conn)
		}()
	}
}

func (s *Server) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

func (s *Server) track(c net.Conn, add bool) {
	s.mu.Lock()
	if add {
		s.conns[c] = struct{}{}
	} else {
		delete(s.conns, c)
	}
	s.mu.Unlock()
}

func (s *Server) shutdown() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
}

// drain waits for live sessions, then force-closes anything left.
func (s *Server) drain() {
	done := make(chan struct{})
	go func() {
		s.sessions.Wait()
		close(done)
	}()
	grace := s.cfg.ShutdownGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	select {
	case <-done:
	case <-time.After(grace):
		s.log.Warn("shutdown grace expired, closing live connections")
	}
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
}

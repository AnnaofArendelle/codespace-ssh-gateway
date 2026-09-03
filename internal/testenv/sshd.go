package testenv

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"sync"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

// SSHSession records what a codespace's SSH server was asked to do.
type SSHSession struct {
	PTY           bool
	Term          string
	Rows, Cols    uint32
	Env           map[string]string
	Kind          string // shell | exec | subsystem
	Command       string
	WindowChanges [][2]uint32
	Signals       []string
}

// SSHD is a stand-in for the SSH server inside a codespace. It speaks real SSH:
// the gateway's client code, key handling and stream forwarding are exercised
// exactly as they would be against a real one.
type SSHD struct {
	Addr string

	ln      net.Listener
	hostKey gossh.Signer

	mu          sync.Mutex
	sessions    []*SSHSession
	offeredKeys []string
	refuse      int
	closed      bool
}

// NewSSHD starts a fake codespace SSH server on localhost.
func NewSSHD(t *testing.T) *SSHD {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &SSHD{Addr: ln.Addr().String(), ln: ln, hostKey: signer}
	go s.accept()
	t.Cleanup(s.Close)
	return s
}

// RefuseNext makes the next n connections fail, as a codespace whose sshd is
// not up yet would.
func (s *SSHD) RefuseNext(n int) {
	s.mu.Lock()
	s.refuse = n
	s.mu.Unlock()
}

// Sessions returns the recorded sessions.
func (s *SSHD) Sessions() []*SSHSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*SSHSession(nil), s.sessions...)
}

// LastSession returns the most recent session, or nil.
func (s *SSHD) LastSession() *SSHSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) == 0 {
		return nil
	}
	return s.sessions[len(s.sessions)-1]
}

// OfferedKeys returns the fingerprints of the public keys clients authenticated
// with, so a test can prove the gateway used its own key.
func (s *SSHD) OfferedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.offeredKeys...)
}

// HostKeyFingerprint is the fingerprint clients should pin.
func (s *SSHD) HostKeyFingerprint() string {
	return gossh.FingerprintSHA256(s.hostKey.PublicKey())
}

func (s *SSHD) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.ln.Close()
}

func (s *SSHD) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.refuse > 0 {
			s.refuse--
			s.mu.Unlock()
			conn.Close()
			continue
		}
		s.mu.Unlock()
		go s.serve(conn)
	}
}

func (s *SSHD) serve(nc net.Conn) {
	defer nc.Close()
	cfg := &gossh.ServerConfig{
		PublicKeyCallback: func(_ gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			s.mu.Lock()
			s.offeredKeys = append(s.offeredKeys, gossh.FingerprintSHA256(key))
			s.mu.Unlock()
			return &gossh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(s.hostKey)

	sc, chans, reqs, err := gossh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go gossh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(gossh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(ch, chReqs)
	}
}

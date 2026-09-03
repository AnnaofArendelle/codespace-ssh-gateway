package github

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	gossh "golang.org/x/crypto/ssh"
)

// keyPair is the gateway's own credential for the gateway -> codespace hop.
//
// It is deliberately distinct from the gateway's SSH host key and from the
// operator's personal keys: clients authenticate to the gateway with their own
// keys, and the gateway authenticates to the codespace with this one. The
// public half is handed to `gh codespace ssh -- -i <key>`, which registers it
// with the codespace's SSH server through GitHub's own RPC.
type keyPair struct {
	PrivatePath string
	PublicPath  string
	Signer      gossh.Signer
}

func ensureKeyPair(dir string) (*keyPair, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	priv := filepath.Join(dir, "codespace_ed25519")
	pub := priv + ".pub"

	if raw, err := os.ReadFile(priv); err == nil {
		signer, err := gossh.ParsePrivateKey(raw)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", priv, err)
		}
		if _, err := os.Stat(pub); err != nil {
			// Regenerate the public half from the private key: gh needs the file.
			if err := writePublicKey(pub, signer.PublicKey()); err != nil {
				return nil, err
			}
		}
		return &keyPair{PrivatePath: priv, PublicPath: pub, Signer: signer}, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := gossh.MarshalPrivateKey(key, "ssh-gateway codespace key")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(priv, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", priv, err)
	}
	if _, err := f.Write(pem.EncodeToMemory(block)); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	signer, err := gossh.NewSignerFromKey(key)
	if err != nil {
		return nil, err
	}
	if err := writePublicKey(pub, signer.PublicKey()); err != nil {
		return nil, err
	}
	return &keyPair{PrivatePath: priv, PublicPath: pub, Signer: signer}, nil
}

func writePublicKey(path string, key gossh.PublicKey) error {
	line := gossh.MarshalAuthorizedKey(key) // ends with \n
	return os.WriteFile(path, line, 0o600)
}

// hostKeyStore verifies the codespace's SSH host key on the inner hop.
//
// The transport itself is a GitHub-issued tunnel to one specific codespace, but
// pinning the host key is cheap defence in depth. The default policy is
// trust-on-first-use: nothing is silently accepted twice.
type hostKeyStore struct {
	path   string
	policy string
	log    *slog.Logger
	mu     sync.Mutex
}

func newHostKeyStore(dir, policy string, log *slog.Logger) *hostKeyStore {
	return &hostKeyStore{path: filepath.Join(dir, "known_codespaces"), policy: policy, log: log}
}

// callback returns a HostKeyCallback bound to one codespace name.
func (s *hostKeyStore) callback(name string) gossh.HostKeyCallback {
	return func(_ string, _ net.Addr, key gossh.PublicKey) error {
		return s.check(name, key)
	}
}

func (s *hostKeyStore) check(name string, key gossh.PublicKey) error {
	fp := gossh.FingerprintSHA256(key)
	if s.policy == PolicyInsecure {
		s.log.Warn("accepting codespace host key without verification (host_key_policy: insecure)",
			slog.String("codespace", name), slog.String("fingerprint", fp))
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	known, err := s.load()
	if err != nil {
		return err
	}
	want := gossh.MarshalAuthorizedKey(key)
	if existing, ok := known[name]; ok {
		if subtle.ConstantTimeCompare([]byte(existing), want) == 1 {
			return nil
		}
		return fmt.Errorf("host key for codespace %s changed (now %s). If the codespace was rebuilt this is "+
			"expected: run `gateway codespace forget-host-key %s` and reconnect", name, fp, name)
	}
	if s.policy == PolicyStrict {
		return fmt.Errorf("codespace %s has no pinned host key and host_key_policy is strict (fingerprint %s)", name, fp)
	}
	if err := s.appendLocked(name, want); err != nil {
		return err
	}
	s.log.Info("pinned codespace host key on first use",
		slog.String("codespace", name), slog.String("fingerprint", fp))
	return nil
}

func (s *hostKeyStore) load() (map[string]string, error) {
	out := map[string]string{}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, keyPart, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		out[name] = strings.TrimSpace(keyPart) + "\n"
	}
	return out, nil
}

func (s *hostKeyStore) appendLocked(name string, key []byte) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s %s", name, strings.TrimSpace(string(key))+"\n")
	return err
}

// Forget drops the pinned host key for one codespace (or all of them).
func (s *hostKeyStore) Forget(name string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	known, err := s.load()
	if err != nil {
		return 0, err
	}
	removed := 0
	var b strings.Builder
	for k, v := range known {
		if name == "" || k == name {
			removed++
			continue
		}
		fmt.Fprintf(&b, "%s %s", k, v)
	}
	if removed == 0 {
		return 0, nil
	}
	if b.Len() == 0 {
		return removed, os.Remove(s.path)
	}
	return removed, os.WriteFile(s.path, []byte(b.String()), 0o600)
}

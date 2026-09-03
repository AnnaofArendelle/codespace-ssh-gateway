package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	gossh "golang.org/x/crypto/ssh"
)

// LoadOrCreateHostKey returns the gateway's SSH host key, generating a new
// ed25519 key on first run. The key is persisted (mode 0600) so clients do not
// see a changing host key, which would look exactly like an attack.
func LoadOrCreateHostKey(path string) (signer gossh.Signer, created bool, err error) {
	raw, readErr := os.ReadFile(path)
	if readErr == nil {
		signer, err = gossh.ParsePrivateKey(raw)
		if err != nil {
			return nil, false, fmt.Errorf("parse host key %s: %w", path, err)
		}
		return signer, false, nil
	}
	if !os.IsNotExist(readErr) {
		return nil, false, fmt.Errorf("read host key %s: %w", path, readErr)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, err
	}
	block, err := gossh.MarshalPrivateKey(priv, "ssh-gateway host key")
	if err != nil {
		return nil, false, err
	}
	// O_EXCL: never clobber a key another process just wrote.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("create host key %s: %w", path, err)
	}
	if _, err := f.Write(pem.EncodeToMemory(block)); err != nil {
		f.Close()
		return nil, false, err
	}
	if err := f.Close(); err != nil {
		return nil, false, err
	}
	signer, err = gossh.NewSignerFromKey(priv)
	if err != nil {
		return nil, false, err
	}
	return signer, true, nil
}

// Fingerprint renders a public key as its SHA256 fingerprint.
func Fingerprint(key gossh.PublicKey) string {
	return gossh.FingerprintSHA256(key)
}

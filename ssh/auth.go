package ssh

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/secret"

	gossh "golang.org/x/crypto/ssh"
)

// AuthConfig is the gateway's own client authentication. It is deliberately
// unrelated to the credentials a provider uses to reach an environment.
type AuthConfig struct {
	AuthorizedKeysFile   string
	AuthorizedKeysInline []string
	PasswordAuth         bool
	Password             secret.Value
	// AllowedUsers restricts the login name. Empty means any name is accepted,
	// so `ssh root@gateway` works without creating a root user anywhere.
	AllowedUsers []string
	// AllowAnonymous lets clients in without any credential when none is
	// configured. The gateway only sets this for a loopback listener, where the
	// machine's own user boundary is the security boundary; that keeps the
	// common local setup free of key and password ceremony.
	AllowAnonymous bool
}

type authorizedKey struct {
	key     gossh.PublicKey
	comment string
	source  string
}

// Authorizer answers "may this client in?".
type Authorizer struct {
	keys         []authorizedKey
	passwordAuth bool
	password     secret.Value
	generated    bool
	anonymous    bool
	allowed      map[string]bool
	log          *slog.Logger
}

// ErrNoAuthMethods means the gateway would have been open to the network.
var ErrNoAuthMethods = errors.New("no authentication configured for a non-loopback listener: " +
	"bind ssh.listen to 127.0.0.1, or add a public key (ssh.authorized_keys / " +
	"ssh.authorized_keys_inline) or set ssh.password_auth: true")

// NewAuthorizer loads the authorized keys and validates that at least one
// authentication method exists. When password auth is enabled without a
// password, a strong one is generated; the caller must show it to the operator.
func NewAuthorizer(cfg AuthConfig, log *slog.Logger) (*Authorizer, error) {
	if log == nil {
		log = slog.Default()
	}
	a := &Authorizer{passwordAuth: cfg.PasswordAuth, password: cfg.Password, log: log}

	for i, line := range cfg.AuthorizedKeysInline {
		k, err := parseAuthorizedKey(line)
		if err != nil {
			return nil, fmt.Errorf("ssh.authorized_keys_inline[%d]: %w", i, err)
		}
		k.source = "config"
		a.keys = append(a.keys, k)
	}

	if cfg.AuthorizedKeysFile != "" {
		raw, err := os.ReadFile(cfg.AuthorizedKeysFile)
		switch {
		case err == nil:
			for n, line := range strings.Split(string(raw), "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					continue
				}
				k, err := parseAuthorizedKey(trimmed)
				if err != nil {
					return nil, fmt.Errorf("%s:%d: %w", cfg.AuthorizedKeysFile, n+1, err)
				}
				k.source = cfg.AuthorizedKeysFile
				a.keys = append(a.keys, k)
			}
		case os.IsNotExist(err):
			// Not an error: the operator may be using inline keys or a password.
		default:
			return nil, fmt.Errorf("read %s: %w", cfg.AuthorizedKeysFile, err)
		}
	}

	if a.passwordAuth && a.password.Empty() {
		pw, err := randomPassword()
		if err != nil {
			return nil, err
		}
		a.password = secret.New(pw)
		a.generated = true
	}
	if len(a.keys) == 0 && !a.passwordAuth {
		if !cfg.AllowAnonymous {
			return nil, ErrNoAuthMethods
		}
		// Local-only and nothing configured: let clients straight in, which is
		// what "ssh root@codespace should just work" means on one's own machine.
		a.anonymous = true
	}

	if len(cfg.AllowedUsers) > 0 {
		a.allowed = map[string]bool{}
		for _, u := range cfg.AllowedUsers {
			if u = strings.TrimSpace(u); u != "" {
				a.allowed[u] = true
			}
		}
	}
	return a, nil
}

func parseAuthorizedKey(line string) (authorizedKey, error) {
	key, comment, options, _, err := gossh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return authorizedKey{}, fmt.Errorf("not a valid public key: %w", err)
	}
	if len(options) > 0 {
		// Silently ignoring options such as command="..." would give a false
		// sense of restriction.
		return authorizedKey{}, fmt.Errorf("authorized_keys options are not supported: %v", options)
	}
	return authorizedKey{key: key, comment: comment}, nil
}

func randomPassword() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)), nil
}

// GeneratedPassword returns the generated password, if one was generated.
func (a *Authorizer) GeneratedPassword() (string, bool) {
	if a.generated {
		return a.password.Reveal(), true
	}
	return "", false
}

// KeyCount is the number of authorized public keys.
func (a *Authorizer) KeyCount() int { return len(a.keys) }

// Anonymous reports whether clients may connect without a credential.
func (a *Authorizer) Anonymous() bool { return a.anonymous }

// Mode describes the effective client authentication, for banners and status.
func (a *Authorizer) Mode() string {
	switch {
	case a.anonymous:
		return "免认证（仅本机监听）"
	case len(a.keys) > 0 && a.passwordAuth:
		return fmt.Sprintf("%d 个公钥 + 密码", len(a.keys))
	case len(a.keys) > 0:
		return fmt.Sprintf("%d 个公钥", len(a.keys))
	default:
		return "密码"
	}
}

// PasswordEnabled reports whether password auth is on.
func (a *Authorizer) PasswordEnabled() bool { return a.passwordAuth }

// CheckUser validates the login name against the allow list.
func (a *Authorizer) CheckUser(user string) error {
	if user == "" {
		return errors.New("empty username")
	}
	if a.allowed == nil {
		return nil
	}
	if !a.allowed[user] {
		return fmt.Errorf("user %q is not in ssh.allowed_users", user)
	}
	return nil
}

// AuthenticateKey checks a public key. The returned permissions carry the
// gateway identity that authenticated, never a credential.
func (a *Authorizer) AuthenticateKey(user string, key gossh.PublicKey) (*gossh.Permissions, error) {
	if err := a.CheckUser(user); err != nil {
		return nil, err
	}
	want := key.Marshal()
	for _, ak := range a.keys {
		if subtle.ConstantTimeCompare(ak.key.Marshal(), want) == 1 {
			return &gossh.Permissions{Extensions: map[string]string{
				"gateway-auth":        "publickey",
				"gateway-key":         gossh.FingerprintSHA256(key),
				"gateway-key-comment": ak.comment,
			}}, nil
		}
	}
	return nil, fmt.Errorf("public key %s is not authorized", gossh.FingerprintSHA256(key))
}

// AuthenticatePassword checks a password in constant time.
func (a *Authorizer) AuthenticatePassword(user string, given []byte) (*gossh.Permissions, error) {
	if !a.passwordAuth {
		return nil, errors.New("password authentication is disabled")
	}
	if err := a.CheckUser(user); err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(a.password.Reveal()), given) != 1 {
		return nil, errors.New("password rejected")
	}
	return &gossh.Permissions{Extensions: map[string]string{"gateway-auth": "password"}}, nil
}

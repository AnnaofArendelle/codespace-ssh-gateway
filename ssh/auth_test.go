package ssh_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/logging"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/secret"
	sshsrv "github.com/AnnaofArendelle/codespace-ssh-gateway/ssh"

	gossh "golang.org/x/crypto/ssh"
)

func testKey(t *testing.T) (gossh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, strings.TrimSpace(string(gossh.MarshalAuthorizedKey(signer.PublicKey())))
}

func TestParseLogin(t *testing.T) {
	cases := map[string]sshsrv.Login{
		"root":        {User: "root"},
		"root+my-box": {User: "root", EnvironmentHint: "my-box"},
		"dev+a+b":     {User: "dev", EnvironmentHint: "a+b"},
		"+only":       {User: "", EnvironmentHint: "only"},
		"ubuntu":      {User: "ubuntu"},
	}
	for in, want := range cases {
		if got := sshsrv.ParseLogin(in); got != want {
			t.Errorf("ParseLogin(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestAuthorizerRequiresAMethod(t *testing.T) {
	_, err := sshsrv.NewAuthorizer(sshsrv.AuthConfig{}, logging.Discard())
	if err == nil {
		t.Fatal("an authorizer with no keys and no password was accepted")
	}
	if !strings.Contains(err.Error(), "authentication") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestAuthorizerAcceptsInlineAndFileKeys(t *testing.T) {
	dir := t.TempDir()
	signerA, lineA := testKey(t)
	signerB, lineB := testKey(t)
	other, _ := testKey(t)

	keyFile := filepath.Join(dir, "authorized_keys")
	content := "# a comment\n\n" + lineB + " user@host\n"
	if err := os.WriteFile(keyFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	auth, err := sshsrv.NewAuthorizer(sshsrv.AuthConfig{
		AuthorizedKeysInline: []string{lineA},
		AuthorizedKeysFile:   keyFile,
	}, logging.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if auth.KeyCount() != 2 {
		t.Fatalf("loaded %d keys, want 2", auth.KeyCount())
	}
	for name, signer := range map[string]gossh.Signer{"inline": signerA, "file": signerB} {
		if _, err := auth.AuthenticateKey("root", signer.PublicKey()); err != nil {
			t.Errorf("%s key rejected: %v", name, err)
		}
	}
	if _, err := auth.AuthenticateKey("root", other.PublicKey()); err == nil {
		t.Error("an unknown key was accepted")
	}
}

func TestAuthorizerRejectsKeyOptions(t *testing.T) {
	_, line := testKey(t)
	_, err := sshsrv.NewAuthorizer(sshsrv.AuthConfig{
		AuthorizedKeysInline: []string{`command="/bin/false" ` + line},
	}, logging.Discard())
	if err == nil {
		t.Fatal("a key with unsupported options was accepted silently")
	}
	if !strings.Contains(err.Error(), "options") {
		t.Errorf("error does not mention options: %v", err)
	}
}

func TestAuthorizerPasswords(t *testing.T) {
	auth, err := sshsrv.NewAuthorizer(sshsrv.AuthConfig{
		PasswordAuth: true,
		Password:     secret.New("correct horse"),
	}, logging.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.AuthenticatePassword("root", []byte("correct horse")); err != nil {
		t.Errorf("correct password rejected: %v", err)
	}
	if _, err := auth.AuthenticatePassword("root", []byte("wrong")); err == nil {
		t.Error("wrong password accepted")
	}
	if _, ok := auth.GeneratedPassword(); ok {
		t.Error("a password was generated although one was configured")
	}

	generated, err := sshsrv.NewAuthorizer(sshsrv.AuthConfig{PasswordAuth: true}, logging.Discard())
	if err != nil {
		t.Fatal(err)
	}
	pw, ok := generated.GeneratedPassword()
	if !ok || len(pw) < 16 {
		t.Fatalf("generated password %q is too weak", pw)
	}
	if _, err := generated.AuthenticatePassword("root", []byte(pw)); err != nil {
		t.Errorf("generated password rejected: %v", err)
	}
}

func TestAuthorizerAllowedUsers(t *testing.T) {
	_, line := testKey(t)
	auth, err := sshsrv.NewAuthorizer(sshsrv.AuthConfig{
		AuthorizedKeysInline: []string{line},
		AllowedUsers:         []string{"root", "dev"},
	}, logging.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.CheckUser("root"); err != nil {
		t.Errorf("root rejected: %v", err)
	}
	if err := auth.CheckUser("nobody"); err == nil {
		t.Error("a user outside the allow list was accepted")
	}
}

func TestHostKeyIsCreatedOnceAndPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys", "host_ed25519")

	first, created, err := sshsrv.LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("first call did not report creating the key")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("host key mode is %#o, want 0600", perm)
	}

	second, created, err := sshsrv.LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("second call regenerated the host key")
	}
	if sshsrv.Fingerprint(first.PublicKey()) != sshsrv.Fingerprint(second.PublicKey()) {
		t.Error("host key changed between loads")
	}
}

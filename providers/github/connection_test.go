package github

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/logging"

	gossh "golang.org/x/crypto/ssh"
)

func TestEnsureKeyPairIsStableAndPrivate(t *testing.T) {
	dir := t.TempDir()
	first, err := ensureKeyPair(dir)
	if err != nil {
		t.Fatalf("ensureKeyPair: %v", err)
	}
	info, err := os.Stat(first.PrivatePath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key mode %#o, want 0600", perm)
	}
	if _, err := os.Stat(first.PublicPath); err != nil {
		t.Fatalf("public key missing: %v", err)
	}

	second, err := ensureKeyPair(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sshFingerprint(first.Signer) != sshFingerprint(second.Signer) {
		t.Error("the gateway key changed on the second call")
	}

	// A missing public half is regenerated, because gh needs the file.
	if err := os.Remove(first.PublicPath); err != nil {
		t.Fatal(err)
	}
	third, err := ensureKeyPair(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(third.PublicPath)
	if err != nil {
		t.Fatalf("public key was not restored: %v", err)
	}
	key, _, _, _, err := gossh.ParseAuthorizedKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if gossh.FingerprintSHA256(key) != sshFingerprint(first.Signer) {
		t.Error("restored public key does not match the private key")
	}
}

func newHostKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	dir := t.TempDir()
	kp, err := ensureKeyPair(dir)
	if err != nil {
		t.Fatal(err)
	}
	return kp.Signer.PublicKey()
}

func TestHostKeyTOFUPinsThenDetectsChange(t *testing.T) {
	dir := t.TempDir()
	store := newHostKeyStore(dir, PolicyTOFU, logging.Discard())
	keyA, keyB := newHostKey(t), newHostKey(t)

	if err := store.check("box", keyA); err != nil {
		t.Fatalf("first use should pin: %v", err)
	}
	if err := store.check("box", keyA); err != nil {
		t.Fatalf("same key rejected on reconnect: %v", err)
	}
	err := store.check("box", keyB)
	if err == nil {
		t.Fatal("a changed host key was accepted")
	}
	if !strings.Contains(err.Error(), "forget-host-key") {
		t.Errorf("error does not explain the fix: %v", err)
	}

	// Another codespace is independent.
	if err := store.check("other", keyB); err != nil {
		t.Fatalf("second codespace rejected: %v", err)
	}

	n, err := store.Forget("box")
	if err != nil || n != 1 {
		t.Fatalf("forget returned %d, %v", n, err)
	}
	if err := store.check("box", keyB); err != nil {
		t.Fatalf("after forgetting, the new key should pin: %v", err)
	}
	if n, _ := store.Forget(""); n != 2 {
		t.Errorf("forget-all removed %d entries, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "known_codespaces")); !os.IsNotExist(err) {
		t.Error("the known hosts file should be gone after forgetting everything")
	}
}

func TestHostKeyStrictAndInsecurePolicies(t *testing.T) {
	key := newHostKey(t)

	strict := newHostKeyStore(t.TempDir(), PolicyStrict, logging.Discard())
	if err := strict.check("box", key); err == nil {
		t.Error("strict policy accepted an unpinned key")
	}

	insecure := newHostKeyStore(t.TempDir(), PolicyInsecure, logging.Discard())
	if err := insecure.check("box", key); err != nil {
		t.Errorf("insecure policy rejected a key: %v", err)
	}
}

func TestUserCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := newUserCache(dir, logging.Discard())
	if _, ok := c.get("box"); ok {
		t.Error("empty cache returned a user")
	}
	c.set("box", "vscode")
	if user, ok := c.get("box"); !ok || user != "vscode" {
		t.Errorf("get returned %q, %v", user, ok)
	}

	// A new cache instance reads what the first one wrote.
	again := newUserCache(dir, logging.Discard())
	if user, ok := again.get("box"); !ok || user != "vscode" {
		t.Errorf("cache did not persist: %q %v", user, ok)
	}
	info, err := os.Stat(filepath.Join(dir, "ssh_users.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cache file mode %#o, want 0600", perm)
	}

	again.forget("box")
	if _, ok := again.get("box"); ok {
		t.Error("forget did not remove the entry")
	}
}

func TestParseSSHConfigFromGH(t *testing.T) {
	out := `Host cs.octo-demo-abc.main
	User node
	ProxyCommand /usr/bin/gh cs ssh -c octo-demo-abc --stdio -- -i /home/u/.ssh/codespaces.auto
	UserKnownHostsFile=/dev/null
	StrictHostKeyChecking no
	LogLevel quiet
	ControlMaster auto
	IdentityFile /home/u/.ssh/codespaces.auto
`
	target := parseSSHConfig(out)
	if target.User != "node" {
		t.Errorf("user %q, want node", target.User)
	}
	if !target.StdioSupported {
		t.Error("--stdio was not detected in the ProxyCommand")
	}
	if target.IdentityFile != "/home/u/.ssh/codespaces.auto" {
		t.Errorf("identity file %q", target.IdentityFile)
	}
}

func TestSafeSetEnvFiltersUnsafeValues(t *testing.T) {
	got := safeSetEnv(map[string]string{
		"LANG":        "en_US.UTF-8",
		"BAD NAME":    "x",
		"WITH_SPACE":  "a b",
		"WITH_QUOTE":  `a"b`,
		"WITH_NEWLIN": "a\nb",
		"OK2":         "value2",
	})
	want := map[string]bool{"LANG=en_US.UTF-8": true, "OK2=value2": true}
	if len(got) != len(want) {
		t.Fatalf("safeSetEnv returned %v, want exactly %v", got, want)
	}
	for _, kv := range got {
		if !want[kv] {
			t.Errorf("unexpected entry %q", kv)
		}
	}
}

func TestParseTerminalModes(t *testing.T) {
	// ECHO=1 then a terminating zero opcode.
	raw := []byte{53, 0, 0, 0, 1, 0}
	modes := parseTerminalModes(raw)
	if modes[gossh.ECHO] != 1 {
		t.Errorf("ECHO not decoded: %v", modes)
	}
	if len(parseTerminalModes(nil)) == 0 {
		t.Error("empty modes should fall back to sane defaults")
	}
}

func TestMergeCreateOptions(t *testing.T) {
	base := CreateConfig{Repository: "octo/demo", IdleTimeoutMinutes: 30}
	out, err := mergeCreateOptions(base, map[string]any{
		"branch":               "feature",
		"machine":              "standardLinux",
		"idle_timeout_minutes": "60",
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if out.Branch != "feature" || out.Machine != "standardLinux" || out.IdleTimeoutMinutes != 60 {
		t.Errorf("merged to %+v", out)
	}
	if out.Repository != "octo/demo" {
		t.Errorf("base value lost: %+v", out)
	}
	if _, err := mergeCreateOptions(base, map[string]any{"nope": "1"}); err == nil {
		t.Error("an unknown option was accepted")
	}
	if _, err := mergeCreateOptions(base, map[string]any{"idle_timeout_minutes": "soon"}); err == nil {
		t.Error("a non-numeric timeout was accepted")
	}
	if _, err := mergeCreateOptions(base, map[string]any{"repository": "bogus"}); err == nil {
		t.Error("a repository without owner/name was accepted")
	}
}

func TestTemporaryConnectErrorClassification(t *testing.T) {
	temporary := []string{
		"ssh: handshake failed: EOF",
		"dial tcp 127.0.0.1:22: connect: connection refused",
		"tunnel closed",
		"error getting ssh server details: could not start ssh server",
		"context deadline exceeded",
	}
	for _, msg := range temporary {
		if !temporaryConnectError(errTest(msg)) {
			t.Errorf("%q should be retryable", msg)
		}
	}
	permanent := []string{
		"unknown flag: --stdio",
		"github.create.repository is not set",
		"ssh: unable to authenticate",
	}
	for _, msg := range permanent {
		if temporaryConnectError(errTest(msg)) {
			t.Errorf("%q should not be retryable", msg)
		}
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

package gateway_test

import (
	"strings"
	"testing"
)

// Deleting a codespace and reconnecting must rebuild it from the repository the
// gateway saw it on, without the operator declaring anything.
func TestRecreatesDeletedCodespaceFromRememberedRepository(t *testing.T) {
	h := newHarness(t, options{state: "Available", autoCreate: true})
	h.start()

	// First connection: the gateway learns which repository this codespace uses.
	client := h.dial("root")
	if _, _, _, err := h.exec(client, "echo before"); err != nil {
		t.Fatalf("first exec: %v", err)
	}
	client.Close()

	// The operator deletes it on github.com.
	h.gh.Delete(h.handle)
	if n := h.gh.Created(); n != 0 {
		t.Fatalf("nothing should have been created yet, got %d", n)
	}

	// Reconnecting recreates it, with no create.repository in the config.
	client2 := h.dial("root")
	defer client2.Close()
	stdout, stderr, code, err := h.exec(client2, "echo after")
	if err != nil || code != 0 {
		t.Fatalf("exec after delete: %v code=%d stderr=%q", err, code, stderr)
	}
	if stdout != "after\n" {
		t.Fatalf("got %q", stdout)
	}
	if n := h.gh.Created(); n != 1 {
		t.Errorf("created %d codespaces, want 1", n)
	}
	if repo := h.gh.CreatedRepository(); repo != "octo/demo" {
		t.Errorf("recreated from %q, want the remembered repository", repo)
	}
	if !strings.Contains(stderr, "正在创建") {
		t.Errorf("the client was not told a codespace is being created: %q", stderr)
	}
}

// Without any history and without create.repository, the failure has to say
// exactly what to do.
func TestMissingCodespaceWithNothingToCreateFrom(t *testing.T) {
	h := newHarness(t, options{state: "", autoCreate: true})
	h.start()

	client := h.dial("root")
	defer client.Close()
	_, stderr, code, _ := h.exec(client, "echo nope")
	if code == 0 {
		t.Fatal("expected the session to fail")
	}
	if !strings.Contains(stderr, "github.create.repository") {
		t.Errorf("error does not name the setting to add: %q", stderr)
	}
}

// With no codespace at all but a repository configured, the first ssh must
// create one under a stable handle.
func TestCreatesFirstCodespaceWhenNothingExists(t *testing.T) {
	h := newHarness(t, options{state: "", autoCreate: true, createRepo: "octo/demo", noDefaultEnv: true})
	h.start()

	client := h.dial("root")
	defer client.Close()
	stdout, stderr, code, err := h.exec(client, "echo first")
	if err != nil || code != 0 {
		t.Fatalf("exec: %v code=%d stderr=%q", err, code, stderr)
	}
	if stdout != "first\n" {
		t.Fatalf("got %q", stdout)
	}
	if n := h.gh.Created(); n != 1 {
		t.Errorf("created %d codespaces, want 1", n)
	}
	// A second connection reuses it rather than creating another.
	client2 := h.dial("root")
	defer client2.Close()
	if _, _, _, err := h.exec(client2, "echo second"); err != nil {
		t.Fatalf("second exec: %v", err)
	}
	if n := h.gh.Created(); n != 1 {
		t.Errorf("created %d codespaces after reconnecting, want 1", n)
	}
}

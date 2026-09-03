package session_test

import (
	"errors"
	"testing"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/session"
)

func TestOpenAndCloseTracksCounts(t *testing.T) {
	m := session.NewManager(0, 0)
	a, err := m.Open(session.Info{User: "root", Environment: "box"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Open(session.Info{User: "root", Environment: "box"})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.CountFor("box"); got != 2 {
		t.Errorf("count %d, want 2", got)
	}
	if got := m.Total(); got != 2 {
		t.Errorf("total %d, want 2", got)
	}
	if a.ID() == b.ID() {
		t.Error("sessions share an id")
	}

	m.Close(a)
	m.Close(a) // idempotent
	if got := m.CountFor("box"); got != 1 {
		t.Errorf("count %d after one close, want 1", got)
	}
	m.Close(b)
	if got := m.CountFor("box"); got != 0 {
		t.Errorf("count %d after all closed, want 0", got)
	}
}

func TestSessionLimits(t *testing.T) {
	m := session.NewManager(1, 0)
	if _, err := m.Open(session.Info{Environment: "box"}); err != nil {
		t.Fatal(err)
	}
	_, err := m.Open(session.Info{Environment: "box"})
	if !errors.Is(err, session.ErrTooManySessions) {
		t.Fatalf("second Open returned %v, want ErrTooManySessions", err)
	}

	perEnv := session.NewManager(0, 1)
	if _, err := perEnv.Open(session.Info{Environment: "box"}); err != nil {
		t.Fatal(err)
	}
	if _, err := perEnv.Open(session.Info{Environment: "other"}); err != nil {
		t.Fatalf("a different environment hit the per-environment limit: %v", err)
	}
	if _, err := perEnv.Open(session.Info{Environment: "box"}); !errors.Is(err, session.ErrTooManySessionsForEnv) {
		t.Fatalf("returned %v, want ErrTooManySessionsForEnv", err)
	}
}

func TestHooksFireOnFirstAndLast(t *testing.T) {
	m := session.NewManager(0, 0)
	var first, last []string
	m.SetHooks(
		func(env string) { first = append(first, env) },
		func(env string) { last = append(last, env) },
	)
	a, _ := m.Open(session.Info{Environment: "box"})
	b, _ := m.Open(session.Info{Environment: "box"})
	m.Close(a)
	if len(last) != 0 {
		t.Errorf("last hook fired while a session remained: %v", last)
	}
	m.Close(b)
	if len(first) != 1 || first[0] != "box" {
		t.Errorf("first hook fired %v, want [box]", first)
	}
	if len(last) != 1 || last[0] != "box" {
		t.Errorf("last hook fired %v, want [box]", last)
	}
}

func TestListIsSortedAndSnapshots(t *testing.T) {
	m := session.NewManager(0, 0)
	s1, _ := m.Open(session.Info{Environment: "box", User: "root"})
	s2, _ := m.Open(session.Info{Environment: "box", User: "dev"})
	s1.SetPhase("connected")
	s2.SetKind("exec", true)

	list := m.List()
	if len(list) != 2 {
		t.Fatalf("list has %d entries", len(list))
	}
	if list[0].ID != s1.ID() {
		t.Errorf("list is not oldest-first: %v", list)
	}
	if list[0].Phase != "connected" {
		t.Errorf("phase not reflected: %+v", list[0])
	}
	if !list[1].PTY || list[1].Kind != "exec" {
		t.Errorf("kind/pty not reflected: %+v", list[1])
	}
}

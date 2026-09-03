// Package testenv provides the stand-ins the test suite needs to exercise the
// real gateway code paths end to end: a GitHub Codespaces API, a codespace SSH
// server, and a stub `gh` binary.
//
// Only the far side of GitHub is simulated. Everything under test (the SSH
// server, the lifecycle state machine, the REST client, the gh invocation and
// the SSH-over-stdio transport) is the production code.
package testenv

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Codespace is the state the fake API keeps per codespace.
type Codespace struct {
	Name               string
	DisplayName        string
	State              string
	Repository         string
	IdleTimeoutMinutes int
	PendingOperation   bool
	// PollsBeforeAvailable makes a start take a few polls, like the real thing.
	PollsBeforeAvailable int
}

// GitHub is a fake GitHub REST API.
type GitHub struct {
	Server *httptest.Server

	mu          sync.Mutex
	codespaces  map[string]*Codespace
	calls       map[string]int
	token       string
	injections  []injection
	created     int
	createdRepo string
	// StartError, if set, makes POST .../start fail with this status.
	StartError int
	// CreateError, if set, makes POST /user/codespaces fail with this status.
	CreateError int
	// FailStartTransition leaves a started codespace in Failed state.
	FailStartTransition bool
	// Latency delays every response.
	Latency time.Duration
}

type injection struct {
	pathContains string
	status       int
	body         string
	times        int
}

// NewGitHub starts a fake API that accepts exactly the given token.
func NewGitHub(t *testing.T, token string) *GitHub {
	t.Helper()
	g := &GitHub{
		codespaces: map[string]*Codespace{},
		calls:      map[string]int{},
		token:      token,
	}
	g.Server = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.Server.Close)
	return g
}

// URL is the API base URL for github.api_url.
func (g *GitHub) URL() string { return g.Server.URL }

// Add registers a codespace.
func (g *GitHub) Add(cs Codespace) {
	g.mu.Lock()
	defer g.mu.Unlock()
	copied := cs
	g.codespaces[cs.Name] = &copied
}

// SetState forces a codespace's state.
func (g *GitHub) SetState(name, state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cs, ok := g.codespaces[name]; ok {
		cs.State = state
	}
}

// State reads a codespace's state.
func (g *GitHub) State(name string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cs, ok := g.codespaces[name]; ok {
		return cs.State
	}
	return ""
}

// Calls counts requests whose path contains the given fragment.
func (g *GitHub) Calls(fragment string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	total := 0
	for path, n := range g.calls {
		if strings.Contains(path, fragment) {
			total += n
		}
	}
	return total
}

// Delete removes a codespace, the way the GitHub UI does.
func (g *GitHub) Delete(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.codespaces, name)
}

// CreatedRepository is the repository the last create used.
func (g *GitHub) CreatedRepository() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.createdRepo
}

// Created counts successful create calls.
func (g *GitHub) Created() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.created
}

// Inject makes the next `times` requests matching a path fragment fail.
func (g *GitHub) Inject(pathContains string, status int, body string, times int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.injections = append(g.injections, injection{pathContains, status, body, times})
}

func (g *GitHub) handle(w http.ResponseWriter, r *http.Request) {
	if g.Latency > 0 {
		time.Sleep(g.Latency)
	}
	g.mu.Lock()
	g.calls[r.Method+" "+r.URL.Path]++
	for i := range g.injections {
		inj := &g.injections[i]
		if inj.times > 0 && strings.Contains(r.URL.Path, inj.pathContains) {
			inj.times--
			status, body := inj.status, inj.body
			g.mu.Unlock()
			if status == 0 {
				// Simulate a dropped connection.
				panic(http.ErrAbortHandler)
			}
			writeRaw(w, status, body)
			return
		}
	}
	g.mu.Unlock()

	if got := r.Header.Get("Authorization"); got != "Bearer "+g.token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}

	switch {
	case r.URL.Path == "/user":
		writeJSON(w, http.StatusOK, map[string]string{"login": "tester"})
	case r.URL.Path == "/user/codespaces" && r.Method == http.MethodGet:
		g.list(w, r)
	case r.URL.Path == "/user/codespaces" && r.Method == http.MethodPost:
		g.create(w, r)
	case r.URL.Path == "/user/repos" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, []map[string]any{
			{"id": 4242, "full_name": "octo/demo", "default_branch": "main",
				"private": false, "pushed_at": "2026-09-01T10:00:00Z"},
			{"id": 4243, "full_name": "octo/other", "default_branch": "trunk",
				"private": true, "pushed_at": "2026-08-20T10:00:00Z"},
		})
	case strings.HasPrefix(r.URL.Path, "/repos/"):
		writeJSON(w, http.StatusOK, map[string]any{
			"id": 4242, "full_name": strings.TrimPrefix(r.URL.Path, "/repos/"), "default_branch": "main",
		})
	case strings.HasSuffix(r.URL.Path, "/start") && r.Method == http.MethodPost:
		g.start(w, nameFromPath(r.URL.Path, "/start"))
	case strings.HasSuffix(r.URL.Path, "/stop") && r.Method == http.MethodPost:
		g.stop(w, nameFromPath(r.URL.Path, "/stop"))
	case strings.HasPrefix(r.URL.Path, "/user/codespaces/") && r.Method == http.MethodGet:
		g.get(w, strings.TrimPrefix(r.URL.Path, "/user/codespaces/"))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func nameFromPath(path, suffix string) string {
	return strings.TrimPrefix(strings.TrimSuffix(path, suffix), "/user/codespaces/")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	if code == http.StatusForbidden {
		w.Header().Set("X-RateLimit-Remaining", "0")
	}
	w.WriteHeader(code)
	if body == "" {
		body = fmt.Sprintf(`{"message":"injected %d"}`, code)
	}
	_, _ = w.Write([]byte(body))
}

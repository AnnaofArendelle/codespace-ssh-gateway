package testenv

import (
	"encoding/json"
	"net/http"
	"time"
)

// payload renders a codespace the way the GitHub API does.
func (cs *Codespace) payload() map[string]any {
	repo := cs.Repository
	if repo == "" {
		repo = "octo/demo"
	}
	return map[string]any{
		"name":                 cs.Name,
		"display_name":         cs.DisplayName,
		"state":                cs.State,
		"web_url":              "https://github.com/codespaces/" + cs.Name,
		"created_at":           time.Now().Add(-time.Hour).Format(time.RFC3339),
		"last_used_at":         time.Now().Add(-time.Minute).Format(time.RFC3339),
		"idle_timeout_minutes": cs.IdleTimeoutMinutes,
		"pending_operation":    cs.PendingOperation,
		"location":             "WestEurope",
		"machine":              map[string]any{"name": "basicLinux32gb", "display_name": "2 cores"},
		"repository":           map[string]any{"id": 4242, "full_name": repo},
		"git_status":           map[string]any{"ref": "main"},
		"owner":                map[string]any{"login": "tester"},
	}
}

func (g *GitHub) list(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	out := make([]map[string]any, 0, len(g.codespaces))
	for _, cs := range g.codespaces {
		out = append(out, cs.payload())
	}
	g.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "codespaces": out})
}

func (g *GitHub) get(w http.ResponseWriter, name string) {
	g.mu.Lock()
	cs, ok := g.codespaces[name]
	if ok {
		// Advance a starting codespace towards Available, like GitHub does.
		if cs.State == "Starting" || cs.State == "Provisioning" || cs.State == "Queued" {
			if cs.PollsBeforeAvailable > 0 {
				cs.PollsBeforeAvailable--
			} else if g.FailStartTransition {
				cs.State = "Failed"
			} else {
				cs.State = "Available"
			}
		}
	}
	var payload map[string]any
	if ok {
		payload = cs.payload()
	}
	g.mu.Unlock()

	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (g *GitHub) start(w http.ResponseWriter, name string) {
	g.mu.Lock()
	if g.StartError != 0 {
		status := g.StartError
		g.mu.Unlock()
		writeRaw(w, status, `{"message":"start refused"}`)
		return
	}
	cs, ok := g.codespaces[name]
	if !ok {
		g.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	if cs.State == "Available" {
		g.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"message": "already running"})
		return
	}
	cs.State = "Starting"
	payload := cs.payload()
	g.mu.Unlock()
	writeJSON(w, http.StatusOK, payload)
}

func (g *GitHub) stop(w http.ResponseWriter, name string) {
	g.mu.Lock()
	cs, ok := g.codespaces[name]
	if ok {
		cs.State = "Shutdown"
	}
	g.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "Shutdown"})
}

func (g *GitHub) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RepositoryID       int64  `json:"repository_id"`
		Ref                string `json:"ref"`
		DisplayName        string `json:"display_name"`
		Machine            string `json:"machine"`
		IdleTimeoutMinutes int    `json:"idle_timeout_minutes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	g.mu.Lock()
	if g.CreateError != 0 {
		status := g.CreateError
		g.mu.Unlock()
		writeRaw(w, status, `{"message":"create refused"}`)
		return
	}
	g.created++
	name := "generated-codespace-1"
	for i := 2; ; i++ {
		if _, clash := g.codespaces[name]; !clash {
			break
		}
		name = "generated-codespace-" + itoa(i)
	}
	cs := &Codespace{
		Name:                 name,
		DisplayName:          body.DisplayName,
		State:                "Queued",
		IdleTimeoutMinutes:   body.IdleTimeoutMinutes,
		PollsBeforeAvailable: 1,
	}
	g.codespaces[name] = cs
	payload := cs.payload()
	g.mu.Unlock()

	writeJSON(w, http.StatusCreated, payload)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

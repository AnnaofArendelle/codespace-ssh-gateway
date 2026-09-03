package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// codespace mirrors the fields of the GitHub Codespaces REST resource that the
// gateway needs. Field names follow the API, not the core model.
type codespace struct {
	Name                           string `json:"name"`
	DisplayName                    string `json:"display_name"`
	State                          string `json:"state"`
	CreatedAt                      string `json:"created_at"`
	LastUsedAt                     string `json:"last_used_at"`
	WebURL                         string `json:"web_url"`
	Location                       string `json:"location"`
	DevContainerPath               string `json:"devcontainer_path"`
	IdleTimeoutMinutes             int    `json:"idle_timeout_minutes"`
	RetentionPeriodMinutes         int    `json:"retention_period_minutes"`
	PendingOperation               bool   `json:"pending_operation"`
	PendingOperationDisabledReason string `json:"pending_operation_disabled_reason"`
	Machine                        struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	} `json:"machine"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
	GitStatus struct {
		Ref string `json:"ref"`
	} `json:"git_status"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// GitHub's state vocabulary, as published by the API and used by the GitHub CLI.
func stateFor(native string) providers.State {
	switch native {
	case "Available":
		return providers.StateRunning
	case "Created", "Queued", "Provisioning":
		return providers.StateProvisioning
	case "Starting", "Awaiting", "Updating", "Rebuilding", "Exporting":
		return providers.StateStarting
	case "ShuttingDown":
		return providers.StateStopping
	case "Shutdown", "Archived":
		return providers.StateStopped
	case "Failed":
		return providers.StateFailed
	case "Unavailable":
		return providers.StateUnavailable
	case "Deleted", "Moved":
		return providers.StateNotFound
	default:
		return providers.StateUnknown
	}
}

func (cs codespace) environment() providers.Environment {
	attrs := map[string]string{}
	if cs.Repository.FullName != "" {
		attrs["repository"] = cs.Repository.FullName
	}
	if cs.GitStatus.Ref != "" {
		attrs["branch"] = cs.GitStatus.Ref
	}
	if cs.Machine.Name != "" {
		attrs["machine"] = cs.Machine.Name
	}
	if cs.Location != "" {
		attrs["location"] = cs.Location
	}
	if cs.DisplayName != "" {
		attrs["display_name"] = cs.DisplayName
	}
	if cs.Owner.Login != "" {
		attrs["owner"] = cs.Owner.Login
	}
	if cs.PendingOperation {
		attrs["pending_operation"] = "true"
		if cs.PendingOperationDisabledReason != "" {
			attrs["pending_reason"] = cs.PendingOperationDisabledReason
		}
	}
	env := providers.Environment{
		ID:          cs.Name,
		DisplayName: cs.DisplayName,
		Provider:    ProviderName,
		State:       stateFor(cs.State),
		NativeState: cs.State,
		Attributes:  attrs,
		WebURL:      cs.WebURL,
	}
	if cs.IdleTimeoutMinutes > 0 {
		env.IdleTimeout = time.Duration(cs.IdleTimeoutMinutes) * time.Minute
	}
	if cs.LastUsedAt != "" {
		if t, err := time.Parse(time.RFC3339, cs.LastUsedAt); err == nil {
			env.LastUsedAt = t
		}
	}
	return env
}

func (c *apiClient) listCodespaces(ctx context.Context) ([]codespace, error) {
	var all []codespace
	const perPage = 100
	for page := 1; page <= 20; page++ {
		var payload struct {
			TotalCount int         `json:"total_count"`
			Codespaces []codespace `json:"codespaces"`
		}
		path := fmt.Sprintf("/user/codespaces?per_page=%d&page=%d", perPage, page)
		if _, err := c.do(ctx, "GET", path, nil, &payload); err != nil {
			return nil, err
		}
		all = append(all, payload.Codespaces...)
		if len(payload.Codespaces) < perPage {
			break
		}
	}
	return all, nil
}

func (c *apiClient) getCodespace(ctx context.Context, name string) (codespace, error) {
	var cs codespace
	path := "/user/codespaces/" + url.PathEscape(name)
	if _, err := c.do(ctx, "GET", path, nil, &cs); err != nil {
		return codespace{}, err
	}
	return cs, nil
}

func (c *apiClient) startCodespace(ctx context.Context, name string) error {
	path := "/user/codespaces/" + url.PathEscape(name) + "/start"
	status, err := c.do(ctx, "POST", path, nil, nil)
	if err != nil {
		// 409 means "already running", which is exactly what we wanted.
		if status == 409 {
			return nil
		}
		return err
	}
	return nil
}

func (c *apiClient) stopCodespace(ctx context.Context, name string) error {
	path := "/user/codespaces/" + url.PathEscape(name) + "/stop"
	if _, err := c.do(ctx, "POST", path, nil, nil); err != nil {
		return err
	}
	return nil
}

type createRequest struct {
	RepositoryID           int64  `json:"repository_id"`
	Ref                    string `json:"ref,omitempty"`
	Location               string `json:"location,omitempty"`
	Machine                string `json:"machine,omitempty"`
	DevContainerPath       string `json:"devcontainer_path,omitempty"`
	IdleTimeoutMinutes     int    `json:"idle_timeout_minutes,omitempty"`
	RetentionPeriodMinutes int    `json:"retention_period_minutes,omitempty"`
	DisplayName            string `json:"display_name,omitempty"`
}

// createCodespace posts a creation request. GitHub answers 201 when the
// codespace exists already, or 202 when provisioning continues asynchronously;
// either way the response names the codespace so the caller can poll it.
func (c *apiClient) createCodespace(ctx context.Context, req createRequest) (codespace, error) {
	var cs codespace
	if _, err := c.do(ctx, "POST", "/user/codespaces", req, &cs); err != nil {
		return codespace{}, err
	}
	if cs.Name == "" {
		return codespace{}, fmt.Errorf("github did not return a codespace name for the created codespace")
	}
	return cs, nil
}

type machine struct {
	Name                 string `json:"name"`
	DisplayName          string `json:"display_name"`
	CPUs                 int    `json:"cpus"`
	MemoryInBytes        uint64 `json:"memory_in_bytes"`
	StorageInBytes       uint64 `json:"storage_in_bytes"`
	PrebuildAvailability string `json:"prebuild_availability"`
}

// listMachines asks GitHub which machine types a repository can use.
func (c *apiClient) listMachines(ctx context.Context, nwo string) ([]machine, error) {
	owner, name, ok := strings.Cut(nwo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("repository %q must be owner/name", nwo)
	}
	var payload struct {
		TotalCount int       `json:"total_count"`
		Machines   []machine `json:"machines"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/codespaces/machines"
	if _, err := c.do(ctx, "GET", path, nil, &payload); err != nil {
		return nil, err
	}
	return payload.Machines, nil
}

type repository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	PushedAt      string `json:"pushed_at"`
}

func (c *apiClient) getRepository(ctx context.Context, nwo string) (repository, error) {
	owner, name, ok := strings.Cut(nwo, "/")
	if !ok || owner == "" || name == "" {
		return repository{}, fmt.Errorf("repository %q must be owner/name", nwo)
	}
	var repo repository
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	if _, err := c.do(ctx, "GET", path, nil, &repo); err != nil {
		return repository{}, err
	}
	return repo, nil
}

// userRepositories lists the token owner's repositories, most recently pushed
// first, so setup can offer them as create sources.
func (c *apiClient) userRepositories(ctx context.Context, limit int) ([]repository, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var repos []repository
	path := fmt.Sprintf("/user/repos?sort=pushed&per_page=%d&affiliation=owner,collaborator", limit)
	if _, err := c.do(ctx, "GET", path, nil, &repos); err != nil {
		return nil, err
	}
	return repos, nil
}

// currentUser validates the token and returns the login it belongs to.
func (c *apiClient) currentUser(ctx context.Context) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if _, err := c.do(ctx, "GET", "/user", nil, &user); err != nil {
		return "", err
	}
	return user.Login, nil
}

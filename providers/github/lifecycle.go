package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// List returns every codespace the token can see.
func (p *Provider) List(ctx context.Context) ([]providers.Environment, error) {
	list, err := p.api.listCodespaces(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]providers.Environment, 0, len(list))
	for _, cs := range list {
		p.rememberSource(cs)
		out = append(out, cs.environment())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get resolves a handle to a codespace. The handle matches the codespace name
// or its display name, so a configured handle keeps working after GitHub
// generates a new codespace name for it.
func (p *Provider) Get(ctx context.Context, id string) (providers.Environment, error) {
	if id == "" {
		return providers.Environment{}, errors.New("no codespace selected: set github.codespace " +
			"or run `gateway codespace select <name>`")
	}
	cs, err := p.api.getCodespace(ctx, id)
	if err == nil {
		p.rememberSource(cs, id)
		return cs.environment(), nil
	}
	if !errors.Is(err, providers.ErrNotFound) {
		return providers.Environment{}, err
	}

	list, listErr := p.api.listCodespaces(ctx)
	if listErr != nil {
		return providers.Environment{}, listErr
	}
	for _, cs := range list {
		if strings.EqualFold(cs.DisplayName, id) {
			p.rememberSource(cs, id)
			return cs.environment(), nil
		}
	}
	return providers.Environment{}, &providers.NotFoundError{Provider: ProviderName, ID: id}
}

// Status is Get reduced to the state.
func (p *Provider) Status(ctx context.Context, id string) (providers.State, error) {
	env, err := p.Get(ctx, id)
	if err != nil {
		if errors.Is(err, providers.ErrNotFound) {
			return providers.StateNotFound, err
		}
		return providers.StateUnknown, err
	}
	return env.State, nil
}

// Start asks GitHub to start the codespace. It is idempotent: an already
// running codespace is not an error.
func (p *Provider) Start(ctx context.Context, id string) error {
	if err := p.awaitNoPendingOperation(ctx, id); err != nil {
		return err
	}
	if err := p.api.startCodespace(ctx, id); err != nil {
		return err
	}
	p.log.Info("requested codespace start", slog.String("codespace", id))
	return nil
}

// Stop asks GitHub to stop the codespace now.
func (p *Provider) Stop(ctx context.Context, id string) error {
	if err := p.api.stopCodespace(ctx, id); err != nil {
		// Stopping something already stopped is a no-op, not a failure.
		if errors.Is(err, providers.ErrConflict) {
			return nil
		}
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 422 &&
			strings.Contains(strings.ToLower(ae.Message), "shutdown") {
			return nil
		}
		return err
	}
	p.log.Info("requested codespace stop", slog.String("codespace", id))
	return nil
}

// awaitNoPendingOperation waits out a GitHub-side operation (a rebuild or an
// export) that would make start/stop fail.
func (p *Provider) awaitNoPendingOperation(ctx context.Context, id string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		cs, err := p.api.getCodespace(ctx, id)
		if err != nil {
			return err
		}
		if !cs.PendingOperation {
			return nil
		}
		if time.Now().After(deadline) {
			return providers.Temporaryf("codespace %s still has a pending operation (%s)",
				id, cs.PendingOperationDisabledReason)
		}
		p.log.Info("waiting for a pending github operation to finish",
			slog.String("codespace", id),
			slog.String("reason", cs.PendingOperationDisabledReason))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// Create provisions a codespace. The handle in spec.Name becomes the display
// name, which is how the gateway keeps addressing it by a stable handle even
// though GitHub picks the codespace's real name.
func (p *Provider) Create(ctx context.Context, spec providers.CreateSpec) (providers.Environment, error) {
	cfg, err := mergeCreateOptions(p.cfg.Create, spec.Options)
	if err != nil {
		return providers.Environment{}, err
	}
	if cfg.Repository == "" {
		// Nothing declared: rebuild from whatever this handle was made of last
		// time, so deleting a codespace and reconnecting just works.
		if src, ok := p.recallSource(spec.Name); ok {
			cfg = src.merge(cfg)
			p.log.Info("rebuilding codespace from a remembered source",
				slog.String("handle", spec.Name),
				slog.String("repository", cfg.Repository),
				slog.String("machine", cfg.Machine))
		}
	}
	if cfg.Repository == "" {
		return providers.Environment{}, fmt.Errorf(
			"这个 codespace 不存在，也没有可用来创建的仓库：请在配置文件里设置 " +
				"github.create.repository（owner/name），或先在 github.com/codespaces 建一个 " +
				"codespace（之后 gateway 会记住它的仓库，删掉再连时自动重建）")
	}
	repo, err := p.api.getRepository(ctx, cfg.Repository)
	if err != nil {
		return providers.Environment{}, fmt.Errorf("look up repository %s: %w", cfg.Repository, err)
	}

	displayName := cfg.DisplayName
	if displayName == "" {
		displayName = spec.Name
	}
	req := createRequest{
		RepositoryID:           repo.ID,
		Ref:                    cfg.Branch,
		Location:               cfg.Location,
		Machine:                cfg.Machine,
		DevContainerPath:       cfg.DevcontainerPath,
		IdleTimeoutMinutes:     cfg.IdleTimeoutMinutes,
		RetentionPeriodMinutes: cfg.RetentionPeriodMinutes,
		DisplayName:            displayName,
	}
	p.log.Info("creating codespace",
		slog.String("repository", repo.FullName),
		slog.String("branch", cfg.Branch),
		slog.String("machine", cfg.Machine),
		slog.String("display_name", displayName))

	cs, err := p.api.createCodespace(ctx, req)
	if err != nil {
		return providers.Environment{}, err
	}
	p.log.Info("codespace created",
		slog.String("codespace", cs.Name), slog.String("state", cs.State))
	p.rememberSource(cs, spec.Name)
	return cs.environment(), nil
}

// rememberSource records what a codespace was built from, under its own name,
// its display name and any handle it was reached by.
func (p *Provider) rememberSource(cs codespace, handles ...string) {
	src := envSource{
		Repository:       cs.Repository.FullName,
		Branch:           cs.GitStatus.Ref,
		Machine:          cs.Machine.Name,
		Location:         cs.Location,
		DevcontainerPath: cs.DevContainerPath,
	}
	names := append([]string{cs.Name, cs.DisplayName}, handles...)
	p.sources.remember(src, names...)
}

// recallSource finds a remembered source for a handle, falling back to the only
// repository the gateway has ever seen.
func (p *Provider) recallSource(handle string) (envSource, bool) {
	if src, ok := p.sources.lookup(handle); ok {
		return src, true
	}
	return p.sources.any()
}

// mergeCreateOptions applies provider-specific overrides from a CreateSpec.
// Unknown keys are rejected rather than silently ignored.
func mergeCreateOptions(base CreateConfig, opts map[string]any) (CreateConfig, error) {
	out := base
	for key, raw := range opts {
		switch key {
		case "repository":
			out.Repository = fmt.Sprint(raw)
		case "branch", "ref":
			out.Branch = fmt.Sprint(raw)
		case "machine":
			out.Machine = fmt.Sprint(raw)
		case "location":
			out.Location = fmt.Sprint(raw)
		case "devcontainer_path":
			out.DevcontainerPath = fmt.Sprint(raw)
		case "display_name":
			out.DisplayName = fmt.Sprint(raw)
		case "idle_timeout_minutes":
			n, err := asInt(raw)
			if err != nil {
				return out, fmt.Errorf("option idle_timeout_minutes: %w", err)
			}
			out.IdleTimeoutMinutes = n
		case "retention_period_minutes":
			n, err := asInt(raw)
			if err != nil {
				return out, fmt.Errorf("option retention_period_minutes: %w", err)
			}
			out.RetentionPeriodMinutes = n
		default:
			return out, fmt.Errorf("unknown create option %q for the github provider", key)
		}
	}
	if out.Repository != "" && strings.Count(out.Repository, "/") != 1 {
		return out, fmt.Errorf("repository %q must be owner/name", out.Repository)
	}
	return out, nil
}

func asInt(raw any) (int, error) {
	switch v := raw.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case float64:
		return int(v), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("%q is not a number", v)
		}
		return n, nil
	}
	return 0, fmt.Errorf("%v is not a number", raw)
}

// Machines lists the machine types available for a repository. An empty target
// falls back to the configured or remembered repository, so
// `gateway codespace machines` works with no arguments.
func (p *Provider) Machines(ctx context.Context, target string) ([]providers.MachineType, error) {
	if target == "" {
		target = p.cfg.Create.Repository
	}
	if target == "" {
		if src, ok := p.recallSource(p.cfg.Codespace); ok {
			target = src.Repository
		}
	}
	if target == "" {
		return nil, fmt.Errorf("不知道要查哪个仓库的机器规格：加个参数（owner/name）" +
			"或者设置 github.create.repository")
	}
	list, err := p.api.listMachines(ctx, target)
	if err != nil {
		return nil, err
	}
	const gb = 1024 * 1024 * 1024
	out := make([]providers.MachineType, 0, len(list))
	for _, m := range list {
		note := ""
		if m.PrebuildAvailability != "" && m.PrebuildAvailability != "none" {
			note = "prebuild: " + m.PrebuildAvailability
		}
		out = append(out, providers.MachineType{
			Name:        m.Name,
			DisplayName: m.DisplayName,
			CPUs:        m.CPUs,
			MemoryGB:    float64(m.MemoryInBytes) / gb,
			StorageGB:   float64(m.StorageInBytes) / gb,
			Note:        note,
		})
	}
	return out, nil
}

// CreateSources lists repositories a codespace can be created from.
func (p *Provider) CreateSources(ctx context.Context, limit int) ([]providers.CreateSource, error) {
	repos, err := p.api.userRepositories(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]providers.CreateSource, 0, len(repos))
	for _, r := range repos {
		detail := "分支 " + r.DefaultBranch
		if r.Private {
			detail += "，私有"
		}
		if len(r.PushedAt) >= 10 {
			detail += "，最近推送 " + r.PushedAt[:10]
		}
		out = append(out, providers.CreateSource{Name: r.FullName, Detail: detail})
	}
	return out, nil
}

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
		return providers.Environment{}, fmt.Errorf(
			"cannot create a codespace: set github.create.repository (owner/name) in the config file")
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
	return cs.environment(), nil
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

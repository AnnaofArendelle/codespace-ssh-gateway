package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/gateway"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

func (a *app) cmdEnvironment(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: gateway codespace <list|select|status|stop|create|forget-host-key>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return a.envList(rest)
	case "select", "use":
		return a.envSelect(rest)
	case "status", "show", "get":
		return a.envStatus(rest)
	case "stop":
		return a.envStop(rest)
	case "create", "new":
		return a.envCreate(rest)
	case "machines", "machine":
		return a.envMachines(rest)
	case "forget-host-key":
		return a.envForgetHostKey(rest)
	default:
		return fmt.Errorf("unknown codespace subcommand %q", sub)
	}
}

// withProvider instantiates the configured provider for one CLI command.
func (a *app) withProvider(timeout time.Duration, fn func(ctx context.Context, cfg *config.Config, p providers.Provider) error) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	_, closeLog, err := a.logger(cfg, true)
	if err != nil {
		return err
	}
	defer closeLog()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	prov, err := gateway.BuildProvider(ctx, cfg, a.log, a.redact)
	if err != nil {
		return err
	}
	defer prov.Close()
	return fn(ctx, cfg, prov)
}

func (a *app) envList(args []string) error {
	fs := a.flagSet("codespace list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return a.withProvider(2*time.Minute, func(ctx context.Context, cfg *config.Config, p providers.Provider) error {
		envs, err := p.List(ctx)
		if err != nil {
			return err
		}
		if a.jsonOut {
			return a.writeJSON(envs)
		}
		if len(envs) == 0 {
			fmt.Fprintf(a.stdout, "no environments found for this token\n")
			return nil
		}
		def := p.DefaultEnvironment()
		tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "  NAME\tSTATE\tPROVIDER STATE\tIDLE\tDETAILS\n")
		for _, e := range envs {
			mark := "  "
			if e.ID == def || (e.DisplayName != "" && e.DisplayName == def) {
				mark = "* "
			}
			idle := "-"
			if e.IdleTimeout > 0 {
				idle = e.IdleTimeout.String()
			}
			fmt.Fprintf(tw, "%s%s\t%s\t%s\t%s\t%s\n",
				mark, e.ID, e.State, fieldOrDash(e.NativeState), idle, e.AttrLine())
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "\n* = default target. Change it with `gateway codespace select <name>`.\n")
		return nil
	})
}

func (a *app) envSelect(args []string) error {
	fs := a.flagSet("codespace select")
	var force bool
	fs.BoolVar(&force, "force", false, "write the name even if the provider does not know it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("usage: gateway codespace select <name>")
	}
	name := rest[0]

	return a.withProvider(2*time.Minute, func(ctx context.Context, cfg *config.Config, p providers.Provider) error {
		reg, ok := providers.Lookup(cfg.Provider)
		if !ok {
			return fmt.Errorf("unknown provider %q", cfg.Provider)
		}
		if !force {
			env, err := p.Get(ctx, name)
			switch {
			case err == nil:
				fmt.Fprintf(a.stdout, "%s is %s (provider state %s)\n", env.ID, env.State, env.NativeState)
			case errors.Is(err, providers.ErrNotFound):
				return fmt.Errorf("%s does not exist yet; use -force to select it anyway "+
					"(the gateway will create it on first connection when github.create.repository is set)", name)
			default:
				return err
			}
		}
		if err := config.Patch(cfg.Path(), []string{reg.ConfigKey, reg.DefaultEnvironmentKey}, name); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "default environment set to %s in %s\n", name, cfg.Path())
		fmt.Fprintf(a.stdout, "restart the gateway for it to take effect (`gateway stop && gateway start`)\n")
		return nil
	})
}

func (a *app) envStatus(args []string) error {
	fs := a.flagSet("codespace status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	return a.withProvider(2*time.Minute, func(ctx context.Context, cfg *config.Config, p providers.Provider) error {
		hint := ""
		if len(rest) > 0 {
			hint = rest[0]
		}
		name, err := gateway.ResolveEnvironment(ctx, p, hint, nil)
		if err != nil {
			return err
		}
		env, err := p.Get(ctx, name)
		if err != nil {
			return err
		}
		if a.jsonOut {
			return a.writeJSON(env)
		}
		tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "name\t%s\n", env.ID)
		if env.DisplayName != "" {
			fmt.Fprintf(tw, "display name\t%s\n", env.DisplayName)
		}
		fmt.Fprintf(tw, "state\t%s (provider: %s)\n", env.State, fieldOrDash(env.NativeState))
		if env.IdleTimeout > 0 {
			fmt.Fprintf(tw, "idle timeout\t%s (enforced by the provider)\n", env.IdleTimeout)
		}
		if !env.LastUsedAt.IsZero() {
			fmt.Fprintf(tw, "last used\t%s\n", env.LastUsedAt.Format(time.RFC3339))
		}
		for _, k := range sortedKeys(env.Attributes) {
			fmt.Fprintf(tw, "%s\t%s\n", strings.ReplaceAll(k, "_", " "), env.Attributes[k])
		}
		if env.WebURL != "" {
			fmt.Fprintf(tw, "web url\t%s\n", env.WebURL)
		}
		return tw.Flush()
	})
}

func (a *app) envStop(args []string) error {
	fs := a.flagSet("codespace stop")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	name := ""
	if len(rest) > 0 {
		name = rest[0]
	}

	// Prefer a running gateway so that its lifecycle view stays accurate.
	if client := newControlClient(cfg.ControlSocketPath()); client.alive() {
		if err := client.stopEnvironment(name); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "stop requested through the running gateway\n")
		return nil
	}

	return a.withProvider(5*time.Minute, func(ctx context.Context, cfg *config.Config, p providers.Provider) error {
		target, err := gateway.ResolveEnvironment(ctx, p, name, nil)
		if err != nil {
			return err
		}
		env, err := p.Get(ctx, target)
		if err != nil {
			return err
		}
		if err := p.Stop(ctx, env.ID); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "requested stop of %s\n", env.ID)
		return nil
	})
}

func (a *app) envCreate(args []string) error {
	fs := a.flagSet("codespace create")
	var options optionFlags
	fs.Var(&options, "option", "provider-specific create option, key=value (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	return a.withProvider(25*time.Minute, func(ctx context.Context, cfg *config.Config, p providers.Provider) error {
		name := p.DefaultEnvironment()
		if len(rest) > 0 {
			name = rest[0]
		}
		opts, err := options.parse()
		if err != nil {
			return err
		}
		env, err := p.Create(ctx, providers.CreateSpec{Name: name, Options: opts})
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "created %s (state %s)\n", env.ID, env.NativeState)
		if env.DisplayName != "" && env.DisplayName != env.ID {
			fmt.Fprintf(a.stdout, "display name %q keeps working as the gateway handle\n", env.DisplayName)
		}
		return nil
	})
}

// envMachines shows the real machine types a new codespace can use.
func (a *app) envMachines(args []string) error {
	fs := a.flagSet("codespace machines")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	return a.withProvider(2*time.Minute, func(ctx context.Context, cfg *config.Config, p providers.Provider) error {
		lister, ok := p.(providers.MachineLister)
		if !ok {
			return fmt.Errorf("provider %s 不区分机器规格", p.Name())
		}
		target := ""
		if len(rest) > 0 {
			target = rest[0]
		}
		machines, err := lister.Machines(ctx, target)
		if err != nil {
			return err
		}
		if a.jsonOut {
			return a.writeJSON(machines)
		}
		tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "NAME\tCPU\t内存\t存储\t说明\n")
		for _, m := range machines {
			fmt.Fprintf(tw, "%s\t%d\t%.0f GB\t%.0f GB\t%s\n",
				m.Name, m.CPUs, m.MemoryGB, m.StorageGB, m.Note)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "\n写进配置：github.create.machine: <NAME>\n")
		return nil
	})
}

func (a *app) envForgetHostKey(args []string) error {
	fs := a.flagSet("codespace forget-host-key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	return a.withProvider(time.Minute, func(ctx context.Context, cfg *config.Config, p providers.Provider) error {
		forgetter, ok := p.(interface{ ForgetHostKey(string) (int, error) })
		if !ok {
			return fmt.Errorf("provider %s does not pin environment host keys", p.Name())
		}
		name := ""
		if len(rest) > 0 {
			name = rest[0]
		}
		n, err := forgetter.ForgetHostKey(name)
		if err != nil {
			return err
		}
		if n == 0 {
			fmt.Fprintf(a.stdout, "no pinned host keys matched\n")
			return nil
		}
		fmt.Fprintf(a.stdout, "forgot %d pinned host key(s)\n", n)
		return nil
	})
}

// optionFlags collects repeated -option key=value flags.
type optionFlags []string

func (o *optionFlags) String() string { return strings.Join(*o, ",") }

func (o *optionFlags) Set(v string) error {
	if !strings.Contains(v, "=") {
		return fmt.Errorf("option %q must be key=value", v)
	}
	*o = append(*o, v)
	return nil
}

func (o *optionFlags) parse() (map[string]any, error) {
	if len(*o) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	for _, kv := range *o {
		k, v, _ := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("option %q has an empty key", kv)
		}
		out[k] = v
	}
	return out, nil
}

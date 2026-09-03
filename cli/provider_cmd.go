package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

func (a *app) cmdProvider(args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		return a.providerList(args[1:])
	default:
		return fmt.Errorf("unknown provider subcommand %q (want list)", args[0])
	}
}

func (a *app) providerList(args []string) error {
	fs := a.flagSet("provider list")
	if err := fs.Parse(args); err != nil {
		return err
	}

	selected := ""
	if cfg, err := config.Load(a.configPath); err == nil {
		selected = cfg.Provider
	}

	regs := providers.Registrations()
	if a.jsonOut {
		type row struct {
			Name      string `json:"name"`
			Summary   string `json:"summary"`
			ConfigKey string `json:"config_key"`
			Selected  bool   `json:"selected"`
		}
		out := make([]row, 0, len(regs))
		for _, r := range regs {
			out = append(out, row{r.Name, r.Summary, r.ConfigKey, r.Name == selected})
		}
		return a.writeJSON(out)
	}

	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "NAME\tCONFIG KEY\tSELECTED\tDESCRIPTION\n")
	for _, r := range regs {
		mark := ""
		if r.Name == selected {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.ConfigKey, mark, r.Summary)
	}
	return tw.Flush()
}

// providerForConfigKey finds the provider owning a top-level config section.
func providerForConfigKey(key string) (providers.Registration, bool) {
	for _, r := range providers.Registrations() {
		if r.ConfigKey == key {
			return r, true
		}
	}
	return providers.Registration{}, false
}

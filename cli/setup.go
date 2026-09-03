package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/gateway"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"

	gossh "golang.org/x/crypto/ssh"
)

const setupBanner = `
ssh-gateway setup
-----------------
This writes %s (mode 0600) and leaves you with a working
"ssh root@codespace". Press enter to accept the value in brackets.

`

// cmdSetup is the one-command path from nothing configured to a running gateway.
func (a *app) cmdSetup(args []string) error {
	fs := a.flagSet("setup")
	var autoStart bool
	fs.BoolVar(&autoStart, "start", false, "start the gateway when setup finishes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !interactive() {
		return errors.New("`gateway setup` needs a terminal; for scripts use `gateway config init` " +
			"plus `gateway config set-token`, or write the config file directly")
	}

	path := a.configPathOrDefault()
	p := newPrompter(a.stdout)
	fmt.Fprintf(a.stdout, setupBanner, path)

	reg, err := a.setupProvider(p)
	if err != nil {
		return err
	}
	if err := a.setupConfigFile(p, path, reg.Name); err != nil {
		return err
	}

	prov := a.setupToken(p, path, reg)
	if p.ended() {
		return errors.New("input ended before setup finished; run `gateway setup` again")
	}
	if prov != nil {
		defer prov.Close()
	}
	if err := a.setupEnvironment(p, path, reg, prov); err != nil {
		return err
	}
	if err := a.setupQuestions(p, path, reg); err != nil {
		return err
	}
	if err := a.setupListen(p, path); err != nil {
		return err
	}
	if err := a.setupClientAuth(p, path); err != nil {
		return err
	}

	fmt.Fprintf(a.stdout, "\nDone. Configuration: %s\n", path)
	if p.confirm("Check it against the real provider now (gateway doctor)?", true) {
		fmt.Fprintln(a.stdout)
		if err := a.cmdDoctor(nil); err != nil {
			fmt.Fprintf(a.stderr, "gateway: %s\n", err)
			if !p.confirm("Continue anyway?", false) {
				return nil
			}
		}
	}
	if autoStart || p.confirm("Start the gateway now?", true) {
		return a.cmdStart(nil)
	}
	fmt.Fprintf(a.stdout, "\nStart it later with:  gateway start\n")
	return nil
}

func (a *app) setupProvider(p *prompter) (providers.Registration, error) {
	regs := providers.Registrations()
	switch len(regs) {
	case 0:
		return providers.Registration{}, errors.New("this build has no providers compiled in")
	case 1:
		fmt.Fprintf(a.stdout, "Provider: %s (%s)\n", regs[0].Name, regs[0].Summary)
		return regs[0], nil
	}
	items := make([]menuItem, 0, len(regs))
	for _, r := range regs {
		items = append(items, menuItem{Label: r.Name, Detail: r.Summary})
	}
	return regs[p.menu("Which provider should serve SSH clients?", items, 0)], nil
}

// setupConfigFile creates the config from the template, or keeps an existing one.
func (a *app) setupConfigFile(p *prompter, path, provider string) error {
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(a.stdout, "\n%s already exists; setup will update it in place.\n", path)
		if !p.confirm("Continue?", true) {
			return errors.New("cancelled")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := config.WriteFile(path, config.Template(provider, "", "", ":2222")); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "\nCreated %s (mode 0600)\n", path)
	return nil
}

// setupToken asks how the gateway should authenticate, then proves it works by
// listing environments. It returns a live provider, or nil if the operator
// chose to configure the credential later.
func (a *app) setupToken(p *prompter, path string, reg providers.Registration) providers.Provider {
	for {
		items := []menuItem{
			{Label: "Paste a token now", Detail: "stored in " + path + " (mode 0600)"},
			{Label: "Use $GITHUB_TOKEN or $GH_TOKEN from the environment", Detail: "nothing is written to disk"},
			{Label: "Use the GitHub CLI's own login", Detail: "the gateway calls `gh auth token`"},
			{Label: "Skip for now", Detail: "you can run `gateway config set-token` later"},
		}
		switch p.menu("How should the gateway authenticate to "+reg.Name+"?", items, 0) {
		case 0:
			token := p.secret("Token (input hidden)")
			if token == "" {
				fmt.Fprintln(a.stdout, "  nothing entered")
				continue
			}
			a.redact.Add(token)
			if err := config.Patch(path, []string{reg.ConfigKey, "token"}, token); err != nil {
				fmt.Fprintf(a.stderr, "gateway: %s\n", err)
				continue
			}
		case 1:
			if os.Getenv("GITHUB_TOKEN") == "" && os.Getenv("GH_TOKEN") == "" {
				fmt.Fprintln(a.stdout, "  neither $GITHUB_TOKEN nor $GH_TOKEN is set in this shell")
				if !p.confirm("  Continue anyway?", false) {
					continue
				}
			}
		case 2:
			fmt.Fprintln(a.stdout, "  the gateway will ask `gh auth token` at startup")
		case 3:
			return nil
		}
		if p.ended() {
			return nil
		}

		prov, err := a.quietProvider(path)
		if err != nil {
			fmt.Fprintf(a.stdout, "\n  could not use that credential: %s\n", a.redact.Redact(err.Error()))
			if !p.confirm("  Try a different option?", true) {
				return nil
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		envs, err := prov.List(ctx)
		cancel()
		if err != nil {
			prov.Close()
			fmt.Fprintf(a.stdout, "\n  the provider rejected it: %s\n", a.redact.Redact(err.Error()))
			if !p.confirm("  Try a different option?", true) {
				return nil
			}
			continue
		}
		fmt.Fprintf(a.stdout, "\n  ok: authenticated, %d environment(s) visible\n", len(envs))
		return prov
	}
}

// quietProvider builds the provider from the config on disk without logging.
func (a *app) quietProvider(path string) (providers.Provider, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if _, _, err := a.logger(cfg, true); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return gateway.BuildProvider(ctx, cfg, a.log, a.redact)
}

// setupEnvironment picks the default target, offering the real list when the
// provider is reachable.
func (a *app) setupEnvironment(p *prompter, path string, reg providers.Registration, prov providers.Provider) error {
	key := []string{reg.ConfigKey, reg.DefaultEnvironmentKey}

	var envs []providers.Environment
	if prov != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		envs, _ = prov.List(ctx)
		cancel()
	}

	items := make([]menuItem, 0, len(envs)+3)
	for _, e := range envs {
		label := e.ID
		detail := fmt.Sprintf("state %s", e.State)
		if e.DisplayName != "" && e.DisplayName != e.ID {
			detail += ", display name " + e.DisplayName
		}
		if repo := e.Attributes["repository"]; repo != "" {
			detail += ", " + repo
		}
		items = append(items, menuItem{Label: label, Detail: detail})
	}
	typeIdx := len(items)
	items = append(items, menuItem{Label: "Type a name", Detail: "an existing environment, or one to create on first connection"})
	skipIdx := len(items)
	items = append(items, menuItem{Label: "Decide later", Detail: "clients must then pass root+<environment>@gateway"})

	choice := p.menu("Which environment should `ssh root@gateway` reach?", items, 0)
	switch choice {
	case skipIdx:
		return nil
	case typeIdx:
		name := p.ask("Environment name", "")
		if name == "" {
			return nil
		}
		return config.Patch(path, key, name)
	default:
		return config.Patch(path, key, envs[choice].ID)
	}
}

// setupQuestions asks whatever the provider declared it needs.
func (a *app) setupQuestions(p *prompter, path string, reg providers.Registration) error {
	for _, q := range reg.SetupQuestions {
		if q.Help != "" {
			fmt.Fprintf(a.stdout, "\n%s\n", q.Help)
		}
		answer := p.ask(q.Prompt, q.Default)
		if answer == "" {
			if q.Optional {
				continue
			}
			return fmt.Errorf("%s is required", q.Prompt)
		}
		if err := config.Patch(path, append([]string{reg.ConfigKey}, q.Key...), answer); err != nil {
			return err
		}
	}
	return nil
}

// setupListen chooses where the gateway listens.
func (a *app) setupListen(p *prompter, path string) error {
	items := []menuItem{
		{Label: "127.0.0.1:2222", Detail: "local only; reach it from elsewhere with an ssh tunnel"},
		{Label: ":2222", Detail: "all interfaces, port 2222"},
		{Label: ":22", Detail: "all interfaces, the standard ssh port (needs privileges)"},
		{Label: "Something else", Detail: "host:port"},
	}
	choice := p.menu("Where should the gateway listen?", items, 0)
	listen := items[choice].Label
	if choice == len(items)-1 {
		listen = p.ask("Listen address", ":2222")
	}
	return config.Patch(path, []string{"ssh", "listen"}, listen)
}

// setupClientAuth makes sure at least one client can log in, without ever
// leaving the gateway open.
func (a *app) setupClientAuth(p *prompter, path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	keyFile := cfg.AuthorizedKeysPath()
	if n := countAuthorizedKeys(keyFile) + len(cfg.SSH.AuthorizedKeysInline); n > 0 {
		fmt.Fprintf(a.stdout, "\nClient auth: %d key(s) already authorized (%s)\n", n, keyFile)
		if !p.confirm("Add another key?", false) {
			return nil
		}
	}

	local := localPublicKeys()
	items := make([]menuItem, 0, len(local)+3)
	for _, k := range local {
		items = append(items, menuItem{Label: "Import " + k.path, Detail: k.summary})
	}
	pasteIdx := len(items)
	items = append(items, menuItem{Label: "Paste a public key", Detail: "ssh-ed25519 AAAA... user@host"})
	passwordIdx := len(items)
	items = append(items, menuItem{Label: "Use a generated password instead", Detail: "printed once at every start; keys are safer"})
	skipIdx := len(items)
	items = append(items, menuItem{Label: "Skip", Detail: "the gateway will refuse to start until a key or password exists"})

	choice := p.menu("Which key may use this gateway?", items, 0)
	switch choice {
	case skipIdx:
		fmt.Fprintln(a.stdout, "  remember: `gateway config authorized-key add <file>`")
		return nil
	case passwordIdx:
		return config.PatchBool(path, []string{"ssh", "password_auth"}, true)
	case pasteIdx:
		line := p.ask("Public key line", "")
		if line == "" {
			return nil
		}
		return addAuthorizedKey(a.stdout, keyFile, line)
	default:
		raw, err := os.ReadFile(local[choice].path)
		if err != nil {
			return err
		}
		return addAuthorizedKey(a.stdout, keyFile, strings.TrimSpace(string(raw)))
	}
}

type localKey struct {
	path    string
	summary string
}

// localPublicKeys lists the operator's own public keys, which is what they
// almost always want to authorize.
func localPublicKeys() []localKey {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(home, ".ssh", "*.pub"))
	if err != nil {
		return nil
	}
	out := make([]localKey, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		key, comment, _, _, err := gossh.ParseAuthorizedKey(raw)
		if err != nil {
			continue
		}
		out = append(out, localKey{
			path:    path,
			summary: fmt.Sprintf("%s %s %s", key.Type(), gossh.FingerprintSHA256(key), comment),
		})
	}
	return out
}

func countAuthorizedKeys(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if _, _, _, _, err := gossh.ParseAuthorizedKey([]byte(line)); err == nil {
			n++
		}
	}
	return n
}

func addAuthorizedKey(out io.Writer, keyFile, line string) error {
	key, comment, _, _, err := gossh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return fmt.Errorf("not a valid public key: %w", err)
	}
	if err := appendLine(keyFile, line); err != nil {
		return err
	}
	fmt.Fprintf(out, "  authorized %s %s %s\n", key.Type(), gossh.FingerprintSHA256(key), comment)
	return nil
}

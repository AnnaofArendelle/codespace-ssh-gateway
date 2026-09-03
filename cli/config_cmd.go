package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/secret"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"

	gossh "golang.org/x/crypto/ssh"
)

func (a *app) cmdConfig(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: gateway config <init|show|path|set-token|authorized-key>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		return a.configInit(rest)
	case "show":
		return a.configShow(rest)
	case "path", "paths":
		return a.cmdConfigPathInfo(rest)
	case "set-token":
		return a.configSetToken(rest)
	case "authorized-key", "authorized-keys", "key":
		return a.configAuthorizedKey(rest)
	default:
		return fmt.Errorf("unknown config subcommand %q", sub)
	}
}

func (a *app) configInit(args []string) error {
	fs := a.flagSet("config init")
	var (
		tokenStdin bool
		environment,
		listen,
		provider string
		force bool
	)
	fs.BoolVar(&tokenStdin, "token-stdin", false, "read the provider token from stdin")
	fs.StringVar(&environment, "codespace", "", "default environment (codespace) name")
	fs.StringVar(&listen, "listen", "127.0.0.1:2222", "ssh listen address")
	fs.StringVar(&provider, "provider", "github", "provider to configure")
	fs.BoolVar(&force, "force", false, "overwrite an existing config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := a.configPathOrDefault()
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists (use -force to overwrite)", path)
	}
	if _, ok := providers.Lookup(provider); !ok {
		return fmt.Errorf("unknown provider %q", provider)
	}

	var token string
	if tokenStdin {
		tok, err := readSecretStdin(a.stderr)
		if err != nil {
			return err
		}
		token = tok.Reveal()
		a.redact.Add(token)
	}

	if err := config.WriteFile(path, config.Template(provider, token, environment, listen)); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "wrote %s (mode 0600)\n", path)

	imported, keyPath, err := importLocalPublicKeys(filepath.Dir(path))
	if err != nil {
		fmt.Fprintf(a.stderr, "gateway: warning: could not import local public keys: %v\n", err)
	} else if imported > 0 {
		fmt.Fprintf(a.stdout, "imported %d public key(s) from ~/.ssh into %s\n", imported, keyPath)
	}

	fmt.Fprintf(a.stdout, "\nNext steps:\n")
	if token == "" {
		fmt.Fprintf(a.stdout, "  1. store a token:   gateway config set-token   (or run `gh auth login`)\n")
	}
	if imported == 0 {
		fmt.Fprintf(a.stdout, "  2. authorize a key: gateway config authorized-key add ~/.ssh/id_ed25519.pub\n")
	}
	fmt.Fprintf(a.stdout, "  3. pick a target:   gateway codespace list && gateway codespace select <name>\n")
	fmt.Fprintf(a.stdout, "  4. verify:          gateway doctor\n")
	fmt.Fprintf(a.stdout, "  5. run:             gateway start\n")
	return nil
}

func (a *app) configPathOrDefault() string {
	if a.configPath != "" {
		return config.ExpandPath(a.configPath)
	}
	return config.DefaultConfigPath()
}

// importLocalPublicKeys seeds the authorized_keys file with the operator's own
// public keys, so a fresh install is usable but never open.
func importLocalPublicKeys(configDir string) (int, string, error) {
	dest := filepath.Join(configDir, "authorized_keys")
	if _, err := os.Stat(dest); err == nil {
		return 0, dest, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, dest, err
	}
	entries, err := filepath.Glob(filepath.Join(home, ".ssh", "*.pub"))
	if err != nil || len(entries) == 0 {
		return 0, dest, nil
	}
	var lines []string
	for _, e := range entries {
		raw, err := os.ReadFile(e)
		if err != nil {
			continue
		}
		if _, _, _, _, err := gossh.ParseAuthorizedKey(raw); err != nil {
			continue
		}
		lines = append(lines, strings.TrimSpace(string(raw)))
	}
	if len(lines) == 0 {
		return 0, dest, nil
	}
	content := "# imported from ~/.ssh by `gateway config init`\n" + strings.Join(lines, "\n") + "\n"
	if err := config.WriteFile(dest, content); err != nil {
		return 0, dest, err
	}
	return len(lines), dest, nil
}

func (a *app) configShow(args []string) error {
	fs := a.flagSet("config show")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := a.configPathOrDefault()
	redacted, err := config.RedactedFile(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "# %s\n%s", path, redacted)
	return nil
}

func (a *app) cmdConfigPathInfo(args []string) error {
	fs := a.flagSet("config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := a.configPathOrDefault()
	cfg, err := config.Load(path)
	if err != nil {
		// Still useful: show where things would go.
		fmt.Fprintf(a.stdout, "config     %s (not present)\n", path)
		fmt.Fprintf(a.stdout, "state dir  %s\n", config.DefaultStateDir())
		return nil
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "config\t%s\n", cfg.Path())
	fmt.Fprintf(tw, "state dir\t%s\n", cfg.StateDir)
	fmt.Fprintf(tw, "host key\t%s\n", cfg.HostKeyPath())
	fmt.Fprintf(tw, "authorized keys\t%s\n", cfg.AuthorizedKeysPath())
	fmt.Fprintf(tw, "control socket\t%s\n", cfg.ControlSocketPath())
	fmt.Fprintf(tw, "provider state\t%s\n", cfg.ProviderStateDir(cfg.Provider))
	return tw.Flush()
}

func (a *app) configSetToken(args []string) error {
	fs := a.flagSet("config set-token")
	var providerName string
	fs.StringVar(&providerName, "provider", "", "provider to set the token for (default: configured provider)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := a.configPathOrDefault()
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if providerName == "" {
		providerName = cfg.Provider
	}
	reg, ok := providers.Lookup(providerName)
	if !ok {
		return fmt.Errorf("unknown provider %q", providerName)
	}

	tok, err := readSecretStdin(a.stderr)
	if err != nil {
		return err
	}
	if tok.Empty() {
		return fmt.Errorf("no token read from stdin")
	}
	a.redact.Add(tok.Reveal())
	if err := config.Patch(path, []string{reg.ConfigKey, "token"}, tok.Reveal()); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "stored a %d character token in %s (%s.token)\n", tok.Len(), path, reg.ConfigKey)
	return nil
}

// readSecretStdin reads a credential from stdin without echoing it back.
func readSecretStdin(warn io.Writer) (secret.Value, error) {
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprintln(warn, "reading token from the terminal; it will be visible as you type.")
		fmt.Fprintln(warn, "paste the token and press Enter (or pipe it in: echo $TOKEN | gateway config set-token):")
	}
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return secret.Value{}, fmt.Errorf("read token from stdin: %w", err)
	}
	return secret.New(line), nil
}

func (a *app) configAuthorizedKey(args []string) error {
	fs := a.flagSet("config authorized-key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	action := "list"
	if len(rest) > 0 {
		action = rest[0]
	}
	path := a.configPathOrDefault()
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	keyFile := cfg.AuthorizedKeysPath()

	switch action {
	case "list":
		raw, err := os.ReadFile(keyFile)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(a.stdout, "no authorized_keys file at %s\n", keyFile)
				if len(cfg.SSH.AuthorizedKeysInline) > 0 {
					fmt.Fprintf(a.stdout, "%d inline key(s) in the config file\n", len(cfg.SSH.AuthorizedKeysInline))
				}
				return nil
			}
			return err
		}
		fmt.Fprintf(a.stdout, "# %s\n", keyFile)
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, comment, _, _, err := gossh.ParseAuthorizedKey([]byte(line))
			if err != nil {
				fmt.Fprintf(a.stdout, "  (unparseable) %s\n", line)
				continue
			}
			fmt.Fprintf(a.stdout, "  %s %s %s\n", key.Type(), gossh.FingerprintSHA256(key), comment)
		}
		return nil

	case "add":
		if len(rest) < 2 {
			return fmt.Errorf("usage: gateway config authorized-key add <path-to-pub-key|key-line>")
		}
		arg := strings.Join(rest[1:], " ")
		line := arg
		if raw, err := os.ReadFile(config.ExpandPath(arg)); err == nil {
			line = strings.TrimSpace(string(raw))
		}
		key, comment, _, _, err := gossh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return fmt.Errorf("not a valid public key: %w", err)
		}
		if err := appendLine(keyFile, line); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "added %s %s %s to %s\n",
			key.Type(), gossh.FingerprintSHA256(key), comment, keyFile)
		return nil

	default:
		return fmt.Errorf("unknown authorized-key action %q (want list or add)", action)
	}
}

func appendLine(path, line string) error {
	if err := config.EnsureDir(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.TrimSpace(line) + "\n")
	return err
}

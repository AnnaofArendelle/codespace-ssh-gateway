// Package cli implements the gateway command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/logging"
)

// Version is the build version, overridable with -ldflags.
var Version = "0.1.0"

type app struct {
	configPath string
	logLevel   string
	jsonOut    bool
	stdout     io.Writer
	stderr     io.Writer
	redact     *logging.Redactor
	log        *slog.Logger
}

const usage = `ssh-gateway - use a cloud development environment as a plain SSH server.

Usage:
  gateway [flags] <command> [args]

Commands:
  setup                     Interactive first-time setup (menu driven)
  start                     Run the gateway in the foreground (default command)
  stop                      Stop the running gateway
  status                    Show the running gateway, or the configuration if it is not running
  doctor                    Check the configuration against the real provider
  config init               Write a starter config file
  config show               Print the effective configuration (token redacted)
  config path               Print the config and state paths
  config set-token          Read a token from stdin and store it
  config authorized-key     Add or list gateway client keys
  provider list             List compiled-in providers
  codespace list            List environments known to the provider
  codespace select <name>   Set the default environment
  codespace status [name]   Show one environment's provider state
  codespace stop [name]     Stop an environment now
  codespace create [name]   Create an environment from the configured template
  codespace forget-host-key [name]
                            Drop pinned codespace host keys
  version                   Print the version

Flags:
  -config <path>   Config file (default %s)
  -log-level <l>   debug | info | warn | error
  -json            Machine-readable output where supported

Environments can also be chosen per connection:
  ssh root+<environment>@gateway
  ssh -o SetEnv=GATEWAY_ENV=<environment> root@gateway
`

// Run executes one CLI invocation and returns a process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	a := &app{stdout: stdout, stderr: stderr, redact: logging.NewRedactor()}

	globals := a.flagSet("gateway")
	globals.Usage = func() { a.printUsage() }
	if err := globals.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := globals.Args()
	if len(rest) == 0 {
		// Bare `gateway` starts the server; with nothing configured yet it
		// walks the operator through setup first.
		return a.exit(a.startOrSetup())
	}

	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "help", "-h", "--help":
		a.printUsage()
		return 0
	case "version":
		fmt.Fprintf(a.stdout, "ssh-gateway %s\n", Version)
		return 0
	case "setup", "wizard":
		return a.exit(a.cmdSetup(cmdArgs))
	case "start", "serve", "run":
		return a.exit(a.cmdStart(cmdArgs))
	case "stop":
		return a.exit(a.cmdStop(cmdArgs))
	case "status":
		return a.exit(a.cmdStatus(cmdArgs))
	case "doctor":
		return a.exit(a.cmdDoctor(cmdArgs))
	case "config":
		return a.exit(a.cmdConfig(cmdArgs))
	case "provider", "providers":
		return a.exit(a.cmdProvider(cmdArgs))
	case "codespace", "environment", "env":
		return a.exit(a.cmdEnvironment(cmdArgs))
	default:
		fmt.Fprintf(a.stderr, "gateway: unknown command %q\n\n", cmd)
		a.printUsage()
		return 2
	}
}

func (a *app) printUsage() {
	fmt.Fprintf(a.stdout, usage, config.DefaultConfigPath())
}

// flagSet returns a flag set carrying the global flags, so they work before or
// after the subcommand.
func (a *app) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.StringVar(&a.configPath, "config", a.configPath, "path to the config file")
	fs.StringVar(&a.logLevel, "log-level", a.logLevel, "log level: debug|info|warn|error")
	fs.BoolVar(&a.jsonOut, "json", a.jsonOut, "machine-readable output")
	return fs
}

func (a *app) exit(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	msg := a.redact.Redact(err.Error())
	fmt.Fprintf(a.stderr, "gateway: %s\n", msg)
	if errors.Is(err, config.ErrNoConfig) {
		fmt.Fprintf(a.stderr, "\nRun `gateway setup` (interactive) or `gateway config init`.\n")
	}
	return 1
}

// startOrSetup is what a bare `gateway` does: serve if configured, otherwise
// offer to configure first. This is the one-command install path.
func (a *app) startOrSetup() error {
	path := a.configPathOrDefault()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if interactive() {
			return a.cmdSetup(nil)
		}
		return fmt.Errorf("%w at %s (run `gateway setup`)", config.ErrNoConfig, path)
	}
	return a.cmdStart(nil)
}

// loadConfig reads the config file and applies CLI overrides.
func (a *app) loadConfig() (*config.Config, error) {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return nil, err
	}
	if a.logLevel != "" {
		cfg.Log.Level = a.logLevel
	}
	for _, w := range cfg.Warnings() {
		fmt.Fprintf(a.stderr, "gateway: warning: %s\n", w)
	}
	if err := a.warnUnknownSections(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (a *app) warnUnknownSections(cfg *config.Config) error {
	for _, key := range cfg.SectionKeys() {
		if _, ok := providerForConfigKey(key); !ok {
			fmt.Fprintf(a.stderr,
				"gateway: warning: config section %q does not belong to any compiled-in provider\n", key)
		}
	}
	return nil
}

// logger builds the logger for commands that instantiate a provider. CLI
// commands log to stderr at warn level unless asked otherwise, so their own
// output stays readable.
func (a *app) logger(cfg *config.Config, quiet bool) (*logging.Redactor, func(), error) {
	opts := logging.Options{Level: cfg.Log.Level, Format: cfg.Log.Format, File: cfg.Log.File}
	if quiet && a.logLevel == "" {
		opts.Level = "warn"
		opts.File = ""
	}
	logger, closer, err := logging.New(opts, a.redact)
	if err != nil {
		return nil, nil, err
	}
	a.log = logger
	return a.redact, func() { _ = closer.Close() }, nil
}

func fieldOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

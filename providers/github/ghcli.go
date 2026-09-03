package github

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/secret"
)

// ghCLI wraps the official GitHub CLI. The gateway shells out to `gh` rather
// than reimplementing the Codespaces connection protocol (Dev Tunnels + the
// internal RPC that starts sshd inside the container).
type ghCLI struct {
	path  string
	log   *slog.Logger
	token func() string
	// apiURL is passed to gh as GITHUB_API_URL when it is not the public API.
	apiURL string
	// extraEnv and commandContext are test seams; production leaves them nil.
	extraEnv       []string
	commandContext func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// ghCandidates are the usual install locations, tried when gh is not on PATH.
var ghCandidates = []string{
	"/usr/bin/gh", "/usr/local/bin/gh", "/opt/homebrew/bin/gh",
	"/home/linuxbrew/.linuxbrew/bin/gh", "/snap/bin/gh",
}

// findGH locates the gh binary. An empty configured path means "search".
func findGH(configured string) (string, error) {
	if configured != "" {
		p := configured
		if !filepath.IsAbs(p) {
			resolved, err := exec.LookPath(p)
			if err != nil {
				return "", fmt.Errorf("github.gh_path %q not found: %w", configured, err)
			}
			p = resolved
		}
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return "", fmt.Errorf("github.gh_path %q is not an executable file", configured)
		}
		return p, nil
	}
	if p, err := exec.LookPath("gh"); err == nil {
		return p, nil
	}
	for _, cand := range ghCandidates {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	return "", fmt.Errorf("the GitHub CLI (gh) was not found on PATH; install it from https://cli.github.com " +
		"or set github.gh_path")
}

func (g *ghCLI) Available() bool { return g != nil && g.path != "" }

func (g *ghCLI) Path() string {
	if g == nil {
		return ""
	}
	return g.path
}

// command builds a gh invocation. Arguments are always passed as separate argv
// entries: no shell is involved anywhere in this package.
func (g *ghCLI) command(ctx context.Context, args ...string) *exec.Cmd {
	newCmd := g.commandContext
	if newCmd == nil {
		newCmd = exec.CommandContext
	}
	cmd := newCmd(ctx, g.path, args...)

	env := os.Environ()
	// Drop inherited GitHub credentials so the gateway's own token is the only
	// identity in play.
	filtered := env[:0]
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GH_TOKEN="),
			strings.HasPrefix(kv, "GITHUB_TOKEN="),
			strings.HasPrefix(kv, "GH_ENTERPRISE_TOKEN="),
			strings.HasPrefix(kv, "GITHUB_API_URL="):
			continue
		}
		filtered = append(filtered, kv)
	}
	env = filtered
	if g.token != nil {
		if tok := g.token(); tok != "" {
			env = append(env, "GH_TOKEN="+tok, "GITHUB_TOKEN="+tok)
		}
	}
	if g.apiURL != "" && g.apiURL != defaultAPIURL {
		env = append(env, "GITHUB_API_URL="+g.apiURL)
	}
	env = append(env,
		"NO_COLOR=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_PROMPT_DISABLED=1",
		"CLICOLOR=0",
	)
	env = append(env, g.extraEnv...)
	cmd.Env = env
	return cmd
}

// scrub removes the token from text that may end up in an error or a log.
func (g *ghCLI) scrub(s string) string {
	if g == nil || g.token == nil {
		return s
	}
	if tok := g.token(); len(tok) >= 8 {
		s = strings.ReplaceAll(s, tok, "***")
	}
	return s
}

const maxCapturedOutput = 64 << 10

// run executes gh and returns stdout. stderr is captured for diagnostics.
func (g *ghCLI) run(ctx context.Context, args ...string) (string, error) {
	if !g.Available() {
		return "", fmt.Errorf("the GitHub CLI (gh) is not available")
	}
	cmd := g.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = nil

	g.log.Debug("running gh", slog.Any("args", args))
	err := cmd.Run()
	out := stdout.String()
	if len(out) > maxCapturedOutput {
		out = out[:maxCapturedOutput]
	}
	if err != nil {
		msg := strings.TrimSpace(g.scrub(stderr.String()))
		if len(msg) > 2000 {
			msg = msg[:2000] + "..."
		}
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out, nil
}

// AuthToken asks the GitHub CLI for the token it is already signed in with,
// rather than reimplementing its credential storage.
func (g *ghCLI) AuthToken(ctx context.Context) (secret.Value, error) {
	out, err := g.run(ctx, "auth", "token")
	if err != nil {
		return secret.Value{}, err
	}
	return secret.New(out), nil
}

// Version returns the first line of `gh --version`.
func (g *ghCLI) Version(ctx context.Context) (string, error) {
	out, err := g.run(ctx, "--version")
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return line, nil
}

// sshTarget is what the gateway needs to speak SSH to a codespace over the
// tunnel that gh provides.
type sshTarget struct {
	// User is the remote login inside the container (vscode, node, codespace...).
	User string
	// IdentityFile is the key gh would use; the gateway passes its own instead.
	IdentityFile string
	// StdioSupported reports whether this gh knows `--stdio`.
	StdioSupported bool
}

// probeSSHTarget runs `gh codespace ssh --config`, the CLI's own documented way
// to describe how OpenSSH should reach a codespace, and reads the remote user
// out of it.
func (g *ghCLI) probeSSHTarget(ctx context.Context, name string) (sshTarget, error) {
	out, err := g.run(ctx, "codespace", "ssh", "-c", name, "--config")
	if err != nil {
		return sshTarget{}, err
	}
	target := parseSSHConfig(out)
	if target.User == "" {
		return sshTarget{}, fmt.Errorf("gh codespace ssh --config did not report an ssh user for %s", name)
	}
	return target, nil
}

func parseSSHConfig(out string) sshTarget {
	var t sshTarget
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, rest, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		rest = strings.TrimSpace(rest)
		switch strings.ToLower(key) {
		case "user":
			t.User = rest
		case "identityfile":
			t.IdentityFile = strings.Trim(rest, `"`)
		case "proxycommand":
			if strings.Contains(rest, "--stdio") {
				t.StdioSupported = true
			}
		}
	}
	return t
}

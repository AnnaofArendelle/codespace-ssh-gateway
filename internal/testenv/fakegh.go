package testenv

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Environment variables that turn a test binary into a stub `gh`.
const (
	EnvFakeGH      = "GATEWAY_FAKE_GH"       // "1" activates stub mode
	EnvSSHDAddr    = "GATEWAY_FAKE_GH_SSHD"  // host:port of the fake codespace sshd
	EnvSSHUser     = "GATEWAY_FAKE_GH_USER"  // remote user reported by --config
	EnvGHToken     = "GATEWAY_FAKE_GH_TOKEN" // answer for `gh auth token`
	EnvNoStdio     = "GATEWAY_FAKE_GH_NO_STDIO"
	EnvConfigFail  = "GATEWAY_FAKE_GH_CONFIG_FAIL"
	EnvStdioDelay  = "GATEWAY_FAKE_GH_STDIO_DELAY"
	EnvGHFailure   = "GATEWAY_FAKE_GH_FAIL"
	EnvArgsLogPath = "GATEWAY_FAKE_GH_ARGS_LOG"
)

// FakeGHMain turns the current process into a stub `gh` when it was started as
// one. Test binaries call it first thing in TestMain:
//
//	func TestMain(m *testing.M) { testenv.FakeGHMain(); os.Exit(m.Run()) }
//
// The gateway then runs the test binary as its gh, so the real subprocess
// handling, argument construction and stdio transport are all exercised.
func FakeGHMain() {
	if os.Getenv(EnvFakeGH) != "1" {
		return
	}
	os.Exit(runFakeGH(os.Args[1:]))
}

func runFakeGH(args []string) int {
	if path := os.Getenv(EnvArgsLogPath); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(f, strings.Join(args, " "))
			f.Close()
		}
	}
	if msg := os.Getenv(EnvGHFailure); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		return 1
	}
	switch {
	case len(args) == 1 && args[0] == "--version":
		fmt.Println("gh version 2.99.0 (fake)")
		fmt.Println("https://github.com/cli/cli/releases/tag/v2.99.0")
		return 0
	case len(args) == 2 && args[0] == "auth" && args[1] == "token":
		tok := os.Getenv(EnvGHToken)
		if tok == "" {
			fmt.Fprintln(os.Stderr, "not logged in")
			return 1
		}
		fmt.Println(tok)
		return 0
	case len(args) >= 2 && args[0] == "codespace" && args[1] == "ssh":
		return fakeCodespaceSSH(args[2:])
	}
	fmt.Fprintf(os.Stderr, "unknown command: %s\n", strings.Join(args, " "))
	return 1
}

type ghSSHArgs struct {
	codespace string
	config    bool
	stdio     bool
	passthru  []string
}

func parseGHSSHArgs(args []string) ghSSHArgs {
	var out ghSSHArgs
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c", "--codespace":
			if i+1 < len(args) {
				i++
				out.codespace = args[i]
			}
		case "--config":
			out.config = true
		case "--stdio":
			out.stdio = true
		case "--":
			out.passthru = append(out.passthru, args[i+1:]...)
			return out
		default:
			out.passthru = append(out.passthru, args[i])
		}
	}
	return out
}

func fakeCodespaceSSH(args []string) int {
	parsed := parseGHSSHArgs(args)
	user := os.Getenv(EnvSSHUser)
	if user == "" {
		user = "vscode"
	}

	if parsed.config {
		if os.Getenv(EnvConfigFail) == "1" {
			fmt.Fprintln(os.Stderr, "error getting ssh server details: could not start ssh server")
			return 1
		}
		fmt.Printf("Host cs.%s.main\n", parsed.codespace)
		fmt.Printf("\tUser %s\n", user)
		fmt.Printf("\tProxyCommand gh cs ssh -c %s --stdio -- -i ~/.ssh/codespaces.auto\n", parsed.codespace)
		fmt.Printf("\tUserKnownHostsFile=/dev/null\n\tStrictHostKeyChecking no\n\tLogLevel quiet\n")
		fmt.Printf("\tControlMaster auto\n\tIdentityFile ~/.ssh/codespaces.auto\n\n")
		return 0
	}

	if parsed.stdio {
		if os.Getenv(EnvNoStdio) == "1" {
			fmt.Fprintln(os.Stderr, "unknown flag: --stdio")
			return 1
		}
		if parsed.codespace == "" {
			fmt.Fprintln(os.Stderr, "`--stdio` requires explicit `--codespace`")
			return 1
		}
		return proxyStdio()
	}
	return execRealSSH(user, parsed.passthru)
}

// proxyStdio mirrors what `gh codespace ssh --stdio` does: pipe stdin/stdout to
// the codespace's SSH server.
func proxyStdio() int {
	if d, err := time.ParseDuration(os.Getenv(EnvStdioDelay)); err == nil && d > 0 {
		time.Sleep(d)
	}
	addr := os.Getenv(EnvSSHDAddr)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect to forwarded port: %v\n", err)
		return 1
	}
	defer conn.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(os.Stdout, conn)
		done <- struct{}{}
	}()
	<-done
	// Real gh always reports a closed tunnel as an error.
	fmt.Fprintln(os.Stderr, "tunnel closed")
	return 1
}

// execRealSSH is the exec-connector path: gh runs the system ssh client against
// the forwarded port. Here it points at the fake codespace sshd.
func execRealSSH(user string, passthru []string) int {
	addr := os.Getenv(EnvSSHDAddr)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad sshd address %q\n", addr)
		return 1
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		fmt.Fprintln(os.Stderr, "no ssh binary available")
		return 1
	}
	args := []string{
		"-o", "NoHostAuthenticationForLocalhost=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "PasswordAuthentication=no",
		"-o", "LogLevel=ERROR",
		"-p", port,
	}
	args = append(args, passthru...)
	// Insert the destination before any trailing command, the way gh does.
	dest := user + "@" + host
	flags, command := splitSSHFlags(args)
	final := append(flags, dest)
	final = append(final, command...)

	cmd := exec.Command(sshPath, final...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "shell closed: %v\n", err)
		return 1
	}
	return 0
}

// splitSSHFlags separates ssh flags from a trailing remote command, using the
// same knowledge of which flags take a value that the ssh CLI has.
func splitSSHFlags(args []string) (flags, command []string) {
	const unary = "bcDEeFIiJLlmOopQRSWw"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") && len(arg) > 1 {
			flags = append(flags, arg)
			if len(arg) == 2 && strings.ContainsRune(unary, rune(arg[1])) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		return flags, args[i:]
	}
	return flags, nil
}

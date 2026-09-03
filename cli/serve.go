package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/gateway"
)

// cmdStart runs the gateway in the foreground.
func (a *app) cmdStart(args []string) error {
	fs := a.flagSet("start")
	var listen string
	fs.StringVar(&listen, "listen", "", "override ssh.listen (host:port)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if listen != "" {
		cfg.SSH.Listen = listen
	}

	_, closeLog, err := a.logger(cfg, false)
	if err != nil {
		return err
	}
	defer closeLog()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gw, err := gateway.New(ctx, cfg, a.log, a.redact)
	if err != nil {
		return err
	}
	defer gw.Close()

	if err := gw.Listen(); err != nil {
		return err
	}
	a.ensureSSHAlias(cfg)
	a.printBanner(gw, cfg)
	return gw.Run(ctx)
}

// ensureSSHAlias keeps ~/.ssh/config in step with the gateway so that
// `ssh root@codespace` is always the shortest way in. It never overwrites an
// entry it does not own, and ssh.install_alias: false turns it off.
func (a *app) ensureSSHAlias(cfg *config.Config) {
	if !cfg.AliasEnabled() {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".ssh", "config")
	block := sshConfigBlock(DefaultSSHHost, hostForConfig(cfg.SSH.Listen), listenPort(cfg.SSH.Listen))
	action, err := patchSSHConfig(path, block, DefaultSSHHost)
	switch {
	case err != nil:
		fmt.Fprintf(a.stderr, "gateway: 没有更新 %s：%s\n", path, err)
	case action == "已写入", action == "已更新":
		fmt.Fprintf(a.stdout, "%s %s：ssh root@%s 现在可用\n", action, path, DefaultSSHHost)
	}
}

// printBanner tells the operator exactly how to connect, which is the whole
// point of the gateway.
func (a *app) printBanner(gw *gateway.Gateway, cfg *config.Config) {
	st := gw.Status()
	out := a.stdout
	fmt.Fprintf(out, "\nssh-gateway %s 已启动，监听 %s\n\n", Version, st.Listen)

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "  provider\t%s\n", st.Provider)
	fmt.Fprintf(tw, "  目标 codespace\t%s\n", fieldOrDash(st.DefaultEnvironment))
	fmt.Fprintf(tw, "  客户端认证\t%s\n", st.ClientAuth)
	fmt.Fprintf(tw, "  host key\t%s\n", st.HostKeyFingerprint)
	fmt.Fprintf(tw, "  停机策略\t%s\n", idleSummary(st))
	if cfg.Path() != "" {
		fmt.Fprintf(tw, "  配置文件\t%s\n", cfg.Path())
	}
	tw.Flush()

	if pw, ok := gw.GeneratedPassword(); ok {
		fmt.Fprintf(out, "\n  本次生成的 ssh 密码（只显示一次）：%s\n", pw)
		fmt.Fprintf(out, "  想固定下来就在配置里写 ssh.password。\n")
	}

	port := listenPort(st.Listen)
	fmt.Fprintf(out, "\n现在就能连：\n  ssh -p %s root@%s\n", port, hostForConfig(st.Listen))
	fmt.Fprintf(out, "\n想直接用 `ssh root@codespace`，把这段加进 ~/.ssh/config：\n")
	fmt.Fprintf(out, "  Host codespace\n    HostName %s\n    Port %s\n    User root\n\n",
		hostForConfig(st.Listen), port)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func idleSummary(st gateway.Status) string {
	if st.StopOnLastDisconnect {
		return "断开最后一个会话即调用 Provider.Stop()（stop_on_last_disconnect: true）"
	}
	if !st.Capabilities.ProviderManagedIdle {
		return "provider 不管理 idle 状态"
	}
	return fmt.Sprintf("交给 %s（ssh 是否算活跃：%s；gateway 自己没有计时器）",
		st.Capabilities.IdleMechanism, st.Capabilities.SSHActivityAttribution)
}

func hostForConfig(listen string) string {
	host, _, found := strings.Cut(listen, ":")
	if host == "127.0.0.1" || host == "localhost" || host == "[::1]" {
		return "127.0.0.1"
	}
	if !found || host == "" || host == "0.0.0.0" || host == "[::]" {
		if h, err := os.Hostname(); err == nil && h != "" {
			return h
		}
		return "localhost"
	}
	return host
}

// cmdStop stops a running gateway through the control socket, falling back to
// a signal via the pid file.
func (a *app) cmdStop(args []string) error {
	fs := a.flagSet("stop")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	client := newControlClient(cfg.ControlSocketPath())
	if client.alive() {
		if err := client.stop(); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "gateway stopping")
		return nil
	}

	pid, err := readPID(gateway.PIDFile(cfg.StateDir))
	if err != nil {
		return fmt.Errorf("no running gateway found (no control socket at %s)", cfg.ControlSocketPath())
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	fmt.Fprintf(a.stdout, "sent SIGTERM to pid %d\n", pid)
	return nil
}

func readPID(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err != nil || pid <= 0 {
		return 0, fmt.Errorf("unreadable pid file %s", path)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, fmt.Errorf("pid %d is not running", pid)
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return 0, fmt.Errorf("pid %d is not running: %w", pid, err)
	}
	return pid, nil
}

// cmdStatus reports a running gateway, or the static configuration if none is
// running.
func (a *app) cmdStatus(args []string) error {
	fs := a.flagSet("status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	client := newControlClient(cfg.ControlSocketPath())
	if client.alive() {
		st, err := client.status()
		if err != nil {
			return err
		}
		if a.jsonOut {
			return a.writeJSON(st)
		}
		a.printStatus(*st, true)
		return nil
	}

	// Not running: describe what a start would do, without touching the network.
	if a.jsonOut {
		return a.writeJSON(map[string]any{
			"running":            false,
			"config_path":        cfg.Path(),
			"listen":             cfg.SSH.Listen,
			"provider":           cfg.Provider,
			"state_dir":          cfg.StateDir,
			"auto_create":        cfg.Lifecycle.AutoCreate,
			"stop_on_disconnect": cfg.Lifecycle.StopOnLastDisconnect,
			"control_socket":     cfg.ControlSocketPath(),
		})
	}
	fmt.Fprintf(a.stdout, "gateway is not running (no control socket at %s)\n\n", cfg.ControlSocketPath())
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "  config\t%s\n", cfg.Path())
	fmt.Fprintf(tw, "  provider\t%s\n", cfg.Provider)
	fmt.Fprintf(tw, "  listen\t%s\n", cfg.SSH.Listen)
	fmt.Fprintf(tw, "  state dir\t%s\n", cfg.StateDir)
	fmt.Fprintf(tw, "  auto create\t%s\n", onOff(cfg.Lifecycle.AutoCreate))
	fmt.Fprintf(tw, "  stop on last disconnect\t%s\n", onOff(cfg.Lifecycle.StopOnLastDisconnect))
	tw.Flush()
	fmt.Fprintf(a.stdout, "\nRun `gateway doctor` to check it against the provider.\n")
	return nil
}

func (a *app) printStatus(st gateway.Status, running bool) {
	out := a.stdout
	fmt.Fprintf(out, "gateway is running (uptime %s)\n\n", st.Uptime)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "  listen\t%s\n", st.Listen)
	fmt.Fprintf(tw, "  provider\t%s (%s)\n", st.Provider, st.ProviderSummary)
	for _, k := range sortedKeys(st.ProviderInfo) {
		fmt.Fprintf(tw, "    %s\t%s\n", k, st.ProviderInfo[k])
	}
	fmt.Fprintf(tw, "  default environment\t%s\n", fieldOrDash(st.DefaultEnvironment))
	fmt.Fprintf(tw, "  host key\t%s\n", st.HostKeyFingerprint)
	fmt.Fprintf(tw, "  sessions\t%d\n", len(st.Sessions))
	fmt.Fprintf(tw, "  idle handling\t%s\n", idleSummary(st))
	fmt.Fprintf(tw, "  stop on last disconnect\t%s\n", onOff(st.StopOnLastDisconnect))
	tw.Flush()

	if len(st.Environments) > 0 {
		fmt.Fprintf(out, "\nEnvironments:\n")
		tw = tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "  HANDLE\tPHASE\tPROVIDER STATE\tSTARTS\tCONNECTS\tSINCE\n")
		for _, e := range st.Environments {
			since := "-"
			if !e.Since.IsZero() {
				since = time.Since(e.Since).Round(time.Second).String()
			}
			name := e.Environment
			if e.Resolved != "" && e.Resolved != e.Environment {
				name = fmt.Sprintf("%s -> %s", e.Environment, e.Resolved)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%d\t%s\n",
				name, e.Phase, fieldOrDash(e.NativeState), e.Starts, e.Connects, since)
		}
		tw.Flush()
		for _, e := range st.Environments {
			if e.LastError != "" {
				fmt.Fprintf(out, "  last error (%s): %s\n", e.Environment, e.LastError)
			}
		}
	}

	if len(st.Sessions) > 0 {
		fmt.Fprintf(out, "\nSessions:\n")
		tw = tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "  ID\tUSER\tENVIRONMENT\tKIND\tPTY\tFROM\tAGE\n")
		for _, s := range st.Sessions {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%v\t%s\t%s\n",
				s.ID, s.User, s.Environment, s.Kind, s.PTY, s.RemoteAddr,
				time.Since(s.StartedAt).Round(time.Second))
		}
		tw.Flush()
	}

	if len(st.Capabilities.Notes) > 0 {
		fmt.Fprintf(out, "\nProvider notes:\n")
		for _, n := range st.Capabilities.Notes {
			fmt.Fprintf(out, "  - %s\n", n)
		}
	}
}

func (a *app) writeJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

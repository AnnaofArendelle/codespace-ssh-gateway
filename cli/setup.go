package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/gateway"
	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

const setupBanner = `
ssh-gateway 首次配置
--------------------
配置文件：%s（权限 0600）
直接回车即接受方括号里的默认值。

`

// cmdSetup is the whole first-run experience: a token, and at most one more
// question. Everything else has a working default.
func (a *app) cmdSetup(args []string) error {
	fs := a.flagSet("setup")
	var autoStart bool
	fs.BoolVar(&autoStart, "start", false, "配置完成后直接启动")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !interactive() {
		return errors.New("gateway setup 需要交互式终端；脚本里请用 `gateway config init` " +
			"再把 token 写进配置文件")
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
		return errors.New("输入已结束，配置未完成；重新运行 `gateway setup` 即可")
	}
	if prov != nil {
		defer prov.Close()
		if err := a.setupEnvironment(p, path, reg, prov); err != nil {
			return err
		}
	}

	fmt.Fprintf(a.stdout, "\n配置完成：%s\n", path)
	a.setupSSHAlias(p)
	a.setupHints(path)
	if autoStart || p.confirm("\n现在启动 gateway？", true) {
		fmt.Fprintln(a.stdout)
		return a.cmdStart(nil)
	}
	fmt.Fprintf(a.stdout, "\n之后用 `gateway start` 启动。\n")
	return nil
}

// setupSSHAlias offers to make `ssh root@codespace` work, which is the point of
// the whole thing.
func (a *app) setupSSHAlias(p *prompter) {
	if !p.confirm(fmt.Sprintf("把 `Host %s` 写进 ~/.ssh/config，以后直接 `ssh root@%s`？",
		DefaultSSHHost, DefaultSSHHost), true) {
		return
	}
	if err := a.cmdSSHConfig([]string{"-write"}); err != nil {
		fmt.Fprintf(a.stderr, "gateway: %s\n", err)
		fmt.Fprintf(a.stdout, "  可以之后手动运行 `gateway ssh-config -write`\n")
	}
}

// setupHints explains how to connect, including whether a credential is needed.
func (a *app) setupHints(path string) {
	cfg, err := config.Load(path)
	if err != nil {
		return
	}
	port := listenPort(cfg.SSH.Listen)
	if gateway.LoopbackListen(cfg.SSH.Listen) {
		fmt.Fprintf(a.stdout, "只监听本机 %s，本机连接不需要密钥或密码。\n", cfg.SSH.Listen)
	} else {
		fmt.Fprintf(a.stdout, "监听 %s（对外开放），请先配置公钥或密码。\n", cfg.SSH.Listen)
	}
	fmt.Fprintf(a.stdout, "\n连接方式：\n  ssh root@%s\n  ssh -p %s root@127.0.0.1   （等价写法）\n",
		DefaultSSHHost, port)
}

func (a *app) setupProvider(p *prompter) (providers.Registration, error) {
	regs := providers.Registrations()
	switch len(regs) {
	case 0:
		return providers.Registration{}, errors.New("这个构建没有编译进任何 provider")
	case 1:
		fmt.Fprintf(a.stdout, "Provider：%s\n", regs[0].Name)
		return regs[0], nil
	}
	items := make([]menuItem, 0, len(regs))
	for _, r := range regs {
		items = append(items, menuItem{Label: r.Name, Detail: r.Summary})
	}
	return regs[p.menu("用哪个 provider？", items, 0)], nil
}

// setupConfigFile creates the config from the template, or keeps an existing one.
func (a *app) setupConfigFile(p *prompter, path, provider string) error {
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(a.stdout, "\n%s 已存在，将就地修改。\n", path)
		if !p.confirm("继续？", true) {
			return errors.New("已取消")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := config.WriteFile(path, config.Template(provider, "", "", "")); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "\n已创建 %s（权限 0600）\n", path)
	return nil
}

// setupToken asks for a credential and proves it works before moving on. It
// returns a live provider, or nil when the operator wants to do it later.
func (a *app) setupToken(p *prompter, path string, reg providers.Registration) providers.Provider {
	// Something may already work: a token in the file, an env var, or gh.
	if prov, envs, err := a.tryProvider(path); err == nil {
		fmt.Fprintf(a.stdout, "\n已检测到可用凭据：可见 %d 个 codespace\n", len(envs))
		return prov
	}

	fmt.Fprintf(a.stdout, "\n需要一个有 codespace 权限的 GitHub token")
	fmt.Fprintf(a.stdout, "（https://github.com/settings/tokens 新建，勾选 codespace）\n")

	for {
		items := []menuItem{
			{Label: "现在粘贴 token", Detail: "写入 " + path + "，权限 0600"},
			{Label: "用环境变量 $GITHUB_TOKEN / $GH_TOKEN", Detail: "不写入磁盘"},
			{Label: "用 gh 已登录的账号", Detail: "gateway 启动时调用 `gh auth token`"},
			{Label: "以后再说", Detail: "之后可运行 `gateway config set-token`"},
		}
		switch p.menu("token 怎么给？", items, 0) {
		case 0:
			token := p.secret("Token（输入不回显）")
			if token == "" {
				fmt.Fprintln(a.stdout, "  没有输入内容")
				if p.ended() {
					return nil
				}
				continue
			}
			a.redact.Add(token)
			if err := config.Patch(path, []string{reg.ConfigKey, "token"}, token); err != nil {
				fmt.Fprintf(a.stderr, "gateway: %s\n", err)
				continue
			}
		case 1:
			if os.Getenv("GITHUB_TOKEN") == "" && os.Getenv("GH_TOKEN") == "" {
				fmt.Fprintln(a.stdout, "  当前 shell 里 $GITHUB_TOKEN 和 $GH_TOKEN 都是空的")
				if !p.confirm("  仍然继续？", false) {
					continue
				}
			}
		case 2:
			fmt.Fprintln(a.stdout, "  gateway 启动时会调用 `gh auth token`")
		case 3:
			return nil
		}
		if p.ended() {
			return nil
		}

		prov, envs, err := a.tryProvider(path)
		if err != nil {
			fmt.Fprintf(a.stdout, "\n  这个凭据用不了：%s\n", a.redact.Redact(err.Error()))
			if !p.confirm("  换一种方式？", true) {
				return nil
			}
			continue
		}
		fmt.Fprintf(a.stdout, "\n  已验证：可见 %d 个 codespace\n", len(envs))
		return prov
	}
}

// tryProvider builds the provider from the config on disk and proves the
// credential by listing environments.
func (a *app) tryProvider(path string) (providers.Provider, []providers.Environment, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	if _, _, err := a.logger(cfg, true); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	prov, err := gateway.BuildProvider(ctx, cfg, a.log, a.redact)
	if err != nil {
		return nil, nil, err
	}
	envs, err := prov.List(ctx)
	if err != nil {
		prov.Close()
		return nil, nil, err
	}
	return prov, envs, nil
}

// setupEnvironment only asks when there is a real choice to make: with a single
// codespace the gateway just uses it.
func (a *app) setupEnvironment(p *prompter, path string, reg providers.Registration, prov providers.Provider) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	envs, err := prov.List(ctx)
	cancel()
	if err != nil {
		return nil // already reported; the gateway will resolve at connect time
	}
	key := []string{reg.ConfigKey, reg.DefaultEnvironmentKey}

	switch len(envs) {
	case 0:
		fmt.Fprintf(a.stdout, "\n这个账号还没有 codespace。先去 https://github.com/codespaces 建一个，\n")
		fmt.Fprintf(a.stdout, "或者填 github.create.repository 让 gateway 在首次连接时自动创建。\n")
		return nil
	case 1:
		fmt.Fprintf(a.stdout, "\n只有一个 codespace，直接使用：%s（%s）\n", envs[0].ID, envs[0].State)
		return config.Patch(path, key, envs[0].ID)
	}

	items := make([]menuItem, 0, len(envs))
	for _, e := range envs {
		detail := fmt.Sprintf("状态 %s", e.State)
		if repo := e.Attributes["repository"]; repo != "" {
			detail += "，" + repo
		}
		items = append(items, menuItem{Label: e.ID, Detail: detail})
	}
	choice := p.menu("连接哪个 codespace？", items, 0)
	return config.Patch(path, key, envs[choice].ID)
}

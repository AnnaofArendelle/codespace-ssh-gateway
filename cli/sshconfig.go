package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/config"
)

// Markers around the block this tool manages, so it can be updated in place
// without touching anything else in ~/.ssh/config.
const (
	sshConfigBegin = "# >>> ssh-gateway >>>"
	sshConfigEnd   = "# <<< ssh-gateway <<<"
)

// DefaultSSHHost is the alias that makes `ssh root@codespace` work.
const DefaultSSHHost = "codespace"

// sshConfigBlock renders the ~/.ssh/config entry for this gateway.
func sshConfigBlock(host, hostname, port string) string {
	return fmt.Sprintf(`%s
# 由 gateway ssh-config 写入，可以用 `+"`gateway ssh-config -remove`"+` 删除。
Host %s
    HostName %s
    Port %s
    User root
    # 网关的 host key 是固定的：首次连接自动信任，之后变了会报警。
    StrictHostKeyChecking accept-new
%s
`, sshConfigBegin, host, hostname, port, sshConfigEnd)
}

// cmdSSHConfig prints or installs the ~/.ssh/config entry.
func (a *app) cmdSSHConfig(args []string) error {
	fs := a.flagSet("ssh-config")
	var (
		write  bool
		remove bool
		host   string
		path   string
	)
	fs.BoolVar(&write, "write", false, "写入 ~/.ssh/config（默认只打印）")
	fs.BoolVar(&remove, "remove", false, "从 ~/.ssh/config 删除这段")
	fs.StringVar(&host, "host", DefaultSSHHost, "ssh 里用的主机别名")
	fs.StringVar(&path, "file", "", "ssh 配置文件路径（默认 ~/.ssh/config）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	target := path
	if target == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		target = filepath.Join(home, ".ssh", "config")
	}

	if remove {
		action, err := patchSSHConfig(target, "", host)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "%s：%s\n", target, action)
		return nil
	}

	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	block := sshConfigBlock(host, hostForConfig(cfg.SSH.Listen), listenPort(cfg.SSH.Listen))

	if !write {
		fmt.Fprint(a.stdout, block)
		fmt.Fprintf(a.stdout, "\n加上 -write 可直接写入 %s\n", target)
		return nil
	}
	action, err := patchSSHConfig(target, block, host)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s：%s\n", target, action)
	fmt.Fprintf(a.stdout, "现在可以直接： ssh root@%s\n", host)
	return nil
}

// patchSSHConfig inserts, replaces or removes the managed block. An empty block
// removes it. It refuses to touch a Host entry it does not own.
func patchSSHConfig(path, block, host string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	content := string(raw)

	begin := strings.Index(content, sshConfigBegin)
	end := strings.Index(content, sshConfigEnd)
	switch {
	case begin >= 0 && end > begin:
		tail := end + len(sshConfigEnd)
		if tail < len(content) && content[tail] == '\n' {
			tail++
		}
		updated := content[:begin] + block + content[tail:]
		if updated == content {
			return "无需改动", writeSSHConfig(path, updated)
		}
		action := "已更新"
		if block == "" {
			action = "已删除"
		}
		return action, writeSSHConfig(path, updated)

	case block == "":
		return "没有找到 gateway 写入的段落", nil

	case hasHostEntry(content, host):
		return "", fmt.Errorf("%s 里已经有一个不是 gateway 写的 `Host %s`，"+
			"请手动检查（或用 -host 换个别名）", path, host)

	default:
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if content != "" {
			content += "\n"
		}
		return "已写入", writeSSHConfig(path, content+block)
	}
}

// hasHostEntry reports whether the file already declares this alias.
func hasHostEntry(content, host string) bool {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "Host") {
			continue
		}
		for _, name := range fields[1:] {
			if name == host {
				return true
			}
		}
	}
	return false
}

func writeSSHConfig(path, content string) error {
	if err := config.EnsureDir(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if strings.TrimSpace(content) == "" {
		return os.WriteFile(path, []byte(""), 0o600)
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

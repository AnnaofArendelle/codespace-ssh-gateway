package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Template renders a starter config file: the few lines that actually matter,
// with everything else left to the documented defaults.
func Template(provider, token, environment, listen string) string {
	if provider == "" {
		provider = "github"
	}
	if listen == "" {
		listen = "127.0.0.1:2222"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `# ssh-gateway 配置文件。可能包含 token，请保持 0600 权限。
# 最少只需要填一个 token，然后 `+"`gateway start`"+` 就能用 ssh root@codespace 连接。

provider: %s

`, provider)
	if provider == "github" {
		fmt.Fprintf(&b, `github:
  # GitHub token，需要 codespace 权限。
  # 留空则依次尝试 $GITHUB_TOKEN、$GH_TOKEN、gh auth token。
  token: %q
  # 要连接的 codespace（名字或显示名都行）。
  # 留空 = 自动使用你账号下唯一的那个 codespace。
  codespace: %q

  # codespace 不存在时自动创建。gateway 会记住你连过的 codespace 是从哪个仓库来的，
  # 所以删掉再连通常不用填这里；想指定规格/地区就取消注释：
  create:
    repository: ""            # owner/name，不填就用记住的那个
    branch: ""                # 默认用仓库默认分支
    machine: ""               # 规格，用 `+"`gateway codespace machines`"+` 查可选值
                              #   basicLinux32gb   = 2 core / 8 GB
                              #   standardLinux32gb = 4 core / 16 GB
                              #   premiumLinux     = 8 core / 32 GB
    location: ""              # 地区，如 WestUs2 / EastUs / WestEurope / SouthEastAsia
    idle_timeout_minutes: 30  # GitHub 自己的空闲停机时间（5-240）
    retention_period_minutes: 0

`, token, environment)
	}
	fmt.Fprintf(&b, `ssh:
  # 只监听本机：本机连接不需要任何密钥或密码。
  # 想让别的机器连进来，就改成 ":2222" 并配置 authorized_keys 或 ssh.password_auth。
  listen: %q

# 其他可选项（连接后端、自动创建 codespace、公钥/密码认证、超时、日志）
# 都有合理默认值，需要时参考 README。
`, listen)
	return b.String()
}

// WriteFile writes content to path with mode 0600, creating parents.
func WriteFile(path, content string) error {
	if err := EnsureDir(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, []byte(content), 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Patch sets a nested string key in an existing config file, preserving
// comments, formatting and every other value (notably: the token).
func Patch(path string, keyPath []string, value string) error {
	return patch(path, keyPath, value, "!!str")
}

// PatchBool sets a nested boolean key.
func PatchBool(path string, keyPath []string, value bool) error {
	return patch(path, keyPath, strconv.FormatBool(value), "!!bool")
}

func patch(path string, keyPath []string, value, tag string) error {
	if len(keyPath) == 0 {
		return fmt.Errorf("empty key path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			root.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: top level is not a mapping", path)
	}

	cur := root
	for _, key := range keyPath[:len(keyPath)-1] {
		next := mappingValue(cur, key)
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			appendMapping(cur, key, next)
		}
		if next.Kind != yaml.MappingNode {
			next.Kind, next.Tag, next.Value, next.Content = yaml.MappingNode, "!!map", "", nil
		}
		cur = next
	}

	last := keyPath[len(keyPath)-1]
	if v := mappingValue(cur, last); v != nil {
		v.Kind, v.Tag, v.Value, v.Content, v.Style = yaml.ScalarNode, tag, value, nil, 0
	} else {
		appendMapping(cur, last, &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return writeAtomic(path, buf.Bytes(), st.Mode().Perm())
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func appendMapping(m *yaml.Node, key string, value *yaml.Node) {
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

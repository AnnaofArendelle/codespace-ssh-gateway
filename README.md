# ssh-gateway

把 GitHub Codespaces 当成一台按需启动的普通 SSH 服务器使用：

```bash
ssh root@codespace
```

用户不需要知道 Codespaces、`gh`、Dev Tunnels 的存在。网关在这一条命令背后完成：

```
SSH 客户端
  → 网关认证（网关自己的 host key + authorized_keys）
  → 解析目标 environment
  → Provider.Get()            查 codespace 状态
  → Provider.Create()         不存在则按模板创建
  → Provider.Start()          stopped 则启动，并等待 RUNNING
  → Provider.Connect()        复用官方 `gh codespace ssh`
  → stdin/stdout/stderr 双向透明转发（含 pty、窗口大小、信号、退出码、subsystem）
```

断开连接后网关**不做任何事**：没有自己的 idle 计时器，也不会伪造 activity。
codespace 由 GitHub 官方 idle 机制自行停止（详见下文 “计费与 idle”）。

## 快速开始

```bash
curl -fsSL --retry 3 https://raw.githubusercontent.com/AnnaofArendelle/codespace-ssh-gateway/main/install.sh | sh
gateway                      # 首次运行走向导：只问一个 token
ssh root@codespace           # 完事
```

**只需要一个 token。** 没有密钥要生成、没有密码要设、没有 codespace 要新建。
默认只监听 `127.0.0.1:2222`，本机连接不需要任何凭据——能连到这个端口的人本来就已经登录了这台机器。

向导长这样（中文，全部有默认值，回车即可）：

```
ssh-gateway 首次配置
--------------------
配置文件：~/.config/ssh-gateway/config.yaml（权限 0600）

Provider：github

需要一个有 codespace 权限的 GitHub token（https://github.com/settings/tokens 新建，勾选 codespace）

token 怎么给？
 * 1) 现在粘贴 token          写入配置文件，权限 0600
   2) 用环境变量 $GITHUB_TOKEN / $GH_TOKEN
   3) 用 gh 已登录的账号
   4) 以后再说
选择 [1]: 1
Token（输入不回显）:
  已验证：可见 1 个 codespace

只有一个 codespace，直接使用：fantastic-tribble-4p5j55gj9f94x（RUNNING）

配置完成。只监听本机 127.0.0.1:2222，本机连接不需要密钥或密码。
现在启动 gateway？ [Y/n]:
```

不想用向导就直接写配置文件，效果完全一样：

```yaml
# ~/.config/ssh-gateway/config.yaml   (0600)
provider: github
github:
  token: ghp_xxxxxxxxxxxx     # 需要 codespace 权限；留空则用 $GITHUB_TOKEN / gh auth token
  codespace: ""               # 留空 = 自动用账号下唯一的那个
```

```bash
gateway start
```

把这段加进 `~/.ssh/config`，`ssh root@codespace` 就能用（也可以直接 `ssh -p 2222 root@127.0.0.1`）：

```
Host codespace
  HostName 127.0.0.1
  Port 2222
  User root
```

`root` 只是网关侧的用户名，和 codespace 里的真实用户（`vscode` 之类）无关。

### 需要更严格 / 对外开放时

默认的"免认证"只在监听地址是回环地址时生效。一旦改成 `:2222` 这种对外监听，
网关会**拒绝启动**，直到你配置了公钥或密码：

```bash
gateway config authorized-key add ~/.ssh/id_ed25519.pub   # 公钥
# 或者在配置里： ssh: { password_auth: true }              # 不填 password 则每次启动随机生成并打印一次
```

## 依赖

- **Go 1.24+**（只依赖 `golang.org/x/crypto`、`golang.org/x/sys`、`gopkg.in/yaml.v3`）
- **GitHub CLI (`gh`)**：只有“建立连接”这一步需要它，因为连接完全复用
  `gh codespace ssh`，网关不重新实现 Dev Tunnels 或 Codespaces 内部 RPC。
  `codespace list/select/status/stop` 只用 REST API，没装 `gh` 也能用。
- **GitHub token**：需要 `codespace` scope。查找顺序：
  `github.token` → `github.token_file` → `$GITHUB_TOKEN` / `$GH_TOKEN` → `gh auth token`。

## 架构

```
main.go                     只负责把 provider 注册进来
cli/                        命令行（start/stop/status/doctor/config/provider/codespace）
core/
  config/                   配置加载：核心字段严格校验，provider 段落原样透传
  gateway/                  组装：provider + lifecycle + session + ssh server + 控制套接字
  lifecycle/                状态机 + single-flight（并发连接只触发一次启动）
  session/                  会话登记与上限（不做活跃度检测，不带计时器）
  logging/  secret/         带脱敏的日志、拒绝被打印的 secret 类型
providers/
  provider.go               Provider 接口 / 中立数据模型 / 注册表
  github/                   GitHub Codespaces 实现（REST + gh codespace ssh）
ssh/                        SSH 服务端：认证、会话通道、双向转发
internal/testenv/           测试替身：假 GitHub API、假 codespace sshd、桩 gh
```

边界是硬的：`ssh/` 不含任何 Codespaces 逻辑，`providers/github/` 不含任何 SSH server 逻辑，
`core/` 只认识 `providers.Provider` 接口。加新 Provider 只需实现接口 + `providers.Register`，
配置文件里多一个同名段落即可，核心代码零改动。

### 连接后端（两种，都走官方 `gh codespace ssh`）

| connector | 做法 | 特点 |
|---|---|---|
| `stdio`（默认） | `gh codespace ssh -c NAME --stdio -- -i <网关私钥>` 把 codespace 的 sshd 代理到管道，网关在管道上跑标准 SSH 客户端 | 精确退出码、窗口变化、信号、subsystem（sftp/scp） |
| `exec` | `gh codespace ssh -c NAME`，需要 pty 时挂在本地 pty 上 | 官方最朴素的用法；退出码是 gh 的、不能转发信号 |

`connector: auto`（默认）先试 `stdio`，若这个 `gh` 版本不支持 `--stdio` 就自动退回 `exec`。
`--stdio` 正是 `gh codespace ssh --config` 生成的 OpenSSH `ProxyCommand` 所用的机制。

远端登录名（`vscode`/`node`/…）通过 `gh codespace ssh --config` 询问 gh 本身，
结果缓存在状态目录；密钥被拒时自动失效重查。

## 生命周期状态机

```
UNKNOWN ─┬─> STOPPED ──(ssh 到来)──> STARTING ──> RUNNING ──> CONNECTING ──> CONNECTED
         ├─> PROVISIONING（不存在 → 创建）        ↑                              │
         └─> STOPPING（正在关机 → 等它关完再启动）┘                              │
                                                                                 │(最后一个会话断开)
                                          IDLE/PROVIDER_MANAGED <────────────────┘
                                                    │
                                    （GitHub 官方 idle 机制）
                                                    ↓
                                                 STOPPED
```

`gateway status` 会打印每个 environment 的当前阶段和迁移历史。

**并发**：多个客户端同时连同一个 environment 时，第一个触发启动，其余等待同一个
operation（single-flight）；不会重复创建或重复启动。触发者中途断开、但还有人在等，
启动继续；所有等待者都走了才取消。

## 计费与 idle（重要，且不粉饰）

- 网关**没有** idle 计时器，**不会**为了保活发送任何流量，**不会**伪造 activity。
- codespace 的停止完全由 GitHub 自己的 idle 机制决定（创建时的 `idle_timeout_minutes`，
  GitHub 允许 5–240 分钟）。
- GitHub 公开文档描述了 idle 超时，但**没有**公开的 activity/session API 说明
  “哪些流量算活跃”。因此“SSH 会话是否会推迟 idle 超时”这一点是**未验证**的，
  网关把它如实上报为 `ssh_activity_attribution: unverified`
  （见 `gateway status` 的 provider notes）。可能出现：会话开着但空闲，
  codespace 仍被 GitHub 停掉；下次 `ssh` 会自动把它拉起来。
- 如果你要的是“断开即停止计费”，可以显式打开：

  ```yaml
  lifecycle:
    stop_on_last_disconnect: true   # 默认 false
  ```

  这是**网关主动调用 Provider.Stop()**，最后一个会话关闭时立刻执行，
  没有计时器、也不假装是官方 idle 机制。代价：短暂断开重连也会触发停止/再启动。
  默认关闭，把生命周期留给 GitHub。

## 配置

默认位置 `~/.config/ssh-gateway/config.yaml`（可用 `-config` 或 `$GATEWAY_CONFIG` 覆盖），
状态目录 `~/.local/state/ssh-gateway`（`$GATEWAY_STATE_DIR`）。核心字段拼错会直接报错；
顶层未知段落被当作 provider 配置原样传给对应 provider。

```yaml
provider: github            # 选哪个 provider

github:                     # 段落名 = provider 的 config key
  token: ""                 # 留空则回退到 token_file / 环境变量 / gh auth token
  token_file: ""
  codespace: my-box         # 默认目标；匹配 codespace 名或 display name
  api_url: https://api.github.com
  gh_path: ""               # 留空自动查找 gh
  connector: auto           # auto | stdio | exec
  host_key_policy: tofu     # tofu | strict | insecure（网关→codespace 这一跳）
  ssh_user: ""              # 覆盖远端登录名，留空则问 gh
  request_timeout: 30s
  create:                   # 仅在目标不存在时使用
    repository: owner/name  # 想让自动创建生效，必填
    branch: ""
    machine: ""
    devcontainer_path: ""
    idle_timeout_minutes: 30
    retention_period_minutes: 0

ssh:                        # 网关自己的 SSH 前门
  listen: "127.0.0.1:2222"  # 默认只监听本机 = 免认证；改成 ":2222"/":22" 则必须配置凭据
  host_key: ""              # 默认 <state>/host_ed25519，自动生成且不会变
  authorized_keys: ""       # 默认配置文件同目录的 authorized_keys
  authorized_keys_inline: []
  password_auth: false      # 默认关闭；打开且未设 password 则启动时随机生成并打印一次
                            # 只监听本机且既无公钥也无密码时 = 免认证（见"快速开始"）
  password: ""
  allowed_users: []         # 空 = 任何用户名都接受（ssh root@… 直接可用）
  max_sessions: 0
  max_sessions_per_environment: 0
  handshake_timeout: 30s
  shutdown_grace: 5s

lifecycle:
  auto_create: true
  start_timeout: 5m
  create_timeout: 20m
  connect_timeout: 2m
  status_poll_interval: 2s
  connect_retries: 6
  stop_on_last_disconnect: false

log:
  level: info               # debug | info | warn | error
  format: text              # text | json
  file: ""                  # 留空 = stderr

control:
  enabled: true             # status/stop 用的本地 unix 套接字（0600）
  socket: ""
```

### 选择目标 environment

1. 登录名：`ssh root+my-box@gateway`
2. 环境变量：`ssh -o SetEnv=GATEWAY_ENV=my-box root@gateway`
3. 都没有 → 用 `github.codespace`

`root` 只是网关侧的用户名，与 codespace 里的真实用户无关（那个由 devcontainer 决定）。

## CLI

```
gateway                               没有配置就走向导，有配置就启动
gateway setup [-start]                交互式菜单配置（可重复运行）
gateway start [-listen host:port]     前台运行
gateway stop                          通过控制套接字停止；否则回退到 pid 文件 + SIGTERM
gateway status [-json]                运行中则显示实时会话/阶段，否则显示配置
gateway doctor [-json]                用真实 API 和 gh 做体检
gateway config init|show|path|set-token|authorized-key
gateway provider list
gateway codespace list|select|status|stop|create|forget-host-key
gateway version
```

`codespace`/`environment`/`env` 是同一组命令的别名（面向未来的其他 provider）。

想让它常驻，用 systemd 用户服务即可（不需要 root）：

```bash
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/ssh-gateway.service <<'EOF'
[Unit]
Description=ssh-gateway
[Service]
ExecStart=%h/.local/bin/gateway start
Restart=on-failure
[Install]
WantedBy=default.target
EOF
systemctl --user enable --now ssh-gateway
```

## 安全

- token 只在配置文件（0600）、`token_file`、环境变量或 `gh auth token` 中获取；
  `secret.Value` 类型拒绝被 `fmt`/JSON/YAML 打印，日志再经过一层脱敏；
  `config show` 输出会把 token/password 替换掉（且仍是合法 YAML）。
  子进程只拿到 `GH_TOKEN`，且会先清掉继承来的 GitHub 凭据。
- 客户端认证与“网关→codespace”认证完全分离：前者是 `authorized_keys`，
  后者是网关自己生成的 ed25519 密钥（`<state>/providers/github/codespace_ed25519`），
  由 `gh codespace ssh -- -i` 注册进 codespace。
- **客户端认证按监听地址决定**：默认 `127.0.0.1:2222` 且没配置任何凭据时走 SSH 的
  `none` 认证（免密钥免密码）——这一跳的安全边界就是"你已经登录了这台机器"。
  一旦监听地址不是回环地址而又没有公钥/密码，网关**拒绝启动**并告诉你怎么改，
  不会把 codespace 静默暴露到网络上。
- 密码认证默认关闭；打开但没设密码时随机生成（20 字节 base32）并只打印一次，绝不使用固定弱口令。
- 网关 host key 持久化，绝不每次重启换一把；`authorized_keys` 里带 `command=` 之类
  选项的行会被拒绝（避免给人“已经限制住了”的错觉）。
- 网关→codespace 这一跳默认 TOFU 固定 host key（`tofu`），不匹配时报错并告诉你
  `gateway codespace forget-host-key <name>`；可选 `strict`/`insecure`。
- 所有外部命令都用结构化 argv 调用，没有任何 `sh -c`；provider 返回的数据从不进入 shell。
  客户端的 `env` 请求只作用于远端会话，绝不进入网关自身进程环境
  （`exec` 后端用 `-o SetEnv=K=V` 传递，并过滤掉不安全的名字/值）。
- 端口转发（`direct-tcpip` 等）目前不支持，会被明确拒绝而不是静默忽略。

## 测试

```bash
go test ./...          # 全部自动化测试，约 25 秒
go vet ./...
```

自动化测试跑的是**真实代码路径**：真实 SSH 服务端 + 真实 SSH 客户端（`golang.org/x/crypto/ssh`
以及系统 `ssh` 二进制）+ 真实子进程 + 真实 SSH-over-stdio 传输 + 真实 REST 客户端。
被替换的只有 GitHub 那一侧（`internal/testenv`：假 GitHub API、假 codespace sshd、
把测试二进制当作 `gh` 的桩），因为跑真实 codespace 需要你自己的 token 和计费。

已覆盖的端到端场景（`core/gateway`）：

| 场景 | 测试 |
|---|---|
| codespace 已 RUNNING | `TestExecOnRunningCodespace`（并校验网关用的是自己生成的密钥） |
| codespace 已 STOPPED | `TestStartsStoppedCodespace`（start 只调一次，阶段迁移正确） |
| codespace 不存在 | `TestCreatesMissingCodespace`（创建一次，handle 靠 display name 复用） |
| 两个客户端同时连 | `TestConcurrentClientsShareOneStart`（single-flight） |
| token 无效 | `TestInvalidTokenIsReported`（客户端看到清晰报错、token 不泄漏、网关存活） |
| 启动失败 | `TestStartAPIFailureIsReported` / `TestCodespaceFailsToStart` |
| codespace SSH 不可用 | `TestRetriesUntilSSHServerAccepts`（退避重试并告知客户端） |
| Provider API 超时 | `TestProviderAPITimeout` |
| 网关重启 | `TestGatewayRestartKeepsHostKey`（host key 不变，重连可用） |
| 真实 OpenSSH 客户端 | `TestSystemSSHClient`（`ssh root@… 'echo'`、退出码 7、第二次连接复用） |
| pty / 窗口变化 / 退出码 / stderr / subsystem | `TestPTYShellAndResize`、`TestExitStatusAndStderr`、`TestSubsystemForwarded` |
| 断开后不停机（默认） | `TestProviderOwnsIdleByDefault`（0 次 stop 调用） |
| 断开后停机（可选） | `TestStopOnLastDisconnect` |
| `exec` 后端 + 本地 pty | `TestExecConnectorInteractive`（经由系统 `ssh`） |
| 只有 token 的最简配置 | `TestTokenOnlyConfigJustWorks`（无密钥无密码连入，自动认出唯一 codespace） |
| 对外监听但无凭据 | `TestPublicListenerNeedsACredential`（拒绝启动） |

向导本身也在真实环境里跑过（伪终端驱动 + 真实 token）：token 不回显、当场验证、
真实 codespace 菜单、密钥导入、写出 0600 配置。

其余单元测试覆盖：配置严格校验/原地 patch 不丢 token/脱敏输出仍是合法 YAML、
secret 拒绝被打印、日志脱敏、生命周期 single-flight 与放弃即取消、
连接重试的可重试/不可重试分类、TOFU host key 的固定与变更检测、
authorized_keys 解析与密码认证、REST 错误分类（401/403 限流/404/5xx 重试）等。

### 用真实 GitHub 验证（需要你自己的 token）

自动化测试不会碰真实 GitHub。请按下面的清单亲自跑一遍：

```bash
./gateway doctor                 # 真实 API + gh 体检（token/scope/codespace/连接后端）
./gateway codespace list         # 真实 REST 调用
./gateway start                  # 另一个终端里：
ssh -p 2222 root@127.0.0.1 'hostname; whoami'   # codespace 已运行
./gateway codespace stop         # 停掉它
ssh -p 2222 root@127.0.0.1       # 应自动启动并进入交互 shell（首次约 20-60 秒）
# 退出后等 idle_timeout_minutes，或用 `gateway codespace status` 观察 GitHub 自己停机
scp -P 2222 file root@127.0.0.1:/tmp/   # subsystem/exec 转发
./gateway status                 # 实时会话与阶段
```

`gateway doctor` 的失败项都带 hint；`GATEWAY_TEST_LOG=debug` 或 `log.level: debug`
可以看到每一步（包括 `gh` 的 argv，token 不会出现在日志里）。

## 已知限制

- 每个 SSH 会话都会新建一条 `gh codespace ssh` 隧道，因此每次连接有 10 秒左右的固定开销
  （codespace 处于停止状态时首次连接 40~50 秒，含 GitHub 启动容器的时间）。

- `exec` 后端的退出码是 `gh` 的退出码，不是远端命令的；也不能转发 SSH 信号
  （这是 `gh codespace ssh` 自身的行为）。默认的 `stdio` 后端没有这个问题。
- 不支持端口转发/agent 转发（会被明确拒绝）。
- `gh codespace ssh --stdio` 是 gh 的隐藏 flag（`gh cs ssh --config` 生成的
  `ProxyCommand` 就用它）。若某天消失，`connector: auto` 会自动退回 `exec`。
- codespace 里必须有 sshd（Codespaces 默认镜像有；自定义镜像见 `gh` 文档的
  `ghcr.io/devcontainers/features/sshd` feature）。
- Windows 上 `exec` 后端不可用（需要本地 pty），请用 `connector: stdio`。

## 加一个新 Provider

```go
func init() {
    providers.Register(providers.Registration{
        Name:                  "codesandbox",
        Summary:               "CodeSandbox devboxes",
        ConfigKey:             "codesandbox",
        DefaultEnvironmentKey: "devbox",
        Factory:               New,   // func(ctx, providers.Deps) (providers.Provider, error)
    })
}
```

实现 `List/Get/Create/Start/Stop/Status/Connect` + `Capabilities/DefaultEnvironment/Close`，
在 `main.go` 里 import 一下，配置文件里加一个 `codesandbox:` 段落即可。
`Capabilities` 请如实填写 idle 归属（`unverified` / `documented` / `none`），不要粉饰。

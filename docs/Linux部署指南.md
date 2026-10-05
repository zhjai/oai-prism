# Linux 部署指南

适用于 Linux x86_64（amd64）和 aarch64（arm64），包括没有桌面的服务器。
本 fork 的下载地址是 [zhjai/oai-prism Releases](https://github.com/zhjai/oai-prism/releases)。

最新版网关把 Chrome TLS 指纹传输和 Sentinel 签发都放在 Go 进程内，只监听 8787。
运行时不需要 Go、Node.js、pnpm、Chrome、Xvfb，也不需要旧版的 TLS 桥或浏览器 oracle。
内置指纹模拟 Windows Chrome，这是上游请求中的浏览器身份，Linux 上保持默认即可。

- [下载 Release 安装](#下载-release-安装)
- [从源码构建](#从源码构建)
- [导入账号](#导入账号)
- [启动与验证](#启动与验证)
- [远程服务器访问](#远程服务器访问)
- [systemd 常驻](#systemd-常驻)
- [日志与排障](#日志与排障)
- [升级与回滚](#升级与回滚)

## 下载 Release 安装

需要 Bash、系统 CA 证书、`curl`、`tar` 和 `sha256sum`。Debian/Ubuntu 可安装：

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl tar coreutils
```

Fedora/RHEL 系列：

```bash
sudo dnf install -y ca-certificates curl tar coreutils
```

检查架构、下载并核对校验和：

```bash
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "不支持的架构：$(uname -m)"; exit 1 ;;
esac
version=v0.1.0-zhjai.2
asset="oaiprism-${version}-linux-${arch}.tar.gz"
url="https://github.com/zhjai/oai-prism/releases/download/${version}"
curl -fLO "${url}/${asset}"
curl -fLO "${url}/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS
```

必须看到已下载的压缩包显示 `OK`，校验失败时先重新下载。
每个安装包都包含 `oaiprism`、`web/dist`、`configs/config.example.yaml`、`tools/start.sh`、
`deploy/oaiprism.service`、README、文档和许可证；不包含任何账号、API Key 或数据库。

下面统一安装到当前用户的 `~/.local/share/oaiprism`，无需 root 权限：

```bash
mkdir -p "$HOME/.local/share/oaiprism"
tar -xzf "$asset" -C "$HOME/.local/share/oaiprism" --strip-components=1
cd "$HOME/.local/share/oaiprism"
./oaiprism version
```

保留安装目录的结构；Dashboard 由 `web/dist` 提供，运行服务时的工作目录应是安装目录。
二进制为 `CGO_ENABLED=0` 构建，不需要外部 SQLite 库。

## 从源码构建

需要 Git 和 Go **1.26+**。先用 `go version` 确认版本；开启 Go 自动工具链时，Go 可以根据
`go.mod` 下载所需工具链。发行版仓库中的旧版 Go 可能无法构建，可参考 [Go 安装文档](https://go.dev/doc/install)。

```bash
git clone https://github.com/zhjai/oai-prism.git
cd oai-prism
go build -trimpath -o oaiprism ./cmd/oaiprism
./oaiprism version
```

源码构建未注入版本号时会显示 `dev`，这是正常的。仓库已包含 Dashboard 构建产物；
只有修改前端时才需要 Node.js 22 与 pnpm：

```bash
cd web
pnpm install --frozen-lockfile
pnpm build
cd ..
```

交叉编译：`make build-linux` 生成 `bin/oaiprism-linux-amd64` 和 `bin/oaiprism-linux-arm64`。
生成与 Release 相同的完整安装包：`./tools/package_linux.sh v0.1.0-zhjai.2`，输出在 `dist/`。

如果后续使用本教程提供的 systemd 单元，请把完整运行目录放到 `~/.local/share/oaiprism`，
或按实际位置修改单元的 `WorkingDirectory` 和 `ExecStart`。

## 导入账号

在自己的浏览器登录 `https://prism.openai.com`，打开开发者工具，在 Network 请求中复制整串 Cookie。
Prism 使用 `prism_oai_access_token` / `prism_session_token`，不能用 ChatGPT 的 next-auth Cookie 替代。

在安装目录执行：

```bash
./oaiprism import -stdin -id main
```

粘贴 Cookie 后，在空行按 **Ctrl+D** 结束输入。此方式不把 Cookie 写进 shell 命令历史。
默认会在线验证，成功后存入 `secrets/accounts.json`，文件权限为 `0600`。
如果只持有 access token 或 refresh token，导入参数详见 [使用指南](使用指南.md#2-准备账号凭据)。

也可以先启动服务，再在 Dashboard 的「账号与计划池」中走 OAuth 授权导入。
无账号时服务仍能启动，`/readyz` 返回 503；账号配置文件的更改会自动热加载。

## 启动与验证

在安装目录运行：

```bash
./tools/start.sh
```

脚本以自身位置定位安装目录；已有二进制就直接运行，源码目录中缺少二进制时才调用 Go 编译。
首次启动自动从示例创建 `configs/config.yaml`，已有配置不会覆盖。它在前台运行，日志输出到终端，
按 **Ctrl+C** 停止。可用 `./tools/start.sh -port 8788` 覆盖端口。

手动启动的等价命令（只在配置不存在时复制示例）：

```bash
test -f configs/config.yaml || cp configs/config.example.yaml configs/config.yaml
./oaiprism serve -config configs/config.yaml
```

`OAIPRISM_BINARY` 和 `OAIPRISM_CONFIG` 可指定现有二进制与配置文件；相对路径相对于安装目录。
自定义路径不存在时脚本会报错，不会自动覆盖其他文件。

另开一个终端：

```bash
curl -fsS http://127.0.0.1:8787/healthz
curl -i http://127.0.0.1:8787/readyz
```

`/healthz` 的 200 只表示进程存活，`/readyz` 的 200 表示账号池有可用账号；
只有真实推理请求成功才能证明完整上游链路可用。

打开 **http://127.0.0.1:8787/dashboard/**，在右上角「API 密钥」生成 Key。
以下命令隐藏输入 Key，调用模型清单和实际推理：

```bash
read -r -s -p 'API Key: ' OAIPRISM_KEY
printf '\n'
export OAIPRISM_KEY
curl -fsS http://127.0.0.1:8787/v1/models \
  -H "Authorization: Bearer $OAIPRISM_KEY"
curl -N http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer $OAIPRISM_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-6.1-sol","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

客户端接入配置见 [README](../README.md#接入客户端) 和 [使用指南](使用指南.md#接入客户端)。

账号编辑中的「计划等级」是保存在本地的标记，不改变上游订阅或额度。
「最大并发槽位」保存后立即用于调度，`0` 表示不限；刷新网页、重载凭据和重启后仍保留。
凭据文件更新时会导入新账号、更新已有账号的 Cookie/Token，已有账号的展示信息和并发设置以 Dashboard 为准。

网页「账号与计划池」提供启用开关，停用后账号仍在列表中，但不再接收新请求。
「对外 API 密钥」可为每个 Key 绑定多个账号，也可修改现有绑定；留空使用全部启用账号。
如果最后一个绑定账号被删除，该 Key 将没有可用绑定账号，需重新选择账号或清空并保存。
网页版「Chat 调试台」输入框旁可选择账号，自动调度遵循当前 Key 的绑定范围；
指定账号不可用时明确报错，不会换到其他账号。账号选择按调试会话保存，刷新网页后恢复。
升级时 SQLite 自动兼容迁移，已有账号、Key 和聊天记录保留。

网页删除账号会同步移除 `accounts.json` 中对应的凭据，并在 SQLite 保存删除记录，防止旧文件快照或静态配置在重载、重启时重新导入。其他账号和文件中的自定义字段保留。若需恢复已删除的账号，请在网页中重新添加或导入；如仅需暂停调度，请使用启用开关。

## 远程服务器访问

服务器无需图形桌面。在服务器的 `configs/config.yaml` 中，把 `server.host` 设为 `127.0.0.1`，
再启动网关：

```yaml
server:
  host: 127.0.0.1
  port: 8787
```

在自己的电脑建立隧道，替换 SSH 用户名和主机名：

```bash
ssh -N -L 8787:127.0.0.1:8787 user@your-server
```

隧道保持运行时，电脑浏览器打开 `http://127.0.0.1:8787/dashboard/`，客户端的 API 地址也使用
`http://127.0.0.1:8787/v1`。若电脑的 8787 已占用，可改为 `-L 18787:127.0.0.1:8787`，
电脑上的访问地址相应改用 18787。

需要直接对外提供服务时，显式配置 API Key 与管理员密码，并使用 HTTPS 反向代理。
Dashboard 生成的普通 Key 没有远程管理权限；具体鉴权规则见 [使用指南](使用指南.md#鉴权与-api-key)。

## systemd 常驻

使用随安装包提供的**用户服务**，目录约定为 `~/.local/share/oaiprism`。
先完成一次前台启动以生成配置，然后 Ctrl+C 停止，避免占用同一个端口。
安装服务单元：

```bash
cd "$HOME/.local/share/oaiprism"
mkdir -p "$HOME/.config/systemd/user"
cp deploy/oaiprism.service "$HOME/.config/systemd/user/oaiprism.service"
systemctl --user daemon-reload
systemctl --user enable --now oaiprism.service
systemctl --user status oaiprism.service
```

希望注销 SSH 后继续运行，并在开机时启动用户服务，可启用 linger：

```bash
sudo loginctl enable-linger "$USER"
```

常用操作：

```bash
journalctl --user -u oaiprism.service -f
systemctl --user restart oaiprism.service
systemctl --user stop oaiprism.service
systemctl --user disable oaiprism.service
```

在没有 systemd 的容器或发行版中，使用前台启动命令交给容器运行时或已有进程管理器。

## 日志与排障

| 症状 | 检查与处理 |
|---|---|
| `Exec format error` | 用 `uname -m` 检查架构，重新下载 amd64 或 arm64 对应安装包 |
| `Permission denied` | 检查二进制和启动脚本可执行权限，以及安装目录所在文件系统是否 `noexec` |
| `address already in use` | 用 `ss -ltnp` 检查 8787，停止自己原有的网关或使用 `-port` 换端口 |
| `/readyz` 返回 503 | 先导入有效账号，再核对 `configs/config.yaml` 的 `creds.file` 与账号状态 |
| Dashboard 404 或空白 | 确认工作目录是安装目录，`web/dist/index.html` 和 `web/dist/assets/` 都存在 |
| 出现 `127.0.0.1:8790` 连接失败 | 配置沿用了旧架构；把 `upstream.base_url` 改为 `https://prism.openai.com` |
| 上游 401/403 | 在 Prism 网站确认登录正常，重新导入凭据，检查服务器出站网络和 Sentinel 自检 |
| `systemctl --user` 无法连接总线 | 在目标用户的正常登录会话执行；不要用 `sudo systemctl --user` 切换到 root |

签发器自检不需要账号，以下命令会访问 Prism 首页和 `sentinel.openai.com`：

```bash
./oaiprism sentinel -config configs/config.yaml -n 3
./oaiprism probe -config configs/config.yaml
```

Sentinel 自检应显示 token 字段、`dx 完整执行` 和失败次数为 0。
它只验证签发器；`probe` 验证账号，真实 API 请求验证完整推理链路。
上传诊断时先删除 Cookie、access token、refresh token 和 API Key。

## 升级与回滚

新安装包只包含程序、控制台、配置示例和文档，解压不会覆盖 `configs/config.yaml` 或 `secrets/`。
升级前停止服务、备份当前安装目录，再下载和验证新安装包。下面的 `version` 与 `asset` 使用目标版本：

```bash
umask 077
systemctl --user stop oaiprism.service
tar -czf "$HOME/oaiprism-backup-$(date +%Y%m%d-%H%M%S).tar.gz" \
  -C "$HOME/.local/share" oaiprism
tar -xzf "$asset" -C "$HOME/.local/share/oaiprism" --strip-components=1
systemctl --user start oaiprism.service
curl -fsS http://127.0.0.1:8787/healthz
```

备份中包含凭据与数据库，请按私密文件保管。
回滚时停止服务，从上一版安装包恢复程序和 `web/dist`，保留当前配置与 `secrets/`，再重启。
从旧三进程架构迁移时先停掉自己原有的桥和 oracle，并改用直连配置；不要把旧 Windows 绝对路径带进 Linux。

# kfadapter

kfadapter 把快帆（KuaiFan）和 QuickFox 账号转换成本地的 SOCKS5 代理与订阅链接。它登录 provider、定期刷新线路目录，并在本机提供：

- 一个 SOCKS5 入口：每条线路对应一组用户名/密码，按凭据把流量转发到对应的 provider 节点；
- 一个 Web 管理控制台：登录 provider 账号、查看节点、探测延迟、复制订阅链接；
- 一个订阅链接：`/sub/<token>`，返回 Base64 编码的 `socks5://` 链接列表，供代理客户端导入。

状态（访问令牌校验值、provider 会话、订阅密钥）保存在单个 SQLite 文件 `data/state.db` 中。

## 部署

仅支持在原生 Linux 的 Docker Engine 中以容器方式运行（例如 OpenWrt 路由器），进程会拒绝在容器外启动。容器使用 host 网络，这样 `config.yaml` 里的 `listenAddr` 可以直接绑定路由器接口地址。

### 1. 准备配置

仓库根目录的 [`config.yaml`](config.yaml) 就是部署配置，字段说明见文件内注释：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `listenAddr` | `0.0.0.0` | 管理端口与 SOCKS 端口共用的数字 IP；通配地址表示绑定该地址族的所有接口 |
| `hostname` | 无 | 可选的 SOCKS DNS 名，也是直接 HTTP 访问时接受的管理域名 |
| `management.port` | `10809` | 管理控制台 HTTP 回源端口 |
| `management.publicOrigin` | 无 | TLS 反代的外部 HTTPS origin，例如 `https://console.example.com`；配置后订阅链接使用此地址 |
| `management.sessionTTL` | `30m` | 浏览器会话时长（1m–24h）；调小后已有会话会被截短 |
| `proxy.port` | `10808` | SOCKS5 端口 |
| `proxy.dialTimeout` | `10s` | 连接上游节点的超时（1s–10s） |
| `proxy.handshakeTimeout` | `15s` | 上游协议握手超时（1s–1m） |
| `provider.requestTimeout` | `15s` | 单次 provider API 请求超时（1s–15s） |
| `provider.refreshInterval` | `2h` | 后台刷新周期（15m–24h） |

在容器里执行 `./kfadapter validate-config` 可以校验配置，出错时会打印具体原因。

### 2. 以固定 digest 启动

生产环境只运行按 digest 固定的镜像，默认仓库为 `ghcr.io/panelatta/kfadapter`。每次发布后，CI 任务摘要会给出完整的 `仓库@digest` 和可直接复制的部署命令。请使用同一次发布提供的仓库与 digest；从 fork 或其他仓库部署时，也同时设置这两个变量。

```bash
export KFADAPTER_IMAGE_REPOSITORY=ghcr.io/panelatta/kfadapter
export KFADAPTER_IMAGE_DIGEST=sha256:<64 位十六进制>
sudo --preserve-env=KFADAPTER_IMAGE_REPOSITORY,KFADAPTER_IMAGE_DIGEST scripts/preflight.sh
docker compose --env-file /dev/null -f compose.yaml up -d
```

`scripts/preflight.sh` 会检查宿主环境（原生 Linux、Docker Engine、Compose v2）、镜像引用格式和状态卷的权限。`--env-file /dev/null` 保证项目目录下的 `.env` 不能改变已校验过的镜像输入。

如需从当前代码构建本地镜像进行测试，使用独立的开发 compose 文件，它的状态卷与生产隔离：

```bash
scripts/preflight.sh --local-build
docker compose --env-file /dev/null --project-name kfadapter-local -f deploy/compose.local-build.yaml up -d --build
```

开发命令必须保留 `--project-name kfadapter-local`，包括后续的 `logs`、`stop` 和 `down`；该参数会覆盖 shell 继承的 `COMPOSE_PROJECT_NAME`，固定使用开发项目及 `kfadapter-local_db_data` 状态卷。预检会核对最终解析出的项目名、卷名和状态挂载。需要指定开发镜像名时，仍可显式设置 `KFADAPTER_LOCAL_IMAGE`。

### 3. 首次设置访问令牌

控制台第一次打开时需要创建访问令牌。**首次设置只能从运行 kfadapter 的设备本机完成**：从局域网其他设备打开时，页面只会显示说明。这样可以防止新部署的实例被局域网内其他主机抢先占用。

在另一台电脑上完成设置，可以用 SSH 端口转发：

```bash
ssh -L 10809:127.0.0.1:10809 root@<路由器地址>
```

然后在本机浏览器打开 `http://127.0.0.1:10809`。设置完成后，任何能访问管理端口的设备都可以用该令牌解锁控制台。

### 4. 连接 provider 账号

解锁后在控制台选择 provider 并输入账号密码。可以同时连接快帆和 QuickFox，每个 provider 各有一个独立账号：

- 一个 provider 刷新失败，不会影响其他 provider 的刷新结果；
- 登录、登出或某个 provider 过期，都**不会**改变其他 provider 的 SOCKS 凭据和订阅链接；
- 只有某个 provider 换成**另一个账号**时，全部 SOCKS 凭据和订阅链接才会轮换。

### 使用 TLS 反向代理

先按上文通过 SSH 完成首次设置，再配置 HTTPS 入口。即使反代从 `127.0.0.1` 回源，外部 HTTPS 入口也不能执行首次设置；本机 HTTP 健康检查与 SSH 初始化入口仍可使用。

```yaml
hostname: socks.example.com # SOCKS 客户端实际连接的主机，可与控制台域名不同
management:
  port: 10809
  publicOrigin: https://console.example.com
  sessionTTL: 30m
```

`publicOrigin` 必须是规范的小写 HTTPS origin，可以带非默认端口（例如 `:8443`），不能使用回环 IP、`localhost` 或其子域名，也不能带路径、末尾 `/`、凭据、查询参数、片段或默认 `:443`。代理必须把回源 `Host` 固定为该 origin 的 authority，并保留浏览器的 `Origin`。例如，在已配置证书的 Nginx HTTPS server 中：

```nginx
server_name console.example.com;
location / {
    proxy_pass http://127.0.0.1:10809;
    proxy_set_header Host console.example.com;
    proxy_http_version 1.1;
    proxy_buffering off;
    proxy_read_timeout 1h;
}
```

如果使用 `https://console.example.com:8443`，回源 `Host` 也必须包含 `:8443`。服务不会信任 `Forwarded` 或 `X-Forwarded-*` 来决定允许的域名、Origin 或协议。外部控制台会话 Cookie 带 `Secure`；直接本机 HTTP 会话保持可用。配置后，订阅链接固定使用 `publicOrigin`，其中的 SOCKS 地址使用 `hostname` 和 `proxy.port`；未设置 `hostname` 时使用 `publicOrigin` 的主机名和 `proxy.port`。TLS 反代只代理管理 HTTP，不代理 SOCKS，因此该 SOCKS 主机和端口也必须能被客户端访问。使用防火墙限制直接管理 HTTP 端口的网络访问，HTTPS 到 HTTP 的回源链路应位于可信网络或本机。

## 升级、备份与恢复

所有脚本都需要 root 权限（用于保持状态文件的数字属主），并且只操作 Compose 管理的 `kfadapter_db_data` 卷。`compose.yaml` 要求设置 `KFADAPTER_IMAGE_DIGEST`，下面的命令都假定当前 shell 已导出目标发布的 `KFADAPTER_IMAGE_REPOSITORY`；`docker compose` 还要求导出当前运行版本的 digest。

```bash
# 升级到新 digest；失败时自动回滚到旧镜像并恢复升级前的状态
sudo --preserve-env=KFADAPTER_IMAGE_REPOSITORY KFADAPTER_IMAGE_DIGEST=sha256:<新 digest> scripts/upgrade.sh

# 备份：服务运行时自动做在线热备份，不中断代理；服务停止时做离线备份
sudo scripts/backup-state.sh

# 也可以显式指定模式和归档路径
sudo scripts/backup-state.sh --online backups/nightly.tar.gz
sudo scripts/backup-state.sh --offline

# 从归档恢复（需要先停止服务；旧状态目录会被保留以便回退）
sudo --preserve-env=KFADAPTER_IMAGE_REPOSITORY KFADAPTER_IMAGE_DIGEST=sha256:<当前 digest> scripts/restore-state.sh backups/state-<时间戳>.tar.gz
```

在线备份通过 `docker exec` 在服务容器里运行 `./kfadapter backup`：它在一个 SQLite 读事务内先做和 `validate-state` 相同的完整校验，再把同一事务看到的数据库页原样输出到标准输出，所以得到的一定是某一次已提交的完整状态；备份持有读锁的时间通常只有几毫秒，期间服务的写入会等待（上限 5 秒）而不是失败。宿主机脚本再核对页数和日志模式，写成与离线备份相同格式的 `tar.gz`，可以直接用 `restore-state.sh` 恢复。容器文件系统保持只读，也不需要额外的挂载。

需要定时备份时，可以在宿主机 root 的 crontab 里加一行，例如每天 4 点：

```cron
0 4 * * * cd /path/to/kfadapter && scripts/backup-state.sh
```

`upgrade.sh` 在升级前总是停机后做离线备份，作为回滚点。

rootless Docker 或启用 userns-remap 时，用 `KFADAPTER_HOST_UID` 和 `KFADAPTER_HOST_GID` 指定映射后的状态属主。

## 日志与排障

- 进程日志是结构化文本，用 `docker compose logs kfadapter` 查看。启动失败时会输出具体原因，例如配置错误、监听地址不在任何已启用的接口上、状态目录权限不对等。
- 刷新成功或失败、provider 过期、凭据轮换也会记录到日志。日志不包含密码、令牌、SOCKS 凭据、订阅链接或访问目标。
- 控制台右上角的下载按钮会导出一份脱敏的诊断报告。
- 健康检查：`./kfadapter healthcheck` 会同时检查管理端口的 `/healthz` 和 SOCKS 端口。

## 已知限制

- **管理监听器提供 HTTP。** 直接连接时，访问令牌、provider 密码和会话 Cookie 以明文在网络上传输。请只在可信网络中使用，或按上文配置 TLS 反向代理及 `management.publicOrigin`。
- **QuickFox 会在本地解析域名。** QuickFox 的隧道协议只能携带 IPv4 地址，所以 SOCKS 客户端请求的域名由 kfadapter 所在主机的 DNS 解析，本地 DNS 服务器能看到这些域名。解析到回环、私有或运营商 NAT 地址的结果会被丢弃。快帆节点不受此限制。
- **QuickFox 不支持 UDP 和 IPv6 目标**，对应请求会返回 SOCKS 应答码 `0x07` 和 `0x08`。
- 已建立的 SOCKS 连接会每 30 秒复查一次凭据：凭据轮换、provider 登出或过期后，连接会被关闭。

## 开发

```bash
# 前端：依赖安装、静态检查、测试、构建（产物写入 internal/web/static/dist，嵌入 Go 二进制）
cd web && npm ci && npm run lint && npm test && npm run build && cd ..

# 后端
go vet ./...
go test -race ./...
```

不构建前端也可以编译和测试 Go 代码。此时访问控制台会返回 `console_not_built`，提示需要先构建前端。

部署脚本的测试是 `scripts/check-*.sh` 和 `scripts/check-compose-security.py`，CI 会在 Ubuntu 上运行它们（其中部分需要 root 和 Docker）。

项目结构：

| 路径 | 内容 |
| --- | --- |
| `cmd/kfadapter` | 进程入口、子命令、组件装配 |
| `internal/app` | 面向控制台的运行时门面：刷新调度、登录、探测、事件 |
| `internal/provider` | provider 无关的账号、节点、传输接口，以及多 provider 协调器 |
| `internal/kuaifan`、`internal/quickfox` | 两个 provider 的控制面客户端与数据面传输 |
| `internal/socks` | SOCKS5 / RFC 1929 入口 |
| `internal/selector` | 由订阅密钥派生每条线路的 SOCKS 凭据 |
| `internal/state` | SQLite 状态存储、迁移、运行时快照与生命周期状态机 |
| `internal/subscription` | 订阅渲染与 `/sub/` 服务 |
| `internal/web` | 管理 API、会话与安全策略、静态资源 |
| `web/` | React 控制台 |
| `scripts/` | 预检、升级、备份与恢复脚本及其测试 |

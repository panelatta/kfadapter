# chinadapter

chinadapter 让你借助已经付费的国内 VPN 服务（快帆 KuaiFan、QuickFox）从海外访问国内网络。它把这些账号变成一个可以放在家里运行的 SOCKS5 代理：

- 一个 SOCKS5 入口：每条线路对应一个用户名，所有线路共用一个密码，流量按用户名转发到对应的 provider 节点；
- 一个 Web 管理控制台：登录 provider 账号、查看节点、探测延迟、复制订阅链接；
- 一个订阅链接：`/sub/<uuid>`，返回 Base64 编码的 `socks5://` 链接列表，供代理客户端或路由器导入；
- 一个智能 SOCKS5 入口：定时测速，自动把新连接交给访问国内网站最快的线路。

本仓库是 [oshinop/chinadapter](https://github.com/oshinop/chinadapter) 的分支（原名 kfadapter），在上游基础上保留了智能代理、TLS 反向代理、按 digest 固定的镜像以及升级、备份、恢复脚本。镜像发布在 `ghcr.io/panelatta/chinadapter`。

状态（访问令牌校验值、provider 会话、智能代理设置）保存在单个 SQLite 文件 `data/state.db` 中，provider 会话在后台自动续期，重启或重建容器后无需重新登录。

## 订阅链接与凭据

订阅链接和 SOCKS 凭据由安装 ID 与访问令牌派生：

- 登录、登出、更换 provider 账号、增减 provider 或重启，都**不会**改变订阅链接和 SOCKS 凭据；
- 只有更换访问令牌才会轮换它们：在控制台右上角点击钥匙图标（**Change access token**），输入当前令牌和两次新令牌（16–128 字节）。更换后其他浏览器会被登出，订阅链接、全部 SOCKS 凭据和智能代理地址都会变化，需要把新链接重新导入各个客户端；也可以直接调用 `POST /api/v1/access/token`；
- 节点从目录中消失时其用户名失效，同一线路重新出现后原用户名恢复可用。

## 智能 SOCKS5 代理

在控制台的 **Smart SOCKS5 proxy** 卡片中打开开关，选择每 **30 分钟**或 **60 分钟**测速一次。开关和周期保存在 SQLite 中，重启后继续生效，默认关闭。开启或重启后立即开始首轮测速，也可以点击 **Probe now** 手动触发。

点击 **Copy smart SOCKS5 address** 获取固定的 `socks5://用户名:密码@VPS地址:代理端口`。它复用 `proxy.port`（默认 `10808`），不需要新增监听端口；使用 VPS 的公网 IP 或配置的 `hostname` 访问控制台后复制即可，客户端需要能访问该端口。智能入口使用独立的用户名、与普通节点共用实例密码，自动切线不会改变地址，只有更换访问令牌时才会变化。现有逐节点订阅仍可照常使用。

- 对当前账号可用的每条线路，实际经过 provider 隧道访问 B 站、小红书、微信官网和 QQ 首页，验证 TLS 并测量收到 HTTPS 响应头的耗时；不跟随重定向。HTTP 2xx/3xx 算成功，超时、TLS 错误、HTTP 4xx/5xx 算失败。
- 优先选择成功访问站点最多的线路，再比较成功请求的平均耗时。页面显示当前线路、上次/下次测速时间以及各站点结果。这个指标衡量网页响应速度，不代表下载带宽、视频吞吐量或微信/QQ 通话质量；网站风控也可能导致探测失败。
- 每次最多并行测 4 条线路，单站点超时 8 秒，每轮最多 10 分钟。线路过多导致结果不完整时，页面会标明覆盖数量，后续轮次轮换起点，避免一直遗漏目录末尾的线路。所有线路都失败时不回退到直连，约一分钟后重试；首轮完成前智能入口暂不可用。
- 自动切换只影响新连接，已建立的连接保留原线路。关闭功能后立即拒绝新的智能连接，已建立的智能连接在最长约 30 秒的复查周期内结束。普通逐节点代理不受开关影响。UDP 能力沿用所选 provider 的支持情况。

智能代理 API 使用现有控制台会话，写操作与其他接口一样受同源保护（`Origin`/`Sec-Fetch-Site`）：`GET /api/v1/smart-proxy`、`PUT /api/v1/smart-proxy/config`、`POST /api/v1/smart-proxy/probe`；SOCKS 凭据仅由按需调用的 `GET /api/v1/smart-proxy/details` 返回。

## 部署

仅支持在原生 Linux 的 Docker Engine 中以容器方式运行（例如 OpenWrt 路由器），进程会拒绝在容器外启动。容器使用 host 网络，这样 `config.yaml` 里的 `listenAddr` 可以直接绑定路由器接口地址。

### 1. 准备配置

仓库根目录的 [`config.yaml`](config.yaml) 就是部署配置，字段说明见文件内注释：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `listenAddr` | `0.0.0.0` | 管理端口与 SOCKS 端口共用的数字 IP（不能带 IPv6 zone）；通配地址表示绑定该地址族的所有接口 |
| `hostname` | 无 | 可选的 SOCKS DNS 名，也是直接 HTTP 访问时接受的管理域名 |
| `management.port` | `10809` | 管理控制台 HTTP 端口 |
| `management.publicOrigin` | 无 | TLS 反代的外部 HTTPS origin，例如 `https://console.example.com`；配置后订阅链接使用此地址 |
| `management.sessionTTL` | `30m` | 浏览器会话时长（1m–24h） |
| `proxy.port` | `10808` | SOCKS5 端口 |
| `proxy.dialTimeout` | `10s` | 连接上游节点的超时（1s–10s） |
| `proxy.handshakeTimeout` | `15s` | 上游协议握手超时（1s–1m） |
| `provider.requestTimeout` | `15s` | 单次 provider API 请求超时（1s–15s） |
| `provider.refreshInterval` | `2h` | 后台刷新周期（15m–24h） |
| `provider.quickfoxDoH` | 无 | 可选的 RFC 8484 DoH 地址，例如 `https://223.5.5.5/dns-query`；QuickFox 用它按所选节点的 /24 解析目标域名，见“已知限制” |

在容器里执行 `./chinadapter validate-config` 可以校验配置，出错时会打印具体原因；检查类命令不会创建任何文件。

### 2. 以固定 digest 启动

生产环境只运行按 digest 固定的镜像，默认仓库为 `ghcr.io/panelatta/chinadapter`。每次发布后，CI 任务摘要会给出完整的 `仓库@digest` 和可直接复制的部署命令。请使用同一次发布提供的仓库与 digest；从其他仓库部署时，也同时设置这两个变量。

```bash
export CHINADAPTER_IMAGE_REPOSITORY=ghcr.io/panelatta/chinadapter
export CHINADAPTER_IMAGE_DIGEST=sha256:<64 位十六进制>
sudo --preserve-env=CHINADAPTER_IMAGE_REPOSITORY,CHINADAPTER_IMAGE_DIGEST scripts/preflight.sh
docker compose --env-file /dev/null -f compose.yaml up -d
```

`scripts/preflight.sh` 会检查宿主环境（原生 Linux、Docker Engine、Compose v2）、镜像引用格式和状态卷的权限；如果还残留改名前的 `KFADAPTER_*` 环境变量或未迁移的 `kfadapter_db_data` 卷，它会直接报错并给出处理方法。`--env-file /dev/null` 保证项目目录下的 `.env` 不能改变已校验过的镜像输入。

如需从当前代码构建本地镜像进行测试，使用独立的开发 compose 文件，它的状态卷与生产隔离：

```bash
scripts/preflight.sh --local-build
docker compose --env-file /dev/null --project-name chinadapter-local -f deploy/compose.local-build.yaml up -d --build
```

开发命令必须保留 `--project-name chinadapter-local`，包括后续的 `logs`、`stop` 和 `down`；该参数会覆盖 shell 继承的 `COMPOSE_PROJECT_NAME`，固定使用开发项目及 `chinadapter-local_db_data` 状态卷。预检会核对最终解析出的项目名、卷名和状态挂载。需要指定开发镜像名时，可设置 `CHINADAPTER_LOCAL_IMAGE`。

### 3. 首次设置访问令牌

控制台第一次打开时需要创建访问令牌。**首次设置只能在运行 chinadapter 的设备本机完成**：从局域网其他设备或 TLS 反代打开时，页面只会显示说明，以防新部署的实例被其他主机抢先占用。

在另一台电脑上完成设置，可以用 SSH 端口转发：

```bash
ssh -L 10809:127.0.0.1:10809 root@<路由器地址>
```

然后在本机浏览器打开 `http://127.0.0.1:10809`。设置完成后，任何能访问管理端口的设备都可以用该令牌解锁控制台。

### 4. 连接 provider 账号

解锁后在控制台选择 provider 并输入账号密码。可以同时连接快帆和 QuickFox，每个 provider 各有一个独立账号：

- 每个 provider 独立刷新、独立退避重试，一个 provider 失败不会影响其他 provider；
- 会话不会因为时间流逝被删除。只有 provider 明确拒绝会话时，控制台才会提示重新登录，此时该 provider 的节点仍然保留；
- 账号与密码只在登录时使用，不会写入磁盘。

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

## 从 kfadapter 迁移

改名后 Compose 项目、服务和状态卷都变成了 `chinadapter` / `chinadapter_db_data`，环境变量前缀也从 `KFADAPTER_` 改为 `CHINADAPTER_`。直接 `docker compose up -d` 会创建一个空的新卷，相当于全新安装，所以已有的 kfadapter 部署请用迁移脚本：

```bash
export CHINADAPTER_IMAGE_REPOSITORY=ghcr.io/panelatta/chinadapter
export CHINADAPTER_IMAGE_DIGEST=sha256:<新 digest>
sudo --preserve-env=CHINADAPTER_IMAGE_REPOSITORY,CHINADAPTER_IMAGE_DIGEST scripts/migrate-legacy-state.sh
```

脚本会停止旧的 kfadapter 容器，把 `kfadapter_db_data` 做成离线备份（`backups/pre-chinadapter-*.tar.gz`），用新镜像校验后恢复到由 Compose 创建的 `chinadapter_db_data`，再启动新服务并等待健康检查通过。旧卷只读不写；任何一步失败都会停止新服务并重新启动旧容器。成功后脚本会打印清理旧容器和旧卷的命令，确认新服务工作正常后再执行。

迁移会把 kfadapter 的 schema v10 状态升级为 v11：

- **保留**：访问令牌、安装 ID、快帆和 QuickFox 的账号会话与节点（无需重新登录）、排除的节点、智能代理的开关和周期；快帆设备标识保持不变。
- **变化**：订阅链接和 SOCKS 凭据会轮换一次（凭据派生方式变了），迁移后请把新的订阅链接和智能 SOCKS 地址重新导入客户端；会话 Cookie 改名，每个浏览器需要重新解锁一次控制台。

## 升级、备份与恢复

所有脚本都需要 root 权限（用于保持状态文件的数字属主），并且只操作 Compose 管理的 `chinadapter_db_data` 卷。`compose.yaml` 要求设置 `CHINADAPTER_IMAGE_DIGEST`，下面的命令都假定当前 shell 已导出目标发布的 `CHINADAPTER_IMAGE_REPOSITORY`；`docker compose` 还要求导出当前运行版本的 digest。

```bash
# 升级到新 digest；失败时自动回滚到旧镜像并恢复升级前的状态
sudo --preserve-env=CHINADAPTER_IMAGE_REPOSITORY CHINADAPTER_IMAGE_DIGEST=sha256:<新 digest> scripts/upgrade.sh

# 备份：服务运行时自动做在线热备份，不中断代理；服务停止时做离线备份
sudo scripts/backup-state.sh

# 也可以显式指定模式和归档路径
sudo scripts/backup-state.sh --online backups/nightly.tar.gz
sudo scripts/backup-state.sh --offline

# 从归档恢复（需要先停止服务；旧状态目录会被保留以便回退）
sudo --preserve-env=CHINADAPTER_IMAGE_REPOSITORY CHINADAPTER_IMAGE_DIGEST=sha256:<当前 digest> scripts/restore-state.sh backups/state-<时间戳>.tar.gz
```

当前状态库为 schema v11。恢复时也接受旧版本的归档：kfadapter 的 v9/v10、上游 chinadapter 的 v9 以及更早的 v4–v8。恢复脚本在无网络、只读的容器里用目标镜像的 `validate-state` 校验归档（旧版本会在临时副本上完整迁移一遍），服务启动后再正式迁移到 v11。

在线备份通过 `docker exec` 在服务容器里运行 `./chinadapter backup`：它在一个 SQLite 读事务内完成与 `validate-state` 相同的校验，再把同一事务看到的数据库页原样输出到标准输出，所以得到的一定是某一次已提交的完整状态；持有读锁的时间通常只有几毫秒，期间服务的写入会等待（上限 5 秒）而不是失败。宿主机脚本再核对页数和日志模式，写成与离线备份相同格式的 `tar.gz`，可以直接用 `restore-state.sh` 恢复。容器文件系统保持只读，也不需要额外的挂载。

需要定时备份时，可以在宿主机 root 的 crontab 里加一行，例如每天 4 点：

```cron
0 4 * * * cd /path/to/checkout && scripts/backup-state.sh
```

`upgrade.sh` 在升级前总是停机后做离线备份，作为回滚点。

rootless Docker 或启用 userns-remap 时，用 `CHINADAPTER_HOST_UID` 和 `CHINADAPTER_HOST_GID` 指定映射后的状态属主。

状态库最大 18 MiB；超出时写入会被回滚并报错，原有状态保持不变。

## 配合 dae 使用

[`deploy/dae/config.dae.template`](deploy/dae/config.dae.template) 是一份 dae 配置示例：它订阅 chinadapter 的链接，把除私有地址外的流量交给这些节点。复制时把文件权限设为 `0600`，并把 `<36-char-uuid>` 换成控制台里复制的订阅链接中的 UUID；订阅 UUID 属于敏感信息，不要提交到版本库或贴进日志。

## 日志与排障

- 进程日志是结构化文本，用 `docker compose logs chinadapter` 查看。启动成功时输出一行 `msg=ready`；启动失败时会输出具体原因，例如配置错误、监听地址不在任何已启用的接口上、状态目录权限不对等。
- 每次 provider 刷新的成功或失败（只记录错误类别）也会记录到日志。日志不包含密码、令牌、SOCKS 凭据、订阅链接或访问目标。
- 控制台右上角的下载按钮会导出一份脱敏的诊断报告。
- 健康检查：`./chinadapter healthcheck` 会同时检查管理端口的 `/healthz` 和 SOCKS 端口，失败时打印原因。

## 已知限制

- **管理监听器提供 HTTP。** 直接连接时，访问令牌、provider 密码和会话 Cookie 以明文在网络上传输。请只在可信网络中使用，或按上文配置 TLS 反向代理及 `management.publicOrigin`。
- **QuickFox 会在本地解析域名。** QuickFox 的隧道协议只能携带 IPv4 地址，所以 SOCKS 客户端请求的域名由 chinadapter 所在主机解析，解析到回环、私有或运营商 NAT 地址的结果会被丢弃。主机在海外时，国内 CDN 会按主机的位置分配节点：2026-10 实测从美国解析，B 站视频域名分到与节点不同运营商的国内节点，图片、静态资源和 API 域名则分到新加坡、美国或香港，流量经过国内节点后又绕回海外。设置 `provider.quickfoxDoH`（例如 AliDNS `https://223.5.5.5/dns-query`）后，chinadapter 改向该 DoH 服务器查询，并把所选 QuickFox 节点所在的 /24 作为 ECS 发送，CDN 便按节点的网络分配；DoH 失败时回退到本机 DNS，并在 30 秒内不再尝试。无论哪种方式，解析服务器都能看到这些域名；ECS 只包含 QuickFox 节点的网段，不含你的地址。快帆节点不受此限制。
- **QuickFox 不支持 IPv6 目标。** 目标是 IPv6 地址或域名没有可用公网 IPv4 地址时，`CONNECT` 返回 SOCKS 应答码 `0x08`；UDP 关联里发往 IPv6 的数据报会被逐个丢弃。QuickFox 的 UDP（包括 QUIC）经 TCP 承载的数据报中继转发，支持 IPv4 和域名目标，超过 2034 字节的单个数据报会被丢弃。
- 已建立的 SOCKS 连接每 30 秒复查一次凭据和线路：更换访问令牌、provider 登出或线路下线后，连接会被关闭；provider 正常刷新不会中断连接。
- 同一客户端 IP 最多同时保持 32 个尚未完成认证的 SOCKS 连接。

## 开发

```bash
# 前端：依赖安装、静态检查、构建、测试（产物写入 internal/web/static，嵌入 Go 二进制）
cd web && npm ci && npm run lint && npm run build && npm test && cd ..

# 后端
go vet ./...
go test -race ./...
golangci-lint run ./...
govulncheck ./...
```

不构建前端也可以编译和测试 Go 代码。部署脚本的测试是 `scripts/check-*.sh` 和 `scripts/check-compose-security.py`，CI 会在 Ubuntu 上运行它们（其中部分需要 root 和 Docker）。设计说明见 [`docs/design.md`](docs/design.md)。

项目结构：

| 路径 | 内容 |
| --- | --- |
| `cmd/chinadapter` | 进程入口与子命令（serve、healthcheck、validate-config、validate-state、backup） |
| `internal/app` | 组件装配与面向控制台的运行时：登录、刷新、探测、事件、诊断 |
| `internal/accounts` | 多 provider 协调器与独立刷新调度 |
| `internal/provider` | provider 无关的账号、节点、线路与传输接口 |
| `internal/provider/kuaifan`、`internal/provider/quickfox` | 两个 provider 的控制面客户端与数据面传输 |
| `internal/credentials` | 由安装 ID 与访问令牌派生订阅、SOCKS 与智能入口凭据 |
| `internal/smart` | 智能代理的测速、排名与选线 |
| `internal/socks` | SOCKS5 / RFC 1929 入口 |
| `internal/state` | 领域类型、聚合校验与运行时快照 |
| `internal/store` | SQLite 持久化、迁移、浏览器会话、热备份与恢复校验 |
| `internal/subscription` | 订阅渲染与 `/sub/` 服务 |
| `internal/web` | 管理 API、会话与安全策略、静态资源 |
| `web/` | React 控制台 |
| `scripts/` | 预检、升级、备份、恢复、迁移脚本及其测试 |

## 免责声明

chinadapter 是非官方项目，与任何 VPN 服务商无关。请只使用你自己拥有的账号。

## 许可证

[MIT](LICENSE)

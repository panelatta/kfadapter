# kfadapter 代码审阅报告

- **审阅日期**：2026-09-26
- **审阅对象**：`main` 分支，提交 `ab351a8`（feat: add multi provider support …）
- **审阅范围**：全部 Go 代码（`cmd/`、`internal/`）、前端（`web/`）、部署脚本（`scripts/`）、`Dockerfile`、`compose.yaml`、`config.yaml`、GitHub Actions 工作流、`docs/`
- **审阅方式**：工具链检查 + 分模块人工审读。高优先级结论均逐条对照源码核实；唯一需要浏览器行为才能确认的一条（SSE 被拒，见 [#1](#1-控制台实时更新完全不工作)）在 Chromium 中做了实测。

## 状态标记说明

| 标记 | 含义 |
| --- | --- |
| ✅ 已核实 | 已对照源码确认逻辑成立 |
| 🧪 已实测 | 通过实际运行复现 |
| 🔍 模块审阅 | 来自分模块审阅，逻辑描述可信，但未逐行复核 |
| ❓ 待确认 | 依赖外部服务行为（如 provider 服务端协议），无法在本地确认 |

## 目录

- [0. 结论摘要](#0-结论摘要)
- [1. 审阅方法与工具结果](#1-审阅方法与工具结果)
- [2. 问题总览](#2-问题总览)
- [3. 高优先级：确认存在的功能性问题](#3-高优先级确认存在的功能性问题)
- [4. 启动与运行稳健性](#4-启动与运行稳健性)
- [5. Provider 协议层](#5-provider-协议层)
- [6. 安全](#6-安全)
- [7. 依赖、CI 与构建](#7-依赖ci-与构建)
- [8. 前端与可维护性](#8-前端与可维护性)
- [9. 其他低优先级发现（按模块）](#9-其他低优先级发现按模块)
- [10. 测试缺口清单](#10-测试缺口清单)
- [11. 建议处理顺序](#11-建议处理顺序)
- [附录 A：复现检查的命令](#附录-a复现检查的命令)
- [附录 B：做得好的地方](#附录-b做得好的地方)

---

## 0. 结论摘要

项目整体工程质量较高：SQLite 事务处理规范，加密实现正确，部署脚本对路径与权限的校验非常严谨，Web 层的 CSP、Cookie、CSRF 等防护也到位。但在最近一次“多 provider 重构”之后，出现了几类值得优先处理的问题：

1. **用户可直接感知的功能缺陷**
   - 控制台的实时事件流在真实浏览器中必然被拒（403），并且前端不会重连。
   - 一个 provider 刷新失败会拖累所有 provider，约 24 小时后健康账号也会一起过期。
   - 任一 provider 的增减都会轮换全部 SOCKS 凭据和订阅 URL。
2. **运维可用性**
   - 升级脚本与 `compose.yaml` 脱节，按现状无法使用。
   - 进程没有任何日志：启动失败时只输出一句 "service stopped"。
   - 调小 `sessionTTL`、或设备时钟大幅落后时，服务无法启动并反复重启。
3. **测试安全网大幅缩水**：最近一次提交删除了约 5,700 行测试，SOCKS、coordinator、runtime、main 现在基本没有有效测试。
4. **依赖**：存在一个可达的第三方漏洞（`golang.org/x/text`），另有多个标准库漏洞需要升级到 Go 1.26.6 才能修复。

---

## 1. 审阅方法与工具结果

### 1.1 构建与测试

| 检查项 | 结果 | 备注 |
| --- | --- | --- |
| `go build ./...` | ✅ 通过 | 前提是先构建前端，否则 `//go:embed all:static` 报 `no matching files found`（见 [#25](#25-文档与开发体验)） |
| `go vet ./...` | ✅ 干净 | |
| `go test -race -count=1 ./...` | ✅ 全部通过 | 部分测试需要绑定本地端口 |
| `npm run build`（`tsc --noEmit && vite build`） | ✅ 通过 | JS 产物 338.56 kB（gzip 106.68 kB） |
| `npm test`（vitest） | ✅ 5 个文件、53 个测试全部通过 | |

### 1.2 Go 测试覆盖率（`go test -cover`）

| 包 | 覆盖率 | 说明 |
| --- | --- | --- |
| `cmd/kfadapter` | **0.0%** | 测试文件只有一个空函数 |
| `internal/app` | **8.0%** | |
| `internal/config` | 86.6% | |
| `internal/endpoint` | 无测试 | |
| `internal/kuaifan` | 72.3% | |
| `internal/kuaifan/profile` | 70.0% | |
| `internal/kuaifan/wifiin` | 63.4% | |
| `internal/lifecycle` | 82.9% | |
| `internal/provider` | 无测试 | |
| `internal/provider/coordinator` | **0.0%** | 测试只测了测试文件里自己定义的桩类型 |
| `internal/quickfox` | 73.0% | |
| `internal/selector` | 39.8% | |
| `internal/socks` | **1.0%** | |
| `internal/state` | 34.5% | |
| `internal/subscription` | **10.7%** | |
| `internal/web` | 63.1% | |

### 1.3 staticcheck（`-checks all,-ST1000,-ST1003`）

| 位置 | 检查项 | 说明 |
| --- | --- | --- |
| [internal/app/runtime.go:992](../internal/app/runtime.go#L992) | U1000 | `(*Runtime).clearActiveSession` 未使用 |
| [internal/kuaifan/refresh.go:492](../internal/kuaifan/refresh.go#L492) | SA1019 | 使用了已弃用的 `net.Error.Temporary` |
| [internal/kuaifan/wifiin/cipher.go:52](../internal/kuaifan/wifiin/cipher.go#L52)、[:65](../internal/kuaifan/wifiin/cipher.go#L65) | SA1019 | `cipher.NewCFBEncrypter/Decrypter` 自 Go 1.24 起弃用。这是协议兼容所需，无法替换，可加 `//lint:ignore` 注释说明原因 |
| [internal/kuaifan/wifiin/hmac.go:9](../internal/kuaifan/wifiin/hmac.go#L9)、[:13](../internal/kuaifan/wifiin/hmac.go#L13) | U1000 | `newHMACSHA1`、`copyBytes` 未使用（整个文件是死代码） |
| [internal/kuaifan/wifiin/tcp.go:277](../internal/kuaifan/wifiin/tcp.go#L277) | SA4004 | 循环体无条件终止。第一次读到 `(0, nil)` 就返回 `io.ErrNoProgress`，比 `io.Reader` 约定更严格（约定只禁止“无限期”返回 `(0, nil)`） |

### 1.4 govulncheck（本地工具链 go1.26.3）

结论：代码可达 **9 个漏洞**，其中 1 个在第三方模块，8 个在标准库。

| ID | 组件 | 说明 | 修复版本 | 可达路径示例 |
| --- | --- | --- | --- | --- |
| GO-2026-5970 | `golang.org/x/text@v0.37.0` | 非法输入导致死循环 | **v0.39.0** | `selector.canonicalHost` → `idna.Profile.ToASCII` → `norm.Form.Bytes`（[credentials.go:89](../internal/selector/credentials.go#L89)） |
| GO-2026-6218 | `net/url` | `resolvePath` 二次复杂度 | go1.26.6 | `kuaifan.Client.post` → `url.URL.ResolveReference` |
| GO-2026-6090 | `crypto/tls` | 握手后消息数量未限制 | go1.26.6 | |
| GO-2026-6089 | `net/http` | 未加密 HTTP/2 检查未应用 `ReadHeaderTimeout` | go1.26.6 | |
| GO-2026-5972 | `encoding/asn1` | 递归深度未限制 | go1.26.6 | |
| GO-2026-5856 | `crypto/tls` | ECH 隐私泄漏 | go1.26.5 | |
| GO-2026-5039 | `net/textproto` | 错误信息未转义任意输入 | go1.26.4 | |
| GO-2026-5037 | `crypto/x509` | 主机名解析低效 | go1.26.4 | |
| GO-2026-5026 | `net/http`（x/net/idna） | 未拒绝纯 ASCII 的 Punycode 标签 | go1.26.6 | `kuaifan.Client.post` → `http.Client.Do` |

注意：CI 固定的是 `go-version: '1.26.5'`，仍然缺少 1.26.6 的修复；`Dockerfile` 使用浮动标签 `golang:1.26-alpine`，构建时通常能拿到最新补丁版本。

### 1.5 npm audit / npm outdated

**npm audit**：6 个漏洞（4 个中危、2 个高危），全部位于构建或测试工具链，不进入最终产物：

| 包 | 严重度 | 受影响版本 |
| --- | --- | --- |
| `browserslist` | 高 | ≤ 4.28.6 |
| `nanoid` | 高 | < 3.3.18 |
| `postcss` | 中 | ≤ 8.5.22（GHSA-fxqj-rqcc-2cmp） |
| `@vitest/mocker` | 中 | 2.1.0 – 4.1.10 |
| `baseline-browser-mapping` | 中 | ≥ 2.0.0 < 2.11.0 |

**npm outdated**（主要项）：`vite` 6.4.3 → 8.x、`vitest` 3.2.7 → 5.x、`typescript` 5.8.3 → 7.x、`@vitejs/plugin-react` 5 → 6，均为大版本升级；`react`/`react-dom` 19.2.7 → 19.3.0、`radix-ui` 1.6.2 → 1.6.7、`lucide-react` 1.24 → 1.48 等为兼容升级。

### 1.6 实测：同源 EventSource 是否携带 `Origin`

在 Claude 桌面应用的内置浏览器（Chromium）中，用一个最小 Go 服务提供同源页面，页面内执行 `new EventSource("/events", { withCredentials: true })`，服务端记录到的请求头为：

```text
Origin="" Sec-Fetch-Mode="cors" Sec-Fetch-Site="same-origin"
```

即：**同源 EventSource 的 GET 请求不带 `Origin` 头**。这与 Fetch 规范一致：只有 response tainting 为 `cors` 或方法不是 GET/HEAD 时才附加 `Origin`，同源请求的 tainting 为 `basic`。该结果是 [#1](#1-控制台实时更新完全不工作) 的直接证据。

---

## 2. 问题总览

| # | 问题 | 严重度 | 主要位置 | 状态 |
| --- | --- | --- | --- | --- |
| 1 | 控制台实时更新完全不工作（SSE 403 且不重连） | 高 | `internal/web/api.go:385` | 🧪 |
| 2 | 一个 provider 刷新失败会拖累所有 provider | 高 | `internal/provider/coordinator/coordinator.go:87` | ✅ |
| 3 | provider 增减会轮换全部 SOCKS 凭据与订阅 URL | 高 | `internal/state/types.go:230`、`:655` | ✅ |
| 4 | 升级脚本与 `compose.yaml` 脱节，无法使用 | 高 | `compose.yaml`、`scripts/upgrade.sh` | ✅ |
| 5 | 测试大幅倒退，关键包几乎无覆盖 | 高 | `ab351a8` | ✅ |
| 6 | 调小 `sessionTTL` 或时钟落后导致无法启动 | 中高 | `internal/web/security.go:116`、`internal/state/store.go:1571` | ✅ |
| 7 | SOCKS `Accept` 出错（如 EMFILE）导致整个进程退出 | 中 | `internal/socks/server.go:223` | ✅ |
| 8 | 进程完全没有日志，启动失败原因不可见 | 中 | `cmd/kfadapter/main.go:49` | ✅ |
| 9 | 订阅服务数据竞争、GET 写库、锁争用 | 中 | `internal/subscription/service.go:136`、`:661` | ✅ |
| 10 | 快帆 Windows 请求的 `nonce` 被覆盖 | 中 | `internal/kuaifan/client.go:391` | ✅ |
| 11 | 单次请求超时不会重试 | 中低 | `internal/kuaifan/refresh.go:480` | ✅ |
| 12 | 一条异常线路导致整个登录/刷新失败 | 中 | `kuaifan/client.go:664`、`quickfox/driver.go:245` | ✅ |
| 13 | QuickFox 在本地解析域名（DNS 泄漏） | 中 | `internal/quickfox/transport.go:108` | ✅ |
| 14 | 拨号成功后 ctx 取消导致连接泄漏 | 低 | `quickfox/transport.go:129`、`kuaifan/transport.go:109` | ✅ |
| 15 | 首次初始化可被局域网内任何人抢占 | 中 | `internal/web/api.go:459` | ✅ |
| 16 | 浏览器会话 token 明文存储 | 低 | `internal/state/store.go:1457` | ✅ |
| 17 | “锁定控制台”失败时服务端会话仍有效 | 中低 | `web/src/App.tsx:544` | ✅ |
| 18 | 可达漏洞：`x/text` 与 Go 标准库 | 中 | `go.mod`、`.github/workflows/ci.yml` | ✅ |
| 19 | 镜像发布不依赖 CI 结果 | 中 | `.github/workflows/publish.yml` | ✅ |
| 20 | Dockerfile 缓存失效、Node 版本不一致 | 低 | `Dockerfile` | ✅ |
| 21 | npm 构建链漏洞 | 低 | `web/package-lock.json` | ✅ |
| 22 | staticcheck：死代码、弃用 API、过严的读循环 | 低 | 见 [1.3](#13-staticcheck-checks-all-st1000-st1003) | ✅ |
| 23 | 前端若干 bug（重载丢失、无加载态、错误码不匹配等） | 中低 | `web/src/App.tsx` | ✅ |
| 24 | `App.tsx` 过大、无 ESLint、死组件 | 低 | `web/src/App.tsx` | ✅ |
| 25 | 缺少 README、注释过时、全新 clone 无法直接构建 | 低 | 仓库根目录、`config.yaml` | ✅ |

第 9 节另列出按模块整理的低优先级发现，第 10 节是测试缺口清单。

---

## 3. 高优先级：确认存在的功能性问题

### 1. 控制台实时更新完全不工作

**状态**：🧪 已实测

**位置**

- 后端：[internal/web/api.go:385](../internal/web/api.go#L385)、[internal/web/security.go:543](../internal/web/security.go#L543)（`originAllowed`）
- 前端：[web/src/api.ts:193](../web/src/api.ts#L193)（创建 `EventSource`）、`web/src/App.tsx` 中订阅事件流的 effect

**原因**

```go
if r.URL.Path == "/api/v1/events" && !originAllowed(r.Header.Get("Origin"), "http://"+r.Host) {
    a.writeProblem(w, http.StatusForbidden, "invalid_origin", "Invalid Origin", "")
    return
}
```

`originAllowed` 在 `Origin` 为空时直接返回 `false`。而浏览器发出的同源 EventSource GET 请求**不带** `Origin`（见 [1.6](#16-实测同源-eventsource-是否携带-origin)），所以每一次连接都会得到 403。

按 HTML 规范，EventSource 收到非 200 响应会“fail the connection”，`readyState` 变为 `CLOSED` 且**永不自动重连**。前端也没有在 `onerror` 中重建连接的逻辑。同样的永久断开也会发生在以下情况：

- 打开第 9 个标签页时收到的 `429 sse_capacity`（[api.go:797](../internal/web/api.go#L797)）
- 任何一次 `503`

**影响**：控制台永远无法反映后台刷新、探测结果和状态变化，必须等用户手动操作或刷新页面。

**为何测试没有发现**

- 后端测试手动设置了 `Origin` 头（`internal/web/api_test.go` 中的 SSE 用例）。
- 前端测试在 `web/src/test/setup.ts` 中 mock 了 `EventSource`。

**修复建议**

1. 后端：`Origin` 存在时必须匹配；缺失时改为检查 `Sec-Fetch-Site`，只接受 `same-origin`，或在该头也缺失时放行。会话 Cookie 已经是 `HttpOnly` + `SameSite=Strict`（[security.go:569-570](../internal/web/security.go#L569)），跨站请求本就不会带上会话，因此对只读 GET 流放宽 `Origin` 要求不会削弱 CSRF 防护。
2. 前端：在 `onerror` 中检测 `readyState === EventSource.CLOSED`，按指数退避重建连接；遇到 401/403 时触发一次会话校验。
3. 测试：
   - 后端增加“不带 `Origin`、带 `Sec-Fetch-Site: same-origin`”的 SSE 用例，以及“`Sec-Fetch-Site: cross-site`”的拒绝用例。
   - 前端增加“流进入 CLOSED 后能重连”的用例。

### 2. 一个 provider 刷新失败会拖累所有 provider

**状态**：✅ 已核实

**位置**

- [internal/provider/coordinator/coordinator.go:87-141](../internal/provider/coordinator/coordinator.go#L87)（`Refresh`）
- [internal/app/runtime.go:681](../internal/app/runtime.go#L681)（定时任务调用 `r.Refresh(ctx, "")`）

**原因**：定时刷新传入空 id，即刷新**所有** provider。`Refresh` 的实现如下：

- 每个 provider 并发刷新；任何一个出错都会调用 `cancel()`，取消其它 provider 正在进行的刷新。
- 收集结果时遇到第一个错误就 `return`，其它 provider 已成功的结果全部丢弃。
- 注释写明了这一设计：“All selected results commit atomically; one failure retains the prior aggregate.”

这对手动“全部刷新”也许合理，但用于定时刷新就会产生连坐。

**触发场景**

1. 用户同时登录了快帆和 QuickFox。
2. QuickFox 的 token 被服务端吊销（例如在其它设备登录），此后每次定时刷新都失败。
3. 快帆的刷新即使成功也从不提交，其快照的 `ExpiresAt` 一直停留在上次成功刷新时设置的 `now + 24h`（[kuaifan/refresh.go:369](../internal/kuaifan/refresh.go#L369)、[quickfox/driver.go:231](../internal/quickfox/driver.go#L231)）。
4. 约 24 小时后，`Expire` 把两个 provider 一起移除，健康的快帆账号也随之下线。

这与“每个 provider 独立一个账号”的设计目标相矛盾。第 [12](#12-一条异常线路导致整个登录或刷新失败) 条会进一步放大此问题：provider 返回一条异常线路就会让刷新失败。

**修复建议**

- 定时刷新改为**按 provider 独立提交**：每个 provider 成功即合并进聚合快照并发布；失败的保留旧快照，单独记录失败与退避。
- 某个 driver 失败时不要取消其它 driver。
- 手动“全部刷新”如需原子语义可保留，但 UI 应显示逐个 provider 的结果。
- 补充使用假 driver 的测试：部分失败、全部失败、一个超时一个成功等。

### 3. provider 增减会轮换全部 SOCKS 凭据与订阅 URL

**状态**：✅ 已核实

**位置**

- [internal/state/types.go:230-250](../internal/state/types.go#L230)（`AccountBindingID`）
- [internal/state/types.go:655-671](../internal/state/types.go#L655)（`EnsureSubscriptionAccountBinding`）
- [internal/app/runtime.go:831-832](../internal/app/runtime.go#L831)

**原因**：账号绑定 ID 由**所有**活跃 provider 的 `(providerID, userID)` 拼接而成。一旦绑定与已存值不一致，`EnsureSubscriptionAccountBinding` 就会：

- 生成新的 `SelectorKey` 和 `ProxyAuthKey`，并擦除旧值；
- 清空 `LastGood` 和 `ActiveSession`。

**触发场景**：以下任一操作都会改变 provider 集合，从而导致全部凭据轮换：

- 登录第二个 provider；
- 登出其中一个 provider；
- 某个 provider 被 coordinator 的 `Expire` 自动移除（与第 2 条叠加时会自动发生）。

结果是已分发出去的 `/sub/...` 订阅链接返回 404，**仍然存活的那个 provider** 对应的 SOCKS 用户名和密码也全部失效。所有客户端都需要重新导入订阅。

**修复建议**（需先明确产品语义）

- 如果希望“同一 provider 的同一账号”凭据保持稳定：改为按 installation 或按 provider 维护密钥，只在同一 provider 的 `UserID` 变化（即切换账号）时轮换该 provider 的凭据。
- 如果这是有意为之：应在 UI 上明确提示“登录或登出任何 provider 都会使现有订阅失效”，并补充测试锁定这一行为。

### 4. 升级脚本与 `compose.yaml` 脱节，无法使用

**状态**：✅ 已核实

**位置**：[compose.yaml](../compose.yaml)、[scripts/upgrade.sh](../scripts/upgrade.sh)、[scripts/preflight.sh](../scripts/preflight.sh)、[scripts/restore-state.sh](../scripts/restore-state.sh)

**问题**

1. `compose.yaml` 写死了 `image: ghcr.io/oshinop/kfadapter:latest`，没有引用 `KFADAPTER_IMAGE_REPOSITORY` 和 `KFADAPTER_IMAGE_DIGEST`。
2. `upgrade.sh` 要求当前运行容器的镜像是 `repository@sha256:<64hex>` 形式，否则报 “current service image must use an untagged repository@sha256…” 并退出。由于 compose 只会产生 `:latest`，**这一步必然失败**。
3. 即使越过第 2 步：
   - `compose pull` 拉取的仍是 `:latest`，而不是指定的 digest；
   - `rollback()` 通过 `export KFADAPTER_IMAGE_*` 想回到旧镜像，但 compose 根本不读这两个变量，回滚会重建成当前的 `:latest`。
4. `preflight.sh --local-build` 引用 `deploy/compose.local-build.yaml`，该文件不在仓库中；`deploy/` 还被 `.dockerignore` 排除。
5. 原本覆盖这些脚本的测试（`scripts/check-*.sh`，共约 2,200 行）和 `deployment-verify.yml` 工作流已在 `133fccf` 中删除，脚本目前没有任何自动化验证。

**修复建议**（二选一）

- **方案 A：修复 digest 固定升级流程。**
  - 把 `compose.yaml` 改为 `image: ${KFADAPTER_IMAGE_REPOSITORY:-ghcr.io/oshinop/kfadapter}@${KFADAPTER_IMAGE_DIGEST:?set KFADAPTER_IMAGE_DIGEST}`。
  - CI 中的 `docker compose config --quiet` 需要同步提供该变量。
  - 恢复或重写 `local-build` compose 文件，并恢复脚本测试。
- **方案 B：简化。** 删除 digest 相关逻辑，在 README 中写明 `docker compose pull && docker compose up -d` 加 `backup-state.sh` 的简单流程。

另见 [9.6](#96-部署脚本) 关于热备份的建议。

### 5. 测试大幅倒退，关键包几乎无覆盖

**状态**：✅ 已核实

提交 `ab351a8` 对测试文件的改动（`git show --numstat`）：

| 文件 | 新增 | 删除 | 现状 |
| --- | ---: | ---: | --- |
| `internal/socks/server_test.go` | 15 | **1540** | 27 行，只测两个比较函数 |
| `internal/state/state_test.go` | 124 | **1477** | 228 行 |
| `cmd/kfadapter/main_test.go` | 2 | **810** | 5 行：`func TestCommandPackageBuilds(t *testing.T) {}` |
| `internal/app/runtime_test.go` | 7 | **678** | 26 行 |
| `internal/subscription/subscription_test.go` | 10 | **572** | 34 行 |
| `internal/kuaifan/refresh_test.go` | 159 | 492 | |
| `internal/selector/credentials_test.go` | 22 | 135 | |
| `internal/web/api_test.go` | 54 | 36 | |
| `internal/provider/coordinator/coordinator_test.go` | 22 | 0 | 只测测试文件内自定义的 `liveBuilder` 桩，完全没有调用 coordinator |
| QuickFox 新增 4 个测试文件 | 575 | 0 | |
| 合计 | 约 1,010 | **约 5,740** | 净减约 4,730 行 |

**影响**：SOCKS5 协议处理（认证、地址解析、UDP 关联、连接槽、优雅关闭）、多 provider 协调、runtime 状态机、订阅渲染、命令行入口，目前都没有有效的回归保护。第 1、2、3 条问题正是这些区域的缺陷。

**修复建议**：见 [第 10 节](#10-测试缺口清单)。优先补 coordinator（部分失败）、SOCKS（协议与槽位）、runtime（刷新调度与凭据绑定）。

---

## 4. 启动与运行稳健性

### 6. 调小 `sessionTTL` 或时钟落后导致无法启动

**状态**：✅ 已核实

**情形一：调小 `management.sessionTTL`**

- 启动时 `NewAPI` 调用 `newSessionStore`（[api.go:296](../internal/web/api.go#L296)），后者通过 `RestoreBrowserSessions`（[store.go:1366](../internal/state/store.go#L1366)）恢复已保存的会话。
- 回调 `sessionStore.restore`（[security.go:115-118](../internal/web/security.go#L115)）会拒绝 `expiresAt > now + ttl` 的会话并返回错误；`RestoreBrowserSessions` 遇到回调错误就中止。
- 场景：把 TTL 从 `12h` 改成 `30m` 后重启，只要库里存在会话，`NewAPI` 就失败，进程以退出码 1 结束。容器 `restart: unless-stopped` 会让它反复重启，最长持续 24 小时（直到旧会话自然过期）。
- `internal/web/session_persistence_test.go` 显示这是有意的“拒绝”行为，但丢弃或截断这些会话显然更合理。

**情形二：设备时钟大幅落后**

- `validBrowserSession`（[store.go:1571-1573](../internal/state/store.go#L1571)）要求 `expiresAt <= now + 24h`（`maxSessionLifetime`，[types.go:30](../internal/state/types.go#L30)）；不满足时，Load/Save/Update 都会报 “persistent state is corrupt: malformed browser session”。
- 场景：OpenWrt 路由器没有 RTC，开机后、NTP 同步前，时钟可能落后较多。只要落后超过约 23.5 小时（24h 减去默认 TTL 30m），整个数据库就被判为“损坏”；10 秒一次的 watchdog（[main.go:451](../cmd/kfadapter/main.go#L451) 起）会让进程反复重启，直到时钟追上。

**修复建议**

- 恢复会话时，对超出当前 TTL 或 `maxSessionLifetime` 的会话**截断到上限或直接删除**，而不是报错。
- 不要把“时间上不合理的会话”归类为数据库损坏；它不是结构性错误。
- 补充测试：TTL 变小后重启、时钟回拨 25 小时后启动。

### 7. SOCKS `Accept` 出错会导致整个进程退出

**状态**：✅ 已核实

**位置**：[internal/socks/server.go:223-229](../internal/socks/server.go#L223)

```go
connection, err := listener.Accept()
if err != nil {
    if ctx.Err() != nil || !s.accepting(listener) {
        return nil
    }
    return err
}
```

`Serve` 返回错误后，supervisor 视其为关键 worker 失败并关闭整个进程（`internal/lifecycle/supervisor.go:72-78` 附近）。

**场景**：文件描述符耗尽（EMFILE）。默认最多 1024 个 SOCKS 连接（[server.go:21](../internal/socks/server.go#L21)），每个连接占用客户端、控制、数据 3 个 socket，约 3,000 个 fd。`compose.yaml` 设置了 `nofile: 65536`，但换成 podman 或手工 `docker run` 等未设置 ulimit 的环境（常见默认值 1024），高峰期就会触发，整个适配器直接退出。

**修复建议**：参照 `net/http.Server.Serve`，对 `net.Error` 临时性错误或 `EMFILE`/`ENFILE` 做指数退避（5ms 起、上限 1s）后继续 `Accept`。

### 8. 进程完全没有日志

**状态**：✅ 已核实

**位置**：[cmd/kfadapter/main.go:49-87](../cmd/kfadapter/main.go#L49)

- 全仓库没有使用 `log` 或 `log/slog`。
- 除启动成功时打印一行 `kfadapter: ready management=... proxy=...` 之外，所有失败都只输出固定文案：
  - `serve` 失败：`kfadapter: service stopped`
  - 配置无效：`kfadapter: invalid configuration`
  - 状态无效：`kfadapter: invalid state`
- 注释给出的理由是“底层错误将来可能包含端点或上游上下文”。

**影响**：配置写错、监听地址不在任何 up 接口上、状态目录权限不对、会话恢复失败（第 6 条）、`Accept` 失败（第 7 条）时，运维都无从定位原因。provider 刷新失败等运行期事件也没有任何记录。

**修复建议**

- 引入 `log/slog`，定义统一的脱敏规则（去除 token、密码、完整 URL 查询串等）。
- 配置校验错误本身不含敏感信息（例如“port must be a decimal integer between 1 and 65535”），可以原样输出。
- 启动阶段的错误可按类别输出（例如 “state directory permissions invalid”）。
- 运行期关键事件（刷新成功或失败、provider 过期、凭据轮换）输出 info/warn 级别日志。

### 9. 订阅服务的数据竞争、GET 写库与锁争用

**状态**：✅ 已核实

**位置**：[internal/subscription/service.go:136-182](../internal/subscription/service.go#L136)（`SetSocksAddress`）、[:640-665](../internal/subscription/service.go#L640)（`serveSubscription`）、[internal/web/subscription_adapter.go:29-31](../internal/web/subscription_adapter.go#L29)

1. **数据竞争**：[service.go:661](../internal/subscription/service.go#L661) 在不持有 `s.mu` 的情况下读取 `s.socksAddress`，而 `SetSocksAddress` 在持锁状态下写它。`go test -race` 之所以没报，是因为没有覆盖这条路径的测试。
2. **请求串扰**：`serveSubscription` 先把请求对应的地址写进**共享**字段，再读共享缓存。两个客户端分别通过不同的本机 IP 访问通配监听时，可能拿到对方的地址。
3. **GET 写库**：
   - `GET /api/v1/subscription/url` → `SetSocksAddress` → 地址变化时执行 `store.Update`，重写整份状态。该路由作为 GET 跳过了 Origin 和 CSRF 检查。
   - 分别用 `localhost` 和局域网 IP 打开的两个控制台，会在每次刷新时来回改写已存的订阅；在路由器上还会磨损闪存。
4. **锁争用**：`SetSocksAddress` 即使地址未变，也要先获取全局 `mutationLocker`（第 143-146 行），然后才比较是否相同。于是每次订阅拉取都要排在登录或刷新的网络调用之后，最长可达 3 × 15 秒；等待期间还占着 8 个响应槽之一，其它客户端会收到 503。
5. **错误路径不回滚**：第 178-180 行 `validateRenderedState` 失败时直接返回，没有恢复 `s.socksAddress = previous`，也没有刷新缓存。

**修复建议**

- 把“SOCKS 地址”作为渲染参数按请求传入，不再写入共享可变字段。
- 地址只在监听配置变化时持久化，不要由 GET 请求触发。
- 先无锁比较（或用读锁比较），确实需要变更时才获取全局变更锁。
- 所有错误路径统一回滚。

---

## 5. Provider 协议层

### 10. 快帆 Windows 请求的 `nonce` 被覆盖

**状态**：✅ 已核实

**位置**：[internal/kuaifan/client.go:377-395](../internal/kuaifan/client.go#L377)

```go
payload["lang"] = language
nonce, err := wireprofile.RandomInt(randomness, 100000000)
...
payload["nonce"] = fmt.Sprintf("%08d", nonce)          // 无条件覆盖
if _, hasTime := payload["time"]; !hasTime {            // time 有“缺失才设置”的保护
    payload["time"] = timestamp
}
```

所有请求都经过 `Client.post` → `requestEnvelope`。Windows 客户端显式构造的 nonce 永远发不出去，包括：

- `internal/kuaifan/windows_client.go` 中 `requestNonce()` 的结果（refresh 等请求）；
- `profile/windows.go` 中的 `WindowsFixedNonce`。

**影响**：如果服务端校验 Windows 请求的 nonce 格式或取值，Windows 侧的鉴权或刷新会失败；而快帆登录要求 iOS 和 Windows 两个 profile 都成功，所以会导致整个账号失败。现有测试（`profile/windows_test.go`）只检查结构体字段，没有检查实际发出的请求体。

**修复建议**：与 `time` 一样，只在 `nonce` 缺失时生成；并增加“捕获 Windows 请求实际发出的 JSON”的测试。

### 11. 单次请求超时不会重试

**状态**：✅ 已核实

**位置**：[internal/kuaifan/refresh.go:480-492](../internal/kuaifan/refresh.go#L480)

`retryable` 把 `context.DeadlineExceeded` 归为不可重试。而每个请求都用 `context.WithTimeout(ctx, c.requestTimeout)` 包裹（[client.go:344](../internal/kuaifan/client.go#L344)），超时后 `http.Client.Do` 返回的错误满足 `errors.Is(err, context.DeadlineExceeded)`。于是**最常见的瞬时故障，即单次请求超时，从不重试**。重试循环本身已经在检查父 context 是否结束，把单次超时判为可重试是安全的。

**修复建议**：先判断父 context 是否已结束；若未结束，把 `DeadlineExceeded` 视为可重试。同时移除已弃用的 `Temporary()` 判断。现有重试测试只用了合成的 `net.Error`，应增加真实超时的用例。

### 12. 一条异常线路导致整个登录或刷新失败

**状态**：✅ 已核实

**快帆**

- [client.go:664-671](../internal/kuaifan/client.go#L664) 遇到第一条解析失败的线路就返回错误。
- iOS 的 `ValidateLine`（[profile/ios.go:31-35](../internal/kuaifan/profile/ios.go#L31)）拒绝任何非 `"WIFIIN"` 的 provider。
- Windows 的 `ValidateLine`（[profile/windows.go:30-41](../internal/kuaifan/profile/windows.go#L30)）拒绝未知 provider，以及密码不等于 `wifiin1234` 的 WIFIIN 线路。
- `fetchBothProfiles` 要求两个 profile 都成功，而 `ErrInvalidLine` 不重试。
- 场景：iOS 的 `getLines` 响应中出现一条 `"WS"` 线路（Windows 侧已经会返回这种线路），登录就直接失败；刷新会持续失败直到旧快照 24 小时后过期。

**QuickFox**

- [driver.go:245-275](../internal/quickfox/driver.go#L245) 和 `catalogNode`（第 279 行起）：只要有一条线路满足以下任一条件，整个目录就被拒绝：
  - `isHide=1`
  - 出现新的 `feeType`
  - `lineName != linePoolName`
  - `connectIp` 不是 IPv4
- 账号解析（[driver.go:164-181](../internal/quickfox/driver.go#L164)）：`vipInfo` 的 grade 不是 1 或 2、付费等级找不到未来的到期时间时，整个账号被拒绝。
- 场景：provider 新增一条隐藏线路，所有 QuickFox 登录和刷新都会失败；叠加第 2 条后，快帆也会被拖下线。

**修复建议**

- 把“单条线路不合法”从致命错误改为**跳过并计数**，只有在全部被跳过、或结构性错误（JSON 结构、字段类型）时才失败。
- 被跳过的数量可以在控制台和日志中展示。
- 未知 grade 或 feeType 按“不可用”处理，而不是 schema 错误。

### 13. QuickFox 在本地解析目标域名

**状态**：✅ 已核实

**位置**：[internal/quickfox/transport.go:108-127](../internal/quickfox/transport.go#L108)

客户端通过 SOCKS5 发送域名目标（socks5h 语义）时，QuickFox 传输层用本地解析器（`net.DefaultResolver`，`"ip4"`）解析域名，只取第一条 A 记录，失败时不尝试其它地址。

**影响**

- **DNS 泄漏**：用户访问的每个域名都会发给本地 DNS 服务器。
- 解析结果按本地而非出口节点做地理定位，还可能被污染，导致 provider 连接到错误或不可达的 IP。
- 本地解析到私有地址的域名（如 `localhost` 解析为 `127.0.0.1`）会原样转发给 provider 中继。

**修复建议**

- 如果 QuickFox 协议支持传递域名，应直接透传。
- 如果协议只支持 IPv4：至少依次尝试多条 A 记录；在文档中明确说明存在 DNS 泄漏；可选择拒绝解析到回环或私有地址的目标。

### 14. 拨号成功后 ctx 取消导致连接泄漏

**状态**：✅ 已核实

**位置**：[internal/quickfox/transport.go:129-139](../internal/quickfox/transport.go#L129)（`dialRelay`）、[internal/kuaifan/transport.go:109-117](../internal/kuaifan/transport.go#L109)（`openWIFIIN`）

```go
connection, err := dial(dialContext, "tcp", ...)
cancel()
if ctx.Err() != nil {
    return nil, ctx.Err()   // connection 可能非 nil，但未关闭
}
```

如果 `dial` 成功返回、随后父 ctx 恰好被取消，函数返回 `ctx.Err()` 却没有关闭 `connection`。快帆那边的 `failed` 清理 defer 在这之后才注册，同样不覆盖这种情况。

**修复建议**：在返回 `ctx.Err()` 之前，若 `connection != nil` 则先 `Close()`。

---

## 6. 安全

### 15. 首次初始化可被局域网内任何人抢占

**状态**：✅ 已核实

**位置**：[config.yaml](../config.yaml)（`listenAddr: 0.0.0.0`）、[internal/web/api.go:459-524](../internal/web/api.go#L459)（`access`，setup 分支）

- 默认配置监听所有接口。
- `POST /api/v1/access/setup` 只有以下两道检查：
  - `Origin` 检查：它能防浏览器跨站，但挡不住 `curl -H "Origin: http://<host>:10809"`；
  - 按 IP 的频率限制。
- 场景：新部署完成后、主人打开控制台之前，同一局域网内的任何主机都可以先设置访问 token。之后它能登录 provider 账号，拿到 SOCKS 凭据和订阅 URL。

**附带风险**：控制台只提供明文 HTTP，访问 token、provider 密码和会话 Cookie 都以明文在局域网传输，Cookie 也无法设置 `Secure`。

**修复建议**

- 启动时若尚未初始化，生成一次性设置码并打印到 stdout（`docker logs` 可见），`/access/setup` 必须携带该设置码。这类似 Jupyter 的 token 机制。
- 或者：未初始化前只允许来自 loopback 的 setup 请求。
- 在文档中建议通过带 TLS 的反向代理访问控制台，或仅在可信网络中使用。

### 16. 浏览器会话 token 明文存储

**状态**：✅ 已核实

**位置**：[internal/state/store.go:1457](../internal/state/store.go#L1457)

会话 token 和 CSRF token 以原文写入 `browser_sessions` 表。任何能读取 `state.db` 或其备份归档的人，都可以直接劫持控制台会话。

**修复建议**：token 本身是高熵随机值，存 `SHA-256(token)` 即可，查询时对请求中的 token 做同样的哈希后精确匹配，不需要慢哈希。

### 17. “锁定控制台”失败时服务端会话仍然有效

**状态**：✅ 已核实

**位置**：[web/src/App.tsx:544-561](../web/src/App.tsx#L544)、`web/src/api.ts` 中的 `lockConsole`、[internal/web/api.go:657-665](../internal/web/api.go#L657)

- 前端先把界面切换到锁定页并清空数据，然后才发 `POST /access/logout`；`finally` 中无条件调用 `api.clearSession()`。
- 当服务端删除持久化会话失败时，会返回 `503 session_unavailable`，并且**不清除 Cookie**；网络失败时也是同样的结果。
- 此时浏览器里 `HttpOnly` 的会话 Cookie 仍然有效。界面看起来已锁定，但刷新页面就能直接回到控制台，也没有“重试锁定”的入口。
- `api.ts` 原本的设计是“网络失败时保留 CSRF 以便重试锁定”（见 `web/src/security.test.ts` 中的相关用例），但被 `App.tsx` 的 `finally` 抵消了。

**修复建议**

- 服务端：无论持久化删除是否成功，都先撤销内存会话并清除 Cookie，持久化删除失败时稍后重试。
- 前端：锁定失败时明确提示并提供重试，不要假装已经锁定。

另见 [9.5](#95-web-后端) 中与安全相关的低优先级项，包括 SOCKS 已建立隧道不随凭据轮换断开、未认证连接占满槽位、访问限流按精确 IP 计数等。

---

## 7. 依赖、CI 与构建

### 18. 可达漏洞：`x/text` 与 Go 标准库

**状态**：✅ 已核实（govulncheck，详见 [1.4](#14-govulncheck本地工具链-go1263)）

**修复建议**

- `go get golang.org/x/text@latest`（至少 v0.39.0），顺带升级 `golang.org/x/net` 和 `golang.org/x/crypto`。
- CI 的 `go-version` 升级到 `1.26.6` 或更高；也可以在 `go.mod` 中加 `toolchain go1.26.6`，统一本地、CI 和 Docker 的工具链下限。
- 在 CI 中增加 `govulncheck ./...` 步骤，发现可达漏洞时让构建失败。

### 19. 镜像发布不依赖 CI 结果

**状态**：✅ 已核实

**位置**：[.github/workflows/publish.yml](../.github/workflows/publish.yml)

- `publish.yml` 由 `push: branches: [main]` 独立触发，与 `ci.yml` 并行执行。即使测试失败，`latest` 镜像也会照常发布，而 `compose.yaml` 默认拉取的正是 `latest`。
- `ci.yml` 中的 actions 都按 commit SHA 固定了版本，`publish.yml` 中的 `docker/metadata-action@v6`、`setup-buildx-action@v4`、`login-action@v4`、`build-push-action@v7` 却只用了浮动标签。这个 job 持有 `packages: write` 权限，供应链风险更高。

**修复建议**

- 把发布 job 合并进 `ci.yml` 并加 `needs: verify`，或改为 `on: workflow_run`（仅在 CI 结论为 success 时触发）。
- 所有 actions 按 SHA 固定。
- 可选：开启 `provenance: true`、`sbom: true`，为镜像签名。

### 20. Dockerfile 缓存失效，Node 版本与 CI 不一致

**状态**：✅ 已核实

**位置**：[Dockerfile](../Dockerfile)

1. 先 `COPY . .` 再 `npm ci`：任何源码改动都会让 npm 依赖层和 Go 模块下载全部失效，每次构建都从零安装。
2. 前端使用 `golang:1.26-alpine` 中 `apk add nodejs npm` 安装的 Node 构建，版本由 Alpine 仓库决定，而 CI 使用的是 Node 24。
3. 镜像没有声明 `HEALTHCHECK`，健康检查只在 `compose.yaml` 中定义；脱离 compose 运行时没有健康检查。

**修复建议**（示意）

```dockerfile
FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build   # 产物输出到 ../internal/web/static

FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/web/static ./internal/web/static
ARG VERSION=devel
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /kfadapter ./cmd/kfadapter
```

### 21. npm 构建链漏洞

**状态**：✅ 已核实（详见 [1.5](#15-npm-audit--npm-outdated)）

受影响的都是构建或测试依赖，不会进入产物。执行 `npm audit fix` 即可处理；`@vitest/mocker` 可能需要升级 vitest 大版本。

### 22. staticcheck 发现的问题

**状态**：✅ 已核实（详见 [1.3](#13-staticcheck-checks-all-st1000-st1003)）

**修复建议**

- 删除 `wifiin/hmac.go` 和 `Runtime.clearActiveSession`。
- `retryable` 中去掉 `Temporary()`。
- CFB 弃用警告加 `//lint:ignore SA1019` 并注明“协议兼容所需”。
- `wifiin/tcp.go:277`：允许有限次数的 `(0, nil)` 后再返回 `io.ErrNoProgress`。
- 把 staticcheck 加入 CI。

---

## 8. 前端与可维护性

### 23. 前端若干 bug

**状态**：✅ 已核实

| 问题 | 位置 | 说明 | 建议 |
| --- | --- | --- | --- |
| 会话切换时丢失排队中的重载 | [App.tsx:363-375](../web/src/App.tsx#L363) | 重载循环检测到 `revision` 变化后，会把 `queued` 置为 false 并返回，丢掉 `completeAccess` 刚排入的重载。场景：`/status` 或 `/nodes` 较慢时先锁定再解锁，`status` 停留在 null，已登录 provider 的情况下仍显示 “Connect account”。 | 返回前若检测到新 revision 且有新排队，应继续调度 |
| 没有加载状态 | [App.tsx:392-398](../web/src/App.tsx#L392) | `!status` 被当成“需要 provider 登录”，每次加载都会闪现登录表单；`/status` 返回 503 时表单一直停留，诱导用户重新输入 provider 密码。 | 区分 loading、error、needs-login 三种状态 |
| 一个请求失败丢掉全部数据 | [App.tsx:336-339](../web/src/App.tsx#L336) | `Promise.all([api.nodes(), api.subscriptionURL()])`：仅订阅 URL 失败时，节点列表也被丢弃。 | 改用 `Promise.allSettled` 分别处理 |
| 错误码不匹配 | [App.tsx:147](../web/src/App.tsx#L147) | 前端判断 `rate_limited`，后端实际返回 `access_rate_limited` 或 `login_rate_limited`（[api.go:486](../internal/web/api.go#L486) 等）。用户只能看到通用的 “Too Many Requests”；`account_exists` 显示为 “Conflict”。 | 对齐错误码，补全文案 |
| 延迟显示为 “0 ms” | [api.go:203](../internal/web/api.go#L203) | `TCPLatencyMS int` 没有 `omitempty`。从未探测过的节点，列表显示 “—”，详情弹窗却显示 “0 ms”。 | 改为 `*int` 或加 `omitempty` |
| 显示 “Jan 1, 1” | [api.go:143-146](../internal/web/api.go#L143)、`Deployment.StartedAt` | `time.Time` 加 `omitempty` 不会被省略，首次刷新前页面显示 “Updated Jan 1, 1”。 | 改用 Go 1.24+ 的 `omitzero` |
| 类型漂移 | [web/src/types.ts:101](../web/src/types.ts#L101) | `LoginResponse { account, status }` 与后端不符：后端直接返回 Account（[api.go:630](../internal/web/api.go#L630)）。`compatibilityError` 从未由后端发送，`SubscriptionMetadata`、`expiresAt` 未使用。 | 从 Go 结构体生成 TS 类型，或至少补一个契约测试 |

### 24. `App.tsx` 过大、无 ESLint、存在死组件

**状态**：✅ 已核实

- **`web/src/App.tsx` 有 2,168 行**，包含 17 个组件和整个会话状态机。重复逻辑：
  - 探测结果合并逻辑重复 3 份（约第 435、1734、1796 行附近）；
  - 复制反馈计时器重复 2 份（约第 1095、2097 行附近）；
  - “执行操作、重载、处理会话丢失、finally 清理”的模式重复 4 份（约第 494-581 行）。
- **拆分建议**：
  - hooks：`useConsoleSession`、`useEventStream`、`useCopyFeedback`、`useNodeProbe`
  - 组件：`AccessTokenCard`、`ProviderCredentialsForm`、`StatusConsole`、`NodeList`、`NodeDetailsPopover`
- **没有 ESLint**，也没有 `eslint-plugin-react-hooks`。例如约第 1102 行的 effect 依赖项漏掉了 `clearCopyMessage`。
- **死代码**：
  - `web/src/components/ui/table.tsx` 未被引用；
  - `PopoverAnchor` 被导出但未使用；
  - `web/tsconfig.node.json` 未被任何配置引用；
  - 后端的 `GET /api/v1/subscription` 和 `POST /api/v1/diagnostics/export` 在前端没有调用方。
- **依赖版本固定方式不一致**：`vite`、`vitest`、`typescript`、`happy-dom`、`@fontsource/*` 为精确版本；`react`、`radix-ui`、`lucide-react`、`tailwindcss`、`@vitejs/plugin-react` 使用 `^`。建议统一策略。

### 25. 文档与开发体验

**状态**：✅ 已核实

- **没有 README**：部署方式、配置字段含义、首次初始化流程、升级与备份恢复流程、支持的 provider、已知限制（如第 13 条的 DNS 泄漏）都没有文档。
- **`config.yaml` 注释过时**：头部仍写着 “Provider requests use https://ws.kuaifan.co.”，没有反映已支持 QuickFox 等多 provider。
- **全新 clone 无法直接构建或测试**：`internal/web/assets.go` 使用 `//go:embed all:static`，而 `internal/web/static/` 被 gitignore，不先执行 `cd web && npm ci && npm run build`，`go build`、`go vet` 和 `go test` 都会失败。可选做法：
  - 在 README 中说明；
  - 提交一个占位的 `static/index.html`（构建时覆盖）；
  - 使用构建标签区分 dev 和 release。

---

## 9. 其他低优先级发现（按模块）

### 9.1 kuaifan

范围：`internal/kuaifan/`（含 `wifiin/`、`profile/`）。

| 发现 | 位置 | 状态 |
| --- | --- | --- |
| **延迟 IV 截止时间实际上是“首字节超时”**：第一次客户端写入时设置 `HandshakeTimeout`（默认 15s）的读截止时间，等待服务端 IV。按 `docs/wifiin_bug_report.md` 的描述，服务端 IV 与首个下行数据同包发送；若确实如此，长轮询或慢 API 在 15 秒内没有响应字节就会 i/o timeout，UDP 关联在首个数据报 15 秒内无回复就会被拆除。 | [wifiin/lazy.go:64-75](../internal/kuaifan/wifiin/lazy.go#L64) | ❓ 取决于服务端 IV 时序 |
| **轮换后的 refresh token 可能丢失**：`refreshBoth` 拿到新 token 后，如果 `fetchBothProfiles` 失败，新 token 被丢弃，coordinator 保留旧的 `RefreshState`。`RefreshSession` 被重试，但它未必幂等：服务端已轮换 token 而响应丢失时，重试会带着旧 token。若服务端让旧 token 失效，账号就会卡住，直到用户重新登录。 | [refresh.go:112-130](../internal/kuaifan/refresh.go#L112)、[:168-175](../internal/kuaifan/refresh.go#L168) | ❓ 取决于服务端 token 轮换语义 |
| `RelayDatagrams` 和握手不响应 `ctx`：数据报中继只在调用方关闭 `Control` 时停止（目前 SOCKS 服务器会这么做）；握手在 ctx 取消后仍会持续到 `HandshakeTimeout`。 | `transport.go` | 🔍 |
| 最大长度（65535）的 UOT 帧会拆掉整个 UDP 关联：输出缓冲为 65535，需要 65536，返回 `io.ErrShortBuffer`，而此时帧已被读走。只有异常或恶意服务端会发送这种帧。 | `wifiin/udp.go:120-122`、`transport.go:170` | 🔍 |
| `RandomInt` 在 `n > 2^32` 时 `limit` 为 0，会死循环。当前调用方不会传这么大的值，但它是导出函数。 | [profile/profile.go:56](../internal/kuaifan/profile/profile.go#L56) | 🔍 |
| 节点上限不一致：每个 profile 允许 4096 条线路（`maxLines`），合并后的快照上限也是 `maxNodes = 4096`。目录很大时 `ValidateSnapshot` 会失败。 | `refresh.go:372` 附近 | 🔍 |
| 重复代码：`buildProviderExtension`、`validProviderExtensionField` 与 `wifiin.ProviderExtensionForProfile` 功能重复，且常量分别定义在 `profile/ios.go:18-21` 和 `wifiin/tcp.go:19-21`。两边一旦不一致，刷新仍然成功，但所有 iOS 隧道会在连接时以 `ErrInvalidTarget` 失败。 | `client.go:577-597` | 🔍 |
| 仅测试使用的代码：`FetchConfig`（完全未用）、`validateLines`/`validateLinesForLanguage`、`EncodeRequest`/`DecodeResponse`、`wifiin.ProviderExtension`。 | 多处 | 🔍 |
| 误导性注释：`refresh.go:93-94,139` 的“清除密码”只清除了局部副本，密码仍留在闭包的 `input` 和调用方结构体中；`codec.go:103` 注释称 “strictly Base64”，但 `StdEncoding` 接受 `\r\n` 且没有使用 `.Strict()`。 | 多处 | 🔍 |

`docs/wifiin_bug_report.md` 是提交给 provider 的服务端 bug 报告（新建隧道的首个大响应丢失 16 字节），与代码中 16 字节 IV 的分帧一致，内容本身不存在误导，但代码中没有针对该问题的检测或缓解。

### 9.2 QuickFox 与 SOCKS

范围：`internal/quickfox/`、`internal/socks/`、`internal/provider/`、`internal/selector/`。

| 发现 | 位置 | 状态 |
| --- | --- | --- |
| **控制 ACK 未校验、数据连接不读回复**：14 字节 ACK 读到后没有检查内容，数据连接写完前导包后也不等回复就调用 `Ready`。provider 拒绝 token 或无法到达目标时，SOCKS 客户端仍会收到 `0x00 succeeded`，随后立即 EOF，QuickFox 的应答码永远无法反映上游失败。 | [quickfox/transport.go:69-98](../internal/quickfox/transport.go#L69) | ❓ 协议为逆向实现 |
| **已建立的隧道不随凭据轮换或登出断开**：`admit` 只在建立时检查一次，TCP 中继和 UDP 关联没有最大生命周期（注释称 “Existing TCP flows are unaffected”，属有意设计）。凭据泄露后即使轮换，攻击者已打开的长连接仍能继续使用。 | [socks/server.go:99-107](../internal/socks/server.go#L99) | ✅ |
| **未认证连接可占满槽位**：槽位在 accept 时占用，未认证连接最多占用 15 秒握手时间，没有按 IP 的上限。`listenAddr` 为局域网 IP 时，单台主机每秒约 70 个连接就能占满 1024 个槽位。 | [socks/server.go:230-246](../internal/socks/server.go#L230)、[:267](../internal/socks/server.go#L267) | ✅ |
| SOCKS 应答码不准确：对 QuickFox 节点发起 UDP ASSOCIATE 时，会先绑定 UDP socket 再以 `0x01` 失败，应为 `0x07`（command not supported）；IPv6 CONNECT 返回 `0x01`，应为 `0x08`（address type not supported）。 | [quickfox/transport.go:104-113](../internal/quickfox/transport.go#L104)、`socks/server.go:357-369` | 🔍 |
| UDP 关联被首个数据报抢占：客户端声明 `0.0.0.0:0`（最常见）时，来自该客户端 IP 任意端口的第一个数据报会占有关联。RFC 1928 允许这种做法，但在共享 IP 上，其它本地进程可以抢占关联并收到回复。 | [socks/udp.go:60-72](../internal/socks/udp.go#L60) | ✅ |
| coordinator 在 `Begin` 之前读取 `Current()`：`Refresh`、`Logout`、`Expire` 都先读快照再 `Begin`；`Expire` 的“全部过期”分支甚至不经过 `Begin` 就提交 `nil` 并 `MarkExpired`。目前安全仅仅是因为 `app.Runtime` 用 `r.mutations` 串行化了所有调用；其它调用方可能把旧 provider 重新发布（例如让刚登出的账号复活）。 | [coordinator.go:87-205](../internal/provider/coordinator/coordinator.go#L87) | ✅ |
| `credentials.Password = ""` 清除的只是局部副本，无实际作用。 | [coordinator.go:69](../internal/provider/coordinator/coordinator.go#L69)、`quickfox/driver.go:72` | ✅ |
| `AuthenticateAt` 忽略时间参数，因此 `HandleConn` 中重复的再认证（`server.go:332, 379`）是多余的。 | `selector/credentials.go:153` | 🔍 |
| 仅测试使用或未使用：`Registry.Resolve`、`Registry.Authenticate`、`ErrAuthorityExpired`、`commandBind`、`methodNoAuthentication`。 | 多处 | 🔍 |
| `buildSelectors` 忽略 `generation` 和 `previous` 参数。 | [coordinator.go:321](../internal/provider/coordinator/coordinator.go#L321) | ✅ |
| `tunnelAuthority.AppID` 从未赋值，恒为 0。 | `quickfox/driver.go:226` | 🔍 |
| `RedactAccount` 重复实现。 | `provider/provider.go:71`、`state/types.go:158` | 🔍 |
| `Build` 对非法 ID 返回的是 `ErrDuplicateNodeID`。 | `selector/credentials.go:186-187` | 🔍 |

### 9.3 state、app、config、lifecycle

范围：`internal/state/`、`internal/app/`、`internal/subscription/`、`internal/config/`、`internal/lifecycle/`、`cmd/kfadapter/`。

| 发现 | 位置 | 状态 |
| --- | --- | --- |
| **遗留的 `refresh_policy` 静默覆盖配置**：代码中已没有任何地方写入 `Preferences`（RefreshPolicy、ExcludedNodeIDs、RevealEndpoints），但迁移保留了旧值。升级安装后，`config.yaml` 中的 `provider.refreshInterval` 会被永久忽略，且 UI 中无法修改。 | [app/runtime.go:255-261](../internal/app/runtime.go#L255) | ✅ |
| **每次存储操作都做完整校验**：`PRAGMA integrity_check(1)`、外键检查和完整状态加载。10 秒一次的 watchdog 和 `AccessStatus`（[runtime.go:351](../internal/app/runtime.go#L351) 调用 `store.Load()`，由 access-status 接口触发）都会走这条路径。每次 `Update` 还会删除并重新插入整份状态（[store.go:998](../internal/state/store.go#L998)），同时开启 `secure_delete` 和 `synchronous=FULL`。在路由器闪存上既耗 CPU 又磨损存储。 | [store.go:750-760](../internal/state/store.go#L750) | ✅ |
| 配置接受带 zone 的 IPv6 地址（如 `fe80::1%eth0`），`validate-config` 能通过，但 `subscription.parseAddress` 拒绝 zone，`serve` 只输出通用的 “service stopped”。 | [config.go:244](../internal/config/config.go#L244)、`subscription/subscription.go:101-105` | 🔍 |
| `healthcheck` 和 `validate-config` 在 `config.yaml` 缺失时会创建它（`openOrCreate`）。校验或探活类命令不应有写副作用。 | [config.go:199-212](../internal/config/config.go#L199) | ✅ |
| **关闭顺序**：supervisor 的循环按逆序遍历 worker，但每个 `Shutdown` 都在独立 goroutine 中**并发**执行，逆序没有意义。`runtime.Stop()` 需要获取变更锁，可能排在 Web UI 发起的登录请求之后，而 `http.Server.Shutdown` 不会取消该请求的 context；排空时间超过 20 秒时，SIGTERM 以退出码 1 结束。排空超时后执行 `store.Close()` 时仍可能有 goroutine 在使用 store，而 `ensureOpenLocked` 会静默重新打开它（`store.go:208-227`）。 | [lifecycle/supervisor.go:86-96](../internal/lifecycle/supervisor.go#L86) | ✅（并发执行） / 🔍（其余） |
| 探测延迟使用墙钟：`finished := r.now().UTC()`，`.UTC()` 会剥离单调时钟读数，时钟跳变时 RTT 会失真。 | [app/runtime.go:730-733](../internal/app/runtime.go#L730) | ✅ |
| 首次运行失败会卡死：初始建库过程中进程被杀，会留下没有表的 `state.db`，此后永远被判为损坏（`store.go:282-330`）。`./data` 中出现任何杂项文件（如 `lost+found`）也会阻止首次运行（`validateEmptyStateDir`）。 | [store.go:333-341](../internal/state/store.go#L333) | ✅ |
| `SetSocksAddress` 在 `validateRenderedState` 失败时不回滚地址。 | [subscription/service.go:178-180](../internal/subscription/service.go#L178) | ✅ |
| 死代码：`PublishAccount`、`PublishSnapshot`、`PublishCurrent` 及配置字段 `Snapshots`；`Runtime.clearActiveSession`；`SelectorCoordinator.now` 字段；遗留的排除节点渲染逻辑（`Node.Excluded` 从未为 true，v8 迁移已丢弃相关订阅）。 | [service.go:350-534](../internal/subscription/service.go#L350) 等 | ✅（前两项） / 🔍 |
| `selector.Derive` 忽略 `NodeIdentity` 的 Provider、Host、Port 字段，容易误导。 | `selector/credentials.go` | 🔍 |
| `PublishAccount` 回滚失败时，返回的错误丢掉了回滚错误本身。 | `service.go:369-370` | 🔍 |
| 清理过期活跃会话的逻辑分散在 3 处：`main.go:198`、`runtime.go:267`、`NewManagerWithSubscription`。 | 多处 | 🔍 |
| `store.go` 有 1,653 行，混合了文件安全、迁移、状态映射和会话，建议拆分；另有一处过时的 v7 注释（`store.go:1032`）。 | `internal/state/store.go` | ✅ |

### 9.4 Web 前端

范围：`web/src/`。

| 发现 | 位置 | 状态 |
| --- | --- | --- |
| 可访问性：`role="tree"` / `"treeitem"` 用在 `<details>` 和 `<article>` 上，但没有 `aria-expanded`、roving focus 或方向键支持，屏幕阅读器会进入一个无法操作的树模式。 | [App.tsx:1864-1898](../web/src/App.tsx#L1864) | ✅ |
| `<dl>` 内嵌套了多层 `div`，不符合规范。 | App.tsx 约第 2120-2134 行 | 🔍 |
| “Sign out all” 没有二次确认，而移除单个账号有。 | [App.tsx:1320](../web/src/App.tsx#L1320) | ✅ |
| 弹窗内的复制回退：在 `http://<局域网 IP>` 上，Clipboard API 不可用，走的是回退路径。回退中 `textarea.select()` 会把焦点移出 Radix Popover，可能导致弹窗关闭、“已复制”提示丢失。目前只有订阅链接的回退路径有测试。 | [App.tsx:190-203](../web/src/App.tsx#L190) | ❓ |
| 测试仍导航到已删除的路由（`/nodes`、`/subscription`）。 | [operations.test.tsx:30](../web/src/operations.test.tsx#L30)、[:184](../web/src/operations.test.tsx#L184)、[:198](../web/src/operations.test.tsx#L198) | ✅ |

### 9.5 Web 后端

范围：`internal/web/`。

| 发现 | 位置 | 状态 |
| --- | --- | --- |
| 已知路由用错 HTTP 方法时返回 404 或 401，而不是 405。 | [api.go:360-431](../internal/web/api.go#L360) | ✅ |
| 访问限流按精确 IP 计数，IPv6 客户端可以轮换地址绕过（loopback 已做合并处理）。 | [security.go:483](../internal/web/security.go#L483) | ✅ |
| 带哈希文件名的静态资源也返回 `Cache-Control: no-store`，每次都会完整重新下载。可对 `/assets/*` 使用 `public, max-age=31536000, immutable`。 | [assets.go:25](../internal/web/assets.go#L25) | ✅ |

已确认没有问题的项：Go 1.26 会在开始流式响应前清除读超时，`ReadTimeout` 不会切断 SSE；CSP 策略可行，react-remove-scroll 注入 `<style>` 只发生在模态 Popover 中，而本项目的 Popover 不是模态的。

### 9.6 部署脚本

范围：`scripts/`。

- 脚本对路径的校验非常严谨：不跟随符号链接、校验 inode 身份、限制归档大小和成员数、原子提交恢复。
- 但仅为了备份一个 SQLite 文件，就有约 1,700 行 shell 和 Python，而且备份和恢复都要求**停机**。
- 可以考虑提供 `kfadapter backup` 子命令，使用 SQLite 在线备份（如 `VACUUM INTO`）实现热备份，把大部分安全校验收敛到 Go 代码中统一测试。
- 与第 [4](#4-升级脚本与-composeyaml-脱节无法使用) 条一并决定这些脚本的去留。

---

## 10. 测试缺口清单

按优先级排列：

1. **provider coordinator**：`Login`、`Refresh`（部分失败、全部失败、取消传播）、`Logout`（最后一个 provider）、`Expire`（部分过期、全部过期）。当前测试完全没有调用 coordinator。
2. **SOCKS 服务器**：
   - 方法协商、RFC 1929 用户名密码认证（成功、失败、畸形包）；
   - 请求与地址解析（IPv4、IPv6、域名、超长域名）；
   - 各种应答码；
   - UDP ASSOCIATE：源地址过滤、关联随控制连接关闭；
   - 连接槽计数与满载拒绝；
   - `Serve`/`Shutdown` 的排空；
   - `Accept` 错误退避（修复第 7 条后）。
3. **runtime**：定时刷新调度与重试、凭据绑定变化（第 3 条）、遗留 `refresh_policy`（9.3）。
4. **SSE**：不带 `Origin` 的同源请求、跨站拒绝、`sse_capacity`、前端 CLOSED 后重连。
5. **state**：
   - Manager 状态迁移（`Begin`/`finish`）与 `InstallEpoch` 回滚；
   - `CommitControlSnapshotLocked` 部分失败回滚；
   - 浏览器会话的存储函数；
   - `Update` 回调出错时回滚；
   - 时钟偏移（第 6 条）。
6. **subscription**：`SetSocksAddress`、订阅服务与过滤、并发访问（开启 `-race`）。
7. **kuaifan**：
   - Windows 请求实际发出的请求体（能覆盖第 10 条）；
   - `RelayStream`、`RelayDatagrams` 的拆除，`openWIFIIN`，`outboundCFBWriter.CloseWrite` 的空闲半关闭路径；
   - `LazyInboundReader` 截止时间的设置与清除、IV 超时；
   - 真实请求超时的重试（第 11 条）。
8. **QuickFox**：`relayXOR` 的半关闭与取消。
9. **Web 后端**：`csrf_failed`、`login_rate_limited`、`login_in_progress`、`body_too_large`、`invalid_host`、静态资源与 SPA 回退、诊断导出脱敏。
10. **前端**：锁定失败、重载竞态（第 23 条）、错误码文案。
11. **命令行**：`healthcheck`、`validate-config`、`validate-state`、`version` 各子命令的退出码与输出。
12. **`internal/endpoint`**：目前没有任何测试。
13. **部署脚本**：恢复或重写原 `check-*.sh` 测试，或随第 4 条的方案一起调整。

---

## 11. 建议处理顺序

| 阶段 | 内容 | 理由 |
| --- | --- | --- |
| **第一批** | #1 SSE、#2 刷新连坐、#3 凭据轮换（先明确产品语义） | 用户可直接感知，改动范围小，收益最大 |
| **第二批** | #6 启动失败、#7 Accept 退避、#18 依赖与工具链升级、#8 基础日志 | 影响可用性与可运维性，改动局部 |
| **第三批** | 按第 10 节补回 SOCKS、coordinator、runtime 测试；#9 订阅服务；#10–#14 provider 问题 | 重建安全网后再做更深的修改 |
| **第四批** | #4 升级流程（修复或删除）、#19 发布门禁、#20 Dockerfile、#15/#16/#17 安全加固 | 部署与安全加固 |
| **持续** | #23/#24 前端修复与拆分、#25 README、第 9 节低优先级项 | 可维护性 |

---

## 附录 A：复现检查的命令

```bash
# 前端构建与测试（Go 构建依赖前端产物）
cd web && npm ci && npm run build && npm test && cd ..

# Go 构建、静态检查与测试
go build ./...
go vet ./...
go test -race -count=1 ./...
go test -count=1 -cover ./...

# 额外静态分析
go install honnef.co/go/tools/cmd/staticcheck@latest
staticcheck -checks all,-ST1000,-ST1003 ./...
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...

# 依赖状态
go list -m -u all | grep '\['
cd web && npm audit && npm outdated

# 测试删除规模
git show --numstat --format= ab351a8 | grep _test
```

## 附录 B：做得好的地方

以下方面经审阅确认质量良好，修改时应注意保持：

- **SQLite 使用规范**：
  - 每个事务都有延迟 `Rollback`，每个 rows 迭代都检查 `Close` 和 `Err`；
  - 单连接池，连接串中设置 `foreign_keys`、`busy_timeout`、`synchronous=FULL`，journal 模式为 DELETE；
  - 每个迁移步骤在独立事务中执行，会重读版本号，可安全重跑。
- **加密实现**：`DeriveKey`、PBKDF2、每连接独立的随机 CFB IV、填充处理均正确；`HandshakeReader` 对分片和合并读取的处理正确；UOT 长度有边界检查。
- **并发清理**：`Relay` 以及并行登录、刷新辅助函数中的 goroutine 和 channel 清理正确；`LazyInboundReader` 的加锁没有发现竞争。
- **Web 安全基线**：
  - 严格的 CSP、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`、`nosniff`；
  - Cookie 为 `HttpOnly` + `SameSite=Strict`；
  - 状态变更请求同时校验 Origin、Content-Type 和 CSRF；
  - 使用常量时间比较；
  - 请求体大小有上限，JSON 解码严格。
- **配置加载**：限制大小、`KnownFields(true)` 拒绝未知字段、拒绝多文档 YAML、所有时长都有范围校验。
- **容器加固**：非 root 用户、只读根文件系统、`cap_drop: ALL`、`no-new-privileges`、有 pids、内存和 CPU 限制。
- **部署脚本**：不跟随符号链接的路径处理、inode 身份校验、归档大小与成员数限制、原子恢复并保留旧目录。

---

## 修复记录（2026-09-26）

以下按原编号记录处理结果。三个需要取舍的问题按以下决定实施：#3 保持凭据稳定；#4 修复 digest 固定升级流程；#15 首次设置仅限本机回环地址。

### 验证结果

| 检查 | 修复前 | 修复后 |
| --- | --- | --- |
| `go vet ./...` | 干净 | 干净 |
| staticcheck | 7 项 | 0 项 |
| govulncheck（第三方模块） | 1 个可达漏洞 | 0 个（剩余均为本地 go1.26.3 标准库，需升级到 go1.26.6+） |
| `go test -race ./...` | 通过 | 通过 |
| `npm audit` | 6 个漏洞 | 0 个 |
| ESLint（新增，含 react-hooks） | 无 | 0 项 |
| 前端测试 | 53 个 | 62 个 |
| 部署脚本测试（Linux 容器） | 已删除 | 恢复并通过：upgrade、backup、restore、restore-commit、preflight、deployment-assets、configured-healthcheck、compose-security、image-security |

测试覆盖率变化：

| 包 | 修复前 | 修复后 |
| --- | ---: | ---: |
| `cmd/kfadapter` | 0.0% | 65.3% |
| `internal/app` | 8.0% | 60.1% |
| `internal/provider/coordinator` | 0.0% | 81.4% |
| `internal/socks` | 1.0% | 74.2% |
| `internal/subscription` | 10.7% | 65.5% |
| `internal/state` | 34.5% | 45.4% |
| `internal/endpoint` | 无测试 | 有测试 |

### 主要问题

| # | 结果 | 处理方式 |
| --- | --- | --- |
| 1 | ✅ | 后端对只读事件流：`Origin` 存在时必须匹配，缺失时依据 `Sec-Fetch-Site` 判断；前端在流被关闭后按 1s→30s 指数退避重建。新增前后端测试。 |
| 2 | ✅ | coordinator 各 provider 独立刷新、互不取消；成功结果照常发布，失败以 `RefreshError`（含 provider 名）合并返回，部分失败时状态为 degraded。新增 7 个 coordinator 测试。 |
| 3 | ✅ | 新增按 provider 记录账号摘要的名册（schema v9，表 `subscription_account_roster`）。增减或过期 provider 不再轮换凭据，只有同一 provider 换账号才轮换。v8 的旧绑定在下一次同组账号登录时自动迁移到名册。 |
| 4 | ✅ | `compose.yaml` 改为 `repo@${KFADAPTER_IMAGE_DIGEST}`；新增 `deploy/compose.local-build.yaml`；从初始提交恢复 `scripts/check-*.sh`，并适配 host 网络与 `config.yaml`；CI 新增 deployment 任务。 |
| 5 | ✅ | 补回并新增 coordinator、SOCKS、runtime 集成、subscription、main、state、web、kuaifan、quickfox、endpoint 的测试，覆盖率见上表。 |
| 6 | ✅ | 恢复会话时，超出当前 TTL 的会话截短到 TTL；超出最大生命周期（时钟回拨）的会话被清理，不再被视为数据库损坏。 |
| 7 | ✅ | `Accept` 遇到 EMFILE、ENFILE、ECONNABORTED 等临时错误时按 5ms→1s 退避后继续。 |
| 8 | ✅ | 新增 `internal/logging`（slog），记录启动失败原因、ready、刷新成功或失败、过期、凭据轮换；`validate-config`、`validate-state`、`healthcheck` 输出具体原因。 |
| 9 | ✅ | 订阅按请求的 SOCKS 地址渲染，不再修改共享状态或写库；删除 `SetSocksAddress`。 |
| 10 | ✅ | 只在请求未提供 `nonce` 时才生成。 |
| 11 | ✅ | 单次请求超时和连接重置视为可重试；移除已弃用的 `Temporary()`。 |
| 12 | ✅ | 快帆和 QuickFox 跳过单条异常线路或分组，只有全部无效时才失败；QuickFox 遇到未知等级或无有效期时按标准账号处理。 |
| 13 | ✅ | 依次尝试所有 A 记录，跳过回环、私有和 CGNAT 地址；README 注明存在 DNS 泄漏。 |
| 14 | ✅ | ctx 取消时关闭已拨通的连接。 |
| 15 | ✅ | 未初始化时只接受回环地址发起的 setup；`/access/status` 返回 `setupAllowed`，前端据此显示说明。 |
| 16 | ✅ | 内存和 SQLite 中只保存 `SHA-256(cookie token)`。升级后已有的浏览器会话需要重新解锁一次。 |
| 17 | ✅ | 锁定时先在内存中撤销会话并清除 Cookie，持久化删除失败后会重试；前端锁定失败时给出明确提示。 |
| 18 | ✅ | x/text、x/net、x/crypto、x/sys 升级到最新；CI 使用 `1.26.x` 加 `check-latest`，并新增 govulncheck 和 staticcheck。 |
| 19 | ✅ | 删除 `publish.yml`，发布任务并入 CI，依赖 `verify` 和 `deployment` 两个任务；所有 actions 按 SHA 固定；开启 provenance 和 SBOM；任务摘要输出镜像 digest。 |
| 20 | ✅ | 多阶段构建：独立的 `node:24-alpine` 前端阶段，依赖清单先复制以利用缓存，`go mod download` 单独成层；新增 `HEALTHCHECK`。 |
| 21 | ✅ | `npm audit fix`，并把 vitest 升级到修复版 4.1.11。 |
| 22 | ✅ | 删除 `wifiin/hmac.go` 和 `clearActiveSession`；为 CFB 加 `lint:ignore` 并说明原因；读循环允许有限次数的 `(0, nil)`。 |
| 23 | ✅ | 修复重载被丢弃、无加载态（新增可重试的错误卡片）、`allSettled`、错误码文案、`tcpLatencyMs`、`omitzero`、`LoginResponse` 类型漂移。 |
| 24 | ✅ | `App.tsx` 从 2,168 行拆为 `App`、`AuthCards`、`StatusConsole`、`NodeList`、`lib/*`、`hooks/useCopyFeedback`；合并重复的探测结果合并和复制反馈逻辑；接入 ESLint 并修复其全部发现；删除 `table.tsx`、`PopoverAnchor`、`tsconfig.node.json`；依赖统一固定为精确版本。 |
| 25 | ✅ | 新增 `README.md`；更新 `config.yaml` 注释；前端产物改到 `internal/web/static/dist/` 并提交占位文件，全新 clone 可直接 `go build`；`web/go.mod` 把前端目录隔离出 Go 模块。 |

### 第 9 节低优先级项

已处理：

- **kuaifan**：握手和 UDP 中继响应 ctx 取消；超长 UOT 帧只丢弃该帧；`RandomInt` 在 n > 2³² 时不再死循环；合并后的节点裁剪到上限，优先保留可用节点；provider extension 统一由 `wifiin` 包构造；删除 `FetchConfig`；修正误导性注释。
- **SOCKS**：已建立的隧道每 30 秒复查一次，凭据轮换或登出后断开；单个 IP 最多 32 个未认证连接；QuickFox 不支持的命令和地址类型返回 `0x07`/`0x08`；coordinator 在取得租约后才读取快照；`AuthenticateAt` 改为 `Authenticate`；删除 `Resolve`、`ErrAuthorityExpired`、`buildSelectors` 的无用参数、重复的 `RedactAccount`；`Build` 对非法 ID 返回正确的错误。
- **state/app**：忽略遗留的 `refresh_policy`；完整性扫描只在打开数据库时执行，watchdog 改用 `Ping`，access status 使用缓存；拒绝带 zone 的 IPv6；检查类命令不再创建配置文件；关机时先取消进行中的请求；store 关闭后不再静默重开；探测延迟使用单调时钟；首次建库中断后可自动恢复；删除遗留的排除节点渲染；过期会话只在一处清理；`store.go` 拆为三个文件。
- **增量写入（9.3）**：`Update` 和 `Save` 在同一事务里比较新旧状态，只插入、更新或删除实际变化的行；先按父表到子表的顺序写入，再按子表到父表的顺序删除，保证级联删除不会带走仍需保留的行。无变化的 `Update` 不写库。首次建库和无法加载旧状态时的 `Save` 仍走整份重写。新增测试逐步比对增量结果与整份重写的数据库完全一致。
- **热备份（9.6）**：新增 `kfadapter backup` 子命令，在一个读事务内完成与 `validate-state` 相同的校验后，把同一事务看到的数据库页输出到标准输出。`scripts/backup-state.sh` 在服务运行时自动通过 `docker exec` 做在线备份，停机时做离线备份，也可用 `--online`/`--offline` 指定；归档格式与离线备份相同，可直接恢复。`upgrade.sh` 固定使用 `--offline`。`compose.yaml` 不变。
- **Web**：已知路由用错方法返回 405；访问限流对 IPv6 按 /64 计数；带哈希的静态资源设置 `immutable` 缓存；节点列表改用原生列表和 `<details>`，不再误用 tree 角色；修正 `<dl>` 结构；"Sign out all" 增加二次确认；复制回退保持弹窗内焦点；删除无调用方的 `GET /api/v1/subscription`，诊断导出新增前端按钮。

未修改：

| 项 | 原因 |
| --- | --- |
| 延迟 IV 截止时间（9.1） | ❓ 取决于服务端 IV 的发送时序，需要抓包确认后再调整。 |
| refresh token 轮换丢失（9.1） | ❓ 取决于服务端 token 轮换语义，无法在本地确认。 |
| QuickFox 控制 ACK 校验（9.2） | ❓ 协议为逆向实现，ACK 的含义未知，贸然校验可能导致误判。 |
| UDP 关联被首个数据报占用（9.2） | 属于 RFC 1928 允许的行为，已限定为客户端 IP，未改动。 |
| `tunnelAuthority.AppID` 恒为 0（9.2） | 与抓包得到的协议一致，未改动。 |
| 状态目录中的 `lost+found` 会阻止首次运行（9.3） | 与部署脚本的严格校验保持一致，只改进了错误信息（列出具体文件名）。 |

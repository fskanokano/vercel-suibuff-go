# vercel-suibuff-go

FreeBuff/Codebuff → OpenAI 兼容网关的 **Vercel 部署版**。

上游 [trefeon/freebuff-proxy](https://github.com/trefeon/freebuff-proxy)（Go 网关，把 Codebuff CLI 私有协议翻译成 OpenAI API，带 token 池化、TLS 隐身、配额锁、hybrid 模式、内嵌 admin dashboard）**逻辑代码零改动**部署到 Vercel —— 根目录 `main.go` 是原生 `net/http` Go 服务器，监听 `$PORT`，由 `vercel.json` 的 `buildCommand: go build -o server .` 构建。

> ⚠️ **部署模型未验证**：Vercel 官方文档的 Go 支持是 **serverless 函数**（`@vercel/go`，每请求导出 `Handler(w, r)`），并未文档化「Go Framework Preset」或「持久化 net/http 二进制」的部署方式。本仓库 `vercel.json` 能否让 Vercel 把 `go build` 产物作为常驻服务器运行，**尚未经真实部署验证**。上线前请先做一次真实 `vercel --prod` 部署确认，或改用原生支持常驻 Go 二进制的平台（Render / Fly.io / Railway）。

```
客户端 (OpenCode/Continue/Cursor/aider/9router)
        │  POST /v1/chat/completions (OpenAI 格式)
        ▼
vercel-suibuff-go  (Vercel Go 容器, iad1 美东出口)
        │  会话握手 + CLI 信封 + TLS 隐身 (uTLS)
        ▼
codebuff.com 上游
```

> ⚠️ **ToS 风险**：使用 FreeBuff token 经由代理违反 Codebuff/FreeBuff 服务条款，账号可能被上游风控封禁。请 `SAFE_MODE=true`、控制用量、勿 7×24 无人值守。

## 为什么用 Go 二进制（部署模型，未验证）

- **原生 Linux 容器**（非 WASM）：uTLS TLS 指纹 + HTTP/2 上游协商完整可用 —— stealth 层完整保留（上游已移除 SOCKS5/HTTP 代理出口，本仓库随之移除）
- **逻辑代码零改动**：`internal/` 全部原样（含 dashboard / logring / egress，egress 探测已按上游改为按需、不再随启动循环）。唯一新增的是根目录 `main.go` —— 作为 `vercel.json` 的构建目标（上游入口在 `cmd/freebuff-proxy/main.go`），根目录提供一个**镜像入口**（完整代理能力，去掉云上无意义的 `-doctor/-update/-setup/-test-token/-install-service/…` 交互子命令与启动 banner，并遵循 12-factor：`PORT` 存在时优先监听它）
- **免费**（Hobby）：100 万 Function Invocations/月 + 360 GB-h + 4 CPU-h + 100 GB 带宽，美东 `iad1` 出口，**无需绑卡**
- **限制**：单次请求最长 300s（含 SSE 流式，单次 AI 推理足够）；实例空闲 scale-to-zero → 内存态会话/run 池、dashboard 状态、日志环会被重置（每次冷启动重新握手，功能不受影响）

## 新特性（本次同步跟进上游）

- **Admin Dashboard**：内嵌单二进制 Web UI（`/admin`，Svelte 5 + Tailwind 4 SPA，静态资源经 `go:embed` 内嵌）。见下方「Admin Dashboard（云端说明）」。
- **模型允许列表**：`MODELS_ALLOW` 逗号分隔白名单，`/v1/models` 只列出允许的 id，其它模型的 chat/messages/responses 请求返回 404 `model_not_found`。
- **`-max` 自动升级**：`PREFER_MAX_MODELS=true` 时把标准模型自动升级到 `-max` 长上下文变体（受 access tier 门控）。
- **推理回放缓存**：新增 `internal/reasoningcache`，缓存推理内容并施加 MiMo V2.5 通用工具调用不变量。
- **开放 Dashboard**：`ADMIN_TOKEN` 完全可选——只读状态页默认开放，写操作 / config / logs / reload 需 `ADMIN_TOKEN` 或 loopback。
- **HTTP/2 上游协商**：`HTTP2_UPSTREAM=true`（默认）以 `h2,http/1.1` 与上游协商，ALPN 对齐真实浏览器（JA4 指纹）。
- **每来源 IP 限流**：`RATE_LIMIT_PER_IP`/`RATE_LIMIT_BURST` 保护上游免遭突发（默认关闭）。
- **Webhook 告警**：`WEBHOOK_URL` 设置后，token 池耗尽/封禁时 fire-and-forget 告警（云端可用，纯出站 HTTPS POST）。
- **Hybrid 模式**：`HYBRID_MODE=true` 时 pooled + bridge 共存。
- **quota / spend 透明**：`/healthz` 新增 per-token `quota` map + `SpendLimit`/`SpendPct`（上次 admission 携带时）；`/v1/models` 携带 `available`/`status`/`current_access_tier`。
- **region/tier 模型可用性**：`MODELS_HIDE_UNAVAILABLE=true` 时 `/v1/models` 裁剪不可用模型。
- **Session + Run 持久化**：`SESSION_PERSIST=true` 时把未过期会话与活动 run 写盘（`SESSION_STATE_FILE`，0600，按 token 的 SHA-256 哈希为键、原始 token 不落盘），重启后恢复而避免烧新的每日会话额度。⚠️ Vercel 只读/易失文件系统上**无法真正持久化**（冷启动即重置），此项在 Vercel 上仅作占位。
- **egress 出口探测已移除**：上游 #123 不再随启动循环探测 Cloudflare trace（官方 CLI 从不请求该域名），改为 `-doctor` 按需探测；本仓库无交互子命令，云端不保留该探测。

## 快速部署

### 方式一：Vercel Dashboard（推荐）

1. 把本仓库导入 Vercel（Import Git Repository）
2. Build 命令为 `go build -o server .`（由 `vercel.json` 指定）。⚠️ 该常驻二进制部署方式未经验证（见顶部警告）
3. **Environment Variables**（Settings → Environment Variables）：
   | 变量 | 必填 | 说明 |
   |---|---|---|
   | `AUTH_TOKENS` | 二选一 | `cb_...`，多个逗号分隔（Pooled 模式） |
   | （不设 `AUTH_TOKENS`） | 二选一 | Bridge 模式：客户端自带上游 token |
   | `AUTO_DISCOVER_TOKEN` | ✅ | `false`（云上没有本地 CLI 登录文件） |
   | `API_KEYS` | 可选 | 代理自身鉴权 key |
   | `ADMIN_TOKEN` | **强烈建议** | 保护 `/admin` dashboard 与 `/admin/reload`（公网可达，见下文） |
4. Deploy。完成后：

```bash
curl https://<your-project>.vercel.app/healthz
curl -H "Authorization: Bearer <key>" https://<your-project>.vercel.app/v1/models
```

### 方式二：Vercel CLI

```bash
npm i -g vercel
vercel env add AUTH_TOKENS        # cb_...
vercel env add AUTO_DISCOVER_TOKEN  # false
vercel env add ADMIN_TOKEN          # openssl rand -hex 16
vercel --prod
```

### 连接 AI 工具

把 OpenAI 兼容端点指向 `https://<your-project>.vercel.app/v1`：

```bash
# opencode / Continue / Cursor / aider 示例
export OPENAI_BASE_URL=https://<your-project>.vercel.app/v1
export OPENAI_API_KEY=<API_KEYS 或 cb_ token>
```

## 环境变量（云上适用子集）

完整清单见上游 README；云端关注这些：

| 变量 | 默认 | 说明 |
|---|---|---|
| `LISTEN_ADDR` | `127.0.0.1:3457` | 无需设置；Vercel 注入 `PORT` 时优先监听它（本地自建才需要） |
| `AUTH_TOKENS` | 空 | Pooled 模式 token 列表；空 = Bridge 模式 |
| `HYBRID_MODE` | false | pooled + bridge 共存：客户端自带 token 走 bridge，无 token 走池 |
| `MODELS_HIDE_UNAVAILABLE` | false | `/v1/models` 裁剪 region/tier 不可用模型 |
| `AUTO_DISCOVER_TOKEN` | true | **必须**设为 `false` |
| `API_KEYS` | 空 | 代理自身客户端鉴权 |
| `ADMIN_TOKEN` | 空 | 保护 `/admin` dashboard 与 `/admin/reload`（**公网必设**，见下文） |
| `SAFE_MODE` | true | 反封禁预设（TLS 指纹/header 清洗/jitter/轮换） |
| `TLS_FINGERPRINT` | auto | `auto \| chrome120 \| chrome126 \| safari17 \| safari18 \| firefox120 \| firefox128 \| edge126 \| random` |
| `UPSTREAM_BASE_URL` | `https://codebuff.com` | 上游地址 |
| `REQUEST_TIMEOUT` | 15m | 单请求超时（≤ Vercel 300s 上限） |
| `SESSION_CALL_TIMEOUT` | 30s | 会话调用超时 |
| `TRANSIENT_RETRIES` | 1 | 瞬时传输失败额外重试次数 |
| `IDLE_ROTATION_TIMEOUT` | 0 | 空闲后结束 run（SAFE_MODE 下默认 30m） |
| `SOCKS5_PROXY` / `SOCKS5_PROXIES` | 空 | 出站 SOCKS5 代理（单/按 token 列表；云上一般无需） |
| `COST_MODE` | free | free 档 |

## Admin Dashboard（云端说明）

上游新增内嵌管理后台（Svelte 5 SPA，静态资源经 `go:embed` 内嵌），Vercel 上同样随二进制提供，但**功能受限**：

- **只读状态页**（`/admin` overview / tokens / models / traces / metrics / setup）在 `ADMIN_TOKEN` 未设时**默认开放**，展示 token 会话状态、配额、用量、最近请求 trace 等。
- **写操作 / 敏感页**（config 编辑器、logs、token add/remove、mode 切换、smoke、diag）在**未设 `ADMIN_TOKEN` 时要求 loopback 客户端**，而 Vercel 上请求来自边缘（非 loopback）→ 实际被拒；且 config 编辑器写 `.env` 在只读文件系统上也会失败。这些功能在 serverless 上本就无意义。
- **重要**：Vercel 实例是**公网可达**，未设 `ADMIN_TOKEN` 时只读状态页（含 token 数、会话状态、模型列表）对任何知道 URL 的人可见。**强烈建议设置 `ADMIN_TOKEN`**（`openssl rand -hex 16`）以保护全部 dashboard 视图与 `/admin/reload`、config/logs。

云端无意义（无需配置）：`LOG_FILE`、`DEBUG_DUMP`（写 `./dump/`）、dashboard 的 `.env` 编辑/持久化、`-doctor/-update/-setup/-test-token`、CLI token 自动发现。

## 本地运行 / 测试

```bash
go build -o server .                     # 与 Vercel buildCommand 一致（根目录入口）
LISTEN_ADDR=:3457 AUTO_DISCOVER_TOKEN=false API_KEYS=test \
  AUTH_TOKENS=cb_your_token ./server
# 另开终端：
curl http://127.0.0.1:3457/healthz
curl -H "Authorization: Bearer test" http://127.0.0.1:3457/v1/models
go test ./...                            # 上游完整测试套件（全过）
```

> 根目录 `main.go` 是 Vercel 检测入口（镜像上游启动逻辑，去掉 `-doctor/-update/-setup/-test-token`）；完整版入口在 `cmd/freebuff-proxy/main.go`，本地自建部署可用它。

## 与上游的差异（云环境所致，非逻辑代码改动）

| 特性 | 本地/自建 | Vercel 上 |
|---|---|---|
| 会话/run 池 | 内存常驻，跨请求复用 | 实例 scale-to-zero，空闲后重置（冷启动重建） |
| dashboard / 日志环 | 内存常驻 | 冷启动重置；写 `.env` 只读失败 |
| egress 探测 | 本地自建可 `-doctor` 按需探测 | 已移除（上游 #123 不再随启动探测） |
| 请求时长 | 无硬限制 | **300s 上限**（Hobby） |
| 出口 | 本机 IP / 自配代理 | 美东 iad1（动态 IP） |
| `/admin/reload` | 热重载本地 .env | 无意义（无本地配置文件） |
| 会话持久化 (`SESSION_PERSIST`) | 重启恢复未过期会话 | 只读/易失 FS，冷启动即重置（仅占位） |
| TLS 隐身 (uTLS) | ✅ | ✅ 完整保留 |

## 上游

- 项目：https://github.com/trefeon/freebuff-proxy
- 同步基线：上游 commit `53260a8`。协议转换层（`internal/convert`、`internal/upstream`、`internal/stealth`、`internal/registry`、`internal/dashboard`、`internal/egress`、`internal/logring`、`internal/session`、`internal/notify`、`internal/reasoningcache`）随上游更新；本仓库逻辑代码与上游逐字节一致，唯一差异是根 `main.go`、`vercel.json`、本 README（Vercel 适配层）。

## License

MIT（上游 [trefeon/freebuff-proxy](https://github.com/trefeon/freebuff-proxy)）

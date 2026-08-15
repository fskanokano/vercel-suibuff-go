# vercel-suibuff-go

FreeBuff/Codebuff → OpenAI 兼容网关的 **Vercel 部署版**。

上游 [trefeon/freebuff-proxy](https://github.com/trefeon/freebuff-proxy)（Go 网关，把 Codebuff CLI 私有协议翻译成 OpenAI API，带 token 池化、TLS 隐身、配额锁）**零源码改动**直接部署到 Vercel —— 借助 Vercel 的 **Go Framework Preset**（原生 `net/http` Go 服务器，监听 `$PORT`）。

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

## 为什么 Vercel Go Framework Preset

- **原生 Linux 容器**（非 WASM）：uTLS TLS 指纹、SOCKS5/HTTP 代理全部可用 —— stealth 层完整保留
- **零源码改动**：`internal/` 全部原样。唯一新增的是根目录 `main.go` —— Vercel Go preset 只识别根级入口（`main.go`/`cmd/api/main.go`/`cmd/server/main.go`），而上游入口在 `cmd/freebuff-proxy/main.go`，所以根目录提供一个**精简镜像入口**（完整代理能力，去掉云上无意义的 `-doctor/-update/-setup`，并遵循 12-factor：`PORT` 存在时优先监听它）
- **免费**（Hobby）：100 万 Function Invocations/月 + 360 GB-h + 4 CPU-h + 100 GB 带宽，美东 `iad1` 出口，**无需绑卡**
- **限制**：单次请求最长 300s（含 SSE 流式，单次 AI 推理足够）；实例空闲 scale-to-zero → 内存态会话/run 池会被重置（每次冷启动重新握手，功能不受影响）

## 快速部署

### 方式一：Vercel Dashboard（推荐）

1. 把本仓库导入 Vercel（Import Git Repository）
2. Framework Preset 自动检测为 **Go**（检测到根目录 `go.mod` + `vercel.json`）
3. **Environment Variables**（Settings → Environment Variables）：
   | 变量 | 必填 | 说明 |
   |---|---|---|
   | `AUTH_TOKENS` | 二选一 | `cb_...`，多个逗号分隔（Pooled 模式） |
   | （不设 `AUTH_TOKENS`） | 二选一 | Bridge 模式：客户端自带上游 token |
   | `AUTO_DISCOVER_TOKEN` | ✅ | `false`（云上没有本地 CLI 登录文件） |
   | `API_KEYS` | 可选 | 代理自身鉴权 key |
4. Deploy。完成后：

```bash
curl https://<your-project>.vercel.app/healthz
curl -H "Authorization: Bearer <key>" https://<your-project>.vercel.app/v1/models
```

### 方式二：Vercel CLI

```bash
npm i -g vercel
vercel env add AUTH_TOKENS        # cb_...
vercel env add AUTH_TOKENS        # cb_...
vercel env add AUTO_DISCOVER_TOKEN  # false
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

完整清单见上游 README；云端只需要这些：

| 变量 | 默认 | 说明 |
|---|---|---|
| `LISTEN_ADDR` | `127.0.0.1:3457` | 无需设置；Vercel 注入 `PORT` 时优先监听它（本地自建才需要） |
| `AUTH_TOKENS` | 空 | Pooled 模式 token 列表；空 = Bridge 模式 |
| `AUTO_DISCOVER_TOKEN` | true | **必须**设为 `false` |
| `API_KEYS` | 空 | 代理自身客户端鉴权 |
| `SAFE_MODE` | true | 反封禁预设（TLS 指纹/header 清洗/jitter/轮换） |
| `TLS_FINGERPRINT` | chrome | `chrome\|firefox\|safari\|edge\|random` |
| `UPSTREAM_BASE_URL` | `https://codebuff.com` | 上游地址 |
| `LOG_LEVEL` | info | 日志级别 |
| `REQUEST_TIMEOUT` | 15m | 单请求超时（≤ Vercel 300s 上限） |
| `COST_MODE` | free | free 档 |

云端无意义（无需配置）：`LOG_FILE`、`ADMIN_TOKEN`、`-doctor/-update/-setup`、token 自动发现。

## 本地运行 / 测试

```bash
go build -o server .                     # 与 Vercel buildCommand 一致（根目录入口）
LISTEN_ADDR=:3457 AUTO_DISCOVER_TOKEN=false API_KEYS=test \
  AUTH_TOKENS=cb_your_token ./server
# 另开终端：
curl http://127.0.0.1:3457/healthz
curl -H "Authorization: Bearer test" http://127.0.0.1:3457/v1/models
go test ./...                            # 上游完整测试套件（11 包全过）
```

> 根目录 `main.go` 是 Vercel 检测入口（镜像上游启动逻辑，去掉 `-doctor/-update/-setup`）；完整版入口在 `cmd/freebuff-proxy/main.go`，本地自建部署可用它。

## 与上游的差异（云环境所致，非代码改动）

| 特性 | 本地/自建 | Vercel 上 |
|---|---|---|
| 会话/run 池 | 内存常驻，跨请求复用 | 实例 scale-to-zero，空闲后重置（冷启动重建） |
| 请求时长 | 无硬限制 | **300s 上限**（Hobby） |
| 出口 | 本机 IP / 自配代理 | 美东 iad1（动态 IP） |
| `/admin/reload` | 热重载本地 .env | 无意义（无本地配置文件） |
| TLS 隐身 (uTLS) | ✅ | ✅ 完整保留 |

## 上游

- 项目：https://github.com/trefeon/freebuff-proxy
- 协议转换层（`internal/convert`、`internal/upstream`、`internal/stealth`、`internal/registry`）随上游更新，本仓库保持零改动镜像，可 `git pull` 上游同步。

## License

MIT（上游 [trefeon/freebuff-proxy](https://github.com/trefeon/freebuff-proxy)）

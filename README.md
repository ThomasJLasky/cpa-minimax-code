# cpa-minimax-code

**CLIProxyAPI 原生插件：把 MiniMax Code 账号的登录态接进 CPA，用原生 Anthropic Messages 协议消耗签到积分池。**

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that turns a signed-in **MiniMax Code** account into Anthropic-native models inside CPA — no sidecar proxy, no API key, credits from the daily check-in.

[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go)](https://go.dev)
[![Protocol](https://img.shields.io/badge/protocol-Anthropic_Messages-d97757)](https://docs.anthropic.com)
[![Plugin](https://img.shields.io/badge/CLIProxyAPI-v8-6E56CF)](https://github.com/router-for-me/CLIProxyAPI)

---

## 它解决什么

MiniMax Code 的订阅额度不在开放平台 API Key 上，而在**账号级积分池**里（签到积分就进这里）。想把它给 Claude Code、Cline 这类工具用，通常得跑一个本地反代 —— 多一个进程、多一份凭据、多一处故障点。

这个插件把整件事搬进 CPA：

| | 本地反代方案 | 本插件 |
|---|---|---|
| 进程 | 额外一个 Node 服务 | **零**，插件在 CPA 进程内 |
| 凭据 | 读 `~/.minimax`，与桌面端抢 refresh token | **独立设备码会话**，互不影响 |
| 协议 | 直连 mavis，原生 | **原生**（声明 `anthropic` 格式，宿主不翻译） |
| 账号管理 | 自己维护 | CPA 面板统一管 |
| 额度/签到 | 另写脚本 | 内置 |

## 特性

- **原生协议**：插件声明 `executor_input_formats: ["anthropic"]`，宿主命中后**不做翻译**，把 `/v1/messages` 原始请求体直接交给插件转发给 mavis。`thinking` 块（带 signature）、`tool_use` 全部原样保真。
- **设备码登录**：OAuth 2.0 + PKCE（`client_id=mcode-public`），面板里点一下就能加账号，不依赖本机装没装 Code 客户端。
- **自动续期**：access token 1 小时过期、refresh token 每次轮换。插件只实现 `auth.refresh`，由宿主按 `NextRefreshAfter` 调度并持久化新凭证。注意宿主只给内置 provider 配了续期提前量，插件必须在 `AuthData.Metadata` 里同时给出 `refresh_interval_seconds`、`expires_at`、`refresh_token`，否则 auto-refresh 循环根本不会调度（详见 `auth.go:buildAuthData`）。
- **单账号不被冷却摘掉**：`Metadata.disable_cooling=true`。本插件只有一个账号，凭据一旦被判不可用就从轮询池摘掉，请求 15ms 内直接 503 `auth_unavailable`、根本进不到插件，也就没法靠「请求顺带触发就地续期」恢复，只能等宿主的下一次刷新 tick。实测同一场景（把 `expires_at` 写成过去值）：不带该开关 → 503 持续 6 分半、期间零重试；带上 → 几秒内就地续期、请求 200。代价是上游 429/5xx 时这个凭据不会被冷却摘掉，会被持续重试。
- **额度查询**：`quota_provider` 读 `credit/details`，汇总成「剩余 / 已消耗 / 累计 / 批次 / 7 天内过期」。
- **每日签到**：后台协程在北京时间 10:00 后自动领取，claim 幂等；也能在控制台页手动点。
- **错误分类**：上游 400/401/402/403/429/529 映射成 Anthropic 的 `error.type`，流式下发 `event: error`；只有 401 例外，走 `host.stream.close` 的错误通道，好让宿主识别成凭据问题、就地续期后重试。
- **零第三方依赖**：只用标准库 + cgo，`go.mod` 两行。

## 安装

### 从插件商店（推荐）

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/ThomasJLasky/cpa-minimax-code/main/registry.json"
```

重启 CPA → 管理面板 → **插件商店** → 搜 `MiniMax Code` → 一键安装。

### 手动

```bash
CPA_HOME=/path/to/cpa
cp dist/minimax-code.dylib "$CPA_HOME/plugins/minimax-code.dylib"
chmod 755 "$CPA_HOME/plugins/minimax-code.dylib"
```

```yaml
plugins:
  enabled: true
  dir: "/path/to/cpa/plugins"
  configs:
    minimax-code:
      enabled: true
      priority: 55        # 低于已有插件，不抢同名模型
      region: cn          # cn | global
```

```bash
launchctl kickstart -k "gui/$(id -u)/com.local.cliproxyapi"   # 或 systemctl restart
grep "minimax-code" "$CPA_HOME/cliproxyapi.log" | tail -3
```

期望看到 `plugin loaded` + `plugin registered`。

## 添加账号

管理面板 → **OAuth 登录** → `minimax-code` 卡片 → 登录。
浏览器确认后凭据自动落盘到 `$CPA_HOME/auth/minimax-code-<name>.json`，**与 MiniMax Code 桌面端的登录态完全独立**。

## 模型

| 客户端 ID | 上游 model | 上下文 | 最大输出 |
|---|---|---|---|
| `mm-m3.1-flash` | `MiniMax-M3.1-Flash` | 1,000,000 | 512,000 |
| `mm-m3` | `MiniMax-M3` | 1,000,000 | 512,000 |
| `mm-m2.7` | `MiniMax-M2.7` | 204,800 | 131,072 |
| `mm-m2.7-highspeed` | `MiniMax-M2.7-highspeed` | 204,800 | 131,072 |

> 上下文/输出取自 MiniMax Code 客户端自己加载的 models.dev 目录
> （`~/.minimax/cache/models-dev-catalog.json`），并且都低于 mavis 实测的 `max_tokens`
> 硬上限（M3 家族 524288、M2.x 196608），所以声明值一定可用。上游没有可查的模型清单接口
> （`/mavis/api/v1/llm/v1/models` 返回 `direct_route_not_configured`），要核对只能拿
> `max_tokens` 超限报错里的模型名 + 响应回显逐个探。
>
> **上游对不认识的模型名不报错，会静默换成 `MiniMax-M3` 跑掉并照常计积分。** 插件因此会比对
> 响应回显的 `model` 与请求的上游名，不一致就直接报 `invalid_request_error`（流式在
> `message_start` 帧上拦，不会先吐内容），并在宿主日志里留一条
> `minimax-code: upstream switched model`。上游改名当天靠这个发现，而不是事后猜。

> 用 `mm-` 前缀是刻意的：宿主按模型 id 去重、priority 高的胜出，`minimax-m3` 这类裸名容易被别的插件（比如 WorkBuddy）遮住。

## 客户端接入

```bash
# Claude Code / 任何 Anthropic 客户端
export ANTHROPIC_BASE_URL=http://127.0.0.1:8317
export ANTHROPIC_API_KEY=<CPA api-key>
export ANTHROPIC_MODEL=mm-m3.1-flash
claude
```

```bash
# OpenAI 协议（宿主自动翻译）
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer $K" -H "Content-Type: application/json" \
  -d '{"model":"mm-m3.1-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":256}'
```

```bash
# Anthropic 协议
curl http://127.0.0.1:8317/v1/messages \
  -H "x-api-key: $K" -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"mm-m3.1-flash","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}'
```

> ⚠️ 这几个模型会先输出 thinking，`max_tokens` 给小了正文会是空的（`stop_reason=max_tokens`）。建议 ≥ 256。

## 额度与签到

控制台页：`http://127.0.0.1:8317/v0/resource/plugins/minimax-code/console`
（积分卡片 + 立即签到 + 模型清单，纯静态，不含凭证）

```bash
# 手动签到（资源路由，不需要 management key）
curl "http://127.0.0.1:8317/v0/resource/plugins/minimax-code/console?action=checkin"
# {"ok":true,"results":[{"account":"MiniMax Code","status":"already","day":6,"points":400,"expire_at":"2026-11-18 00:00"}]}

# 状态 JSON
curl "http://127.0.0.1:8317/v0/resource/plugins/minimax-code/status"
```

> 面板会固定提示一条当前行为：**签到仅发放积分，不会触发重置卡获取**（上游文案位 `signin/config` 全空、
> `reset_card` 接口在本机探测下 404，所以这条是插件侧写死的行为说明，改 `checkin.go:checkinNotice` 一处即可）。

签到积分是**分批发放、各自过期**的（每天 400，连签第 4/7 天 1000，30 天过期），额度页会把最近一批的到期时间单独标出来。

控制台页的**连签进度条**取自 claim 响应自带的 `data.panel.days`（不用另开 `signin/status`，
那个接口还强制要 `timezone_id`）：绿底=已领（`status=3`），蓝框=今天。

## 错误处理

上游错误转成 Anthropic 形状：

```json
{"type":"error","error":{"type":"invalid_request_error",
 "message":"minimax upstream status 400: invalid params, model[MiniMax-M3] does not support max tokens > 524288 (2013)"}}
```

| 上游状态 | `error.type` |
|---|---|
| 400 | `invalid_request_error` |
| 401 | `authentication_error` |
| 402 | `billing_error` |
| 403 | `permission_error` |
| 429 | `rate_limit_error` |
| 529 | `overloaded_error` |

> **HTTP 状态码仍然是 500**：插件错误只能以字符串跨 C ABI 传回宿主，`ErrorMessage.StatusCode` 拿不到。
> `error.type` 是准的，按类型分支处理的客户端不受影响。
>
> 消息里刻意写成 `upstream status 401` 而不是 `upstream 401`：宿主的 `isUnauthorizedError` 只认
> `StatusCode==401` 或文本 `status 401` / `401 unauthorized`，命中它才会先调 `auth.refresh` 再重试，
> 而不是把这个账号判死、让后面的请求全变成 `auth_unavailable` 503。
>
> 但**只有确凿的 401 才能用这个形状**，两处的区别很要紧：
>
> - 执行路径不再本地预判 `expires_at` 然后凭空造一条 401，一律让 mavis 判——真过期才有证据。
> - `auth.refresh` 的失败：如果错误文本长得像 `status 401`，宿主会把 `NextRefreshAfter` **清零**并把
>   凭据永久摘出轮询池（`sdk/cliproxy/auth/conductor_refresh.go:495-507`），之后 auto-refresh 和
>   选择器双双跳过它，只有重启进程或重新授权才出得来。所以刷新失败里只有响应带 OAuth `error`
>   字段（`invalid_grant` 这类，本来就该重新登录）才用 `status 401`；换新网络时强制门户/透明代理
>   伪装的 401 改用别的措辞，让宿主走 5 分钟退避、保留自愈。传输层错误还会做有界重试（0/2/5 秒）。

## 工作原理

| 用途 | 方法与端点 |
|---|---|
| 设备码 | `POST https://account.minimax.cn/oauth2/device/code` |
| 换 token | `POST https://account.minimax.cn/oauth2/token`（device_code / refresh_token） |
| 对话 | `POST https://agent.minimax.cn/mavis/api/v1/llm/v1/messages`（**Anthropic Messages**） |
| 积分 | `GET https://agent.minimax.cn/minimax-cloud/api/v1/credit/details` |
| 签到 | `POST https://agent.minimax.cn/minimax-cloud/api/v1/signin/claim`（`x-signature` + `yy` + `timezone_id`） |

请求头必须是 Code 客户端的形状：

```
Authorization: Bearer <mmoat_…>     # 只能用这个，x-api-key 会 401
x-api-key: sk-xxx                   # 占位，网关忽略
User-Agent: MiniMaxAgent
X-Mavis-Agent-Id: main
anthropic-version: 2023-06-01
```

### 插件 ABI

| 能力 | 用途 |
|---|---|
| `auth_provider` | `auth.parse` / `auth.login.start` / `auth.login.poll` / `auth.refresh` |
| `model_provider` | `model.static` / `model.for_auth` |
| `executor` | `executor.execute` / `execute_stream`（`host.stream.emit` 推流）/ `count_tokens` |

> **`host.stream.emit` 的 chunk 必须一行一个**（`data: {…}\n` 单独一块，`event: …\n`、空行各自一块）。
> 宿主的 claude→openai / claude→responses 流式转换器是先 `bytes.HasPrefix(chunk, "data:")` 再解析的，
> 把 `event:`+`data:` 打包成一帧发过去会被整帧丢弃，表现成无法定位的
> `empty_stream: upstream stream closed before first payload`（同协议直透 `/v1/messages` 时看不出来）。
> 逐行发和宿主内置 claude executor 的行为一致，字节流重组后客户端看到的 SSE 不变。
| `quota_provider` | `quota.describe` / `quota.fetch` |
| `management_api` | `/status`、`/checkin`、`/console` |

`ABIVersion = 1`、`SchemaVersion = 6`（对应 CLIProxyAPI v8.0.20+）。

## 构建

```bash
# 本机
./scripts/build.sh

# 指定平台 + 打商店格式的 zip
GOOS=linux GOARCH=arm64 ./scripts/build.sh --package
```

产物：

```
dist/minimax-code.dylib
dist/minimax-code_0.2.0_darwin_arm64.zip
dist/checksums.txt
```

打 tag（`v0.2.0`）会触发 GitHub Actions，在 darwin/linux/windows 的原生 runner 上各编一份并自动发布 release。

## 测试桩

`testharness/` 是一个 dlopen 驱动，不启动 CPA 就能验证 ABI 与上游链路：

```bash
cd testharness && CGO_ENABLED=1 go build -o /tmp/plugtest .
/tmp/plugtest ../dist/minimax-code.dylib register   # 只看注册 JSON
/tmp/plugtest ../dist/minimax-code.dylib quota      # 额度
/tmp/plugtest ../dist/minimax-code.dylib login      # 发起设备码，打印 URL
/tmp/plugtest ../dist/minimax-code.dylib poll       # 轮询，成功后写 /tmp/mm-auth.json
```

> `auth.refresh` 会**轮换 refresh token**。测试桩默认读 `~/.minimax` 的登录态，跑之前先备份那两个 json，
> 跑完把新 token 写回，否则 MiniMax Code 桌面端会被登出。

## 已知限制

1. **多账号会撞名**：设备码响应里没有 `uid`，凭证文件名用 label，第二个同 label 的账号会覆盖第一个。
2. **签到归因用 `user_id=0`**：拿不到 `realUserID`，服务端按 token 认账号；实测可正常 claim，但出现 `not-claimable` 时需补一次 `user/info`。
3. **`model_prefix` 不生效**：插件 provider 的前缀来自 auth 的 `Prefix` 字段，config 里的 `model_prefix` 不会自动传下去，所以模型 id 是硬编码的 `mm-*`。
4. **私有端点**：`/mavis/api/v1/llm/v1/messages` 是 Code 客户端的内部路由，上游改版即失效。base-url 和 headers 都可以在 `upstream.go` 一处改掉。
5. **设备码登录会话在插件内存里**：宿主要求 `auth.login.start` 返回的 `State` ≤128 字符且只含 `[A-Za-z0-9._-]`，所以 `device_code` + PKCE verifier 只能留在插件进程内，`State` 只是一个 32 位短 token。代价是宿主重启或插件热重载会丢弃未完成的登录，面板上重新点一次即可（`auth.login.poll` 会明确报「登录会话已过期或不存在」）。

## 安全与隐私

这个仓库**不含任何凭据、账号或本机信息**，也不采集任何数据。上线前做过一次逐项核查：

| 检查项 | 结果 |
|---|---|
| 令牌 / 私钥（`mmoat_` / `mmort_` / `sk-` / `ghp_` / PEM） | 无（README 里只有占位符 `<mmoat_…>`） |
| 本机路径与用户名（`/Users/…`、`/home/…`） | 无 |
| 邮箱、手机号、QQ | 无（提交身份用的是 GitHub noreply） |
| 真实 uid / 账号标识 | 无（示例一律用 `<name>` / `<uid>`） |
| `auth/`、`.env`、`config.json` 等凭据文件 | 未跟踪，且已写进 `.gitignore` |

**运行时的数据流向**（全部在你本机与 MiniMax 官方端点之间，插件不向外发任何东西）：

```
你的客户端 ──► CPA(127.0.0.1:8317) ──► 本插件 ──► agent.minimax.cn
                    │                      │
                    └─ auth/*.json         └─ 只做 Bearer 转发，不落盘、不上报
```

- 凭据由宿主写在 `$CPA_HOME/auth/` 下，权限 `0600`，文件名 `minimax-code-<name>.json`。
- 插件不写日志文件、不埋点、不请求第三方域名（`upstream.go` 里只有 `account.minimax.*` 和 `agent.minimax.*` 两组域名）。
- 控制台页是纯静态 HTML，所有数据都从本机 `/v0/resource/plugins/minimax-code/status` 现取，不含凭证。
- `testharness/` 会读 `~/.minimax` 的登录态做验证，**它只是本地调试工具，不会被 CPA 加载**。

仓库自带一个推送前自检脚本，上面这些项它会逐条扫一遍：

```bash
./scripts/audit.sh
AUDIT_EXTRA='你的本机用户名|你的邮箱' ./scripts/audit.sh   # 追加本机专属词
```

> 提交前请自行确认：`git log -1 --format='%an <%ae>'` 显示的是你希望公开的身份。
> 本仓库首次提交已使用 `22903680+ThomasJLasky@users.noreply.github.com`，避免暴露真实邮箱。

## 免责声明

**本项目是第三方非官方工具，与 MiniMax、Anthropic、CLIProxyAPI 官方均无任何关联、背书或支持关系。**

1. **仅供个人学习与研究**。它复用**你自己**账号的登录态，请勿用于任何商业用途、代运营或对外提供服务。
2. **共享 / 转售 / 多人拼车额度存在风控与封号风险**，后果由使用者自行承担。请遵守 [MiniMax 服务条款](https://www.minimax.io/terms) 及你所在地区的法律法规。
3. **走的是客户端内部路由**（`/mavis/api/v1/llm/v1/messages` 等），不是公开 API。上游随时可能改版、限流或封禁，导致插件失效；作者不承诺可用性与兼容性。
4. **按「现状」提供，不附带任何明示或暗示的担保**（包括但不限于适销性、特定用途适用性、不侵权）。因使用本软件造成的任何直接或间接损失（含额度损失、账号受限、数据损坏），作者不承担责任。
5. **不要把你的 `auth/*.json`、`~/.minimax` 目录或任何令牌提交到公开仓库**。本仓库的 `.gitignore` 已覆盖常见凭据文件，但这不构成任何保证。
6. 如果你 fork 或二次分发，请保留本免责声明，并自行承担合规责任。

**下载或使用即表示你已阅读、理解并同意上述条款。** 如果你不同意，请不要使用本软件。

## License

[MIT](LICENSE) © 2026 ThomasJLasky

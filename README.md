
# TokenPulse

**看清每一个 Token，算清每一分钱。**
*See every token. Know every dollar.*

TokenPulse 是一个面向个人开发者的开源 **AI 消费记账本**：自动采集本机所有 AI 编程工具的使用记录，在一个面板里统一呈现 Token 消耗与成本账单。

An open-source **AI usage ledger** for individual developers: it automatically collects usage records from every AI coding assistant on your machine, and turns them into one unified token & cost dashboard.

## 为什么需要 TokenPulse / Why

你很可能同时用着 Claude Code、Codex、Gemini CLI、Cursor……每家的用量数据散落在各自的日志和账单页里。**"我这个月 AI 到底用了多少 Token、花了多少钱"——没有任何工具能直接回答。**

Chances are you use Claude Code, Codex, Gemini CLI, Cursor… at the same time. Usage data is scattered across each tool's logs and billing pages. **"How many tokens did I actually burn this month, and how much would that cost?" — no existing tool answers this.**

| | TokenPulse | Langfuse | CCDash | ai-observer |
|---|---|---|---|---|
| 零配置启动 / zero-config | ✅ | ❌ 需要 SDK | ✅ | ✅ |
| 跨工具统一视图 / all tools in one panel | ✅ | ✅ | ❌ 仅 Claude | ⚠️ 体验一般 |
| 个人记账本视角 / personal ledger view | ✅ | ❌ 团队遥测 | ✅ | ❌ 技术指标 |
| 部署重量 / footprint | 单二进制 | 重（ClickHouse） | Python | Go 单二进制 |

## 快速开始 / Quick Start

```bash
# 安装（三选一）
brew install tokenpulse          # Homebrew（上架前可 go install）
go install github.com/tokenpulse-hub/tokenpulse/cmd/tokenpulse@latest
# 或从 GitHub Releases 下载对应平台的单二进制

# 扫描本机日志并生成账单
tokenpulse scan

# 终端查看汇总
tokenpulse status
tokenpulse top          # 本月项目 TOP5
tokenpulse models       # 本月模型明细

# 打开 Web 面板（http://localhost:8420）
tokenpulse serve
# 面板内置每小时自动扫描；页面上可随时开关自动扫描、点"立即扫描"手动更新
tokenpulse serve --auto-scan 30m
```

零配置：不集成 SDK、不改代码、不注册账号。数据只存本机（`~/.tokenpulse/tokenpulse.db`）。

Zero config: no SDK, no code change, no signup. Data never leaves your machine (`~/.tokenpulse/tokenpulse.db`).

## 功能 / Features

- **自动发现**：扫描 Claude Code 的本地会话日志（`~/.claude/projects/**/*.jsonl`），增量导入、幂等去重
- **成本折算**：内置各模型定价表（USD / 1M tokens，含缓存读写价），Token 自动折算成美元账单
- **多维账单**：按今日 / 本周 / 本月、按项目、按模型、按工具四个维度拆账
- **趋势图**：最近 30 天逐日 Token 消耗趋势
- **CLI + Web 双入口**：终端党一行命令，面板党一个浏览器
- **隐私优先**：单二进制 + 本地 SQLite，不上传任何数据

## 支持工具 / Supported Tools

| 工具 | 状态 | 采集方式 |
|---|---|---|
| Claude Code | ✅ v0.1 | 本地 JSONL 日志解析（`~/.claude/projects/**/*.jsonl`） |
| OpenCode | ✅ v0.2 | 本地 SQLite 只读解析（`~/.local/share/opencode/opencode.db`） |
| Codex CLI | ✅ v0.2 | JSONL 事件流解析（`~/.codex/sessions/**/rollout-*.jsonl`） |
| Gemini CLI | ✅ v0.2 | JSON / JSONL 会话文件解析（`~/.gemini/tmp/**/session-*.json[l]`） |
| TeleAgent | ✅ v0.2 | 本地 SQLite 只读解析（`~/.local/share/TeleAgent/users/*/teleagent.db`） |
| Cursor / Trae / Copilot | 🔜 | 陆续适配中 |
| 任意 OpenAI 兼容 API | 🔜 | 本地反向代理网关 |

> **设计原则**：TokenPulse 面向独立安装的 AI 编程工具——每个适配器只读取该工具自身标准数据路径下的数据，不依赖任何宿主环境。你可以通过环境变量覆盖路径（`TOKENPULSE_OPENCODE_DIR` 等），方便自定义或测试。

**支持环境变量覆盖**（工具不在默认位置或测试时使用）：
`TOKENPULSE_CLAUDE_DIR` / `TOKENPULSE_CODEX_DIR` / `TOKENPULSE_GEMINI_DIR` / `TOKENPULSE_OPENCODE_DIR` / `TOKENPULSE_TELEAGENT_DIR`

**接入原则**：只要一个智能体把用量落在本机（日志文件或本地数据库），就可以为它写一个适配器——比如 TeleAgent 把 `tokens.input/output/cache` 存进本地 SQLite，TokenPulse 以只读方式采集并用定价表补算成本（TeleAgent 自身不折算成本）。智能体没有本地日志？走 v0.4 的代理网关，任何 OpenAI 兼容调用都能统计。

**Adding a tool** = implementing the `collector.Adapter` interface (just `Discover` + `Parse`). Any agent that persists usage locally — a log file or a local database — can be adapted. PRs welcome.

## 路线图 / Roadmap

- **v0.1** — Claude Code 解析 + 面板 + CLI ✅
- **v0.2** — Codex / Gemini CLI 适配器；定价表社区维护机制
- **v0.3** — 预算与预警（日/周/月预算、Webhook 告警）
- **v0.4** — 本地代理网关：统计一切 OpenAI 兼容 API 调用（含免费额度追踪）
- **v0.5** — 云厂商账单 CSV 导入对账
- **v1.0** — 多机聚合、数据导出、协议稳定承诺

## 定价表说明 / Pricing

`internal/pricing/data/pricing.json` 内置的价格为**初始参考值**，模型价格随官方调整变化，请以各厂商官方定价页为准。发现价格不准？欢迎提 PR 修正——价格数据独立于代码，无需发版。

Prices in the built-in table are **initial reference values**. Model pricing changes over time — please verify against official pricing pages. Found a stale price? PRs are welcome; pricing data is decoupled from code, no release needed.

## 构建 / Build

```bash
git clone https://github.com/tokenpulse-hub/tokenpulse
cd tokenpulse
go mod tidy
go build -o tokenpulse ./cmd/tokenpulse

# 交叉编译（无 CGO 依赖，任意平台可互编）
GOOS=darwin  GOARCH=arm64 go build -o tokenpulse-darwin-arm64  ./cmd/tokenpulse
GOOS=linux   GOARCH=amd64 go build -o tokenpulse-linux-amd64   ./cmd/tokenpulse
GOOS=windows GOARCH=amd64 go build -o tokenpulse.exe           ./cmd/tokenpulse
```

## License

MIT

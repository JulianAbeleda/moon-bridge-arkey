# Configuration

> 完整示例见 [`config.example.yml`](config.example.yml)，JSON Schema 见 [`config.schema.json`](config.schema.json)

Moon Bridge 使用 YAML 配置文件。未传 `-config` 时默认读取 `$HOME/moonbridge/config.yml`；如果该文件不存在，会自动创建 starter 配置后继续启动。starter 配置启用 SQLite，数据库路径为 `$HOME/moonbridge/data/moonbridge.db`。

通过 `-config <path>` 可指定任意路径；显式 `-config` 指向的文件不存在时不会自动创建，程序会 fail fast。

Web Console 的常规配置流程不要求用户直接编辑 YAML 或了解配置文件路径。启用持久化存储后，Console 通过 `/api/v1/config/graph` 以资源字段形式实时保存配置；YAML 文件仍保留给 CLI、部署脚本、备份、迁移和管理员批量编辑使用。

## 顶层结构

```yaml
mode: "Transform"  # Transform / CaptureAnthropic / CaptureResponse

log:
  level: "info"    # debug / info / warn / error
  format: "text"   # text / json

server:
  addr: "127.0.0.1:38440"
  auth_token: ""

system_prompt: ""  # 全局 system prompt（可选）

defaults:
  model: "moonbridge"
  max_tokens: 65536
```

## Web Console 与配置图

Console 将当前 `FileConfig` 映射为配置图资源：

| Console 页面 | 主要资源 |
|--------------|----------|
| Overview | `mode`、运行态、校验状态、重启要求 |
| Models & Providers | `provider`、`provider_offer`、`model` |
| Routes | `route` |
| Defaults | `defaults`、`trace`、`log` |
| Search & Tools | `web_search`、`extension`、`proxy` |
| Storage | `cache`、`persistence` |
| Security | `server` |
| Logs | 后台日志输出，不修改配置 |

字段编辑会发送配置图 patch。普通可热重载字段提交后直接生效；`server`、`mode`、`proxy`、`persistence` 等字段可能显示需要重启。密钥字段在读取时脱敏，Console 将其作为 write-only 输入处理。

使用 Console 前通常需要启用持久化配置存储，例如 SQLite：

```yaml
persistence:
  active_provider: db_sqlite

extensions:
  db_sqlite:
    enabled: true
    config:
      path: ~/.moon-bridge/moonbridge.db
      wal: true
      busy_timeout_ms: 5000
      max_open_conns: 1
```

## Mode

| 值 | 行为 |
|-----|------|
| `Transform` | 接收 OpenAI Responses 请求，按 Provider 协议转换后转发 |
| `CaptureAnthropic` | 透明代理到 Anthropic 上游（不转换） |
| `CaptureResponse` | 透明代理到 OpenAI 上游（不转换） |

## Server

```yaml
server:
  addr: "127.0.0.1:38440"    # 监听地址
  auth_token: ""              # Bearer 认证 Token（空 = 不认证）
```

## Models

模型定义包含上下文窗口、推理能力、扩展支持等元信息：

```yaml
models:
  my-model:
    context_window: 1000000
    max_output_tokens: 384000
    display_name: "My Model"
    default_reasoning_level: "high"
    supported_reasoning_levels:
      - effort: "low"
        description: "Low effort reasoning"
      - effort: "medium"
        description: "Medium effort reasoning"
      - effort: "high"
        description: "High effort reasoning"
      - effort: "xhigh"
        description: "Extra high effort reasoning"
    supports_reasoning_summaries: true
    input_modalities:
      - "text"
      - "image"
    web_search:
      support: "auto"     # auto / enabled / disabled / injected
    extensions:
      deepseek_v4:
        enabled: true
      visual:
        enabled: true
```

## Providers

Provider 定义上游 API 的连接信息和协议类型。

```yaml
providers:
  my-provider:
    base_url: "https://api.example.com"
    api_key: "sk-..."
    version: "2023-06-01"
    user_agent: "moonbridge/1.0"
    protocol: "anthropic"         # 默认 anthropic
    local: false                    # 本地模型 Provider 设为 true

    # Google GenAI 特有字段（protocol: google-genai）
    project: "my-gcp-project"
    location: "us-central1"
    api_version: "v1beta"

    web_search:
      support: "auto"
      max_uses: 1
      tavily_api_key: "tvly-..."
      firecrawl_api_key: "fc-..."
      search_max_rounds: 3

    offers:
      - model: my-model
        pricing:
          input_price: 2
          output_price: 8
          cache_write_price: 1
          cache_read_price: 0.25
```

### Protocol 类型

| 值 | 上游格式 | 对应 Adapter |
|-----|----------|-------------|
| `anthropic`（默认） | Anthropic Messages API | `internal/protocol/anthropic` |
| `openai-response` | OpenAI Responses API | `internal/protocol/openai`（直通） |
| `google-genai` | Google Generative AI (Gemini) API | `internal/protocol/google` |
| `openai-chat` | OpenAI Chat Completions API | `internal/protocol/chat` |

本地 OpenAI Chat 兼容服务（例如 llama.cpp）应在 Provider 上设置
`local: true`。该标记会为此 Provider 提供的所有模型启用 Codex 兼容的
reasoning summary 流生命周期；未标记的 Frontier Provider 和 DeepSeek
扩展不会经过这条本地兼容路径。

## Routes

路由将模型别名映射到特定 Provider 的上游模型：

```yaml
routes:
  alias-name:              # 客户端使用的模型名
    model: my-model         # models 段定义的模型名
    provider: my-provider   # providers 段定义的 Provider 名
```

## Web Search

Web Search 支持可在模型、Provider 和全局三个层级覆盖（优先级：模型 > Provider > 全局）。

| 模式 | 行为 |
|------|------|
| `auto` | 优先使用 Provider 原生 web_search API，不支持时回退到注入模式 |
| `enabled` | 启用 Provider 原生 web_search |
| `disabled` | 禁用 Web Search |
| `injected` | 通过 Tavily/Firecrawl 后端注入搜索结果 |

## Cache

```yaml
cache:
  mode: "explicit"              # off / explicit / automatic / hybrid
  ttl: "5m"
  prompt_caching: true
  automatic_prompt_cache: false
  explicit_cache_breakpoints: true
  allow_retention_downgrade: false
  max_breakpoints: 4
  min_cache_tokens: 1024
  expected_reuse: 2
  minimum_value_score: 2048
  min_breakpoint_tokens: 1024
```

## Extensions

```yaml
extensions:
  deepseek_v4:
    enabled: true
    config:
      reinforce_instructions: true
  visual:
    enabled: true
    config:
      provider: "kimi"
      model: "kimi-for-coding"
      max_rounds: 4
      max_tokens: 2048
  db_sqlite:
    enabled: true
    config:
      path: ./data/moonbridge.db
      wal: true
      busy_timeout_ms: 5000
      max_open_conns: 1
  metrics:
    enabled: true
    config:
      default_limit: 100
      max_limit: 1000
```

## Proxy（Capture 模式）

仅在 Capture 模式下有效：

```yaml
proxy:
  response:
    base_url: "https://api.openai.com"
    api_key: "sk-..."
  anthropic:
    base_url: "https://provider.example.com"
    api_key: "sk-..."
    version: "2023-06-01"
```

## CLI 标志

| 标志 | 默认值 | 说明 |
|------|--------|------|
| `-config` | `$HOME/moonbridge/config.yml` | 配置文件路径 |
| `-addr` | 来自配置文件 | 覆盖监听地址 |
| `-mode` | 来自配置文件 | 覆盖运行模式（Transform/CaptureAnthropic/CaptureResponse） |
| `-print-addr` | — | 打印配置的监听地址后退出 |
| `-print-mode` | — | 打印配置的运行模式后退出 |
| `-print-default-model` | — | 打印默认模型别名后退出 |
| `-print-codex-model` | — | 打印 Codex 模型后退出 |
| `-print-codex-config <model>` | — | 为指定模型生成 Codex config.toml 后退出 |
| `-dump-config-schema` | — | 生成 config.schema.json 后退出 |

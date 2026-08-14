# OpenAI Chat Completions ingress (`POST /v1/chat/completions`)

Moon Bridge accepts two inbound protocols in `Transform` mode:

| Ingress | Route | Clients |
|---------|-------|---------|
| OpenAI Responses | `/v1/responses` | Codex CLI, Kimi Code |
| OpenAI Chat Completions | `/v1/chat/completions` | Crush, Cline, Aider, the OpenAI SDK completions surface |

Both ingresses converge on the same Core intermediate format, so a Chat
Completions client reaches every configured upstream protocol — Anthropic
Messages, Google GenAI, OpenAI Chat, OpenAI Responses — with the same routing,
plugins, web-search injection, caching and usage accounting as a Responses
client. Nothing in `config.yml` is ingress-specific: the same `providers`,
`models` and `routes` serve both.

## Using it

Point the client at the Moon Bridge address with `/v1` appended and give it a
model name that resolves to a route alias or a `provider/model` reference:

```bash
curl -s http://127.0.0.1:38440/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"moonbridge","max_tokens":256,
       "messages":[{"role":"user","content":"hello"}]}'
```

For [Crush](https://github.com/charmbracelet/crush), the provider type is
`openai-compat` (Crush's generic Chat Completions client):

```json
{
  "providers": {
    "moonbridge": {
      "name": "Moon Bridge",
      "type": "openai-compat",
      "base_url": "http://127.0.0.1:38440/v1",
      "api_key": "<server.auth_token, or any value when auth is off>",
      "models": [{"id": "moonbridge", "name": "Moon Bridge",
                  "context_window": 200000, "default_max_tokens": 8192,
                  "can_reason": true, "supports_attachments": true,
                  "cost_per_1m_in": 0, "cost_per_1m_out": 0,
                  "cost_per_1m_in_cached": 0, "cost_per_1m_out_cached": 0}]
    }
  },
  "models": {
    "large": {"model": "moonbridge", "provider": "moonbridge"},
    "small": {"model": "moonbridge", "provider": "moonbridge"}
  }
}
```

## What the adapter maps

`internal/protocol/chatingress` implements `format.ClientAdapter` and
`format.ClientStreamAdapter` for this protocol.

Inbound (`Request` → `CoreRequest`):

- `system` / `developer` messages → `CoreRequest.System`
- `user` / `assistant` messages → `CoreMessage`, with `tool_calls` becoming
  `tool_use` blocks and `reasoning_content` becoming a `reasoning` block
- `tool` messages → a `tool` role message holding one `tool_result` block
- `max_tokens` **and** `max_completion_tokens` are both accepted; the modern
  field wins when both are present
- `stop` accepts a bare string or an array
- content parts: `text`, and `image_url` with a `data:` URL (a remote image URL
  is passed through as a text reference, since Core carries base64 payloads)
- `tools`, `tool_choice` (string or named-function object), `temperature`,
  `top_p`, `reasoning_effort`, `metadata`, `user`

Outbound (`CoreResponse` → response):

- one choice, `object: "chat.completion"`, `finish_reason` mapped from the Core
  stop reason (`tool_use` → `tool_calls`, `max_tokens` → `length`, …)
- tool call `arguments` are emitted as a JSON **string**, as the wire format
  requires

Streaming follows the Chat Completions shape exactly: anonymous `data:` frames
(no `event:` lines), a first chunk carrying `delta.role`, incremental
`delta.content` / `delta.reasoning_content` / `delta.tool_calls`, a
`finish_reason` chunk, an optional usage-only chunk when the client sent
`stream_options.include_usage`, and the literal `data: [DONE]` sentinel.

## Limitations

- **`openai-response` upstreams are not reachable from this ingress.** Moon
  Bridge serves those providers by forwarding a Responses body verbatim
  (`handleOpenAIResponse`); there is no Core → Responses provider adapter to
  convert into. A Chat Completions request routed to such a provider gets a 502
  naming the protocol. Route Chat Completions clients to `anthropic`,
  `google-genai` or `openai-chat` providers, or add a Responses ProviderAdapter.
- `response_format` / `json_schema`, `n`, `seed`, `logprobs` and the penalty
  parameters are dropped: the Core format has no equivalent, so they cannot be
  carried to any upstream.
- Remote (`http(s)`) image URLs are passed through as a text reference, because
  Core carries images as base64 payloads. Inline `data:` images convert fully.

## Implementation notes

- The dispatcher is ingress-agnostic: `handleWithAdapters` /
  `handleAdapterStream` take an `inboundRequest` (protocol, model, stream flag,
  decoded DTO) and look the client adapter up by protocol, rather than assuming
  OpenAI Responses.
- Client stream adapters emit `format.ClientStreamFrame` values, which the
  server writes as SSE. The Responses adapter's typed event channel is
  normalized into the same frames, so both ingresses share one write loop.
- Error envelopes are identical in both ingresses (`{"error":{...}}`), so
  existing tracing and client error handling are unchanged.

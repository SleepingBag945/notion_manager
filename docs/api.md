# API Usage

[← Back to README](../README.md)

## Supported API shapes

- `POST /v1/messages` — Anthropic Messages API
- `POST /v1/chat/completions` — OpenAI Chat Completions API
- `POST /v1/responses` — OpenAI Responses API

All three routes reuse the same multi-account pool, file upload pipeline, tool bridge, and failover logic.

`/v1/responses` is currently stateless, so `previous_response_id` is not supported.

## Anthropic Messages request

`/v1/messages` accepts Anthropic Messages API payloads.

```bash
curl http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "opus-4.6",
    "max_tokens": 1024,
    "messages": [
      { "role": "user", "content": "Describe the main components of this project." }
    ]
  }'
```

If `model` is omitted, the service falls back to `proxy.default_model`.

## OpenAI Chat Completions request

```bash
curl http://localhost:3000/v1/chat/completions \
  -H "Authorization: Bearer <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.4",
    "messages": [
      { "role": "user", "content": "Summarize the architecture of notion-manager." }
    ]
  }'
```

Supported request features include `messages`, `stream`, `tools`, `tool_choice`, `response_format`, and inline file/image inputs.

## OpenAI Responses request

```bash
curl http://localhost:3000/v1/responses \
  -H "Authorization: Bearer <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.4",
    "input": "List the main subsystems in this project."
  }'
```

Supported request features include `input`, `instructions`, `stream`, `tools`, `tool_choice`, `text.format`, and inline file/image inputs.

## Search overrides

```bash
curl http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -H "X-Web-Search: true" \
  -H "X-Workspace-Search: false" \
  -d '{
    "model": "sonnet-4.6",
    "messages": [
      { "role": "user", "content": "Search for recent information about Go 1.25." }
    ]
  }'
```

## File uploads

Supported media types:

- `image/png`
- `image/jpeg`
- `image/gif`
- `image/webp`
- `application/pdf`
- `text/csv`

```bash
curl http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "sonnet-4.6",
    "max_tokens": 600,
    "messages": [{
      "role": "user",
      "content": [
        {
          "type": "document",
          "source": {
            "type": "base64",
            "media_type": "application/pdf",
            "data": "<base64>"
          }
        },
        {
          "type": "text",
          "text": "Summarize this PDF."
        }
      ]
    }]
  }'
```

## Research mode

```bash
curl -N http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "researcher",
    "stream": true,
    "max_tokens": 16000,
    "thinking": { "type": "enabled", "budget_tokens": 50000 },
    "messages": [
      { "role": "user", "content": "Map common architectural patterns used by Notion AI proxy tools." }
    ]
  }'
```

Research mode is single-turn, ignores file uploads, ignores custom tools, and runs with a longer timeout path.

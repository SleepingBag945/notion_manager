# API 接入

[← 返回 README](../README_CN.md)

## 支持的 API 形态

- `POST /v1/messages` — Anthropic Messages API
- `POST /v1/chat/completions` — OpenAI Chat Completions API
- `POST /v1/responses` — OpenAI Responses API

三条路由共用同一套多账号池、文件上传链路、工具桥接和失败切换逻辑。

当前 `/v1/responses` 是无状态桥接，因此暂不支持 `previous_response_id`。

## Anthropic Messages 基本请求

`/v1/messages` 使用 Anthropic Messages API 结构。

```bash
curl http://localhost:3000/v1/messages \
  -H "Authorization: Bearer <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "opus-4.6",
    "max_tokens": 1024,
    "messages": [
      { "role": "user", "content": "请总结一下 notion-manager 的用途。" }
    ]
  }'
```

如果不传 `model`，会自动使用 `proxy.default_model`。

## OpenAI Chat Completions 基本请求

```bash
curl http://localhost:3000/v1/chat/completions \
  -H "Authorization: Bearer <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.4",
    "messages": [
      { "role": "user", "content": "请总结 notion-manager 的主要架构。" }
    ]
  }'
```

已支持的常用字段包括 `messages`、`stream`、`tools`、`tool_choice`、`response_format`，以及内联图片 / 文件输入。

## OpenAI Responses 基本请求

```bash
curl http://localhost:3000/v1/responses \
  -H "Authorization: Bearer <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.4",
    "input": "列出这个项目的主要子系统。"
  }'
```

已支持的常用字段包括 `input`、`instructions`、`stream`、`tools`、`tool_choice`、`text.format`，以及内联图片 / 文件输入。

## 搜索控制

全局开关由 `config.yaml` 和 Dashboard 管理，请求级覆盖使用这两个请求头：

- `X-Web-Search: true|false`
- `X-Workspace-Search: true|false`

示例：

```bash
curl http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -H "X-Web-Search: true" \
  -H "X-Workspace-Search: false" \
  -d '{
    "model": "sonnet-4.6",
    "messages": [
      { "role": "user", "content": "搜索最近关于 Go 1.25 的信息。" }
    ]
  }'
```

## 文件上传

支持以下媒体类型：

- `image/png`
- `image/jpeg`
- `image/gif`
- `image/webp`
- `application/pdf`
- `text/csv`

示例：

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
          "text": "总结这份 PDF。"
        }
      ]
    }]
  }'
```

文件会由代理自动完成上传、轮询处理和转录注入。

## 研究模式

研究模式由模型名触发：

- `researcher`
- `fast-researcher`

示例：

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
      { "role": "user", "content": "梳理一下 2026 年前后 Notion AI 代理工具常见架构。" }
    ]
  }'
```

研究模式注意事项：

- 只使用最后一条用户消息，属于单轮研究
- 会忽略文件上传
- 会忽略 `tools`
- 超时比普通对话更长，默认研究超时为 `360s`

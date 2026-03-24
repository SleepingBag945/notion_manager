# Configuration

[← Back to README](../README.md)

## Priority

```text
environment variables > config.yaml > code defaults
```

## Key settings

| Setting | Repo sample | Code default | Notes |
| --- | --- | --- | --- |
| `server.port` | `3000` | `8081` | This README uses the checked-in sample port |
| `server.accounts_dir` | `accounts` | `accounts` | Account JSON directory |
| `server.api_key` | empty | auto-generated | Used for API-key protected endpoints |
| `server.admin_password` | empty | auto-generated | Auto-generated if empty; hashed on first startup |
| `server.debug_logging` | `true` | `true` | High-volume debug logging |
| `proxy.default_model` | `opus-4.6` | `opus-4.6` | Fallback model |
| `proxy.disable_notion_prompt` | `true` | `false` | Removes the built-in Notion prompt for leaner API usage |
| `proxy.enable_web_search` | `true` | `true` | Global web search toggle |
| `proxy.enable_workspace_search` | `false` | `false` | Global workspace search toggle |
| `refresh.interval_minutes` | `30` | `30` | Background refresh interval |
| `refresh.quota_recheck_minutes` | `30` | `30` | Exhausted-account recheck interval |
| `timeouts.inference_timeout` | `300` | `300` | Standard inference timeout |
| `timeouts.research_timeout` | not set explicitly | `360` | Research timeout |

## Main Endpoints

| Path | Purpose | Auth |
| --- | --- | --- |
| `GET /health` | Health and account pool summary | None |
| `POST /v1/messages` | Anthropic Messages API | API key |
| `POST /v1/chat/completions` | OpenAI Chat Completions API | API key |
| `POST /v1/responses` | OpenAI Responses API | API key |
| `GET /dashboard/` | Dashboard UI | Dashboard login |
| `GET /proxy/start` | Create a targeted proxy session | Dashboard login |
| `GET /ai` | Local Notion Web proxy entry | `np_session` |
| `GET /admin/accounts` | Account detail backing API | Dashboard session |
| `GET /admin/models` | Model list backing API | Dashboard session |
| `GET/POST /admin/refresh` | Refresh status and trigger | Dashboard session |
| `GET/PUT /admin/settings` | Read and update dashboard settings | Dashboard session |

## Notes

- `admin_password` left empty will auto-generate a random password printed to the console, then hash and write it back to `config.yaml`. Save the plaintext shown on first startup.
- The reverse proxy works best with account files that include `full_cookie`.
- Free accounts can stay exhausted for long periods; paid accounts are a better fit for a stable pool.
- If you change the dashboard source under `web/`, rebuild and sync it into `internal/web/dist/` before running.

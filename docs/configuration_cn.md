# 配置说明

[← 返回 README](../README_CN.md)

## 配置优先级

```text
环境变量 > config.yaml > 代码默认值
```

## 关键配置项

| 配置项 | 仓库样例值 | 代码默认值 | 说明 |
| --- | --- | --- | --- |
| `server.port` | `3000` | `8081` | 文档示例默认按仓库样例的 `3000` 编写 |
| `server.accounts_dir` | `accounts` | `accounts` | 账号 JSON 目录 |
| `server.api_key` | 空 | 自动生成 | 仅用于 `/v1/messages` 等 API Key 保护接口 |
| `server.admin_password` | 空 | 自动生成 | 留空时自动生成；首次启动后哈希回写 |
| `server.debug_logging` | `true` | `true` | 控制高频调试日志 |
| `proxy.default_model` | `opus-4.6` | `opus-4.6` | 请求未传 `model` 时的回退模型 |
| `proxy.disable_notion_prompt` | `true` | `false` | 关闭 Notion 内置系统提示词，减少输入 token |
| `proxy.enable_web_search` | `true` | `true` | 默认联网搜索开关 |
| `proxy.enable_workspace_search` | `false` | `false` | 默认工作区搜索开关 |
| `refresh.interval_minutes` | `30` | `30` | 后台刷新间隔 |
| `refresh.quota_recheck_minutes` | `30` | `30` | 耗尽账号重新检查间隔 |
| `timeouts.inference_timeout` | `300` | `300` | 普通推理超时 |
| `timeouts.research_timeout` | 未显式设置 | `360` | 研究模式超时 |

## 环境变量

```bash
export PORT=3000
export API_KEY=sk-your-api-key
export ENABLE_WEB_SEARCH=true
export ENABLE_WORKSPACE_SEARCH=false
```

## 主要端点

| 路径 | 说明 | 认证方式 |
| --- | --- | --- |
| `GET /health` | 健康检查与账号池摘要 | 无 |
| `POST /v1/messages` | Anthropic Messages API | API Key |
| `POST /v1/chat/completions` | OpenAI Chat Completions API | API Key |
| `POST /v1/responses` | OpenAI Responses API | API Key |
| `GET /dashboard/` | 管理面板 | Dashboard 登录 |
| `GET /proxy/start` | 为选定账号创建代理会话 | Dashboard 登录 |
| `GET /ai` | Notion Web 代理入口 | `np_session` |
| `GET /admin/accounts` | 账号详情，供 Dashboard 使用 | Dashboard 会话 |
| `GET /admin/models` | 模型列表，供 Dashboard 使用 | Dashboard 会话 |
| `GET/POST /admin/refresh` | 刷新状态 / 触发刷新 | Dashboard 会话 |
| `GET/PUT /admin/settings` | 读取 / 更新搜索相关设置 | Dashboard 会话 |

## 项目结构

```text
notion-manager/
├── cmd/notion-manager/        # 程序入口
├── internal/proxy/            # 账号池、API 处理、反向代理、上传、配置
├── internal/web/dist/         # 嵌入式 Dashboard 静态资源
├── web/                       # Dashboard 前端源码
├── chrome-extension/          # 提取 Notion 会话配置的 Chrome 扩展
├── accounts/                  # 账号 JSON（运行时目录）
├── example.config.yaml       # 示例配置文件
├── README.md
└── README_CN.md
```

## 使用建议与限制

- `admin_password` 留空时会自动生成随机密码并打印到控制台，哈希后写回 `config.yaml`。请务必记住首次启动时显示的明文密码。
- 反向代理最好使用包含 `full_cookie` 的账号配置，这样 Web 页面兼容性更好。
- 免费账号可能在额度耗尽后长期不可用，付费账号更适合作为稳定池。
- `researcher` 模式适合长耗时研究，不适合多轮连续对话。
- 如果你修改了 `web/` 前端源码，需要重新构建并同步到 `internal/web/dist/`，否则运行时仍会使用已嵌入的旧前端资源。

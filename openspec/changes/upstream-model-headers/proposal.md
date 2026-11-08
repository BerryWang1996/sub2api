## Why

Composite 与 AI 路由把客户端请求的公开模型名改写成上游模型后，响应体的 `model` 字段会回写成客户端原名（`openai_gateway_responses_anthropic_native.go` 的 `responsesResp.Model = originalModel`），以保持客户端身份与缓存键稳定。副作用是客户端本机没有任何渠道知道这次请求实际落到了哪家、哪个模型：Claude Code / Claude Desktop 的会话日志、`/tasks`、本地代理的用量统计记的都是公开别名；真实落点只在服务端 `usage_log` 的 `upstream_model`、`upstream_response_model`、`upstream_model_mismatch`、`model_mapping_chain` 列，普通用户和本机审计脚本看不到。

2026-11-08 用户决定把「把上游模型带回本机」登记进 fork 开发计划，作为别名重构（四档 × 厂商映射表）的配套；实现、发布与生产升级各需另行授权，本文只是计划。

## What Changes

- 在 Messages、Responses HTTP、Chat Completions 三类客户端出口统一加两条响应头：`X-Sub2API-Upstream-Model: <upstream_model>`、`X-Sub2API-Route: <group>/<platform>`。流式响应在首个 SSE 事件前写头；非流式随正文返回。Responses WebSocket 与 Gemini 原生入口本期不改，另立条目。
- 不改响应体 `model` 字段，不改用量、计费与缓存键；响应头只读，不参与任何决策。
- 头的取值与 `usage_log` 同源：`upstream_model` 取实际发往上游的模型，`route` 取最终解析的组与平台；AI 路由走 `execution_model` 与候选目标。请求失败、未命中路由或在决策前被拒时不写头。
- 普通用户用量页展示「公开模型 → 实际模型」两列（现有字段，仅补前端），管理员页不变。
- 可选第二步（不在本条目内承诺）：识别 Claude Code 官方请求头 `x-claude-code-request-class`，把 auxiliary / compaction 桶强制走指定便宜档；需先核实该头在 Claude Desktop 托管会话里是否存在。

## Capabilities

### New Capabilities
- `upstream-model-headers`：三类客户端出口的上游模型与路由响应头，流式与非流式一致；失败路径不写头；用量页两列展示。

### Modified Capabilities
<!-- openspec/specs 目前为空，没有既有能力需要修改。 -->

## Non-goals

- 不改变 `model` 字段、不暴露账号 ID、不暴露决策提示词或候选池。
- 不替代服务端 usage_log 作为计费与审计的权威来源。
- 不为 CC Switch 等本地代理做专门适配；它们按普通 HTTP 头透传即可。

## Impact

- 后端：三个出口的响应写入路径与 SSE 首块；`usage_log` 不变，无迁移。
- 前端：用量页增加两列。
- 版本：预计随 v0.2.20-barry.4 发布；按 `FORK_MAINTENANCE.md` 走完整预检、不可变镜像核验与生产升级验证。
- 验证：curl 对三类出口各发一次流式与非流式请求，核对头值与 usage_log 一致；Claude Desktop 经 CC Switch 直连时抓包确认头可见；失败请求无头。
- 回滚：回退镜像到 v0.2.20-barry.3，无数据层改动。

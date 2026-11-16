# 模型列表额度查询

在现有模型列表请求中加入 `include=quota`，即可按当前 API Key 所属分组读取各模型的额度参考值。先看 Key 是否受预算限制，再看准备使用的模型和入口。模型 ID 仍用于原推理接口。

```http
GET /v1/models?include=quota
Authorization: Bearer <现有 API Key>
```

默认按 Messages 入口计算。使用其他推理接口时传 `endpoint=chat_completions`、`endpoint=responses` 或 `endpoint=gemini`。每次只返回一个入口的结果。普通模型列表、根路径 `/models`、Codex 专用目录和 Google 原生模型目录沿用原协议。

## 读结果

1. 确认响应根对象的 `sub2api_quota.schema_version` 为 `1`。旧服务可能忽略参数并返回普通模型列表。缺少根扩展或版本未知时，客户端应提示额度信息不可用。
2. 查看根扩展的 `key_admission`。如果为 `blocked`，这把 Key 受到共同预算阻断。模型有余量也不能解除这道限制。按当前运行模式报告实际适用的预算门槛。
3. 查看该模型的 `sub2api_quota`。它包含 `status`、`recovers_at`、`accounts_available`、`accounts_total` 和代表账号的 `windows`。根扩展的 `endpoint` 和 `queried_at` 记录本次评估入口和查询时刻。

| 状态 | 含义 |
| --- | --- |
| `available` | 至少一个符合当前路由的账号没有已知额度阻断。缺用量数字时可标 `quota_not_observed` |
| `exhausted` | 没有已知可派账号或优先的未知状态，且至少一个账号额度用完或限流冷却。已知最早恢复时间写入 `recovers_at` |
| `blocked` | 无合格账号，或有过载、策略暂停等其他已知限制 |
| `unknown` | 本次读取失败等原因导致无法判断 |

每个窗口给出已用比例 `used_percent`、复位时刻 `resets_at`、观测时刻 `observed_at` 及匿名池编号 `pool`。剩余比例可以用 100 减去已用比例。同一响应里两行出现同一个池编号，表示共用同一份窗口，不能将它重复相加。不同网关账号若使用同一个上游订阅，本版仍按各账号展示。账号身份和凭据不进入响应。

已知复位时刻过去的窗口会从结果中省略。复位前，旧的耗尽样本仍用于判断。额度来自上次观测或当前本地计量，其他客户端的消耗、供应商提前重置或网关账号换绑都可能使结果与实时余量不同。`recovers_at` 为条件估计，值为 `null` 时没有可用的恢复时刻。未提供余量的平台仍可根据网关状态返回可用性。

Antigravity 会复用后台额度读取。每个实例对同一账号共享一次刷新，成功结果缓存三分钟，失败也会短暂缓存。缓存未命中时会访问 Google 的额度接口。额度查询最多等待三秒，超时可先读 `unknown`，后台完成后再查。Antigravity 窗口按实际发往上游的模型匹配，分组摘要缺少明确的模型映射。额度读取本身不会发送推理请求。

## 鉴权和客户端兼容

- 预算耗尽的合法分组 Key 仍能查询。停用、过期、撤权、IP 不匹配或 Full 模式缺有效订阅时拒绝。推理沿用当前运行模式的鉴权与预算规则。
- `include=quota` 与 `client_version` 同时出现、参数重复或入口非法时返回 400。AI 路由分组和 OpenAI pinned/live 源目录返回 501。扩展响应使用 `Cache-Control: private, no-store`。
- 使用可保留根对象扩展字段的 JSON 入口。原生模型菜单或 `/models` 命令是否附加参数、展示额度或把结果传给智能体，取决于客户端集成。

回退到 v0.2.21-barry.4 时，调用方按根扩展检测能力。已有账号额度字段会保留，本功能无数据库结构迁移。

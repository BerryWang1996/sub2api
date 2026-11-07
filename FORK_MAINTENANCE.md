# BerryWang1996 fork：维护与镜像发布

当前发布候选为 **v0.2.20-barry.2**（更新 Gemini / Google One 测试模型目录，默认 3.8 Flash），上游基线为官方标签 v0.2.20（dab829a4e）。保留 fork 的跨厂商、Composite 和 AI 路由定制；以下 v0.2.19-barry.3 发布与部署说明为历史基线。新版本须通过完整检查、不可变镜像核验及生产升级验证后才能标记完成。

维护仓库：<https://github.com/BerryWang1996/sub2api>

上游仓库：<https://github.com/Wei-Shaw/sub2api>

本 fork 保留上游许可证、版权及协议文件。独立运行和维护 fork 不依赖上游合并 PR；贡献者 CLA 与软件使用许可证、管理员合规确认及上游模型服务条款是不同事项，本流程不替任何人签署协议。

## 分支与更新原则

- `main`：已审查的定制版本，保留跨厂商思考转换及 Composite 目标路由。
- `sync/upstream-<sha>`：上游同步工作流创建的合并审查分支。
- 不使用 GitHub 的强制覆盖／丢弃定制提交方式同步上游；同步通过正常 merge，冲突必须人工处理。
- 不自动合并同步 PR，不自动部署服务器。

## 手动同步上游

在 fork 的 **Actions → Fork upstream sync → Run workflow**，选择 `main`。

工作流固定读取 `Wei-Shaw/sub2api:main`：

1. 从本 fork 最新 `main` 建立审查分支。
2. 合并上游而不是替换历史。
3. 无变化则结束；冲突则失败并保留远端现状。
4. 成功后只推送本 fork 的同步分支，创建指向本 fork `main` 的内部 PR。

`GITHUB_TOKEN` 创建的分支／PR 通常不会自动触发其他 workflow。不要把同步成功当作测试通过：在 **Fork image** 中选择同步分支，保持 `publish=false`，运行完整预检；合并后再对 `main` 运行发布。若仓库禁止 Actions 创建 PR，同步分支仍会保留，可以手动创建内部 PR，或由仓库所有者在正常设置流程中批准该权限。

合并成功后，工作流还会运行[数据库迁移检查](#数据库迁移与上游同步)，把结果写入运行摘要和 PR 描述。检查失败时仍推送同步分支并创建 PR，便于在该分支上修复，但工作流标记为失败；不要合并检查失败的同步 PR。

本机同步（不推送）：

```bash
scripts/fork-sync.sh
```

脚本要求已提交或移开所有已跟踪改动，然后抓取上游、`--no-ff` 合并并运行迁移检查。合并冲突时按提示人工解决并提交，再执行 `scripts/fork-sync.sh --check-only <同步前的提交>`。检查通过后再运行完整测试并推送到本 fork。

## 数据库迁移与上游同步

迁移执行器按文件名字节序执行 `backend/migrations/*.sql`，在 `schema_migrations` 中记录文件名和内容校验和；已执行文件的内容改变会导致启动失败，已记录但已不存在的文件会被忽略。上游迁移按 `NNN_` 递增编号，fork 若沿用同一编号空间，迟早会与上游新迁移同名（例如上游已用到 `248_`，fork 原有的 `249_composite_route_destinations.sql` 会与上游下一个 `249_` 冲突），出现校验和不一致或某一方迁移被跳过。为此约定：

1. **fork 迁移使用独立命名空间** `fork_NNN_<描述>.sql`（小写、下划线，编号三位且不重复）。字节序中 `f` 排在所有数字之后，因此 fork 迁移总是在全部上游迁移之后执行，与上游新增编号互不干扰。
2. **发布后文件名和内容冻结**。需要修改结构时新增 `fork_NNN_` 迁移，不改旧文件。
3. **fork 迁移必须幂等**：`CREATE TABLE/INDEX ... IF NOT EXISTS`、`ADD COLUMN IF NOT EXISTS`、`DROP ... IF EXISTS`、`CREATE OR REPLACE`，约束放进检查 `pg_constraint` 的 `DO` 块，`INSERT` 带 `ON CONFLICT` 或 `NOT EXISTS`，不用 `RENAME`。原因：上游新增迁移在已有 fork 数据库上排在 fork DDL 之后运行，在全新安装中排在其之前；改名迁移也会让旧名称已执行过的数据库重新执行一次。
4. **永不修改或删除上游 `NNN_*.sql`**。需要调整上游对象时写 fork 迁移。
5. **逃生口** `NNN-fork-pre_<描述>.sql`：`-` 排在 `_` 之前，该文件会在上游同号 `NNN_` 迁移之前执行。仅在上游新迁移与 fork 已建对象冲突、且无法在 fork 迁移中调和时使用（例如上游新迁移无条件 `CREATE TABLE` 一张 fork 已建的表，可在其前把 fork 的表改造成上游预期的结构）。同样必须幂等。

历史名称：`fork_001_composite_route_destinations.sql` 早先以 `246_`、`249_` 发布过，已执行旧名称的数据库会因幂等语句无变化地再执行一次。

### 自动检查

| 检查 | 位置 | 内容 |
| --- | --- | --- |
| 命名与幂等策略 | `go test ./migrations/`（`backend/migrations/fork_migration_policy_test.go`） | fork 迁移命名、编号唯一，DDL 写法满足第 3 条 |
| 上游冲突守卫 | `scripts/fork-migration-guard.sh <上游ref> [<同步前fork ref>]` | 错误：fork 中存在上游没有的数字编号迁移、上游迁移内容被改、上游迁移被删。警告：同步引入的上游新迁移提到了 fork 迁移定义的表、列、索引、约束等 |
| 升级路径回放 | `scripts/fork-migration-upgrade-test.sh <ref>...`（需要 Docker） | 对每个 ref 的迁移目录建库，再执行当前迁移两次，与全新安装比较列、约束、索引、触发器、函数和视图 |

`scripts/fork-sync.sh` 和同步工作流依次运行这三项；升级回放使用同步前的 fork 提交（现有 fork 数据库）和最新上游标签（从官方镜像切换过来的数据库）。本机 Docker 无法拉取测试镜像时，可用 `SUB2API_TEST_POSTGRES_IMAGE` 指定已有的 PostgreSQL 镜像；`SKIP_UPGRADE_TEST=1` 可跳过回放，但上游新增迁移时不建议跳过。

### 守卫报警或回放失败时

1. 阅读警告中的上游迁移，判断它对 fork 已建对象做了什么。
2. 若上游实现了与 fork 相同的对象：让 fork 迁移的定义与上游一致（新增 `fork_NNN_` 收敛差异），或在必要时用 `NNN-fork-pre_` 预处理。
3. 若上游迁移在 fork 数据库上会失败（如重复建表、重复约束）：使用 `NNN-fork-pre_` 逃生口。
4. 重新运行 `scripts/fork-sync.sh --check-only <同步前的提交>`，直到回放结果与全新安装一致。

## 手动构建与发布

### 版本规则

版本唯一来源为 `backend/cmd/server/VERSION`，格式为 **`vMAJOR.MINOR.PATCH-barry.N`**。前三段保留上游基线，同一基线下 `N` 是从 1 开始的发布迭代号，按数值递增：`.1 < .2 < .10`。拼写固定为 `barry`。一次迭代中的编辑、构建和测试不另加号；成功发布后，下次开发才递增，不复用已发布标签。

当前发布候选是 **`v0.2.19-barry.3`**，五入口 AI 路由前台请求已完成本地验收，后台功能不开放。2026-11-06 用户已授权提交/push 及发布此镜像；以实际 Fork image 成功结果和不可变 digest 确认发布完成，生产升级仍须另行确认。`v0.2.19-barry.2` 已于 2026-11-05 发布及升级生产，历史证据见 `artifacts/PROJECT_LOCATION.md`；本轮只读核实生产并准备方案。旧的 `barry-YYYYMMDD.N` 镜像和带 commit SHA 的程序版本只作为历史标识保留，不重发旧标签。

源码构建、嵌入程序版本、镜像发布标签与页面显示使用同一版本。发布输入必须与 VERSION 完全一致；`sha-<完整提交 SHA>` 仍作为源码定位标签。更新检查按 fork 渠道读取版本元数据，保留旧格式识别，按上游基线和迭代号比较。fork 页面提供 GHCR 镜像入口，应用内二进制更新和回退被禁用。若只有 GHCR 镜像而没有 GitHub Release 元数据，页面明确提示检查不可用，不能据此断言已是最新；实际发布版本与 digest 以 Fork image 摘要及包页面为准。

### 构建流程

在 **Actions → Fork image → Run workflow**：

- **仅验证**：可选择功能／同步分支，`publish=false`。
- **发布镜像**：必须选择 `main`，`publish=true`，输入与 VERSION 一致且未使用过的标签，下一次拟为 `v0.2.19-barry.3`；通过发布门槛并获准发布后执行，不覆盖已有 `.2`。

发布前强制执行：

- 前端 lint、类型检查、完整测试和生产构建；
- 后端完整 unit-tag 测试和静态检查；
- 目标路由／真实本地 TLS 思考恢复的 race 回归；
- 构建嵌入前端的 Linux 程序；
- 构建 Docker 镜像，并在无网络容器中检查版本、许可证和资源文件。

测试失败时不会推送镜像。使用 Actions 的临时 `GITHUB_TOKEN` 推送本 fork 的 GHCR 包，不需要 Docker Hub 凭据或私人 PAT。

发布直接为通过 smoke 检查的本地镜像 ID 添加版本和 SHA 标签再推送，不再次构建替代它。随后核对两标签指向同一 digest，并核对该不可变 manifest 的 config digest 与本地镜像 ID 相同。探测旧标签时只有明确不存在才继续，认证、网络或未知错误会停止发布。该流程的版本与发布脚本有离线模拟回归；离线通过不等于 GHCR 实际发布成功。

当前发布目标是 **linux/amd64**。ARM 服务器不能直接使用该镜像；需先扩展多架构验证与发布流程。

镜像地址：

```text
ghcr.io/berrywang1996/sub2api:v0.2.19-barry.3
ghcr.io/berrywang1996/sub2api:sha-<完整源代码提交SHA>
```

工作流摘要会输出 digest 和对应源码地址。**生产优先使用 digest**，而不是 `latest`、`main` 或其他可变标签。本工作流不发布 `latest`，并拒绝覆盖已经存在的发布标签。

首次发布的 GHCR 包可能默认为私有。若希望服务器匿名拉取，在 GitHub 的该包设置中由所有者确认可见性；否则服务器需要具有包读取权限的登录。仓库公开不等于包必然公开。

## 服务器改用 fork 镜像

以下是通用部署说明；Taom 是会话可能依赖的网关，获准操作时必须按 `artifacts/PROJECT_LOCATION.md` 使用一条 detached 端到端命令，包含代理网络恢复、健康检查、失败回滚及日志，不能照下面的分步命令逐次操作。

先备份数据库及当前部署配置，记录原镜像 digest，并在预发布环境验证。不要改变数据库、Redis、网络或数据卷路径，不要运行 `down -v`。

项目提供只覆盖 `sub2api.image` 的 [deploy/compose.fork.yml](deploy/compose.fork.yml)。将它放到当前部署目录；在该目录的 `.env` 中加入实际发布摘要输出的 digest：

```dotenv
SUB2API_FORK_IMAGE=ghcr.io/berrywang1996/sub2api@sha256:<实际镜像摘要>
```

假设现有配置叫 `docker-compose.local.yml`，从原部署目录执行：

```bash
docker compose -f docker-compose.local.yml -f compose.fork.yml config --images
```

```bash
docker compose -f docker-compose.local.yml -f compose.fork.yml pull sub2api
```

```bash
docker compose -f docker-compose.local.yml -f compose.fork.yml up -d --no-deps sub2api
```

```bash
docker compose -f docker-compose.local.yml -f compose.fork.yml logs --tail=100 sub2api
```

若当前服务名、Compose 文件名不同，先按现有部署调整，不要直接复制一套新数据库配置替代正在运行的数据卷。首次升级会执行目标路由迁移，详情见 [功能与回滚说明](docs/composite-cross-provider-routing.zh-CN.md)。

### 回滚

把 `.env` 中的镜像引用改回之前记录的 digest，按相同 Compose 命令只重建应用服务。若回滚到不识别目标 ID 的旧代码，应先停用或调整新规则；旧代码可能忽略目标限制。数据库新增列不要在紧急回滚时随意删除；需要恢复数据库时使用维护窗口和备份。

回滚到不识别 AI 路由的旧版本前，必须先在当前版本停用所有 AI 组，并确认其关联 Key 不能继续使用这些组；旧版本会将其视为静态 Composite，无法保持 AI 策略约束。首次升级时若尚未创建 AI 组，失败可以直接恢复旧应用镜像，保留 `fork_002` 新增表和列，不回删迁移或生产数据。

`barry.3` 按用户决定**不开放 AI 路由 Responses 后台任务**：生成入口提前拒绝 `background=true`，生产不启动该 worker，页面没有后台任务操作。后台实验代码及 `fork_003` 保留，余额/订阅/Key/平台额度读取仍尊重已有账本，不能通过关闭功能绕开冻结额。

若库中存在后台实验或未来版本产生的任务，另有回退前提：目标旧版本没有 `fork_003` 的持久预留检查与结算 worker。必须先停止新后台任务准入，并确认全部已提交任务及预留已结算、用量日志和额度缓存已同步；缺响应 ID 或可信用量的任务不能按超时自动退款，也不能改库清空预留后回退。未满足时保留支持该账本的版本处理恢复。停用 AI 组本身不等于完成后台结算。`fork_003` 只做向前新增，不回删表或已发布迁移。当前本机旧版回放只验证仓储/普通计费兼容，不能据此声称旧完整应用可无条件安全接管后台任务。

若目标是会话依赖的 Taom 网关，以上命令仅说明 Compose 操作含义，不能逐条执行。先只读核实实际容器标签中的部署目录、Compose 文件、运行镜像和代理网络；获准升级时用一条 detached 端到端命令完成备份、配置变更、仅重建应用、代理网络恢复、健康检查、失败自动回滚及日志。不得重建数据库/Redis、删除数据卷，或依赖断线后的第二次调用恢复代理。

## 许可证和对应源码

保留 `LICENSE`、`CLA.md` 和上游通知，不修改或伪造已签署记录。镜像包含 `LICENSE` 与 `NOTICE`；OCI 标签关联本 fork 和完整源码 SHA。公开分发镜像时，应使对应源码、构建文件及许可证可获得，并自行评估 LGPL-3.0 等适用要求；本文件不构成法律意见。

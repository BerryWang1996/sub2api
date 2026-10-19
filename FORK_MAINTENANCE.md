# BerryWang1996 fork：维护与镜像发布

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

## 手动构建与发布

在 **Actions → Fork image → Run workflow**：

- **仅验证**：可选择功能／同步分支，`publish=false`。
- **发布镜像**：必须选择 `main`，`publish=true`，输入未使用过的标签，如 `barry-20261019.1`。

发布前强制执行：

- 前端 lint、类型检查、完整测试和生产构建；
- 后端完整 unit-tag 测试和静态检查；
- 目标路由／真实本地 TLS 思考恢复的 race 回归；
- 构建嵌入前端的 Linux 程序；
- 构建 Docker 镜像，并在无网络容器中检查版本、许可证和资源文件。

测试失败时不会推送镜像。使用 Actions 的临时 `GITHUB_TOKEN` 推送本 fork 的 GHCR 包，不需要 Docker Hub 凭据或私人 PAT。

当前发布目标是 **linux/amd64**。ARM 服务器不能直接使用该镜像；需先扩展多架构验证与发布流程。

镜像地址：

```text
ghcr.io/berrywang1996/sub2api:barry-YYYYMMDD.N
ghcr.io/berrywang1996/sub2api:sha-<完整源代码提交SHA>
```

工作流摘要会输出 digest 和对应源码地址。**生产优先使用 digest**，而不是 `latest`、`main` 或其他可变标签。本工作流不发布 `latest`，并拒绝覆盖已经存在的发布标签。

首次发布的 GHCR 包可能默认为私有。若希望服务器匿名拉取，在 GitHub 的该包设置中由所有者确认可见性；否则服务器需要具有包读取权限的登录。仓库公开不等于包必然公开。

## 服务器改用 fork 镜像

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

## 许可证和对应源码

保留 `LICENSE`、`CLA.md` 和上游通知，不修改或伪造已签署记录。镜像包含 `LICENSE` 与 `NOTICE`；OCI 标签关联本 fork 和完整源码 SHA。公开分发镜像时，应使对应源码、构建文件及许可证可获得，并自行评估 LGPL-3.0 等适用要求；本文件不构成法律意见。

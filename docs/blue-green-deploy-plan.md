# 实施计划:蓝绿部署 — Nginx + Docker 多站点零停机发布

| 项 | 内容 |
|---|---|
| 文档版本 | v1.0 |
| 日期 | 2026-07-21 |
| 状态 | 评审中 |
| 关联文档 | 技术设计:`blue-green-deploy-design.v1.md`;需求:`blue-green-deploy-prd.md` |
| 文档定位 | 将 PRD 的 FR-1~FR-9 与阶段 0-4 分解为可执行任务,粒度到文件路径与验收动作 |
| 估算口径 | 单工程师人日;日历排期取决于投入人力,不在本文档承诺 |

---

## 1. 范围与交付物总览

### 1.1 交付物清单

| 类别 | 交付物 | 所属阶段 |
|---|---|---|
| 后端代码 | `backend/cmd/server/main.go` 优雅关闭改造 | 阶段 0 |
| 后端代码 | `/health` 响应增加 `version` / `slot` 字段 | 阶段 0 |
| 部署配置 | `deploy/docker-compose.yml` 与 `docker-compose.local.yml` 移除共 6 处 `container_name` | 阶段 0 |
| 部署配置 | `deploy/.env.example` 新变量说明 + `BIND_HOST` 安全注释 | 阶段 0 |
| 分析报告 | 后台任务幂等性排查结论表(阶段 1 准入门槛) | 阶段 0 |
| CI | `.github/workflows/ci.yml` 迁移门禁 job | 阶段 0 |
| 部署工具 | `deploy/blue-green/` 全套:4 份模板 + 1 份 nginx snippet + 6 个脚本 + 清单示例 + 操作手册 | 阶段 1 |
| 部署工具 | `deploy/tests/` 新增脚本测试(沿用现有纯 shell 测试惯例) | 阶段 1 |
| 服务器产物 | `/srv/sub2api/` 目录结构、nginx include 配置、staging stack | 阶段 1 |
| 运维文档 | 生产迁移 Runbook(本文档 §3,执行后归档实际记录) | 阶段 2 |
| CI | staging 自动发布 workflow + 生产 `workflow_dispatch` | 阶段 3 |
| 后端代码(可选) | `/readyz`、in-flight gauge | 阶段 4 |

### 1.2 不在范围

引用 PRD §2.2:多节点高可用、数据库层蓝绿、schema 自动回滚、金丝雀灰度、证书管理、编排器引入均不做。此外本计划不改动 `deploy/Caddyfile`、`apple-container.sh` 等与 Nginx 路线无关的现有部署产物。

---

## 2. 工作分解(WBS)

任务编号 `P<阶段>-<序号>`。「验收」列映射 PRD 的 FR 编号与 UAT 条目。

### 2.1 阶段 0:前置代码改动(合并为 1 个 PR,≈4–5 人日)

| 编号 | 任务 | 涉及文件 | 依赖 | 估算 | 验收 |
|---|---|---|---|---|---|
| P0-1 | 优雅关闭超时可配 | `backend/cmd/server/main.go` | 无 | 0.5d | FR-1 / UAT 13.1 |
| P0-2 | `/health` 增加 version+slot | `backend/internal/server/routes/common.go`、`internal/server/router.go`、`internal/handler/handler.go` | 无 | 0.5d | FR-8.1 / UAT 13.1 |
| P0-3 | 移除 `container_name` | `deploy/docker-compose.yml`、`deploy/docker-compose.local.yml` | 无 | 0.5d | FR-5.7 / UAT 13.1 |
| P0-4 | `.env.example` 补充 | `deploy/.env.example` | P0-1 定名 | 0.25d | FR-1.5 |
| P0-5 | 后台任务幂等性排查 | `backend/internal/` 下 10 处候选(见 §2.1.3) | 无 | 2d | PRD §8.2 / UAT 13.1 末条 |
| P0-6 | CI 迁移门禁 | `.github/workflows/ci.yml` | 无 | 0.5d | PRD §8.1 配套 / UAT 13.4 |

P0-1~P0-4 合入同一 PR;P0-5 产出为文档(附修复任务清单);P0-6 可独立 PR 并行。

#### 2.1.1 P0-1 细则:优雅关闭

改动点(`main.go:182-187`):

1. import 增加 `strconv`(`os`、`time` 已存在);
2. 超时值改为读取环境变量 `SERVER_SHUTDOWN_TIMEOUT_SECONDS`(正整数秒),未设置或非法时默认 **30 秒**(现值 5 秒);
3. 关闭开始时打印 `Draining in-flight requests (timeout %s)...`;
4. `log.Fatalf` → `log.Printf`:关闭超时仅记 warning,进程以 0 码退出。

自测:构建镜像,设 `SERVER_SHUTDOWN_TIMEOUT_SECONDS=120`,发起一条持续 SSE 流后 `docker stop -t 150`,流完整收到结束事件、退出码 0;不设变量时 30 秒后退出。

#### 2.1.2 P0-2 细则:/health 版本字段

复用既有 BuildInfo 注入链(`main.go:146` 构造 `handler.BuildInfo{Version, BuildType}` → wire,参见 `internal/handler/wire.go:167`、`internal/service/wire.go:36`),**不新建 buildinfo 包**:

1. `handler.Handlers` 结构(`internal/handler/handler.go:30`)增加 `BuildInfo` 字段,wire 注入;
2. `registerRoutes`(`internal/server/router.go:96`)已持有 `h *handler.Handlers`,将 version 下传:`RegisterCommonRoutes(r, cfg)` → `RegisterCommonRoutes(r, cfg, h.BuildInfo.Version)`(调用点 `router.go:113`);
3. `routes/common.go:25` 的 `/health` 响应体增加 `"version"` 与 `"slot"`(读 `os.Getenv("APP_SLOT")`,未设置为空串);
4. 同步更新 `internal/server/` 下引用 `/health` 响应结构的测试(`router_embedded_metrics_test.go` 等如有断言)。

#### 2.1.3 P0-5 细则:后台任务幂等性排查

**目的**:蓝绿并存窗口内两个进程同时运行全部后台 runner,不幂等的任务会产生重复副作用。本排查是阶段 1 的准入门槛(PRD §10 阶段 0 准入条件)。

候选清单(进程级 runner;已排除每请求/每连接生命周期的 stream/ping ticker):

| # | 位置 | 任务 |
|---|---|---|
| 1 | `internal/service/subscription_expiry_service.go:77` | 订阅过期处理 |
| 2 | `internal/service/batch_image_cleanup.go:151` | 批量图片清理 |
| 3 | `internal/service/batch_image_worker.go:206` | 批量图片 worker 心跳 |
| 4 | `internal/service/batch_image_public.go:417` | 批量图片公共轮询 |
| 5 | `internal/service/radar_runner.go:662` | radar 指标同步 |
| 6 | `internal/service/upstream_billing_probe.go:275` | 上游账单探测 |
| 7 | `internal/service/deepseek_balance_health_runner.go:67` | DeepSeek 余额健康检查 |
| 8 | `internal/service/minimax_remains_sync_runner.go:67` | MiniMax 余量同步 |
| 9 | `internal/securityaudit/prompt_worker.go:88,262` | prompt 审计 worker |
| 10 | `internal/securityaudit/prompt_config_store.go:392` | prompt 审计配置刷新 |

每项产出一行结论:**任务 / 触发周期 / 写目标(DB 表、Redis key、外部 API) / 双实例并发是否安全 / 处置**(无需处理 | 加 Redis 锁 | 加 PG advisory lock | 改为单实例选主)。结论表归档至 `blue-green-deploy-plan.md` 附录或独立文档;发现的阻塞项各自立任务修复,修复完成才算 P0-5 关闭。

#### 2.1.4 P0-6 细则:CI 迁移门禁

`.github/workflows/ci.yml` 新增 job(PR 触发):

1. `git diff --name-only origin/main...HEAD -- backend/migrations/` 取本次变更的迁移文件;
2. 对变更文件内容匹配 `DROP COLUMN|DROP TABLE|RENAME|ALTER COLUMN .* TYPE`(忽略大小写与 SQL 注释行);
3. 命中且 PR 无 `breaking-migration` 标签 → job 失败,输出 expand-contract 规则说明链接(PRD §8.1);
4. 已应用的历史迁移文件被修改时直接失败(与运行时 SHA256 校验一致,提前到 CI 拦截)。

### 2.2 阶段 1:部署工具与 staging 跑通(≈5–6 人日)

| 编号 | 任务 | 涉及文件/位置 | 依赖 | 估算 | 验收 |
|---|---|---|---|---|---|
| P1-1 | `deploy/blue-green/` 全套工具 | 见 §2.2.1 | P0-1 | 3d | FR-2~FR-7 |
| P1-2 | 服务器引导配置 | 服务器 `/srv/sub2api/`、`/etc/nginx/` | P1-1 | 0.5d | FR-6 |
| P1-3 | staging stack 建立与首次发布 | 服务器 | P1-2、P0 PR 出镜像 | 0.5d | UAT 13.2 首条 |
| P1-4 | 验证矩阵执行 | 服务器 + `deploy/tests/` | P1-3 | 1.5d | UAT 13.2 全部 |

#### 2.2.1 P1-1 文件清单

```
deploy/blue-green/
├── templates/
│   ├── compose.data.yml.tmpl        # postgres+redis,external network(design v1 §5.1)
│   ├── compose.app.yml.tmpl         # app 单服务,slot 参数化(design v1 §5.2)
│   ├── nginx-site.conf.tmpl         # server block,渲染一次不再动(design v1 §5.5)
│   └── nginx-upstream.conf.tmpl     # 部署时唯一被改写的文件(design v1 §5.3)
├── snippets/
│   └── sub2api-proxy.conf           # 静态文件,SSE 透传参数(design v1 §5.4)
├── bin/
│   ├── s2a-render                   # sites.yaml → 产物,含端口/域名/证书校验(FR-5)
│   ├── s2a-init                     # network/目录/data 层初始化
│   ├── s2a-deploy                   # 发布主流程(FR-2/FR-3,design v1 §6.2)
│   ├── s2a-rollback                 # 快速/降级回滚(FR-4,design v1 §6.3)
│   ├── s2a-status                   # 状态查询与不一致检测(FR-7)
│   └── s2a-teardown                 # 排空定时器的回收执行体(systemd-run 调用)
├── sites.example.yaml               # 清单示例(design v1 §4)
└── README.md                        # 操作手册:发布/回滚/新增站点/排障(UAT 13.4)
```

实现约束:

- 脚本统一 `set -euo pipefail`,bash;运行时依赖 `yq`、`envsubst`(gettext)、`curl`、`docker compose` v2、`systemd-run`,在 README 声明并在 `s2a-render` 开头做依赖检查;
- 模板内不得出现 `container_name`(FR-5.7);app 端口只绑定 `127.0.0.1`(FR-2.7);`stop_grace_period` 与 `SERVER_SHUTDOWN_TIMEOUT_SECONDS` 绑定同一变量 `DRAIN_SECONDS`(FR-2.6);
- `s2a-deploy` 失败路径必须满足 FR-3.3/3.4:门禁失败自动回收新 slot、`nginx -t` 失败还原备份,全程不触碰线上;
- `deploy/tests/` 新增 `bluegreen-render-test.sh`(渲染幂等性、端口冲突拦截、域名重复拦截)与 `bluegreen-deploy-dryrun-test.sh`(mock docker/nginx 验证主流程分支),风格沿用 `install-github-token-test.sh`。

#### 2.2.2 P1-2 服务器引导

1. 创建 `/srv/sub2api/{bin,registry/envs,templates,stacks}`,rsync 仓库 `deploy/blue-green/` 内容;
2. `/etc/nginx/nginx.conf`:`http {}` 增加两行 include(`sub2api/upstreams/*.conf`、`sub2api/sites/*.conf`);`main` 上下文增加 `worker_shutdown_timeout 1200s`(FR-6.5);
3. 落盘 `/etc/nginx/snippets/sub2api-proxy.conf`;
4. `nginx -t && nginx -s reload`,确认存量站点无回归。

#### 2.2.3 P1-4 验证矩阵

| 用例 | 操作 | 预期 |
|---|---|---|
| 正常发布 | `s2a-deploy api-staging <tag>` | 两 app 容器并存;postgres `Created` 时间不变;流量切至新 slot |
| 长流不中断 | 发起 1 条长 SSE 流后立即发布 | 流自然结束,无中断(M1=0) |
| 并发压测 | 10 并发 `curl -N` 长流贯穿发布窗口 | 非正常结束数 = 0;nginx log 无 5xx(M2=0) |
| 门禁失败 | 用必然启动失败的配置发布 | 超时自动回收新 slot;upstream 文件未变;线上 200 |
| nginx -t 失败 | 人为构造坏 upstream 模板 | 还原 `.bak`,不执行 reload |
| 快速回滚 | 发布后立即 `s2a-rollback` | < 10s;drain 定时器被取消;流量回旧 slot |
| 降级回滚 | 手动回收旧 slot 后 `s2a-rollback` | 以 prev_tag 重新发布,命令明示耗时差异 |
| 状态一致性 | 手改 STATE 制造不一致 | `s2a-status` 以 nginx 配置为准并告警 |
| 排空回收 | 发布后等待 `DRAIN_SECONDS` | 旧 slot 被 `s2a-teardown` 自动回收 |

**准入阶段 2**:staging 连续 5 次发布,上表全绿。

### 2.3 阶段 2:生产接入与第二站点(≈3 人日)

| 编号 | 任务 | 依赖 | 估算 | 验收 |
|---|---|---|---|---|
| P2-1 | 生产迁移至 stack 结构(§3 Runbook) | 阶段 1 准入达成 | 1.5d(含演练) | UAT 13.3 部分 |
| P2-2 | 第二站点接入 | P2-1 | 0.5d | UAT 13.3 / M6 |
| P2-3 | 备份脚本适配 `stacks/*/postgres_data` 与 `data/` | P2-1 | 0.5d | NFR-2 |

**准入阶段 3**:生产完成 3 次发布,PRD M1–M5 全部达标。

### 2.4 阶段 3:CI 集成(≈1–1.5 人日)

| 编号 | 任务 | 涉及文件 | 估算 |
|---|---|---|---|
| P3-1 | 镜像推送后自动发布 staging | `.github/workflows/docker-ghcr.yml` 后接 job 或新 `deploy-staging.yml`:`ssh <server> 's2a-deploy api-staging <tag>'` | 0.5d |
| P3-2 | 生产手动发布入口 | 新 workflow,`workflow_dispatch` + tag 输入,不自动触发 | 0.25d |
| P3-3 | 发布结果告警推送 | workflow 末步(FR-8.3) | 0.25d |

前置:服务器侧建只读部署专用账号 + 受限 SSH key(command 限定 `s2a-deploy`),key 入 GitHub Secrets。

### 2.5 阶段 4:可观测性补强(可选,≈2–3 人日)

| 编号 | 任务 | 说明 |
|---|---|---|
| P4-1 | `/readyz` | DB `PingContext` + Redis `Ping`,2s 超时;nginx 层 `deny all`(FR-9) |
| P4-2 | in-flight gauge + 探测式排空 | 应用暴露 `sub2api_http_requests_in_flight`;`s2a-teardown` 改轮询归零即回收,`DRAIN_SECONDS` 降级为上限(PRD §12.1) |

---

## 3. 生产迁移 Runbook(P2-1)

**目标**:把现有生产单容器部署平移为 `/srv/sub2api/stacks/api-prod` 的 stack 结构。需一次停机窗口(预估 10–20 分钟);先在 staging 全流程演练一遍。

### 3.1 预检(窗口外完成)

1. `docker inspect sub2api --format '{{json .Mounts}}'` 判断现状:
   - **分支 A**:named volume(`sub2api_data`/`postgres_data`/`redis_data`,对应 `docker-compose.yml`);
   - **分支 B**:bind mount(`./data` 等,对应 `docker-compose.local.yml`);
2. 记录当前镜像 tag、`.env` 全量内容、数据量(`du -sh`)以估算拷贝时长;
3. `sites.yaml` 预登记 api-prod 记录,`s2a-render` 预渲染产物(不切流量);
4. 确认磁盘余量 ≥ 数据量 2 倍(备份 + 新目录)。

### 3.2 窗口内步骤(每步附回退动作)

| # | 操作 | 回退动作 |
|---|---|---|
| 1 | 发布停机公告;nginx 挂维护页(可选) | — |
| 2 | 双备份:`pg_dump -Fc` + 旧 data 目录/volume tar | — |
| 3 | `docker compose down`(旧栈,**不带 `-v`**) | `docker compose up -d` 原样恢复 |
| 4 | 数据平移:分支 A 用临时容器把 volume 内容拷至 `stacks/api-prod/{data,postgres_data,redis_data}`;分支 B 直接 `mv` | 目录移回/删除拷贝,回退到步骤 3 |
| 5 | 密钥迁移:旧 `.env` 值填入 `registry/envs/api-prod.env`(chmod 600)——`JWT_SECRET`/`TOTP_ENCRYPTION_KEY`/`POSTGRES_PASSWORD` 必须原值平移,否则会话与 2FA 全部失效 | — |
| 6 | `s2a-init api-prod`(network + data 层拉起),`pg_isready` 确认 | down data 层,回退步骤 4 |
| 7 | `s2a-deploy api-prod <当前生产 tag>`(首次发布,同版本) | down app,回退步骤 6 |
| 8 | 内网验证:`curl 127.0.0.1:<port>/health` 返回 version 一致;抽查登录/网关转发 | 同上 |
| 9 | nginx 切换:启用新 site/upstream 文件,禁用旧 server block,`nginx -t && reload` | 还原旧 server block reload,服务回旧结构(旧栈可随时 `up -d`) |
| 10 | 公网全链路验证(含一条真实流式请求);观察 30 分钟错误率 | 步骤 9 回退 |
| 11 | 归档:旧 compose 目录保留 ≥ 2 周不删;记录实际执行偏差回填本 Runbook | — |

---

## 4. 里程碑与关键路径

```
M0 阶段0 PR 合并 + P0-5 结论归档(阻塞项清零)   ← 阶段 1 准入
 └─→ M1 staging 连续 5 次发布 M1=0 / M2=0      ← 阶段 2 准入
      └─→ M2 生产 3 次发布 M1–M5 达标          ← 阶段 3 准入
           └─→ M3 CI 自动发布 staging 上线
                └─→ (可选) M4 阶段 4 交付
```

- **关键路径**:P0-1 → P1-1 → P1-3 → P1-4 → P2-1。P0-5 与 P0-1 并行但同为 M0 门槛;P0-6、P2-3、P3-x 均可并行穿插;
- 核心工作量(阶段 0–3)合计 **≈13–16 人日**;阶段 4 另计 2–3 人日;
- 无外部团队依赖;唯一外部条件是服务器 root/nginx 权限与 GitHub Secrets 管理权限。

---

## 5. 测试与验证计划

| 层级 | 手段 | 覆盖 |
|---|---|---|
| Go 单测 | 现有测试套 + P0-2 触碰的 `/health` 断言更新 | P0-1/P0-2 不破坏现有行为 |
| 脚本测试 | `deploy/tests/bluegreen-*.sh`(mock docker/nginx) | render 校验、deploy 失败分支、rollback 分支 |
| 单容器验证 | 本地 `docker stop` + SSE 流(P0-1 自测步骤) | FR-1 排空语义 |
| staging 集成 | §2.2.3 验证矩阵 9 用例 | FR-2~FR-7 全量 + M1/M2 指标 |
| SSE 压测 | 10 并发 `curl -N --no-buffer` 长流脚本,统计以 `data:` 终止事件正常收尾的比例;贯穿发布窗口执行 | M1(中断数=0) |
| nginx 日志断言 | 发布窗口切片 grep `" 5\d\d "` | M2(5xx=0) |
| 生产演练 | Runbook 先在 staging 全流程走一遍(含分支 A/B 各一次) | P2-1 风险消减 |
| CI 门禁自测 | 构造含 `DROP COLUMN` 的假迁移 PR,验证拦截与标签放行 | P0-6 |

---

## 6. 执行期风险

| # | 风险 | 缓解 |
|---|---|---|
| E1 | P0-5 排查发现多个不幂等任务,修复量冲击排期 | 排查放阶段 0 最先做;修复可与 P1-1 并行;实在无法及时修复的任务采用「发布后立即回收旧 slot」的保守排空参数过渡 |
| E2 | 生产迁移数据平移失误(volume→bind 拷贝遗漏 PGDATA 层级) | Runbook 步骤 4 明确校验:平移后 `ls postgres_data/` 必须直接可见 `PG_VERSION`;双备份兜底 |
| E3 | 密钥平移遗漏导致全员登出/2FA 失效 | Runbook 步骤 5 单列;窗口内步骤 8 验证登录 |
| E4 | CI SSH 私钥泄露风险 | 部署专号 + authorized_keys `command=` 限定 + IP 白名单 |
| E5 | staging 与生产 nginx 版本/模块差异导致配置不可移植 | P1-2 时核对两端 `nginx -V`;snippet 避免非标模块指令 |
| E6 | 排空定时器依赖 systemd,非 systemd 环境不可用 | `s2a-deploy` 检测 `systemd-run` 缺失时降级为同步 sleep 后台子进程,README 注明 |
| E7 | 迁移含 `CREATE INDEX CONCURRENTLY` 与事务包裹冲突(PRD §8.1) | P0-6 门禁同时匹配 `CONCURRENTLY`,提示需走迁移 runner 的非事务路径评估 |

PRD §14 的 R1–R9 长期风险仍然有效,此处不重复。

---

## 7. 验收对照表

| 任务 | PRD FR | UAT |
|---|---|---|
| P0-1 | FR-1.1~1.4 | 13.1 前三条 |
| P0-2 | FR-8.1、FR-3.5 | 13.1 第四条 |
| P0-3 | FR-5.7 | 13.1 第五条 |
| P0-4 | FR-1.5 | 13.1 |
| P0-5 | §8.2 前置动作 | 13.1 末条 |
| P0-6 | §8.1 配套要求 | 13.4 首条 |
| P1-1 | FR-2、FR-3、FR-4、FR-5、FR-6、FR-7 | 13.2 |
| P1-2 | FR-6.1~6.6 | 13.2 |
| P1-4 | M1、M2、M5 | 13.2 全部 |
| P2-1 | NFR-2、NFR-9 | 13.3 部分 |
| P2-2 | FR-5、M6、M7 | 13.3 |
| P3-1~P3-3 | FR-8.3 | — |
| P4-1 | FR-9 | — |
| P4-2 | §12.1 | — |

---

## 附:阶段 0 PR 的提交拆分建议

1. commit 1:P0-1 main.go 改造(含自测说明);
2. commit 2:P0-2 /health 字段 + 受影响测试更新;
3. commit 3:P0-3 compose 清理;
4. commit 4:P0-4 `.env.example` 文案;
P0-6 独立 PR;P0-5 结论表以文档 PR 或 issue 归档,不进代码 PR。

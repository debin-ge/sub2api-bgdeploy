# 蓝绿部署后台任务幂等性排查结论（P0-5）

| 项 | 内容 |
|---|---|
| 目的 | 蓝绿并存窗口（最长 960s）内两个应用进程共享同一 PostgreSQL 与 Redis，所有进程级后台任务双份运行；逐一判定双实例并发是否安全（PRD §8.2，阶段 1 准入门槛） |
| 基线 | main @ edb8c64b3（行号以该基线为准） |
| 日期 | 2026-07-22 |
| 方法 | 只读代码走查（service → repository → SQL/Redis 命令），结论均有 file:line 证据支撑 |

## 总体结论

**无阻塞蓝绿发布的硬伤。** 全部进程级后台循环（约 45 处，远多于计划文档预估的
10 处）中：

- 绝大多数**安全**：或有跨实例保护（Redis SETNX leader 锁、PG advisory lock、
  `FOR UPDATE SKIP LOCKED`、CAS 条件更新、计费 dedup 表），或写入天然幂等
  （重算式 UPSERT、条件 DELETE、进程本地状态）；
- **2 项每次发布必触发的问题已随本次改动修复**（见 §3）；
- **3 项遗留修复任务**（见 §4），并存窗口内后果有界且自愈/可运营规避，不阻塞
  阶段 1（staging），建议在阶段 2（生产接入）前完成；
- 若干「有条件安全」项需发布负责人知情（见 §5）。

**前提**：以上结论基于当前基线代码。蓝绿窗口中的「旧版本」若构建自更早的
commit（早于相应保护机制引入），需按旧版本实际代码复核。

## 1. 范围与计划清单勘误

计划文档 §2.1.3 列出 10 个候选。完整性扫描（`NewTicker`/`time.Tick`/`AfterFunc`
自重排/cron/常驻 goroutine 循环，排除请求级与一次性任务）发现实际进程级循环约
45 处。对原清单的两处勘误：

- **#4 `batch_image_public.go:417`** 不是进程级任务：该 ticker 是 `Submit()` 请求
  期间的心跳，随请求结束（batchID 全局唯一，双实例天然分区）；
- **#3 `batch_image_worker.go:206`** 是 per-job 心跳；真正的进程级循环是
  `batch_image_worker.go` 的 `Run`/`RunDelayedMover`/`RunStaleActiveRecovery` 与
  `batch_image_worker_runtime.go` 的 `runBillingRecovery`（已一并审计，见 §2.4）。

## 2. 分组结论表

结论列：✅ 安全 | ⚠️ 有条件安全（见 §5 说明）| ❌ 需处理（见 §3/§4）。

### 2.1 计划原 10 项

| 任务 | 周期 | 写目标 | 现有保护 | 双实例最坏后果 | 结论 |
|---|---|---|---|---|---|
| 订阅过期+到期提醒 subscription_expiry_service | 1min | user_subscriptions、SMTP | leader 锁（fail-closed）+ 条件 UPDATE + 送达幂等键 | 无 | ✅ |
| 批量图片清理 batch_image_cleanup | 30min | batch_image_jobs/events、Gemini/Vertex 删文件 API | 无锁；幂等条件 UPDATE + 终态守卫 + 404 容忍 | 重复外部删除调用与审计噪声，无损坏 | ⚠️ |
| 批量图片 worker（含心跳） | 常驻 | Redis 队列、PG 状态机、计费表 | 原子 Lua 队列 + token 锁 + FOR UPDATE 状态机 + usage_billing_dedup | 极端下重复只读轮询，无重复扣费 | ✅ |
| Submit 心跳（请求级，勘误项） | 200s/请求 | batch_image_jobs.updated_at | 唯一 batchID 分区 + 条件写 | 无 | ✅ |
| radar 指标同步 radar_runner | 30s | 进程内 Prometheus、Redis 收敛型维护写 | 实例隔离；邻居 fetcher/聚合各有 SETNX 锁 | Redis 采样负载 ×2 | ✅ |
| 上游账单探测 upstream_billing_probe | 1min cycle | accounts.extra、只读远端 GET | leader 锁 + 分钟 cadence 锁 + DB CAS | 极端下多一次只读 GET | ✅ |
| DeepSeek 余额检查 | 300s | 只读 GET、accounts.extra jsonb merge | jsonb merge 原子 | 双份只读外呼、观测字段一轮抖动 | ✅ |
| MiniMax 余量同步 | 300s | 只读 GET、Redis ZSET 配额校准、accounts.extra | 原子 Lua、幂等收敛、不删真实成员 | 校准旧值覆盖新值→配额闸门放宽 ≤1 周期 | ⚠️ |
| prompt 审计 worker | 500ms/1min | prompt_audit_jobs/events、scanner 外呼 | SKIP LOCKED + claim_version CAS 全链路 | 吞吐×2；event 唯一 | ✅ |
| prompt 审计配置刷新 | 5s | 无共享写（只读→进程内快照） | 不需要 | 无 | ✅ |

### 2.2 ops / 审计 / cron 族（完整性扫描新增）

| 任务 | 周期 | 写目标 | 现有保护 | 双实例最坏后果 | 结论 |
|---|---|---|---|---|---|
| ops 指标采集 | 60s | ops_system_metrics INSERT | 逐轮 SETNX 锁（不跨周期）；表无唯一约束 | 错峰时同分钟冗余行（读取方取最新行，不翻倍） | ⚠️ |
| ops 小时/日聚合 | 10min/1h | ops_metrics_hourly/daily | SETNX 锁 + **全量重算 UPSERT 整行替换（非累加）** | 重复重算，结果一致 | ✅ |
| 告警规则评估 | 60s | ops_alert_events、告警邮件 | SETNX 锁（默认开）+ active-event 查重 + cooldown | sustained 计数在内存→窗口内 sustained 告警可能延迟/漏报（非重复） | ⚠️ |
| ops 数据保留清理 | cron 0 2 * * * | 8 张 ops 表 DELETE | leader 锁属实 + 删除幂等 | 纯浪费 | ✅ |
| 系统日志落库 | 1s | ops_system_logs 追加 | 本进程自产数据 | 各写各的（预期） | ✅ |
| 定时报表派发 | 1min tick | 报表邮件、Redis last_run | SETNX 锁（fail-closed）+ 发送前写 last_run 标记 | Redis 健康不重发；故障时漏发而非重发 | ⚠️ |
| ingress-reject 聚合 | 5s | ops_ingress_reject_aggregates | 本地增量 + 累加式 ON CONFLICT | 无（多实例聚合正确语义） | ✅ |
| ops 运行时设置刷新 | 30s | 无（只读） | 不需要 | 无 | ✅ |
| 审计日志 flush+保留 | 1s / 24h | audit_logs INSERT/DELETE | flush 自产数据；清理幂等 | 纯浪费 | ✅ |
| **渠道监控 channel_monitor_runner** | 每 monitor interval | **外部渠道真实探测**、channel_monitor_history | **无跨实例保护**（inFlight 仅进程内） | 每 monitor 探测 ×2（消耗被测渠道配额/费用）、样本密度 ×2 | ❌ §4.2 |
| **定时账号测试 scheduled_test_runner** | cron 每分钟+10s | **上游真实测试请求**、scheduled_test_results | **无**（ListDue 裸 SELECT、无 CAS、定时精确对齐） | 到期 plan 执行 ×2（上游配额/费用 ×2） | ❌ §4.1 |
| **定时备份 backup_service** | cron（设置） | pg_dump→S3、settings 台账 JSON | 仅进程内互斥 | 双份 dump；同秒同 key 后写覆盖；台账 RMW 丢更新；启动误杀对端在途备份记录 | ❌ §3.2/§4.3 |

### 2.3 过期清理 / 外部同步族（完整性扫描新增）

| 任务 | 周期 | 写目标 | 现有保护 | 双实例最坏后果 | 结论 |
|---|---|---|---|---|---|
| 账号过期扫描 | 1min | accounts、scheduler_outbox | 条件 UPDATE 幂等 | 重复 outbox（消费端幂等重建） | ✅ |
| 代理过期扫描 | 1min | proxies、accounts 改投 | 改投守卫 `fallback_origin_id IS NULL` | 改投只生效一次 | ✅ |
| 支付订单过期 | 60s | payment_orders 状态机、上游查单 | leader 锁属实 + 全链路 CAS + 履约租约 | 极端双跑仅重复查单，状态机不破坏 | ✅ |
| 幂等记录清理 | 60s | idempotency_records DELETE | DELETE 幂等 | 短暂行锁等待 | ✅ |
| usage 日志清理 | 10s | usage_cleanup_tasks、usage_logs | 领取 FOR UPDATE SKIP LOCKED + 状态机 | 各领不同任务（by design） | ✅ |
| 一次性邮箱刷新 | 24h | Redis 域名 set、GitHub 下载 | SETNX 刷新锁 | 极端双下载同源数据 | ✅ |
| OAuth token 刷新 | 5min | accounts.credentials、上游 OAuth | per-账号 SETNX 锁 + 锁内重读 + 二次过期检查 + invalid_grant 竞争恢复 + Grok CAS | Redis 健康时闭环；Redis 故障降级无锁，轮换型 token 极端时序可被误标 error | ⚠️ |
| 模型目录刷新 | 300s | 仅进程内缓存、上游 models 列表 | singleflight | 上游调用 ×2 | ✅ |
| 定价远程同步 | 10min | **data_dir 本地文件（蓝绿共享 /app/data）**、GitHub | 无；os.WriteFile 非原子 | 并发写同一文件→瞬时半写→解析失败回退 fallback 自愈（内容同源一致） | ⚠️ |
| auth 缓存失效 outbox | 500ms | Redis DEL、outbox 表 | SKIP LOCKED + claimed_by owner 校验 | 多 worker 原生设计 | ✅ |
| 调度器快照（双 ticker） | 1s / 300s | Redis 快照桶、scheduler_outbox | epoch fencing + 分组租约 + 清理 advisory lock | 事件重复消费/重建 ×2，无数据破坏 | ✅ |
| dashboard 聚合 | 60s + 启动重算 | usage_dashboard_* 聚合表 | 定时路径 leader 锁；**启动重算无锁**；写为重算式 UPSERT | 并发桶重算 last-writer-wins，下轮收敛；偶发死锁自动重试 | ⚠️ |
| last_used 延迟落库 | 10s | accounts.last_used_at | 各实例只 flush 本地流量 | 时间戳回拨 ≤10s，调度偏好噪声自愈 | ✅ |

### 2.4 进程本地 / 队列 / 事件驱动族（完整性扫描新增）

| 任务 | 周期 | 写目标 | 现有保护 | 双实例最坏后果 | 结论 |
|---|---|---|---|---|---|
| OAuth 会话内存 GC ×5（oauth/openai/geminicli/xai/antigravity） | 5min | 进程内 map | 进程私有 | 无 | ✅ |
| OpenAI WS 池 ping+清理 | 30s×2 | 本进程 WS 连接 | 连接进程私有 | 无 | ✅ |
| 用量记录池自动扩缩 | 3s | 本地 pond.Pool | 纯本地 | 无 | ✅ |
| timing wheel 基础设施 | — | 进程内槽位 | 内存实现 | 无（承载任务已各自审计） | ✅ |
| 并发槽位周期清理 | 30s | Redis ZSET | score 幂等裁剪 + Redis TIME 统一时钟 + 索引自愈 | 索引项短暂误删后自愈 | ✅（**启动清理另有问题，已修复** §3.1） |
| 用户消息队列孤儿锁回收 | 可配 | Redis 锁+索引 | Lua 原子 PTTL 复核，仅删异常锁 | 重复 reconcile（幂等） | ✅ |
| 邮件发送 worker 池 | 事件驱动 | SMTP（队列为本地 chan） | 请求单实例入队，不重发 | 实例下线时最多丢 100 封缓冲邮件（改进项，非蓝绿特有） | ✅ |
| 计费缓存写 worker | 事件驱动 | Redis（Lua 原子增量） | 增量语义 + 关停排空 | 无 | ✅ |
| 内容审核 worker 池 | 事件驱动 | DB 审核日志（本地 chan） | 队列进程私有 | 不重复审核 | ✅ |
| 认证/订阅缓存失效 pub/sub ×2 | 长连接 | 仅本地 L1 缓存 | 广播语义即多实例设计 | 无（订阅侧无重连为既有单实例问题） | ✅ |
| 批图 Run/DelayedMover/StaleRecovery + 计费恢复 | 常驻/5s/5min | Redis 队列、PG、退款 | 原子 Lua + token 锁 + CAS + usage_billing_dedup | 崩溃 job 最迟 10min 重投（延迟非重复），不重复退款 | ✅ |

## 3. 已随本次改动修复（每次发布必触发的两项）

### 3.1 并发槽位启动清理误清对端实例

`ProvideConcurrencyService` 启动时调用 `CleanupStaleProcessSlots`
（`internal/repository/concurrency_cache.go`），按「进程随机前缀」清空**所有非本
进程**的槽位成员——蓝绿窗口内新实例启动即清空旧实例正在使用的账号/用户并发
槽位，窗口内并发限制事实失效（可短时超限，960s 后收敛，无资金/数据影响）。

**修复**：新增配置 `gateway.scheduling.startup_slot_cleanup_disabled`（env
`GATEWAY_SCHEDULING_STARTUP_SLOT_CLEANUP_DISABLED`，默认 false 保持原行为），
蓝绿 compose 模板（`templates/compose.app.yml.tmpl`）置 true。
跳过后，崩溃残留槽位仍由周期清理（30s）与 key TTL 收敛。

### 3.2 备份服务启动恢复误杀对端在途备份记录

`BackupService.recoverStaleRecords` 启动时把**所有** `running` 状态的备份记录
无条件标记为 failed——每次发布新实例启动都会触发，若此刻旧实例恰有备份在跑
（定时备份常配凌晨、发布也常在凌晨），其记录被误标失败。

**修复**：`running` 记录仅在 `StartedAt` 距今超过 35 分钟（备份操作自身超时
30 分钟 + 余量）或时间戳缺失/损坏时才回收；窗口内的 running 记录跳过。
已补单元测试 `TestRecoverStaleRecordsKeepsRecentRunning`。

**未修的关联限制**：restore 记录无独立开始时间戳，`RestoreStatus=running` 仍会
被启动恢复无条件标记失败——发布窗口内请勿执行数据库恢复操作（本就不应同时
进行）。

## 4. 遗留修复任务（建议阶段 2 生产接入前完成）

### 4.1 定时账号测试双跑（优先级最高）

`scheduled_test_runner_service.go`：cron 每分钟 + 固定 sleep 10s 使两实例精确
对齐；`ListDue` 为裸 SELECT、`UpdateAfterRun` 无 CAS。窗口内每个到期测试计划
被执行两次（真实上游请求，配额/费用 ×2）。

**建议修复**：`ListDue` 改 `SELECT ... FOR UPDATE SKIP LOCKED` 并在同事务内先
推进 `next_run_at`（行级抢占，天然多实例安全）；或按 ops 模式加 Redis SETNX
leader 锁包住 `runScheduled`。
**过渡规避**：窗口仅 ~16 分钟，接受到期 plan 双测；或发布前临时停用测试计划。

### 4.2 渠道监控探测翻倍

`channel_monitor_runner.go`：每个启用的 monitor 一条 goroutine，无跨实例去重。
窗口内每个 monitor 探测请求 ×2（消耗被测渠道配额/费用），history 样本密度 ×2
（成功率等比例指标不失真）。该路径无告警外发，不会重复告警。

**建议修复**：per-monitor 期次锁（Redis `SET NX cm:probe:<id>:<期次>`,
TTL≈interval）。
**过渡规避**：书面接受 16 分钟内探测翻倍。

### 4.3 备份服务跨实例互斥与台账

§3.2 只修复了启动误杀。仍遗留：备份 cron 落在窗口内时双份 pg_dump（同秒时
S3 同 key 后写覆盖先写）；`backup_records` 台账为 settings 单 key JSON 的
读-改-写，跨实例并发可丢更新。

**建议修复**：Redis SETNX（TTL ≥ 35min）包住 `runScheduledBackup`/
`CreateBackup`；台账 RMW 纳入同一锁；长期将台账迁为 DB 行级表。
**过渡规避**：发布窗口避开备份 cron 时段（`./bgdeploy status` 前置检查亦可人工确认）。

## 5. 知情项（有条件安全，无需改动）

| 项 | 说明 |
|---|---|
| MiniMax 配额校准 TOCTOU | 旧值覆盖新值使配额闸门放宽 ≤1 同步周期（秒级窗口内真实用量），上游硬限额兜底，自愈 |
| 批量图片清理 | 重复外部删除调用（404 容忍）与 batch_image_events 重复审计行；可选按 leader 锁模式补锁 |
| ops 指标采集 | 错峰双跑产生同分钟冗余行；读取方取最新行不受影响；根治需唯一索引 + DO NOTHING |
| 告警 sustained 计数 | 连续违例计数在进程内存，窗口内 leader 交替会使 sustained 告警延迟/漏报（方向是漏报非重复）；建议后续计数落 Redis |
| 定时报表 | 去重依赖共享 Redis 可用（fail-closed：故障时漏发而非重发） |
| OAuth token 刷新 | 四层机制在共享 Redis 前提下闭环；Redis 故障时降级无锁，轮换型 refresh_token 极端时序可被误标 error（可手动恢复）。可选加固：锁释放 owner 校验、锁报错改跳过 |
| 定价数据 data_dir | 蓝绿两 slot 共享 /app/data，并发写 pricing JSON 非原子→瞬时半写自愈（内容同源）。可选：改临时文件+rename 原子写。此为 §8.2「数据目录写竞争」专项确认的唯一运行时写点 |
| dashboard 聚合 | 新实例启动重算（默认 2 天）不走 leader 锁，与旧实例定时聚合并发→重算式 UPSERT 收敛，偶发死锁自动重试；可选发布时置 recompute_days=0 |
| prompt 审计 lease | reclaimer 以本进程时钟判定 90s lease——同机蓝绿无碍；未来跨机部署需 NTP |
| 进程内并发闸 | `GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS` 为进程内信号量，窗口内实际并发 ×2（PRD §8.2 已知，短期按 ceil(N/2) 配置） |

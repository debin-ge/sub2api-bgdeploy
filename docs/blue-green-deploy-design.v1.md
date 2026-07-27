# Sub2API 多站点蓝绿部署方案（Nginx + Docker）

> 目标：在单台（或少数几台）VPS 上，用 Nginx + 双容器实现零停机蓝绿发布，同时支持**多站点 × 多环境**，且配置文件数量不随站点数线性膨胀。

---

## 0. 结论先行

| 维度 | 决策 |
|---|---|
| 流量切换 | Nginx `upstream` 单行改写 + `nginx -s reload` |
| 蓝绿粒度 | **只对 app 容器做蓝绿**，Postgres/Redis 常驻不参与 |
| 多站点复用 | 一份 `sites.yaml` 清单 + 4 份模板 → 渲染出全部产物 |
| 排空策略 | 定时排空（默认 960s），后台异步执行，部署命令立即返回 |
| 回滚 | 改回 upstream + reload，秒级；**只回滚代码，不回滚 schema** |
| 前置代码改动 | 3 项（详见 §7），其中「优雅关闭超时」是硬阻塞项 |

---

## 1. 现状核实（基于代码，不是假设）

### 1.1 阻塞项：优雅关闭只有 5 秒，且超时会硬退

`backend/cmd/server/main.go:182`：

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := app.Server.Shutdown(ctx); err != nil {
    log.Fatalf("Server forced to shutdown: %v", err)   // ← 第 186 行
}
```

而这是个 AI 网关，`GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT` 默认 **900 秒**，单条 SSE 流可以挂 15 分钟。

后果：切完流量后，旧容器收到 SIGTERM，5 秒后 `log.Fatalf` 直接终止进程，**所有在途流被拦腰砍断**。再叠加 compose 未设置 `stop_grace_period`（Docker 默认 10s 后 SIGKILL），旧容器最多活 10 秒。

**蓝绿相对滚动更新的核心价值就是「旧实例可以慢慢排空」，这 5 秒把这个价值完全抵消了。不改这一项，做出来的是「有损蓝绿」。**

### 1.2 好消息：迁移每次启动都跑，且已有分布式锁

`backend/internal/repository/ent.go:72` 在建 Ent client 时调用 `applyMigrationsFS`：

- 走 PostgreSQL Advisory Lock，多实例并发启动天然串行化
- `schema_migrations` 表 + SHA256 校验和，已应用的迁移被改会报错
- 超时 10 分钟

所以蓝绿期间「绿容器跑迁移 / 蓝容器还在服务」不会撞车。**但这恰恰强制要求 expand-contract 迁移纪律**（详见 §8）——绿容器把 schema 升到 N，蓝容器还是 N-1 的代码，此刻两者读写同一个库。

### 1.3 `/health` 可以当健康门禁，但偏弱

`backend/internal/server/routes/common.go:25`：

```go
r.GET("/health", func(c *gin.Context) {
    c.JSON(http.StatusOK, gin.H{"status": "ok"})
})
```

静态返回，不探 DB/Redis。但它注册在主路由上，而主路由要等 `NewEntClient` 完成（= 迁移成功 + 配置校验通过）才会起来。

**所以 `/health` 返回 200 ≈ 「迁移跑完了、配置合法、端口在听」**——作为蓝绿门禁够用。但它无法区分「新镜像」和「上一次没删干净的旧容器」，建议在响应里加上 version（§7.3）。

### 1.4 compose 里挡多站点的具体位置

`deploy/docker-compose.yml` 三处硬编码容器名：

```yaml
container_name: sub2api            # :20
container_name: sub2api-postgres
container_name: sub2api-redis
```

多站点会直接撞名。好消息是 volume / network 不用改——设了 `COMPOSE_PROJECT_NAME` 后 compose 自动加项目名前缀。

`deploy/docker-compose.local.yml` 用的是 bind mount（`./data`、`./postgres_data`、`./redis_data`），**多站点场景应以 local.yml 为模板基线**，目录隔离比 named volume 更好备份、好排查。

### 1.5 安全问题：BIND_HOST 默认对公网暴露

`deploy/.env.example:18` 是 `BIND_HOST=0.0.0.0`。Nginx 在宿主机上时，应用端口不需要也不应该对外暴露——否则客户端可以绕过 Nginx 的 TLS / 限流 / 头部清洗直连 8080。多站点场景下端口更多，风险更大。

**方案中统一强制 `BIND_HOST=127.0.0.1`。**

---

## 2. 目标架构

```
                    公网 :443
                        │
                 ┌──────▼───────┐
                 │  Nginx (宿主) │  TLS 终止 / 多域名 / SSE 透传
                 └──────┬───────┘
        ┌───────────────┼───────────────┐
        │ upstream      │ upstream      │ upstream
        │ api-prod      │ api-staging   │ site2-prod
        ▼               ▼               ▼
  127.0.0.1:18080  127.0.0.1:18090  127.0.0.1:18100
        │
   ┌────┴─────────────────────────────┐
   │  stack: api-prod                 │
   │  ┌────────────┐  ┌────────────┐  │  ← 蓝绿只在这层
   │  │ app-blue   │  │ app-green  │  │
   │  │ :18080     │  │ :18081     │  │
   │  └─────┬──────┘  └─────┬──────┘  │
   │        └───────┬───────┘         │
   │        ┌───────▼────────┐        │  ← 常驻，不参与蓝绿
   │        │ postgres │ redis│        │
   │        └────────────────┘        │
   │        共享 bind mount: ./data    │
   └──────────────────────────────────┘
```

**关键设计点：**

1. **data 层与 app 层拆成两个 compose project**，通过一个 external network 连接。部署只动 app project，数据库不重启。
2. **蓝绿两个 slot 是两个 compose project**（`<slug>-blue` / `<slug>-green`），共享同一份 `./data` bind mount 和同一个 network。
3. `./data` 里有 `config.yaml` 和 `.installed`——蓝绿两个 slot **必须共享**，否则绿容器会以为是全新安装。共享后 `NeedsSetup()` 返回 false，绿容器直接进主流程（迁移仍会在 ent client 初始化时执行）。

---

## 3. 服务器目录布局

```
/srv/sub2api/
├── registry/
│   ├── sites.yaml                    # ★ 唯一真相源，新增站点只改这里
│   └── envs/
│       ├── api-prod.env              # chmod 600，密钥不进 git
│       ├── api-staging.env
│       └── site2-prod.env
├── templates/                        # 从 repo deploy/blue-green/templates rsync
│   ├── compose.data.yml.tmpl
│   ├── compose.app.yml.tmpl
│   ├── nginx-site.conf.tmpl
│   └── nginx-upstream.conf.tmpl
├── bin/
│   ├── s2a-render                    # sites.yaml → 渲染全部产物
│   ├── s2a-init                      # 首次创建 stack（network/目录/data 层）
│   ├── s2a-deploy                    # 蓝绿发布
│   ├── s2a-rollback                  # 秒级回滚
│   └── s2a-status                    # 查看所有 stack 当前 slot / 健康
└── stacks/
    ├── api-prod/
    │   ├── compose.data.yml          # 渲染产物
    │   ├── compose.app.yml           # 渲染产物
    │   ├── .env -> /srv/sub2api/registry/envs/api-prod.env
    │   ├── STATE                     # 内容形如: slot=blue tag=v1.4.2 at=2026-07-21T10:00:00Z
    │   ├── data/                     # /app/data（config.yaml + .installed），蓝绿共享
    │   ├── postgres_data/
    │   └── redis_data/
    └── api-staging/
        └── ...
```

Nginx 侧：

```
/etc/nginx/
├── nginx.conf                        # http {} 内加一行 include
├── snippets/
│   └── sub2api-proxy.conf            # ★ 公共 proxy 参数，手写一次
└── sub2api/
    ├── upstreams/
    │   ├── api-prod.conf             # ★ 部署时唯一被改写的文件（1 行）
    │   └── api-staging.conf
    └── sites/
        ├── api-prod.conf             # 渲染一次，之后不动
        └── api-staging.conf
```

`nginx.conf` 的 `http {}` 块里加：

```nginx
include /etc/nginx/sub2api/upstreams/*.conf;
include /etc/nginx/sub2api/sites/*.conf;

# 旧 worker 保留在途 SSE 连接，但不无限期堆积
worker_shutdown_timeout 1200s;
```

> `worker_shutdown_timeout` 需放在 `main` 上下文（`http {}` 外）。nginx 默认不设置 = 旧 worker 永不强制退出，长期频繁 reload 会堆积 worker 进程；显式设成 1200s（> 900s 流上限 + 余量）。

---

## 4. 站点清单 `sites.yaml`

```yaml
defaults:
  image_repo: weishaw/sub2api
  bind_host: 127.0.0.1
  drain_seconds: 960              # 900(流上限) + 60(余量)
  health_timeout_seconds: 300     # 含迁移时间
  health_interval_seconds: 3
  tz: Asia/Shanghai

stacks:
  - slug: api-prod
    domain: api.sub2api.com
    port_base: 18080              # blue=18080, green=18081
    image_tag: v1.4.2
    tls:
      cert: /etc/letsencrypt/live/api.sub2api.com/fullchain.pem
      key:  /etc/letsencrypt/live/api.sub2api.com/privkey.pem
    nginx:
      client_max_body_size: 32m
      proxy_read_timeout: 960s

  - slug: api-staging
    domain: staging.sub2api.com
    port_base: 18090
    image_tag: main
    drain_seconds: 60             # 覆盖 defaults，staging 不需要长排空
    tls:
      cert: /etc/letsencrypt/live/staging.sub2api.com/fullchain.pem
      key:  /etc/letsencrypt/live/staging.sub2api.com/privkey.pem

  - slug: site2-prod
    domain: api.example.net
    port_base: 18100
    image_tag: v1.4.2
    tls: { cert: ..., key: ... }
```

**端口分配规则**：每个 stack 占 10 个端口的块（`port_base` ~ `port_base+9`），app blue/green 用 `+0`/`+1`，其余预留给调试用的 pg/redis 端口暴露。`s2a-render` 启动时校验所有 `port_base` 块无重叠，撞了直接报错退出。

密钥（`POSTGRES_PASSWORD` / `JWT_SECRET` / `TOTP_ENCRYPTION_KEY` / OAuth secret 等）**不进 sites.yaml**，仍在各自的 `envs/<slug>.env` 里，沿用现有 `.env.example` 的全部 485 行结构，`chmod 600`。

---

## 5. 模板

### 5.1 `compose.data.yml.tmpl`（每 stack 起一次，之后不动）

```yaml
# 渲染自 templates/compose.data.yml.tmpl —— 请勿手工编辑
name: ${SLUG}-data

services:
  postgres:
    image: postgres:18-alpine
    restart: unless-stopped
    ulimits:
      nofile: { soft: 100000, hard: 100000 }
    command: >
      postgres
      -c max_connections=${POSTGRES_MAX_CONNECTIONS:-100}
      -c shared_buffers=${POSTGRES_SHARED_BUFFERS:-128MB}
      -c effective_cache_size=${POSTGRES_EFFECTIVE_CACHE_SIZE:-4GB}
      -c maintenance_work_mem=${POSTGRES_MAINTENANCE_WORK_MEM:-64MB}
    volumes:
      - ./postgres_data:/var/lib/postgresql/data:Z
    environment:
      - PGDATA=/var/lib/postgresql/data
      - POSTGRES_USER=${POSTGRES_USER:-sub2api}
      - POSTGRES_PASSWORD=${POSTGRES_PASSWORD:?required}
      - POSTGRES_DB=${POSTGRES_DB:-sub2api}
      - TZ=${TZ:-Asia/Shanghai}
    networks: [stack]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER:-sub2api} -d ${POSTGRES_DB:-sub2api}"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 10s

  redis:
    image: redis:8-alpine
    restart: unless-stopped
    ulimits:
      nofile: { soft: 100000, hard: 100000 }
    volumes:
      - ./redis_data:/data:Z
    command: >
      sh -c '
        redis-server \
        --save 60 1 \
        --appendonly yes \
        --appendfsync everysec \
        ${REDIS_PASSWORD:+--requirepass "$REDIS_PASSWORD"}'
    environment:
      - TZ=${TZ:-Asia/Shanghai}
      - REDISCLI_AUTH=${REDIS_PASSWORD:-}
    networks: [stack]
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 5s

networks:
  stack:
    name: ${SLUG}-net
    external: true          # 由 s2a-init 预先创建
```

> 注意：**没有 `container_name`**，靠 compose project name 自动命名（`${SLUG}-data-postgres-1`），多站点天然隔离。

### 5.2 `compose.app.yml.tmpl`（同一份文件跑两次，slot 不同）

```yaml
# 渲染自 templates/compose.app.yml.tmpl —— 请勿手工编辑
name: ${SLUG}-${SLOT}

services:
  app:
    image: ${IMAGE_REPO}:${IMAGE_TAG}
    restart: unless-stopped
    ulimits:
      nofile: { soft: 100000, hard: 100000 }
    ports:
      - "${BIND_HOST}:${APP_PORT}:8080"        # BIND_HOST 强制 127.0.0.1
    volumes:
      - ./data:/app/data:Z                      # ★ blue/green 共享
    env_file:
      - .env
    environment:
      - AUTO_SETUP=true
      - SERVER_HOST=0.0.0.0
      - SERVER_PORT=8080
      - DATABASE_HOST=postgres                  # 走 stack network 内 DNS
      - REDIS_HOST=redis
      - SERVER_SHUTDOWN_TIMEOUT_SECONDS=${DRAIN_SECONDS}   # ★ 见 §7.1
      - APP_SLOT=${SLOT}
    stop_grace_period: ${DRAIN_SECONDS}s        # ★ 必须 ≥ shutdown timeout
    networks: [stack]
    healthcheck:
      test: ["CMD", "wget", "-q", "-T", "5", "-O", "/dev/null", "http://localhost:8080/health"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 30s
    logging:
      driver: json-file
      options: { max-size: "50m", max-file: "5" }

networks:
  stack:
    name: ${SLUG}-net
    external: true
```

**注意 `DATABASE_HOST=postgres`**：data 层的服务名是 `postgres`，两个 project 在同一个 external network 上，compose 会把服务名注册为网络别名，跨 project 可解析。

### 5.3 `nginx-upstream.conf.tmpl` ★ 部署时唯一被改写的文件

```nginx
# ${SLUG} —— 由 s2a-deploy 自动改写，请勿手工编辑
# slot=${SLOT} tag=${IMAGE_TAG} at=${TIMESTAMP}
upstream ${SLUG_US} {
    server 127.0.0.1:${APP_PORT};
    keepalive 64;
    keepalive_timeout 120s;
}
```

`SLUG_US` = slug 里的 `-` 换成 `_`（nginx upstream 名不能有 `-`... 实际可以，但避免歧义统一用下划线）。

### 5.4 `snippets/sub2api-proxy.conf`（手写一次，全站共用）

```nginx
proxy_http_version 1.1;
proxy_set_header Connection "";          # 保持 upstream keepalive

# SSE / 流式响应必须
proxy_buffering off;
proxy_cache off;
proxy_request_buffering off;
gzip off;                                 # 别压 SSE
chunked_transfer_encoding on;

# 只从真实 TCP 对端生成转发头，避免透传客户端伪造值
proxy_set_header X-Real-IP        $remote_addr;
proxy_set_header X-Forwarded-For  $remote_addr;
proxy_set_header X-Forwarded-Proto $scheme;
proxy_set_header X-Forwarded-Host $host;
proxy_set_header Host             $host;

proxy_connect_timeout 10s;
proxy_send_timeout    960s;
```

> 如果 Nginx 前面还有 CDN，`X-Real-IP` 需要改用 `real_ip_module` + `set_real_ip_from <CDN CIDR>`，参考仓库里已有的 `deploy/EDGE_SECURITY.md`。

### 5.5 `nginx-site.conf.tmpl`（每站点渲染一次，之后不动）

```nginx
# ${DOMAIN} —— 渲染自 templates/nginx-site.conf.tmpl，请勿手工编辑

server {
    listen 80;
    listen [::]:80;
    server_name ${DOMAIN};
    location /.well-known/acme-challenge/ { root /var/www/certbot; }
    location / { return 301 https://$host$request_uri; }
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name ${DOMAIN};

    ssl_certificate     ${TLS_CERT};
    ssl_certificate_key ${TLS_KEY};
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_prefer_server_ciphers off;
    ssl_session_cache   shared:SSL:10m;
    ssl_session_timeout 1d;

    client_max_body_size ${CLIENT_MAX_BODY_SIZE};

    access_log /var/log/nginx/${SLUG}.access.log;
    error_log  /var/log/nginx/${SLUG}.error.log warn;

    # /metrics 不对外（应用侧已有 bearer 保护，这里再兜一层）
    location = /metrics { return 404; }

    location / {
        proxy_pass http://${SLUG_US};
        include /etc/nginx/snippets/sub2api-proxy.conf;
        proxy_read_timeout ${PROXY_READ_TIMEOUT};
    }
}
```

---

## 6. 部署脚本

### 6.1 `s2a-render` —— 清单 → 产物

```
读 sites.yaml
  ├─ 校验：port_base 块无重叠、domain 无重复、TLS 证书文件存在
  ├─ 对每个 stack：
  │    渲染 stacks/<slug>/compose.data.yml
  │    渲染 stacks/<slug>/compose.app.yml
  │    渲染 /etc/nginx/sub2api/sites/<slug>.conf
  │    若 upstreams/<slug>.conf 不存在则初始化（指向 blue 端口）
  └─ nginx -t；通过才 nginx -s reload
```

渲染工具用 `envsubst`（零依赖）或 `gomplate`（需要循环/条件时）。`sites.yaml` 用 `yq` 解析。

**幂等**：`s2a-render` 可以随时重跑，不影响运行中的容器（它不碰 `upstreams/`，除非文件不存在）。

### 6.2 `s2a-deploy <slug> [image_tag]` —— 核心流程

```bash
#!/usr/bin/env bash
set -euo pipefail

SLUG="$1"; TAG="${2:-}"
STACK_DIR="/srv/sub2api/stacks/${SLUG}"
UPSTREAM_CONF="/etc/nginx/sub2api/upstreams/${SLUG}.conf"

# ── 1. 读取当前状态 ──────────────────────────────────────────
source "${STACK_DIR}/STATE"                    # slot=blue tag=v1.4.1
CUR_SLOT="${slot}"
NEW_SLOT=$([ "$CUR_SLOT" = blue ] && echo green || echo blue)
TAG="${TAG:-$(yq ".stacks[] | select(.slug==\"$SLUG\") | .image_tag" sites.yaml)}"

PORT_BASE=$(yq "... .port_base" sites.yaml)
CUR_PORT=$([ "$CUR_SLOT" = blue ] && echo $PORT_BASE || echo $((PORT_BASE+1)))
NEW_PORT=$([ "$NEW_SLOT" = blue ] && echo $PORT_BASE || echo $((PORT_BASE+1)))

log "deploy ${SLUG}: ${CUR_SLOT}(${tag}) → ${NEW_SLOT}(${TAG})"

# ── 2. 确认 data 层在跑 ──────────────────────────────────────
docker compose -p "${SLUG}-data" -f "${STACK_DIR}/compose.data.yml" up -d --wait

# ── 3. 拉起新 slot（此处会自动跑 DB 迁移，advisory lock 保护）──
SLOT="${NEW_SLOT}" APP_PORT="${NEW_PORT}" IMAGE_TAG="${TAG}" \
  docker compose -p "${SLUG}-${NEW_SLOT}" -f "${STACK_DIR}/compose.app.yml" \
  up -d --pull always --force-recreate

# ── 4. 健康门禁（含迁移时间，默认 300s）─────────────────────
deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
until curl -fsS --max-time 5 "http://127.0.0.1:${NEW_PORT}/health" >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
        err "健康检查超时，回收 ${NEW_SLOT}，流量未切换"
        docker compose -p "${SLUG}-${NEW_SLOT}" -f "${STACK_DIR}/compose.app.yml" logs --tail 200
        docker compose -p "${SLUG}-${NEW_SLOT}" -f "${STACK_DIR}/compose.app.yml" down
        exit 1
    fi
    sleep "${HEALTH_INTERVAL}"
done

# ── 4b. 版本校验（需 §7.3 改动）：确认起来的确实是新镜像 ────
ACTUAL=$(curl -fsS "http://127.0.0.1:${NEW_PORT}/health" | jq -r '.version // "unknown"')
[ "$ACTUAL" = "unknown" ] || [ "$ACTUAL" = "$TAG" ] || { err "版本不符: 期望 $TAG 实际 $ACTUAL"; ...down; exit 1; }

# ── 5. 切流量：改一行 + 校验 + reload ────────────────────────
cp "$UPSTREAM_CONF" "${UPSTREAM_CONF}.bak"
render_upstream "$SLUG" "$NEW_PORT" "$NEW_SLOT" "$TAG" > "$UPSTREAM_CONF"
if ! nginx -t; then
    err "nginx 配置校验失败，还原"
    mv "${UPSTREAM_CONF}.bak" "$UPSTREAM_CONF"
    docker compose -p "${SLUG}-${NEW_SLOT}" ... down
    exit 1
fi
nginx -s reload
log "流量已切至 ${NEW_SLOT}:${NEW_PORT}"

# ── 6. 记录状态 ──────────────────────────────────────────────
printf 'slot=%s\ntag=%s\nprev_slot=%s\nprev_tag=%s\nat=%s\n' \
  "$NEW_SLOT" "$TAG" "$CUR_SLOT" "$tag" "$(date -Iseconds)" > "${STACK_DIR}/STATE"

# ── 7. 异步排空旧 slot，命令立即返回 ────────────────────────
systemd-run --unit="s2a-drain-${SLUG}-${CUR_SLOT}" --collect \
  --on-active="${DRAIN_SECONDS}s" \
  /srv/sub2api/bin/s2a-teardown "$SLUG" "$CUR_SLOT"

log "旧 slot ${CUR_SLOT} 将在 ${DRAIN_SECONDS}s 后回收；期间可 s2a-rollback 秒级回退"
```

**排空为什么用定时而不是探测连接数**：宿主机侧 `ss` 因为 docker NAT 看不准，容器里没有 `ss`，应用也没暴露 in-flight 指标。定时 `900 + 60` 是当前唯一可靠的做法。§12 给了改进路径。

### 6.3 `s2a-rollback <slug>`

```
读 STATE 的 prev_slot / prev_tag
  ├─ 检查 prev_slot 容器是否还在（在排空窗口内 = 在）
  │    在  → 改 upstream 回 prev 端口 + nginx -t + reload   （秒级）
  │    不在 → 以 prev_tag 重新走一遍 s2a-deploy            （分钟级）
  ├─ 取消该 slug 的 drain 定时器（systemctl stop s2a-drain-...）
  └─ 重写 STATE
```

**回滚只回滚代码，不回滚 schema。** 详见 §8。

### 6.4 `s2a-status`

遍历所有 stack，输出：slug / domain / 当前 slot / tag / upstream 实际指向端口 / 两个 slot 的容器状态 / `/health` 探测结果 / 待回收的 drain 定时器。用于确认「STATE 文件」和「nginx 实际配置」和「容器实际状态」三者一致。

---

## 7. 必须的代码改动

### 7.1 【硬阻塞】优雅关闭超时可配置

`backend/cmd/server/main.go:182-187`：

```go
// 改前
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
if err := app.Server.Shutdown(ctx); err != nil {
    log.Fatalf("Server forced to shutdown: %v", err)
}

// 改后
timeout := 30 * time.Second   // 默认值上调，5s 对流式网关本就太短
if v := os.Getenv("SERVER_SHUTDOWN_TIMEOUT_SECONDS"); v != "" {
    if n, err := strconv.Atoi(v); err == nil && n > 0 {
        timeout = time.Duration(n) * time.Second
    }
}
ctx, cancel := context.WithTimeout(context.Background(), timeout)
defer cancel()
log.Printf("Draining in-flight requests (timeout %s)...", timeout)
if err := app.Server.Shutdown(ctx); err != nil {
    // 超时只意味着还有连接没结束，不是致命错误，不该 Fatalf
    log.Printf("Shutdown timed out with connections still open: %v", err)
}
log.Println("Server exited")
```

两处要点：
- `log.Fatalf` → `log.Printf`。关闭超时不是致命错误，`Fatalf` 会以非 0 码退出，让 `restart: unless-stopped` 产生噪音。
- compose 侧 `stop_grace_period` 必须 **≥** 这个值，否则 Docker 先 SIGKILL，配置白设。

配套：`.env.example` 加一节说明；`SERVER_SHUTDOWN_TIMEOUT_SECONDS` 与 `GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT` 应保持 `shutdown ≥ stream + 余量` 的关系。

### 7.2 移除硬编码 container_name

`deploy/docker-compose.yml:20` 及另两处、`deploy/docker-compose.local.yml:28` 及另两处 —— 删掉 `container_name`，改由 compose project name 派生。

对单站点用户是无感变化（容器名从 `sub2api` 变成 `sub2api-app-1`），但 README / DOCKER.md 里所有 `docker logs sub2api` 之类的命令要同步更新为 `docker compose logs app`。

### 7.3 【建议】`/health` 返回版本号

`backend/internal/server/routes/common.go:25`：

```go
r.GET("/health", func(c *gin.Context) {
    c.JSON(http.StatusOK, gin.H{
        "status":  "ok",
        "version": buildinfo.Version,   // main.go 里的 Version 变量需要导出到可访问处
        "slot":    os.Getenv("APP_SLOT"),
    })
})
```

价值：部署脚本能验证「起来的确实是新镜像」，而不是误把一个残留的旧容器当成部署成功。成本极低。

### 7.4 【可选】增加 `/readyz` 探活 DB/Redis

`/health` 是静态 200，只能证明进程活着。加一个 `/readyz` 做一次 `db.PingContext` + `redis.Ping`（带 2s 超时），用于：
- 蓝绿门禁的第二道校验
- nginx `upstream` 的被动健康检查语义更准确
- 监控告警

注意 `/readyz` 不要放进 nginx 对外路由（`location = /readyz { deny all; }`）。

---

## 8. 迁移纪律：expand-contract（不可协商）

`backend/internal/repository/ent.go:72` 决定了：**绿容器一启动就把 schema 升到最新，而此时蓝容器（旧代码）还在服务。** 这段并存窗口最短几十秒，最长 = 排空时间（960s）。

因此每个版本的迁移必须满足：**迁移 N 与应用版本 N-1 兼容。**

| 操作 | 允许？ | 正确做法 |
|---|---|---|
| `ADD COLUMN`（可空 / 有默认值） | ✅ | 直接加 |
| `ADD COLUMN NOT NULL` 无默认 | ❌ | 拆两步：先可空 → 回填 → 下版本加约束 |
| `DROP COLUMN` | ❌ | 版本 K 停止读写 → 版本 K+1 才 DROP |
| `RENAME COLUMN` | ❌ | 加新列 → 双写 → 回填 → 停旧列 → 下版本删 |
| `ALTER TYPE` | ❌ | 加新列 → 双写 → 切读 → 删旧列 |
| `CREATE INDEX` | ⚠️ | 必须 `CONCURRENTLY`，且不能在事务里（迁移 runner 用事务包裹，需单独处理） |
| `DROP TABLE` | ❌ | 同 DROP COLUMN，跨两个版本 |

**回滚的额外约束**：回滚只把 upstream 指回旧 slot，**不会撤销已经应用的迁移**（`schema_migrations` 有校验和，也不该撤）。所以旧代码必须能在新 schema 上正常跑——这正是 expand-contract 保证的东西。**如果某个版本破坏了这条规则，它就不能用蓝绿发布，必须走停机窗口。**

建议在 CI 加一道门禁：迁移文件 diff 中出现 `DROP COLUMN|DROP TABLE|ALTER COLUMN .* TYPE|RENAME` 时，要求 PR 显式打 `breaking-migration` 标签才能合并。

---

## 9. 并存窗口的其他副作用

### 9.1 进程内并发闸翻倍

`backend/internal/handler/openai_gateway_handler.go:2096` 的 `GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS` 是**进程内**信号量。蓝绿并存期间实际打到上游的并发是配置值的 2 倍。

缓解：
- 短期：把该值按 `ceil(N/2)` 配置，或接受发布窗口内的短暂翻倍（上游有配额时要评估风控风险）
- 长期：迁到 Redis 分布式限流（Redis 已经是必备组件）

同样的问题适用于任何进程内状态：账号轮询游标、本地缓存、内存计数器。发布前应盘一遍。

### 9.2 定时任务重复执行

如果有 cron / 后台 worker（radar 聚合、清理任务等），并存窗口内会**跑两份**。检查这些任务是否幂等；不幂等的需要用 Redis 锁或 PG advisory lock 保护。这一项建议在实施前专门排查一遍 `internal/` 下的后台 goroutine。

### 9.3 data 目录共享的写竞争

`./data` 被 blue/green 同时挂载。`config.yaml` 在首次安装后基本只读（`.installed` 存在 → `NeedsSetup()` 返回 false → 不会重写）。但要确认没有运行时写 data 目录的逻辑（日志文件、缓存文件等）会互相覆盖。

---

## 10. 分阶段落地计划

### 阶段 0：前置代码改动（1 个 PR）
- [ ] `SERVER_SHUTDOWN_TIMEOUT_SECONDS` 可配 + 去掉 `log.Fatalf`（§7.1）
- [ ] 移除 3+3 处 `container_name`（§7.2）
- [ ] `/health` 加 version（§7.3）
- [ ] `.env.example` 补充新变量说明 + `BIND_HOST` 默认值改 `127.0.0.1` 并加注释
- [ ] README / DOCKER.md 里的容器名相关命令同步更新
- [ ] 排查 §9.2 的后台任务幂等性

**验收**：单站点场景下 `docker compose up -d` 行为不变；`docker stop` 时日志出现 "Draining in-flight requests"，一条进行中的 SSE 流不被中断。

### 阶段 1：单站点跑通蓝绿（不涉及多站点）
- [ ] 写 4 份模板 + `s2a-render` / `s2a-init` / `s2a-deploy` / `s2a-rollback` / `s2a-status`
- [ ] 在 staging 上建第一个 stack，手工验证：正常发布 / 健康检查失败自动回收 / 回滚
- [ ] 验证排空：发起一条长 SSE 流 → 执行 deploy → 确认流不中断直到自然结束

**验收**：staging 上连续 5 次发布，`curl` 持续压测（含流式）零 5xx。

### 阶段 2：接入生产 + 第二个站点
- [ ] 把现有生产实例迁成 stack 结构（一次停机窗口，或用同样的蓝绿手法平迁）
- [ ] 加入第二个站点，验证 `s2a-render` 的端口冲突校验、nginx 多域名共存
- [ ] 备份脚本适配新目录结构（`stacks/*/postgres_data`）

### 阶段 3：CI 集成
- [ ] GH Actions 构建并推送镜像后，`ssh <server> 's2a-deploy api-staging <tag>'`
- [ ] 生产环境保持手动触发（`workflow_dispatch`），不自动发布
- [ ] 部署结果推送到告警渠道

### 阶段 4（可选）：可观测性补强
- [ ] `/readyz`（§7.4）
- [ ] in-flight 请求 gauge → 排空改为「探测归零即回收」（§12）

---

## 11. 故障剧本

| 症状 | 处理 |
|---|---|
| 健康检查超时 | 脚本已自动 `down` 新 slot，流量从未切换，线上无影响。看 `docker compose logs` 定位（多半是迁移失败或配置校验失败） |
| 切完流量后新版本报错 | `s2a-rollback <slug>` —— 排空窗口内旧容器还在，秒级回退 |
| 排空窗口已过才发现问题 | `s2a-rollback` 会以 `prev_tag` 重新部署，分钟级。**前提是 schema 向后兼容** |
| `nginx -t` 失败 | 脚本已自动还原 `upstream.conf.bak`，不会 reload 坏配置 |
| 迁移卡住（advisory lock 争抢） | 检查是否有僵尸容器持锁：`SELECT * FROM pg_locks WHERE locktype='advisory'` |
| 两个 slot 都在跑，不确定哪个在服务 | `s2a-status` 以 nginx 的 `upstream.conf` 为准（那是唯一真相），STATE 文件仅作记录 |
| 磁盘被日志撑满 | 模板已配 `json-file` + `max-size 50m` + `max-file 5`，每容器上限 250MB |

---

## 12. 已知限制与后续改进

1. **排空是定时而非探测。** 900s 的固定等待意味着旧容器多占一份内存约 15 分钟。改进路径：应用暴露 `sub2api_http_requests_in_flight` gauge，`s2a-teardown` 改为轮询该指标，归零即回收，同时保留 `DRAIN_SECONDS` 作为上限。

2. **单机 VPS 无法做真正的多副本高可用。** 本方案解决的是「发布不中断」，不解决「宿主机挂了」。后者需要多节点 + 外部 LB，届时本方案的 nginx 层可以平移。

3. **每站点独立 PG/Redis 吃内存。** 站点数超过约 5 个时，考虑共享一个 PG 实例、每站点独立 database + role。代价是爆炸半径变大、迁移的 advisory lock 变成全局争抢（当前 lock key 是常量，需要改成按 database 区分）。

4. **证书由 certbot 管理，不在本方案范围。** 模板里只引用证书路径；certbot 续期后需要 `nginx -s reload`（certbot 的 deploy-hook 里配）。

5. **`sites.yaml` 目前是手工维护的服务器端文件。** 站点多了之后可以纳入一个独立的私有 git 仓库做版本管理和 review。

---

## 附：新增一个站点的完整操作

```bash
# 1. 在 sites.yaml 里加一条记录（分配一个新的 port_base 块）
vim /srv/sub2api/registry/sites.yaml

# 2. 准备该站点的密钥
cp /srv/sub2api/registry/envs/_template.env /srv/sub2api/registry/envs/site3-prod.env
chmod 600 /srv/sub2api/registry/envs/site3-prod.env
openssl rand -hex 32   # 依次填 POSTGRES_PASSWORD / JWT_SECRET / TOTP_ENCRYPTION_KEY
vim /srv/sub2api/registry/envs/site3-prod.env

# 3. 申请证书
certbot certonly --webroot -w /var/www/certbot -d api.site3.com

# 4. 渲染 + 初始化 + 首次部署
s2a-render                    # 校验冲突 → 生成 compose/nginx 产物 → nginx -t && reload
s2a-init   site3-prod         # 创建 network、目录、启动 data 层
s2a-deploy site3-prod v1.4.2  # 首次部署（此时无旧 slot，跳过排空）

# 5. 确认
s2a-status
```

新增站点需要人工做的事：**改 sites.yaml 一条记录 + 填一个 env 文件 + 申请证书**。不需要写任何 nginx conf 或 compose yml。

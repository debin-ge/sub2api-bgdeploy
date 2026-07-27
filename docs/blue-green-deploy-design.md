# Sub2API 蓝绿部署方案（简化版）

Nginx + Docker 双容器，多站点多环境，零停机发布。

---

## 0. 简化了什么

相比初版，砍掉三层机制：

| 砍掉的东西 | 为什么可以砍 |
|---|---|
| `sites.yaml` + 渲染脚本 + yq/envsubst 依赖 | Compose 原生支持 `${VAR}` 插值，不需要预渲染；nginx 的 server block 一个站点写一次就永远不动，写模板比直接写更麻烦 |
| 排空定时器（systemd-run 异步回收） | **旧 slot 不主动回收，留到下次发布时才 down**。回滚窗口从 960s 变成「直到下次发布」，代价只是一个空闲容器 |
| `STATE` 状态文件 | 当前 slot 直接从 nginx upstream 文件读，单一真相源，不会出现「文件说 blue、nginx 说 green」 |
| data/app 拆两个 project + external network + external volume | 合回一个 compose 文件，用 YAML anchor 定义两个 app 服务 |
| `/readyz`、`/health` 加版本号 | 部署脚本用明确的 tag 拉镜像，起来了就是对的；要验证用 `docker inspect` 即可 |

最终：**1 处 Go 代码改动 + 2 个新文件 + 每站点 2 个 nginx 文件**。

---

## 1. 代码改动清单

### 1.1 【必须】`backend/cmd/server/main.go` —— 唯一的 Go 改动

这是整个方案的硬前提。现在的代码：

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)   // :182
defer cancel()

if err := app.Server.Shutdown(ctx); err != nil {
    log.Fatalf("Server forced to shutdown: %v", err)                      // :186
}
```

5 秒对流式网关远远不够（`GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT` 默认 900s，单条 SSE 流可挂 15 分钟），超时还走 `log.Fatalf` 硬退。不改的话，每次发布都会砍断所有在途流。

**改动一：import 加 `strconv`**（`os`、`time` 已有）

```go
import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"          // ← 新增
	"strings"
	"syscall"
	"time"
	...
)
```

**改动二：替换 182–187 行**

```go
shutdownTimeout := 30 * time.Second
if v := os.Getenv("SERVER_SHUTDOWN_TIMEOUT_SECONDS"); v != "" {
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		shutdownTimeout = time.Duration(n) * time.Second
	}
}

ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
defer cancel()

log.Printf("Draining in-flight requests (timeout %s)...", shutdownTimeout)
if err := app.Server.Shutdown(ctx); err != nil {
	// 超时只代表还有连接没结束，不是致命错误——原来的 Fatalf 会以非 0 码退出，
	// 在 restart: unless-stopped 下产生无意义的重启噪音。
	log.Printf("Shutdown timed out with connections still open: %v", err)
}
```

两个要点：
1. 默认值从 5s 提到 30s —— 即使不用蓝绿，现在这个值对流式网关也太短了
2. `log.Fatalf` → `log.Printf`

**注意**：compose 的 `stop_grace_period` 必须 ≥ `SERVER_SHUTDOWN_TIMEOUT_SECONDS`，否则 Docker 先 SIGKILL，这个配置白设。新 compose 文件里两者都绑到同一个变量。

### 1.2 【建议】`deploy/.env.example` 加一段注释

`BIND_HOST` 当前默认 `0.0.0.0`（第 18 行）。Nginx 在宿主机做反代时，应用端口不该对公网暴露——否则客户端能绕过 TLS 终止、限流和头部清洗直连。加个注释提醒：

```bash
# IPv4 bind address for host port mapping
# 若前面有 Nginx/Caddy 反代，应设为 127.0.0.1，避免绕过反代直连应用
BIND_HOST=0.0.0.0
```

（蓝绿 compose 文件里已硬编码 `127.0.0.1`，这条只是给单站点用户的提醒。）

### 1.3 【建议】排查后台任务幂等性

蓝绿并存窗口内，新旧两个容器**同时在跑后台 goroutine**（radar 聚合、清理任务、定时刷新等）。不幂等的任务会执行两次。

发布前应人工过一遍 `backend/internal/` 下用 `time.Ticker` / `cron` 起的循环，确认要么幂等、要么有 Redis/PG 锁保护。这不阻塞方案落地，但会决定并存窗口能开多长。

### 1.4 明确不改的东西

- `deploy/docker-compose.yml` / `docker-compose.local.yml` —— **完全不动**，单站点用户零影响，README/DOCKER.md 也不用改
- 不需要 `/readyz`、不需要给 `/health` 加版本号
- 不需要动迁移逻辑（`internal/repository/ent.go:72` 已有 PG advisory lock，并发启动天然串行）

---

## 2. 新增文件（2 个）

### 2.1 `deploy/docker-compose.bluegreen.yml`

一个站点一份拷贝，放在 `/srv/sub2api/stacks/<slug>/docker-compose.yml`。**站点之间内容完全相同，差异全在 `.env` 里。**

```yaml
# Sub2API 蓝绿部署 compose —— 与 docker-compose.yml 并存，不影响单站点用法。
# 用法见 deploy/bluegreen.sh
name: ${STACK_NAME}

x-app: &app
  image: ${IMAGE_REPO:-weishaw/sub2api}:${IMAGE_TAG:-latest}
  restart: unless-stopped
  env_file: [.env]
  ulimits:
    nofile: { soft: 100000, hard: 100000 }
  environment:
    AUTO_SETUP: "true"
    SERVER_HOST: 0.0.0.0
    SERVER_PORT: "8080"
    DATABASE_HOST: postgres
    REDIS_HOST: redis
    SERVER_SHUTDOWN_TIMEOUT_SECONDS: ${DRAIN_SECONDS:-960}
  volumes:
    - ./data:/app/data:Z              # ★ blue/green 共享，config.yaml 和 .installed 在这里
  stop_grace_period: ${DRAIN_SECONDS:-960}s
  depends_on:
    postgres: { condition: service_healthy }
    redis:    { condition: service_healthy }
  healthcheck:
    test: ["CMD", "wget", "-q", "-T", "5", "-O", "/dev/null", "http://localhost:8080/health"]
    interval: 30s
    timeout: 10s
    retries: 3
    start_period: 30s
  logging:
    driver: json-file
    options: { max-size: "50m", max-file: "5" }

services:
  app-blue:
    <<: *app
    ports: ["127.0.0.1:${PORT_BLUE}:8080"]

  app-green:
    <<: *app
    ports: ["127.0.0.1:${PORT_GREEN}:8080"]

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
      PGDATA: /var/lib/postgresql/data
      POSTGRES_USER: ${POSTGRES_USER:-sub2api}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?required}
      POSTGRES_DB: ${POSTGRES_DB:-sub2api}
      TZ: ${TZ:-Asia/Shanghai}
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
      TZ: ${TZ:-Asia/Shanghai}
      REDISCLI_AUTH: ${REDIS_PASSWORD:-}
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 5s
```

关键点：
- **没有 `container_name`**，靠 `name: ${STACK_NAME}` 自动隔离（容器名形如 `api-prod-app-blue-1`）
- 两个 app 服务共用 YAML anchor，只有端口不同
- pg/redis 无 profile，`up -d app-green` 会自动带起它们；发布只 recreate 指定的 app 服务

### 2.2 `deploy/bluegreen.sh`

```bash
#!/usr/bin/env bash
# Sub2API 蓝绿发布 —— deploy / rollback / status
#
#   bluegreen.sh deploy   <stack> [tag]
#   bluegreen.sh rollback <stack>
#   bluegreen.sh status   [stack]
set -euo pipefail

STACK_ROOT="${STACK_ROOT:-/srv/sub2api/stacks}"
UPSTREAM_DIR="${UPSTREAM_DIR:-/etc/nginx/sub2api/upstreams}"

die() { echo "!! $*" >&2; exit 1; }

# 当前 slot 的唯一真相源就是 nginx 的 upstream 文件
current_port() { grep -oE '127\.0\.0\.1:[0-9]+' "$1" | head -1 | cut -d: -f2; }

write_upstream() {   # <stack> <port> <slot> <tag>
    cat <<EOF
# 由 bluegreen.sh 自动生成，请勿手工编辑
# slot=$3 tag=$4 at=$(date -Iseconds)
upstream ${1//-/_} {
    server 127.0.0.1:$2;
    keepalive 64;
}
EOF
}

load_stack() {       # 设置 DIR / UP / PORT_BLUE / PORT_GREEN / ...
    STACK="$1"
    DIR="$STACK_ROOT/$STACK"
    UP="$UPSTREAM_DIR/$STACK.conf"
    [ -d "$DIR" ] || die "stack 不存在: $DIR"
    [ -f "$UP" ]  || die "upstream 配置不存在: $UP（新站点需先手工创建，见文档 §5）"
    set -a; . "$DIR/.env"; set +a
}

switch_upstream() {  # <port> <slot> <tag>；失败自动还原
    cp "$UP" "$UP.bak"
    write_upstream "$STACK" "$1" "$2" "$3" > "$UP"
    if ! nginx -t 2>/dev/null; then
        mv "$UP.bak" "$UP"
        die "nginx -t 校验失败，已还原配置"
    fi
    nginx -s reload
    rm -f "$UP.bak"
}

cmd_deploy() {
    load_stack "$1"
    local tag="${2:-${IMAGE_TAG:-latest}}"

    local cur_port cur_slot new_slot new_port
    cur_port="$(current_port "$UP")"
    if [ "$cur_port" = "$PORT_BLUE" ]; then
        cur_slot=blue;  new_slot=green; new_port="$PORT_GREEN"
    else
        cur_slot=green; new_slot=blue;  new_port="$PORT_BLUE"
    fi

    echo "==> $STACK: $cur_slot(:$cur_port) → $new_slot(:$new_port)  tag=$tag"

    cd "$DIR"

    # 1) 回收目标 slot 上一轮遗留的容器。它早已无流量，drain 瞬间完成。
    #    （若两次发布间隔小于排空时间，这里会等旧流结束——这是正确行为。）
    docker compose rm -sf "app-$new_slot" >/dev/null 2>&1 || true

    # 2) 起新容器。DB 迁移在此发生，PG advisory lock 保证不与旧容器冲突。
    IMAGE_TAG="$tag" docker compose up -d --pull always "app-$new_slot"

    # 3) 健康门禁（含迁移时间）
    local deadline=$(( SECONDS + ${HEALTH_TIMEOUT:-300} ))
    until curl -fsS --max-time 5 "http://127.0.0.1:$new_port/health" >/dev/null 2>&1; do
        if [ "$SECONDS" -ge "$deadline" ]; then
            echo "!! 健康检查超时（${HEALTH_TIMEOUT:-300}s），流量未切换，线上无影响" >&2
            docker compose logs --tail 100 "app-$new_slot" >&2
            docker compose rm -sf "app-$new_slot"
            exit 1
        fi
        sleep 3
    done

    # 4) 切流量
    switch_upstream "$new_port" "$new_slot" "$tag"
    sed -i "s|^IMAGE_TAG=.*|IMAGE_TAG=$tag|" "$DIR/.env"

    echo "==> 完成。旧 slot $cur_slot 保持运行作为回滚目标，下次发布时才回收。"
}

cmd_rollback() {
    load_stack "$1"
    cd "$DIR"

    local cur_port old_slot old_port
    cur_port="$(current_port "$UP")"
    if [ "$cur_port" = "$PORT_BLUE" ]; then
        old_slot=green; old_port="$PORT_GREEN"
    else
        old_slot=blue;  old_port="$PORT_BLUE"
    fi

    docker compose ps -q "app-$old_slot" | grep -q . \
        || die "app-$old_slot 已不在运行，无法秒级回滚。请用 deploy 指定旧 tag 重新发布。"
    curl -fsS --max-time 5 "http://127.0.0.1:$old_port/health" >/dev/null \
        || die "app-$old_slot 健康检查不通过，拒绝回滚"

    switch_upstream "$old_port" "$old_slot" "rollback"
    echo "==> 已回滚至 $old_slot(:$old_port)"
}

cmd_status() {
    for dir in "$STACK_ROOT"/${1:-*}/; do
        local s; s="$(basename "$dir")"
        [ -f "$UPSTREAM_DIR/$s.conf" ] || continue
        local p; p="$(current_port "$UPSTREAM_DIR/$s.conf")"
        echo "── $s  →  127.0.0.1:$p"
        ( cd "$dir" && docker compose ps --format '   {{.Service}}\t{{.Image}}\t{{.Status}}' \
            2>/dev/null | grep '^   app-' || true )
        for port in $(grep -hoE '^PORT_(BLUE|GREEN)=[0-9]+' "$dir/.env" | cut -d= -f2); do
            printf '   :%s  health=%s\n' "$port" \
                "$(curl -fsS --max-time 3 "http://127.0.0.1:$port/health" >/dev/null 2>&1 && echo ok || echo down)"
        done
    done
}

case "${1:-}" in
    deploy)   shift; cmd_deploy   "$@" ;;
    rollback) shift; cmd_rollback "$@" ;;
    status)   shift; cmd_status   "${1:-}" ;;
    *) echo "用法: $0 {deploy <stack> [tag] | rollback <stack> | status [stack]}" >&2; exit 1 ;;
esac
```

---

## 3. Nginx 配置

### 3.1 全局，改一次

`/etc/nginx/nginx.conf` 的 `http {}` 块内加：

```nginx
include /etc/nginx/sub2api/upstreams/*.conf;
include /etc/nginx/sub2api/sites/*.conf;
```

`main` 上下文（`http {}` 之外）加：

```nginx
# 旧 worker 保留在途 SSE 连接，但不无限期堆积（默认不设置 = 永不强制退出）
worker_shutdown_timeout 1200s;
```

### 3.2 `/etc/nginx/snippets/sub2api-proxy.conf` —— 写一次，全站共用

```nginx
proxy_http_version 1.1;
proxy_set_header Connection "";

# SSE / 流式响应必须
proxy_buffering off;
proxy_cache off;
proxy_request_buffering off;
gzip off;

# 只从真实 TCP 对端生成转发头，避免透传客户端伪造值
proxy_set_header Host              $host;
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-For   $remote_addr;
proxy_set_header X-Forwarded-Proto $scheme;
proxy_set_header X-Forwarded-Host  $host;

proxy_connect_timeout 10s;
proxy_send_timeout    960s;
proxy_read_timeout    960s;
```

> 前面还有 CDN 的话，`X-Real-IP` 需改用 `real_ip_module` + `set_real_ip_from <CDN CIDR>`，参考仓库已有的 `deploy/EDGE_SECURITY.md`。

### 3.3 每站点两个文件

`/etc/nginx/sub2api/sites/api-prod.conf` —— **手写一次，之后永不修改**：

```nginx
server {
    listen 80;
    server_name api.sub2api.com;
    location /.well-known/acme-challenge/ { root /var/www/certbot; }
    location / { return 301 https://$host$request_uri; }
}

server {
    listen 443 ssl;
    http2 on;
    server_name api.sub2api.com;

    ssl_certificate     /etc/letsencrypt/live/api.sub2api.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/api.sub2api.com/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;

    client_max_body_size 32m;
    access_log /var/log/nginx/api-prod.access.log;

    location = /metrics { return 404; }

    location / {
        proxy_pass http://api_prod;              # ← upstream 名 = slug 的 - 换成 _
        include /etc/nginx/snippets/sub2api-proxy.conf;
    }
}
```

`/etc/nginx/sub2api/upstreams/api-prod.conf` —— **脚本自动改写，唯一会变的文件**：

```nginx
# 由 bluegreen.sh 自动生成，请勿手工编辑
# slot=blue tag=v1.4.2 at=2026-07-21T10:00:00+08:00
upstream api_prod {
    server 127.0.0.1:18080;
    keepalive 64;
}
```

---

## 4. 服务器目录

```
/srv/sub2api/
├── bin/bluegreen.sh
└── stacks/
    ├── api-prod/
    │   ├── docker-compose.yml     # 各站点内容完全相同
    │   ├── .env                   # ★ 站点间唯一的差异，chmod 600
    │   ├── data/                  # /app/data，blue/green 共享
    │   ├── postgres_data/
    │   └── redis_data/
    └── api-staging/
        └── ...（同上）
```

`.env` 在现有 `deploy/.env.example` 基础上，头部加 5 行蓝绿专用变量：

```bash
# ── 蓝绿部署 ──────────────────────────────────────────
STACK_NAME=api-prod
PORT_BLUE=18080
PORT_GREEN=18081
IMAGE_TAG=v1.4.2          # 由 bluegreen.sh 自动更新
DRAIN_SECONDS=960         # 900(流上限) + 60 余量；staging 可设 60
HEALTH_TIMEOUT=300        # 含迁移时间

# ── 以下为 .env.example 原有内容 ────────────────────
POSTGRES_PASSWORD=...
JWT_SECRET=...
...
```

**端口约定**：每站点占 10 个端口，`PORT_BLUE` 取 18080 / 18090 / 18100…，`PORT_GREEN = PORT_BLUE + 1`。端口撞了 `docker compose up` 会直接报错，不需要额外校验脚本。

---

## 5. 日常操作

```bash
# 发布
bluegreen.sh deploy api-prod v1.4.3

# 回滚（旧 slot 还在跑 → 秒级）
bluegreen.sh rollback api-prod

# 查看所有站点当前状态
bluegreen.sh status
```

**新增一个站点**：

```bash
SLUG=site3-prod
mkdir -p /srv/sub2api/stacks/$SLUG/{data,postgres_data,redis_data}
cd /srv/sub2api/stacks/$SLUG

cp <repo>/deploy/docker-compose.bluegreen.yml docker-compose.yml
cp <repo>/deploy/.env.example .env && chmod 600 .env
vim .env          # 填 STACK_NAME / PORT_BLUE / PORT_GREEN / 三个密钥

certbot certonly --webroot -w /var/www/certbot -d api.site3.com

# nginx：复制一份 sites/*.conf 改域名和 upstream 名
cp /etc/nginx/sub2api/sites/api-prod.conf /etc/nginx/sub2api/sites/$SLUG.conf
vim /etc/nginx/sub2api/sites/$SLUG.conf

# upstream 初始文件（首次指向 blue 端口）
cat > /etc/nginx/sub2api/upstreams/$SLUG.conf <<'EOF'
upstream site3_prod {
    server 127.0.0.1:18100;
    keepalive 64;
}
EOF
nginx -t && nginx -s reload

bluegreen.sh deploy $SLUG v1.4.2
```

---

## 6. 迁移纪律：expand-contract（唯一不能简化的约束）

`backend/internal/repository/ent.go:72` 每次启动都跑迁移。蓝绿意味着**绿容器把 schema 升到 N 时，蓝容器（N-1 的代码）还在服务同一个库**。而本方案里旧 slot 会一直留到下次发布，这个并存窗口可能是几小时甚至几天。

所以：**迁移 N 必须与应用版本 N-1 兼容。**

| 操作 | 允许 | 正确做法 |
|---|---|---|
| `ADD COLUMN`（可空/有默认值） | ✅ | 直接加 |
| `ADD COLUMN NOT NULL` 无默认 | ❌ | 先可空 → 回填 → 下版本加约束 |
| `DROP COLUMN` / `DROP TABLE` | ❌ | 版本 K 停止读写 → 版本 K+1 才删 |
| `RENAME` / `ALTER TYPE` | ❌ | 加新列 → 双写 → 回填 → 切读 → 下版本删旧列 |
| `CREATE INDEX` | ⚠️ | 用 `CONCURRENTLY`，且不能在事务里（迁移 runner 用事务包裹，需单独处理） |

**回滚不撤销迁移**（`schema_migrations` 有 SHA256 校验和，也不该撤）。回滚只把 upstream 指回旧 slot，所以旧代码必须能在新 schema 上跑——这正是 expand-contract 保证的。**破坏这条规则的版本不能用蓝绿发布，必须走停机窗口。**

建议在 CI 加一道门禁：迁移文件 diff 出现 `DROP COLUMN|DROP TABLE|RENAME|ALTER COLUMN .* TYPE` 时要求 PR 显式打标签。

---

## 7. 另一个不能忽略的点：进程内状态在并存期翻倍

`backend/internal/handler/openai_gateway_handler.go:2096` 的 `GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS` 是**进程内**信号量。旧 slot 常驻意味着并存不是几分钟而是常态——但旧 slot 没有新流量进来，所以实际并发不会真的翻倍，只有在途的老连接。**这是「旧 slot 常驻」相比「定时排空」额外的好处：并存窗口虽长，但旧实例是静默的。**

真正需要注意的是**后台定时任务**（见 §1.3）：那些不依赖流量、按 ticker 触发的 goroutine，在旧 slot 常驻期间会一直跑第二份。这是本简化方案唯一新增的风险点，必须在阶段 0 排查清楚。

如果排查下来有不幂等的后台任务，两个选择：
- 给它们加 Redis / PG advisory lock（推荐，一劳永逸）
- 或者恢复「定时排空」：deploy 末尾加一行 `nohup sh -c "sleep $DRAIN_SECONDS; cd $DIR && docker compose rm -sf app-$cur_slot" &`，代价是回滚窗口缩短到 960s

---

## 8. 落地顺序

**阶段 0：一个 PR**
- [ ] `main.go` 的 shutdown 改动（§1.1）
- [ ] `.env.example` 的 `BIND_HOST` 注释（§1.2）
- [ ] 新增 `deploy/docker-compose.bluegreen.yml`、`deploy/bluegreen.sh`
- [ ] 排查后台任务幂等性（§1.3）——决定是否需要定时排空

验收：单站点 `docker compose up -d` 行为不变；`docker stop` 时日志出现 `Draining in-flight requests`，一条进行中的 SSE 流不被中断。

**阶段 1：staging 跑通**
- [ ] 建第一个 stack，验证正常发布 / 健康检查失败自动回收 / 回滚
- [ ] 关键验证：发起一条长 SSE 流 → 执行 deploy → 确认流不中断直到自然结束

**阶段 2：生产 + 第二站点**
- [ ] 生产实例迁成 stack 结构（一次停机窗口）
- [ ] 加第二个站点，验证 nginx 多域名共存
- [ ] 备份脚本适配 `stacks/*/postgres_data`

**阶段 3：CI**
- [ ] GH Actions 推镜像后 `ssh <server> 'bluegreen.sh deploy api-staging <tag>'`
- [ ] 生产保持 `workflow_dispatch` 手动触发

---

## 9. 故障剧本

| 症状 | 处理 |
|---|---|
| 健康检查超时 | 脚本已自动回收新 slot，流量从未切换，线上无影响。看打印的日志（多半是迁移失败或配置校验失败） |
| 切完流量后新版本报错 | `bluegreen.sh rollback <stack>`，旧 slot 还在，秒级 |
| `nginx -t` 失败 | 脚本自动还原 `.bak`，不会 reload 坏配置 |
| 不确定哪个 slot 在服务 | `bluegreen.sh status`，以 nginx upstream 文件为准（那是唯一真相源） |
| 迁移卡住 | 查僵尸容器持锁：`SELECT * FROM pg_locks WHERE locktype='advisory'` |
| 连续两次发布，第二次卡在 `rm -sf` | 正常——它在等上一轮的在途流排空。要么等，要么确认可以砍：`docker compose kill app-<slot>` |

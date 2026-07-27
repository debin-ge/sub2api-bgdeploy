# sub2api 蓝绿部署 CLI

`sub2api-bgdeploy` 是专门为
[sub2api](https://github.com/Wei-Shaw/sub2api) 构建的单机、多站点蓝绿部署 CLI，
可执行文件名为 `bgdeploy`。它负责管理 sub2api 的 PostgreSQL/Redis 数据层、
blue/green 应用实例、健康门禁、Nginx 流量切换、排空回收和回滚。
同时提供站点级 `stop`、`start`、`restart`，用于维护窗口和主机运维。

本项目不是通用部署框架。内置 Compose 模板、环境变量校验、`/health` 身份校验、
`APP_SLOT`、共享 `/app/data`、数据库迁移窗口及流式请求超时均针对 sub2api 的运行
约定设计。

`bgdeploy` 使用原生 Go 实现。YAML 解析、Compose/Nginx 模板、proxy snippet 和
HTTP 健康探测均已编译进二进制。服务器不需要复制源码、模板或脚本，也不依赖
Bash、curl、jq、yq、Python/PyYAML。

除状态查询外，所有写操作必须使用 root 权限，并直接在部署目录执行：

```bash
cd /srv/sub2api
sudo ./bgdeploy <command>
```

## 服务器依赖

只需要：

- Docker Engine，且 daemon 可访问；
- Docker Compose v2（`docker compose`）；
- 宿主机 Nginx；
- `systemd-run`（可选）。不存在时，工具会启动自身的后台子进程完成排空回收。

`sudo ./bgdeploy check` 和 `sudo ./bgdeploy init <slug>` 会在改变 Docker 状态前检查：

- 当前是否为 root；
- `docker compose version`；
- `docker info`；
- `nginx -t`；
- `nginx -T` 是否已加载 `blue-green-managed-http-config`；
- main 上下文是否配置 `worker_shutdown_timeout`。

## 构建二进制

需要 Go 1.22 或更高版本：

```bash
cd sub2api-bgdeploy

make test
make build                 # 当前操作系统/架构
make release               # Linux amd64 + arm64
```

产物：

```text
dist/sub2api-bgdeploy
dist/sub2api-bgdeploy-linux-amd64
dist/sub2api-bgdeploy-linux-arm64
```

构建使用 `-trimpath` 和 `CGO_ENABLED=0`，Linux 产物为静态二进制，服务器无需安装 Go。

### 使用 GitHub Actions 发布

仓库中的 `Build and release` 工作流支持两种运行方式：

- 在 GitHub Actions 页面手动运行：构建 Linux amd64 和 arm64 二进制，构建结果可在
  本次工作流的 Artifacts 中下载，保留 14 天；
- 推送 `v` 开头的 Git 标签：构建并自动创建同名 GitHub Release，同时上传两个架构
  的压缩包和 `checksums.txt`。

例如发布 `v1.0.0`：

```bash
git tag v1.0.0
git push origin v1.0.0
```

Release 产物：

```text
sub2api-bgdeploy-linux-amd64.tar.gz
sub2api-bgdeploy-linux-arm64.tar.gz
checksums.txt
```

每个压缩包内的可执行文件都名为 `sub2api-bgdeploy`。下载后可校验并安装为日常使用的
`bgdeploy` 命令：

```bash
sha256sum --ignore-missing -c checksums.txt
tar -xzf sub2api-bgdeploy-linux-amd64.tar.gz
sudo install -m 755 sub2api-bgdeploy /srv/sub2api/bgdeploy
```

## 项目结构

```text
.
├── cmd/
│   └── bgdeploy/          # 可执行文件入口
├── internal/
│   ├── cli/               # 命令解析与 sub2api 蓝绿部署编排
│   └── assets/            # 编译进二进制的 Compose/Nginx 模板和示例配置
├── Makefile
├── go.mod
└── README.md
```

运行时示例配置不再散落在源码根目录；执行 `bgdeploy bootstrap` 时会从内嵌资源生成。

## 一次性安装

以 Linux amd64 为例：

```bash
sudo mkdir -p /srv/sub2api
sudo cp dist/sub2api-bgdeploy-linux-amd64 /srv/sub2api/bgdeploy
sudo chmod 755 /srv/sub2api/bgdeploy
cd /srv/sub2api

sudo ./bgdeploy bootstrap
```

`bootstrap` 不覆盖已有文件，会创建：

```text
/srv/sub2api/
├── bgdeploy
├── runtime.yaml
├── env.example
├── sites.yaml
├── envs/
│   └── <slug>.env
└── stacks/
```

正常情况下，日常只需要编辑：

```text
sites.yaml
envs/<slug>.env
```

`runtime.yaml` 只保存主机级路径，通常在首次安装时确认一次即可。`stacks/`、Compose
文件、Nginx 配置、STATE 和排空 PID 均由工具维护，不应手工修改。

从使用 `registry/sites.yaml` 的旧版本升级时，先迁移清单：

```bash
sudo mv registry/sites.yaml sites.yaml
sudo rmdir registry
```

## 可执行文件运行配置

工具默认读取当前工作目录的 `./runtime.yaml`，部署根目录也默认是启动命令时的
当前工作目录（`pwd`）。因此应先进入部署目录，再执行 `./bgdeploy`。

完整配置：

```yaml
root: /srv/sub2api
nginx_dir: /etc/nginx/sites
nginx_snippet_dir: /etc/nginx/sites/snippets
```

所有路径必须是绝对路径。该文件不包含站点信息和密钥。

也可以使用环境变量：

| 环境变量 | 说明 |
|---|---|
| `BGDEPLOY_CONFIG` | 指定运行配置文件路径 |
| `BGDEPLOY_ROOT` | 部署根目录 |
| `BGDEPLOY_NGINX_DIR` | Nginx 蓝绿配置目录 |
| `BGDEPLOY_NGINX_SNIPPET_DIR` | Nginx snippet 目录 |

对应命令行参数：

```text
--config
--root
--nginx-dir
--nginx-snippet-dir
```

配置优先级从高到低：

```text
命令行参数 > BGDEPLOY_* 环境变量 > runtime.yaml > 内置默认值
```

示例：

```bash
sudo BGDEPLOY_ROOT=/srv/sub2api \
  BGDEPLOY_NGINX_DIR=/etc/nginx/sites \
  ./bgdeploy render

sudo ./bgdeploy \
  --config /etc/sub2api-bgdeploy/runtime.yaml \
  render
```

内置默认值：

```text
root                 当前工作目录（pwd）
BGDEPLOY_CONFIG      当前工作目录的 ./runtime.yaml
nginx_dir            /etc/nginx/sites
nginx_snippet_dir    /etc/nginx/sites/snippets
```

## Nginx 一次性接入

在 `/etc/nginx/nginx.conf` 的 main 上下文加入：

```nginx
worker_shutdown_timeout 1200s;
```

在 `http {}` 内加入：

```nginx
include /etc/nginx/sites/*.conf;
```

不要再同时 include `/etc/nginx/sites/upstreams/*.conf` 或
`/etc/nginx/sites/servers/*.conf`。这两个 include 由生成的 `http.conf` 统一维护，
否则会产生重复配置。

`worker_shutdown_timeout` 应大于最长流式响应时间。默认建议值 1200 秒，可覆盖应用
默认 900 秒流上限及额外排空余量，避免多次 reload 后旧 worker 无限堆积。

`render` 会先执行 `nginx -t` 和 `nginx -T` 检测以上两项一次性配置。缺少
`worker_shutdown_timeout` 或 `include /etc/nginx/sites/*.conf;` 时会立即中断，
在终端打印需要添加的完整配置，并且不会生成任何 stack 或 Nginx 文件。若
`nginx -t` 的错误明确来自工具管理目录中的旧渲染产物，`render` 会先重新生成，
再执行完整检查；修复后的配置仍未通过时不会 reload。

首次执行 `render` 前 `http.conf` 尚不存在，可以先检查 Nginx 基础配置：

```bash
sudo nginx -t
```

## 站点配置

`sites.yaml` 是非密钥站点配置的唯一真相源：

```yaml
defaults:
  image_repo: weishaw/sub2api
  bind_host: 127.0.0.1
  drain_seconds: 960
  health_timeout_seconds: 300
  health_interval_seconds: 3
  client_max_body_size: 32m
  proxy_connect_timeout: 10s
  proxy_send_timeout: 960s
  proxy_read_timeout: 960s
  tz: Asia/Shanghai

stacks:
  - slug: api-staging
    domain: staging.example.com
    port_base: 18080
    image_tag: v1.4.2
    tls:
      cert: /etc/letsencrypt/live/staging.example.com/fullchain.pem
      key: /etc/letsencrypt/live/staging.example.com/privkey.pem
```

参数说明：

| 参数 | 必填 | 说明 |
|---|---:|---|
| `slug` | 是 | 站点标识，仅允许小写字母、数字和连字符 |
| `domain` | 是 | 单个完整域名，用于 Nginx `server_name` |
| `port_base` | 是 | blue 使用该端口，green 使用 `port_base+1`；每站点预留 10 个端口 |
| `image_repo` | 是 | sub2api 镜像仓库，可放在 defaults 或 stack 中 |
| `image_tag` | 否 | deploy 未传 tag 时使用 |
| `bind_host` | 否 | 宿主机监听 IPv4 地址，默认 `127.0.0.1` |
| `drain_seconds` | 否 | 旧实例排空时间，也用于应用优雅关闭 |
| `health_timeout_seconds` | 否 | 新实例健康门禁总超时，应覆盖数据库迁移时间 |
| `health_interval_seconds` | 否 | 健康探测间隔 |
| `client_max_body_size` | 否 | Nginx 请求体上限 |
| `proxy_*_timeout` | 否 | Nginx 上游连接、发送和读取超时 |
| `tz` | 否 | 容器时区 |
| `tls.cert` / `tls.key` | 是 | Nginx 可读取的证书和私钥绝对路径 |

`render` 会在写文件前检查未知字段、slug/域名重复、端口区间重叠、端口范围、镜像
参数和 TLS 文件。已存在的 upstream 不会被 render 覆盖，当前流量方向始终以它为准。

每个 sub2api 站点的环境变量：

```bash
sudo cp env.example envs/api-staging.env
sudo chmod 600 envs/api-staging.env
sudo vim envs/api-staging.env
```

`init` 和每次 `deploy` 都会在改变容器状态前检查：

- `envs/<slug>.env` 存在且是普通文件，不接受目录或软链接；
- 文件权限严格为 `0600`；
- `POSTGRES_PASSWORD`、`REDIS_PASSWORD`、`JWT_SECRET`、
  `TOTP_ENCRYPTION_KEY`、`ADMIN_EMAIL`、`ADMIN_PASSWORD` 全部存在且非空；
- 上述参数已经修改，不得继续使用 `env.example` 中的示例值；
- stack 内的 `.env` 软链接指向当前部署根目录下正确的站点环境文件。

任一检查失败都会中断并显示具体文件、参数或修复命令。`init` 检查通过后会在生成的
stack 中创建 `.env` 软链接。

从旧目录布局升级时，先复制环境文件并重新初始化软链接：

```bash
sudo mkdir -p envs
sudo install -m 600 <原环境文件> envs/api-staging.env
sudo ./bgdeploy init api-staging
```

## 首次部署

```bash
cd /srv/sub2api

sudo ./bgdeploy render
sudo ./bgdeploy init api-staging
sudo ./bgdeploy deploy api-staging v1.4.2
./bgdeploy status api-staging
```

`render` 会生成 Compose/Nginx 配置、安装公共 proxy snippet、执行 `nginx -t` 后
reload。工具会通过 `nginx -v` 自动选择 HTTP/2 写法：Nginx 1.25.1 及以上使用
`http2 on;`，更早版本或无法识别版本时使用 `listen ... ssl http2` 兼容语法。
`init` 会执行完整依赖检查、创建共享目录和 external network，然后启动
PostgreSQL/Redis 并等待健康。

首次 deploy 会在 upstream 当前指向的 blue slot 原地启动，不创建排空任务。

## 日常发布

```bash
sudo ./bgdeploy deploy api-staging v1.4.3
```

流程：

1. 从 Nginx upstream 读取当前 slot；
2. 对同一 stack 加操作锁，并清理已退出进程留下的死锁；
3. 确认数据层健康；
4. 拉起另一 slot，并由 sub2api 执行数据库迁移；
5. 使用内置 HTTP 客户端轮询 `/health` 并要求 `status=ok`；
6. 校验响应中的 `slot` 和 `version`；对于尚未返回这两个字段的旧版镜像，自动通过
   Docker 元数据复核容器的 `APP_SLOT` 和完整镜像标签；
7. 备份并原子改写 upstream，`nginx -t` 成功后 reload；
8. 写入 STATE，异步排空旧 slot。

健康门禁或身份校验失败会输出新容器日志、回收新 slot，并保持 upstream 不变。
`nginx -t` 或 reload 失败会还原 upstream 备份，不切换线上流量。

新版本 sub2api 的 `/health` 固定返回 `status`、`version`、`slot`，并设置
`Cache-Control: no-store`。蓝绿环境的 `APP_SLOT` 仅允许 `blue` 或 `green`；非法值
返回 HTTP 503，避免配置错误的实例进入流量。

## 回滚和回收

```bash
sudo ./bgdeploy rollback api-staging
```

- 排空窗口内，旧 slot 仍运行、健康且身份正确时，仅切回 upstream；
- 旧 slot 已回收或不健康时，使用 STATE 的 `prev_tag` 重新执行完整发布；
- 回滚不会撤销已执行的数据库迁移，旧代码必须兼容新 schema。

手工回收：

```bash
sudo ./bgdeploy teardown api-staging green
```

teardown 会再次读取 Nginx upstream，拒绝回收当前生效 slot。

## 启动、停止和重启

整站停止：

```bash
sudo ./bgdeploy stop api-staging
```

`stop` 会先取消 blue/green 的待回收任务，再依次停止非生效应用 slot、生效应用
slot，最后停止 PostgreSQL/Redis。停止遵守 Compose 的 `stop_grace_period`，并保留：

- 应用和数据层容器；
- PostgreSQL、Redis 和 `/app/data` 持久化数据；
- `STATE` 中的蓝绿发布历史；
- Nginx upstream 的当前流量方向。

停止期间 Nginx 配置仍然存在，但由于上游应用未运行，请求会返回上游不可用。需要
自定义维护页时，应在外层负载均衡或 Nginx 中单独配置。

恢复整站：

```bash
sudo ./bgdeploy start api-staging
```

`start` 会先启动 PostgreSQL/Redis 并等待其健康，然后只启动 Nginx 当前指向的
生效 slot，最后执行与发布相同的 `/health` 健康和实例身份校验。非生效 slot 不会
自动启动。该命令只恢复 `stop` 保留的已有应用容器；首次发布或容器已经被
`teardown` 删除时应使用 `deploy`。若应用未通过门禁，工具会再次停止该应用 slot，
数据层保持运行以便排查。

原地重启：

```bash
sudo ./bgdeploy restart api-staging
```

`restart` 在同一个站点操作锁内依次执行完整停止和恢复，避免发布、回滚或其他
生命周期操作插入停启过程。

## 命令

```text
bootstrap
check
render
init <slug>
deploy <slug> [image-tag]
rollback <slug>
status [slug]
stop <slug>
start <slug>
restart <slug>
teardown <slug> <blue|green>
version
```

The executable includes an English operations guide covering configuration, initial setup,
routine releases, rollback, and troubleshooting:

```bash
./bgdeploy --help
./bgdeploy help
./bgdeploy deploy --help
```

状态查询不要求 root，但执行用户必须有读取 Docker daemon 的权限：

```bash
./bgdeploy status
./bgdeploy status api-staging
```

输出会显示 Nginx 实际方向、STATE、PostgreSQL/Redis 数据层、两个 slot 的
容器/健康状态和待回收任务。状态不一致时始终以 Nginx upstream 为准。

## 更新工具

只需原子替换单个文件，配置和运行数据不变：

```bash
sudo cp sub2api-bgdeploy /srv/sub2api/bgdeploy.new
sudo chmod 755 /srv/sub2api/bgdeploy.new
/srv/sub2api/bgdeploy.new version
sudo mv /srv/sub2api/bgdeploy.new /srv/sub2api/bgdeploy
```

## 开发测试

```bash
cd sub2api-bgdeploy
go test -race ./...
make release
```

测试使用假的 Docker/Nginx/systemd 命令和本地 HTTP 服务，覆盖运行配置优先级、
内嵌资源渲染、依赖预检、初始化、首次发布、blue→green、快速及降级回滚、
整站停止/恢复/重启、Nginx 校验失败还原和 teardown 安全闸。数据库迁移兼容性由
sub2api 版本负责。

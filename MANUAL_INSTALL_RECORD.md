# ARTEX 手动编译安装记录

日期：2026-11-17  
分支：`codex/long-task-governance`  
提交：`a242dc42424d6aed2a24129b520c2c547c1e4321`  
目标主机：Ubuntu Linux x86_64，Docker 29.9.3

## 1. 背景

上游项目已停止维护，本次没有使用 `install.sh`、`docker-compose.yml` 或页面一键更新链路。安装方式改为：

1. 在 GitHub 拉取指定分支源码；
2. 使用容器内 Node.js 构建前端静态资源；
3. 使用容器内 Go 构建内嵌前端的 Linux amd64 单二进制；
4. 使用 Docker 运行独立 PostgreSQL；
5. 使用 systemd 直接运行二进制。

## 2. 源码

```bash
rm -rf /opt/ARTEX
git clone \
  --branch codex/long-task-governance \
  --depth 1 \
  https://github.com/printmybutt/ARTEX.git \
  /opt/ARTEX
cd /opt/ARTEX
git rev-parse HEAD
```

验证结果：

```text
a242dc42424d6aed2a24129b520c2c547c1e4321
```

## 3. 前端构建

宿主机没有 Node.js/npm，因此使用 Node 22 容器构建，不污染宿主机：

```bash
docker run --rm \
  -v /opt/ARTEX/web:/workspace \
  -w /workspace \
  node:22-bookworm-slim \
  bash -lc 'npm ci && npm run build:static'
```

构建完成后同步静态导出结果：

```bash
cd /opt/ARTEX
rm -rf server/webui/dist
mkdir -p server/webui/dist
cp -a web/out/. server/webui/dist/
```

结果：31 个静态页面导出成功，TypeScript 检查通过。

## 4. 后端二进制

宿主机没有 Go 工具链，因此使用 Go 1.26 容器构建。模块缓存写入 `/opt/ARTEX/.cache/go`，便于重复构建：

```bash
cd /opt/ARTEX
docker run --rm \
  -v /opt/ARTEX:/workspace \
  -v /opt/ARTEX/.cache/go:/go/pkg/mod \
  -w /workspace \
  golang:1.26-bookworm \
  /bin/bash -c 'CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOSUMDB=sum.golang.org \
    /usr/local/go/bin/go build \
    -tags embedui \
    -trimpath \
    -ldflags "-s -w -buildid= -X main.version=a242dc4" \
    -o /workspace/artex \
    ./cmd/artex'
chmod +x /opt/ARTEX/artex
```

产物：

```text
/opt/ARTEX/artex
ELF 64-bit LSB executable, x86-64, statically linked, stripped
约 38 MB
```

## 5. PostgreSQL

创建独立 Docker 网络和 PostgreSQL 16 容器。不暴露 5432 到宿主机，只允许同网络访问：

```bash
docker network create artex-net
docker run -d \
  --name artex-postgres \
  --restart unless-stopped \
  --network artex-net \
  -e POSTGRES_USER=artex \
  -e POSTGRES_PASSWORD=artex_test_password \
  -e POSTGRES_DB=artex \
  -v artex-postgres:/var/lib/postgresql/data \
  postgres:16-alpine
```

健康检查：

```bash
docker run --rm --network artex-net \
  postgres:16-alpine \
  pg_isready -h artex-postgres -U artex -d artex
```

结果：

```text
artex-postgres:5432 - accepting connections
```

## 6. 运行配置

ARTEX 二进制运行在宿主机 systemd 中，而 PostgreSQL 在 Docker 网络内。宿主机不能解析 Docker 网络别名，因此配置中直接使用容器 IP。

当前容器地址为 `172.19.0.2`。如果 PostgreSQL 容器重建后 IP 变化，需要同步更新 `config.json`。

`/opt/ARTEX/config.json`：

```json
{
  "database": {
    "host": "172.19.0.2",
    "port": 5432,
    "user": "artex",
    "password": "artex_test_password",
    "dbname": "artex",
    "sslmode": "disable"
  }
}
```

## 7. systemd

`/etc/systemd/system/artex.service`：

```ini
[Unit]
Description=ARTEX long-task governance test build
After=docker.service network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/ARTEX
ExecStart=/opt/ARTEX/artex -addr 0.0.0.0:8787 -proxy 0.0.0.0:8788 -data /opt/ARTEX/data
Restart=on-failure
RestartSec=5
User=root
Environment=ARTEX_CONFIG=/opt/ARTEX/config.json
Environment=ARTEX_SKILL_DIR=/opt/ARTEX/skills

[Install]
WantedBy=multi-user.target
```

启用：

```bash
systemctl daemon-reload
systemctl enable --now artex
```

常用操作：

```bash
systemctl status artex --no-pager -l
journalctl -u artex -f
systemctl restart artex
systemctl stop artex
```

## 8. 启动验证

服务状态：

```text
Active: active (running)
Main PID: 151116
```

健康检查：

```bash
curl http://127.0.0.1:8787/api/health
```

返回：

```json
{"ok":true,"service":"artex","version":"a242dc4"}
```

首页：

```bash
curl -I http://127.0.0.1:8787/
```

返回 `HTTP/1.1 200 OK`，说明内嵌前端已经生效。

监听端口：

```text
0.0.0.0:8787  HTTP / API
0.0.0.0:8788  流量录制代理
```

访问地址：

```text
http://59.152.125.117:8787
```

首次访问会进入 `/setup` 设置管理员密码。

## 9. 本次遇到的问题和处理

第一次启动失败，原因是 systemd 进程在宿主机运行，`artex-postgres` 是 Docker 网络内的容器名，宿主机 DNS 无法解析该名称。

处理方式是把数据库主机地址改为 Docker 网络 IP `172.19.0.2`，然后重启服务。重启后启动、数据库连接和 HTTP 服务均正常。

这个方案可用但有一个注意点：Docker 网络容器重建后 IP 可能变化。如果之后 PostgreSQL 重建导致服务启动失败，需要执行：

```bash
docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' artex-postgres
```

然后把 `/opt/ARTEX/config.json` 中的 `database.host` 更新为该 IP，并执行 `systemctl restart artex`。

更稳妥的后续做法有两个：

1. 在 Docker 网络内固定 PostgreSQL 的容器 IP；
2. 或者给 PostgreSQL 增加一个 `127.0.0.1:5432` 端口映射，配置改为 `127.0.0.1`。

## 10. 重新编译升级

后续更新代码后可以重复以下流程：

```bash
cd /opt/ARTEX
git fetch origin
git checkout codex/long-task-governance
git reset --hard origin/codex/long-task-governance

docker run --rm -v /opt/ARTEX/web:/workspace -w /workspace \
  node:22-bookworm-slim bash -lc 'npm ci && npm run build:static'

rm -rf server/webui/dist
mkdir -p server/webui/dist
cp -a web/out/. server/webui/dist/

docker run --rm \
  -v /opt/ARTEX:/workspace \
  -v /opt/ARTEX/.cache/go:/go/pkg/mod \
  -w /workspace \
  golang:1.26-bookworm \
  /bin/bash -c 'CGO_ENABLED=0 GOOS=linux GOARCH=amd64 /usr/local/go/bin/go build \
    -tags embedui -trimpath \
    -ldflags "-s -w -buildid= -X main.version=<git-short-hash>" \
    -o /workspace/artex ./cmd/artex'

chmod +x /opt/ARTEX/artex
systemctl restart artex
curl http://127.0.0.1:8787/api/health
```

注意：上面 `git reset --hard` 只用于测试环境 `/opt/ARTEX` 这个部署目录，不要在本地开发仓库执行。

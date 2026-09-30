# Solo Drive

一个单人管理、凭链接分享的轻量网盘。Go 服务将前端静态文件打包进同一个可执行文件，SQLite 保存账号、文件和分享元数据，本地磁盘保存文件内容。无需 Redis、MySQL 或对象存储服务。

支持浏览器断点续传、拖拽上传、文件管理、永久或到期分享、撤销链接，以及兼容 HTTP Range 的下载器多连接下载。分享链接持有者可以下载；管理端仅有一个账号，没有开放注册。

## 运行结构

如果服务器已有 HAProxy、Nginx 和其他服务，可以复用现有 HTTPS 入口：

```text
浏览器 / 下载器
      │ HTTPS
已有 HAProxy :443
      │ 仅网盘域名
Nginx 127.0.0.1:8092
      │ 流式反代，保留 HEAD / PATCH / Range
Solo Drive 127.0.0.1:8091 → 容器内 :8091
      ├── SQLite：账号、文件、分享、会话
      └── /data/uploads：文件内容与续传信息
```

网盘容器只向宿主机回环地址发布 `8091`，不会与现有 `80/443` 冲突。镜像以 UID/GID `10001` 运行；命名卷 `solo-drive-data` 持久化整个 `/data`。运行镜像不包含 Node.js 或 Go 编译器。

默认配置面向小型 VPS（例如 2 vCPU、2 GiB 内存）。默认将 Go 内存目标设为 192 MiB，容器内存上限设为 384 MiB；这是资源配置，不是固定消耗。文件始终流式读写，不能把完整文件读入内存。没有应用层上传/下载速度配额，也没有按文件大小设置的业务上限。

## Docker 部署

需要 Docker Engine、Docker Compose 和 `openssl`。以下命令在项目根目录执行。

```bash
sudo ./scripts/init.sh
```

初始化脚本仅生成本地配置和随机密码，不输出密码，也不会启动服务。重复执行会保留现有密码。

- `.env`：从 `deploy/env.example` 复制；将 `SOLODRIVE_PUBLIC_URL` 改为实际 HTTPS 域名，例如 `https://drive.example.com`。
- `secrets/admin_password.txt`：12–72 字节的管理密码；默认生成 48 字符随机密码。
- 默认用户名：`admin`。

需要查看密码时，在服务器终端自行执行：

```bash
sudo cat secrets/admin_password.txt
```

`secrets/` 目录归 root、权限 `0700`；密码文件归 `10001:10001`、权限 `0600`，以只读文件挂载给容器。这样容器非 root 用户可以读取指定文件，其他主机普通用户无法穿透父目录。不要改成依赖 Compose `secrets.mode` 的方案：本地文件挂载会保留主机权限。

构建并启动应用：

```bash
docker compose config --quiet
docker compose up -d --build
docker compose ps
docker compose exec drive /solodrive healthcheck
```

首次构建需要下载固定版本的基础镜像和依赖。镜像使用 Go `1.27.1`、Node.js `24.20.0` 构建；应用镜像标签为 `solo-drive:0.1.0`，不会自动拉取 `latest` 更新。

应用启动后还需要把专用域名接入现有 HTTPS 入口。`deploy/nginx.conf` 是监听 `127.0.0.1:8092` 的独立站点示例，`deploy/haproxy-snippet.cfg` 提供需要合并进现有 HTTPS frontend 的路由及独立 backend。将两个示例中的 `drive.example.com` 替换为实际域名，并确保现有 TLS 证书覆盖它。不要直接用示例覆盖整份 HAProxy 配置。

修改后先执行服务器现有的 `nginx -t` 和 `haproxy -c -f /etc/haproxy/haproxy.cfg` 检查，再 reload 相应服务。当前仓库只提供配置文件；构建或启动 Compose 不会自动修改宿主机反代、DNS 或证书。

### 反代关键配置

- Nginx：`client_max_body_size 0`、`proxy_request_buffering off`、`proxy_buffering off`，避免先把大文件缓存在另一份临时文件中。
- 保留上传使用的 `POST/PATCH/HEAD/DELETE` 方法，保留下载的 `Range`、`If-Range` 和响应 `206/Content-Range`。不要将 Range 请求改写成完整文件响应。
- 示例中读写空闲超时为一小时，表示两次 I/O 之间的允许空闲时间，不是整个文件必须在一小时内完成。HAProxy 的对应空闲超时也要按现有 frontend 检查。
- HAProxy 必须覆盖来自公网的 `X-Forwarded-For`，Nginx 只信任回环上的 HAProxy，再向应用写入确定的 HTTPS 和 Host 信息。不要信任任意客户端提供的转发头。应用分享链接使用显式配置的 `SOLODRIVE_PUBLIC_URL`。
- 分享路径中的随机令牌相当于访问凭证。示例 Nginx 关闭 access log；已有 HAProxy/CDN 日志也应避免保存完整分享 URL。
- 上传下载域名建议 DNS 直连这台服务器；如果使用 CDN 或其他中间代理，需要单独核实其请求体大小、空闲超时和下载政策，应用无法消除中间代理限制。

### 独立机器的可选 HTTPS 入口

只有在另一台机器的 `80/443` 均未被占用时，才使用 Caddy 覆盖文件。已有 HTTPS 入口的机器可以复用上述 HAProxy + Nginx。

设置 `.env` 中的 `SOLODRIVE_PUBLIC_URL` 和 `SOLODRIVE_DOMAIN`，让域名 DNS 指向这台独立机器，允许 `80/tcp`、`443/tcp`；`443/udp` 可用于 HTTP/3。然后运行：

```bash
docker compose -f compose.yaml -f deploy/compose.caddy.yaml --profile standalone-https up -d --build
```

覆盖文件用单独的 profile 显式启用 Caddy，默认 Compose 不会启动它。Caddy 示例固定为 `2.11.4`，版本来源为 [Caddy 官方发行记录](https://github.com/caddyserver/caddy/releases/tag/v2.11.4)。

## 上传、恢复和下载

浏览器默认最多同时上传两个文件；每个文件按 tus 协议顺序上传分块，不并行写同一个文件的不同位置。网络中断时按服务器确认的偏移继续，进程重启后仍能从磁盘恢复已确认进度。浏览器对每个 8 MiB 请求计算 SHA-256，服务端流式校验完整分块，失败会回滚并重试；断连时保留的部分请求未完成整块校验，因此这不等于对所有文件完成了最终的端到端全文件摘要校验。

关闭页面或刷新后，浏览器通常无法继续读取之前选择的本地文件。重新进入管理端，重新选择原来的文件即可恢复匹配的上传。请保留原始文件，不要在上传过程中改写它；跨浏览器或清除站点数据可能丢失本地续传记录。

上传的临时内容与完成后的文件位于同一个数据卷，不需要把分片再复制合并成第二份完整文件。取消未完成上传会释放对应占用及预留。完成上传后才能建立下载分享。

分享默认永久有效，也可以设定到期时间或随时撤销；重命名文件不会改变已有分享链接。分享接收者不需要登录，也不需要安装客户端。复制直链即可交给浏览器、`curl` 或支持 HTTP Range 的多连接下载器。分享密码不在当前版本中。

服务器支持 `GET`、`HEAD`、`Range`、`If-Range` 和文件 ETag，多连接下载由下载器发起；推荐从每文件 4 个连接开始，根据实际线路调整。没有下载次数或速度配额。多个接收者会共享 VPS 总带宽，服务商月流量、磁盘容量和链路速度仍是实际边界。

撤销或到期在每次新的下载请求时检查，**不会强制切断已经开始的响应**。已经建立的单次下载或 Range 请求可能继续完成；后续请求会被拒绝。

## 容量和超大文件

页面容量来自实际数据卷所在文件系统，不能把磁盘容量全部算作网盘专用空间：

```text
可接受的新上传大小 = 当前文件系统可用空间
                    - 系统保留空间
                    - 未完成上传尚未写入的预留字节
```

“磁盘已用”包括其他应用、系统、数据库、已完成文件以及已经落盘的上传部分。未完成任务的待传部分另外预留，防止多个上传同时获准后写满磁盘。默认系统保留空间为 5 GiB，可以通过 `SOLODRIVE_RESERVE_BYTES` 调整。

容量统计会随其他应用写入而变化。预留是应用自己的准入机制，不能约束同盘其他服务，因此上传过程中仍会检查空间，避免耗尽根分区。清理不再需要的未完成任务可收回其空间和预留。

例如，40 GiB 的系统盘不能直接存放 50–100 GB 的单文件。要达到该目标，需要先扩容或挂载足够大的数据盘，并把整个数据目录迁移过去。**50–100 GB 文件以及弱网长时间传输仍需在目标存储和真实线路上验收，不能以小文件测试替代。**

## 配置

| 环境变量 | 默认值 / 作用 |
| --- | --- |
| `SOLODRIVE_ADDR` | 裸机 `127.0.0.1:8091`；Compose 容器内 `0.0.0.0:8091` |
| `SOLODRIVE_DATA_DIR` | 裸机 `./data`；Compose `/data` |
| `SOLODRIVE_PUBLIC_URL` | 生产填实际 HTTPS origin，例如 `https://drive.example.com`，不含子路径 |
| `SOLODRIVE_ADMIN_USER` | `admin` |
| `SOLODRIVE_ADMIN_PASSWORD_FILE` | 必填；指向密码文件，不通过日志或环境变量传明文密码 |
| `SOLODRIVE_RESERVE_BYTES` | `5368709120`，即 5 GiB 系统保留空间 |
| `SOLODRIVE_COOKIE_SECURE` | `true`；只有回环 HTTP 本地测试时设为 `false` |
| `SOLODRIVE_TRUSTED_PROXY_CIDRS` | 裸机默认仅信任回环代理；Compose 加入 Docker 默认私网 172.16.0.0/12，自定义网络时改为实际代理来源 CIDR |
| `SOLODRIVE_SESSION_HOURS` | `168`，登录会话有效期（小时） |

`.env` 由 Docker Compose 读取；裸机二进制不会自动加载它。上传内容、续传信息和 SQLite 数据均需放在可写且持久化的 `SOLODRIVE_DATA_DIR`。同一个数据目录仅运行一个应用进程。

登录失败按来源地址限流；只有来自配置的可信代理时才读取其转发地址。若修改网络结构，需同步收窄/更新可信代理 CIDR，并确保代理覆盖客户端伪造的转发头。

管理端使用 HttpOnly、SameSite=Strict 的会话 cookie，生产启用 Secure；管理写操作受 CSRF 检查保护。账号密码启动时读取并通过 bcrypt 哈希保存。更换密码文件内容或管理用户名后重新启动应用，会撤销旧登录会话；既有分享链接保持原状态。

Docker 下更换密码后，保留文件的 `10001:10001` 所有权和 `0600` 权限，再执行 `docker compose up -d --force-recreate drive`，使文件挂载和配置一起重新读取。密码长度为 12–72 字节（中文等字符可能占多个字节）。

## 本地开发

需要 Go 1.27.1、Node.js 24 和 npm。

```bash
./scripts/init.sh --local
make build
SOLODRIVE_PUBLIC_URL=http://127.0.0.1:8091 \
SOLODRIVE_COOKIE_SECURE=false \
SOLODRIVE_ADMIN_PASSWORD_FILE=./secrets/admin_password.txt \
./solodrive
```

打开 `http://127.0.0.1:8091`。开发初始化把密码和数据保留为当前用户所有权；不要混用同一目录的 Docker/root 初始化和普通用户开发初始化。健康检查使用 `./solodrive healthcheck`，监听地址有变化时传入同样的 `SOLODRIVE_ADDR`。

```bash
make test
make check
```

前端构建结果位于 `web/dist`，构建 Go 程序前必须先生成它。Dockerfile 自动完成前端构建和嵌入。


## 已完成的验证

本次实现通过了 25 个 Go 测试、Go race 并发竞争检查、静态检查以及前端 TypeScript 检查和构建。覆盖续传偏移、重启恢复、分块摘要失败回滚、空间预留、CSRF、分享到期与撤销、八连接 Range 下载内容一致性等行为。

浏览器端实际上传 20 MiB 文件，暂停后刷新页面，再选择原文件续传；下载结果与原文件的 SHA-256 一致。桌面和手机页面均已检查，截图及结果见 [verification](verification/)。

该浏览器测试中，独立进程空闲 RSS 约 11 MiB，采样峰值约 14 MiB；Linux amd64 可执行程序约 12.3 MB。这是小规模测试的观测值，不代表生产资源上限或几十 GB 传输的性能保证。100 GiB 文件偏移由稀疏文件测试验证，尚未完成 50–100 GB 全量实传。

Docker 镜像约 18.1 MB，16 MiB 容器端到端测试已通过；容器重建后可继续上传，八连接下载重组后的 SHA-256 与原文件一致。详情见 [容器验证记录](verification/docker-results.json)。

容器端到端测试脚本可在构建镜像后重复运行：

```bash
python3 scripts/docker-smoke.py
```

测试创建独立临时容器和数据卷，检查 16 MiB 上传、重建容器后的续传、八连接下载、分享及撤销，结束时清理测试资源。仅监听回环地址，不读取生产账号或数据；要求 Python 3、Docker 和 Compose v2。

## 备份与恢复

备份必须同时覆盖数据库、完整文件、未完成上传的元数据，以及管理密码和部署配置。不要在上传或数据库写入过程中直接复制正在使用的 SQLite 文件；仅备份 `drive.db` 而遗漏 WAL 和上传文件可能得到不一致状态。

最简单可靠的方式是停写备份：

1. 暂停上传、通知接收者此次维护会中断下载。
2. `docker compose stop -t 90 drive`，确认应用已经停止。
3. 对整个 `solo-drive-data` 卷做快照或完整归档，包含 `drive.db`、可能存在的 `drive.db-wal` / `drive.db-shm`、`.lock` 和整个 `uploads/`。
4. 另行备份 `.env` 和 `secrets/admin_password.txt`，限制备份访问权限；如使用 Caddy，也备份它的数据卷。
5. `docker compose start drive`，确认健康后恢复使用。

Docker 本地卷目录可用以下只读命令查询：

```bash
docker volume inspect solo-drive-data --format '{{ .Mountpoint }}'
```

可以在应用停止后，以 root 使用 `tar -cpf /另一块磁盘/solo-drive-data.tar -C 查询到的卷目录 .` 归档。将备份放到独立磁盘或异地存储，避免备份本身填满当前根分区；原地副本不能应对整台 VPS 丢失。

恢复到新机器时，先构建相同应用版本并创建空的 `solo-drive-data` 卷，保持应用停止，把整个归档解压到该卷目录。恢复 `.env` 和密码文件，恢复数据卷 `10001:10001` 所有权、密码文件 `0600` 及父目录 `0700` 权限，然后启动并执行健康检查。不要把恢复文件合并进正在运行的实例。

迁移必须保留全部数据及 SQLite 记录，原有分享令牌才会继续有效。若保持同一域名和 HTTPS origin，仅切换 DNS/反代入口，原分享 URL 可继续使用；换域名会改变 URL，需要更新对外分享链接。未完成上传是否能从原浏览器直接继续，还取决于是否保留原站点 origin 和浏览器的续传记录。

升级前执行一致性备份，固定目标镜像/应用版本，验证后再切换。不要接入无人值守的 `latest` 自动更新流程。

# WorkBuddy Helper

腾讯 WorkBuddy / CodeBuddy 的**多账户签到与积分看板**，支持 Windows 桌面版与 Docker/Linux 容器两种运行方式。

> ⚠️ **免责声明**：本项目仅用于**管理你本人有权操作的账号**。请遵守腾讯服务条款，不要用于批量注册、共享令牌、绕过验证码或风控。使用风险自负。

---

## 目录

- [功能特性](#功能特性)
- [界面预览](#界面预览)
- [下载发布包](#下载发布包)
- [快速开始（Windows）](#快速开始windows)
- [Docker 部署](#docker-部署)
- [命令行选项](#命令行选项)
- [环境变量](#环境变量)
- [数据与加密](#数据与加密)
- [账号迁移（Windows ↔ 容器）](#账号迁移windows--容器)
- [从源码构建](#从源码构建)
- [项目结构](#项目结构)
- [常见问题](#常见问题)
- [更新日志](#更新日志)
- [开源协议](#开源协议)

---

## 功能特性

- **多账户管理**：通过腾讯官方登录页逐个添加账号，程序内**不输入、不保存密码**。
- **批量签到**：一键对全部启用账号签到，也支持单账号操作。
- **积分查询**：按有效套餐聚合展示余额 / 已用 / 总量，保留每个套餐明细。
- **幂等保护**：自动识别"今日已签到"，同一北京时间日期内不会重复请求。
- **版本切换 = 账号列表切换**：页头下拉在**国内版 / 国际版**间切换，两个版本各自独立账号容器：
  - **国内版**（默认）：每日签到领取积分，行为与历史版本一致；国内账号列表。
  - **国际版**（workbuddy.ai）：向服务端发送对话以申请当日活跃积分；会话返回有效回答不等于积分已经到账，最终发放由服务端决定。国际账号列表的请求参照国际版客户端格式：
    - 端点 `https://www.workbuddy.ai/v2/chat/completions`（流式，system 消息必须在前，否则 400 code=11128）
    - 客户端特征头：`WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/2.137.1` UA + `X-IDE-Type/Name: WorkBuddy` + `X-Product: SaaS` + `X-User-Id` + `X-Domain`（逆向自国际版客户端 CLIENT_INFO 注入链路，plans-usage 页识别为 WorkBuddy 客户端）
    - 模型可选 `hy3` / `hy4-preview`；两者都是 `onlyReasoning` 纯推理模型，推理内容同样占用输出预算，活跃请求统一下发 `max_tokens=4096`（与真实客户端同量级），避免推理耗尽预算导致 `content` 为空而被误判为无效会话
    - 积分查询走 `https://www.codebuddy.ai/v2/billing/meter/get-user-resource`（实测 credits 单位）
    - 国际版账号通过「添加账户」在 WorkBuddy 登录页完成浏览器授权——授权页为 `https://www.workbuddy.ai/login?platform=workbuddy-ai&state=...`（对齐国际版客户端登录链路：state → 授权页 → token 轮询 → account，全部走 workbuddy.ai 同域）。
  - **积分延迟复核**：国际版积分非即时到账；会话发出前先校验本程序保存的服务端登录态及积分查询，收到有效对话回答后按设置延迟（默认 840 分钟=14h，可调 60-1440）自动复查积分变化，无变化最多重试 6 次；界面区分“已发送待复核”“积分已到账”和“复核未到账”。
  - **活跃链路对齐真实客户端**：每次活跃都**新开一个云端会话**，先 `POST /console/as/conversations/v2` 登记会话（`conversationOrigin=legacy_workbuddy_local`），再以服务端返回的会话 ID 作为 `X-Conversation-ID` 发送对话，并把 `chat_request_send` 遥测事件（`extra_vars.growthEvent`）随请求一起上报——让服务端把该账号识别为「真实客户端正在活跃」，而不是在同一个旧会话上反复发送。会话登记失败不阻断对话，但会在运行日志中告警「云端会话登记失败（活跃可能不被认定）」。
- **一键刷新积分**：位于「全部签到」左侧，立即刷新**当前版本列表**全部启用账号的积分（不触发签到/活跃）。
- **活跃提示词自定义**：国际版设置区可自定义活跃会话的消息内容（留空使用默认「你是谁，谁开发你的，现在是什么时间，什么天气」）。过短问候（如 hi）可能被服务端判定为非有效会话；每次活跃（含定时任务）均使用当前保存的提示词；手动点击「活跃」按钮会强制重发（不受"今日已活跃"跳过限制）。
- **网络链路双通道**：
  - **国内版**：永远直连 + **固定公共 DNS**（`data/dns.conf`，默认 223.5.5.5 / 119.29.29.29 / 1.1.1.1），完全不依赖容器内 Docker DNS——容器 DNS 瘫痪时国内版依旧正常。
  - **国际版**：直连或代理（设置面板可配，协议 `socks5`/`socks5h`/`http`/`https`，支持认证）。代理独立持久化到 `data/proxy.conf`（容器重建/数据迁移不丢），**仅对国际版链路生效**。
  - 解析兜底：国际版直连路径解析失败时自动回退固定公共 DNS 再解析。
- **每日定时**：可开启每日北京时间自动执行（国内版签到 / 国际版活跃，需程序/容器保持运行）。
- **本地看板**：响应式 Web 界面、运行日志、账号启停、显示名编辑。
- **双加密后端**：Windows 用 DPAPI，Linux/容器用 `key.bin` + AES-256-GCM。
- **跨平台迁移**：`--export-portable` / `--import` 支持在 Windows 与容器之间搬运账号。
- **单实例保护**：同一数据目录只允许一个进程运行（Windows 用文件独占，Unix 用 `flock`）。

---

## 界面预览

看板包含三部分：

1. **页头**：版本切换下拉（国内版 / 国际版，切换对应账号列表）、连接状态、运行日志、添加账户。
2. **总览卡片**：当前版本账号数、今日完成数（签到/活跃）、总积分、启用数。
3. **账号列表**：显示名、状态、今日签到/活跃、积分余额与套餐明细，以及「签到或活跃 / 查积分 / 启停 / 重命名 / 删除」操作按钮。
4. **侧边栏**：每日任务版本与时间设置、运行日志；顶部「一键刷新积分」与「全部签到/全部活跃」。

---

## 下载发布包

不想自己编译的话，直接到 Releases 下载单文件程序（最新已发布版本 **v1.0.8**）：

| 文件 | 说明 |
|---|---|
| [`WorkBuddyHelper-windows-amd64.exe`](https://github.com/autumnsentiment/workbuddy-helper/releases/latest) | Windows 64 位免安装单文件，双击即用，无需 Go 或任何运行库 |
| [`WorkBuddyHelper-linux-amd64`](https://github.com/autumnsentiment/workbuddy-helper/releases/latest) | Linux 64 位静态二进制（amd64），可直接在服务器运行 |
| [`WorkBuddyHelper-source.zip`](https://github.com/autumnsentiment/workbuddy-helper/releases/latest) | 完整源码压缩包（含 Dockerfile / docker-compose.yml，不含任何账号数据） |
| `SHA256SUMS.txt` | 校验值，用于确认下载文件未被篡改 |

```powershell
# 下载后校验完整性（应与 SHA256SUMS.txt 中的值一致）
certutil -hashfile WorkBuddyHelper-windows-amd64.exe SHA256
```

> ✅ **发布包不含任何账号数据。** 二进制是纯程序本体，没有任何令牌、账号或密钥。
> 首次运行会在数据目录创建**空的**存储，你需要自己通过腾讯官方登录页添加账号。
> 想自行确认的话，启动后打开看板即可看到账号数为 0。

---

## 快速开始（Windows）

1. 下载 [`WorkBuddyHelper-windows-amd64.exe`](https://github.com/autumnsentiment/workbuddy-helper/releases/latest)（或自行[从源码构建](#从源码构建)）并双击运行。程序会启动一个**仅监听 `127.0.0.1`** 的本地服务，并自动打开浏览器。
2. 点击「添加账户」，在腾讯官方登录页完成登录/授权（验证码、设备验证等由你本人处理）。
3. 回到看板，点击单账号「签到」或顶部「全部签到」。
4. 在账号行点击「积分」按钮刷新余额。
5. 需要每日自动执行时，在右侧打开「自动签到」并设置北京时间。

默认数据目录：`%LOCALAPPDATA%\WorkBuddyHelper`

> 使用 `--host 0.0.0.0` 可让局域网内其他设备访问看板，请仅在可信网络下使用，
> 详见 [端口与访问控制](#3-端口与访问控制)。

---

## Docker 部署

### 1. 获取镜像

**方式 A：直接拉取已构建镜像（推荐，无需本地 Go 环境）**

每次打 `v*` 标签发布时，GitHub Actions 会自动构建并推送多架构镜像到 GitHub Container Registry：

```bash
docker pull ghcr.io/autumnsentiment/workbuddy-helper:latest
# 或锁定具体版本
docker pull ghcr.io/autumnsentiment/workbuddy-helper:1.0.8
```

镜像同时包含 `linux/amd64` 与 `linux/arm64` 两个架构，Docker 会自动选择匹配当前机器的版本。
如需使用该镜像，把 `docker-compose.yml` 中的 `build: .` 注释掉、并将 `image:` 改为
`ghcr.io/autumnsentiment/workbuddy-helper:latest` 即可。

**方式 B：本地构建镜像**

在仓库根目录（含 `go.mod` / `cmd/` / `internal/`）执行：

```bash
docker build -t workbuddy-helper:latest .
```

镜像采用多阶段构建：`golang:1.26-alpine` 编译 → `alpine:3.20` 运行，静态二进制、体积小。首次构建会按 `go.mod` 下载 Go 模块依赖，运行镜像不携带 Go 工具链。

### 2. 启动容器

**方式 A：docker compose（推荐）**

```bash
# 准备数据目录（可为空，容器会自动初始化）
mkdir -p ./data

docker compose up -d
docker compose logs -f        # 查看启动日志
```

**方式 B：docker run**

```bash
docker run -d --name workbuddy-helper --restart unless-stopped \
  -p 18080:18080 \
  -e TZ=Asia/Shanghai \
  -e WBH_HOST=0.0.0.0 \
  -e WBH_DATA=/data \
  -v "$PWD/data:/data" \
  workbuddy-helper:latest
```

启动后访问：<http://127.0.0.1:18080>

### 3. 端口与访问控制

| 场景 | `WBH_HOST` | 端口映射 | 说明 |
|---|---|---|---|
| 仅本机 | `0.0.0.0` | `127.0.0.1:18080:18080` | 容器内监听全部网卡，但宿主机只绑回环，外部不可达 |
| 局域网 | `0.0.0.0` | `18080:18080` | 局域网内均可访问 |

> **安全提示**：看板本身**没有登录口令**，且其中包含账号令牌。若映射到 `0.0.0.0`，请确保仅在**受信任的内网**使用，或自行在前置 Nginx 上加认证；通过反向代理域名访问时，同时设置 `WBH_ALLOWED_HOSTS`，例如 `panel.example.com:8443`。

也可以直接启用内置 Basic Auth：同时设置 `WBH_AUTH_USER` 与 `WBH_AUTH_PASSWORD`。浏览器首次访问会弹出登录框；`/healthz` 保留匿名访问供 Docker 健康检查使用。Basic Auth 本身不加密传输，跨网络访问时仍应放在 HTTPS 反向代理后。密码不会写入 `state.bin`，请通过容器环境变量或 secret 注入，不要提交到仓库。

> **注意**：容器内 `WBH_HOST` 必须为 `0.0.0.0`，否则 Docker 端口映射（DNAT 到容器 eth0）无法投递到仅监听回环的进程。

### 4. 健康检查

容器内置健康检查，访问 `GET /healthz` 返回 `200 ok`：

```bash
docker inspect --format '{{.State.Health.Status}}' workbuddy-helper
curl -s http://127.0.0.1:18080/healthz
```

### 5. 每日自动签到

在界面右侧打开「自动签到」并设置北京时间（默认 `09:15`）。容器需保持运行；`restart: unless-stopped` 会随 Docker 自动重启。

也可用一次性模式（适合外部 cron）。由于数据目录有单实例锁，不能在常驻容器运行时直接 `docker exec` 启动第二个进程；请先停止常驻服务，再用临时容器执行：

```bash
docker compose stop
docker run --rm --name workbuddy-helper-once \
  -e TZ=Asia/Shanghai -e WBH_DATA=/data \
  -v "$PWD/data:/data" \
  workbuddy-helper:latest --once --no-open
docker compose start
```

---

## 命令行选项

```text
workbuddy-helper [选项]

  --no-open                启动服务但不自动打开浏览器
  --port 18080             指定监听端口（默认 0 = 自动选择空闲端口）
  --host 127.0.0.1         监听地址（默认 127.0.0.1；容器内需设为 0.0.0.0）
  --data-dir DIR           指定数据目录
  --once                   按当前每日任务版本对所有启用账号执行一次签到或活跃后退出
  --export-portable DIR    导出当前数据为可移植副本（供 Docker 使用）
  --import DIR             从 DIR 导入账号到数据目录（按 UID 去重）
```

---

## 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `WBH_DATA` | Windows：`%LOCALAPPDATA%\WorkBuddyHelper`<br>容器：`/data` | 数据目录 |
| `WBH_HOST` | `127.0.0.1` | 监听地址；容器内需为 `0.0.0.0` |
| `WBH_ALLOWED_HOSTS` | 空 | 反向代理域名白名单，逗号分隔（例如 `panel.example.com:8443`）；通配监听默认只接受 IP/localhost |
| `WBH_AUTH_USER` | 空 | 内置 Basic Auth 用户名；需与 `WBH_AUTH_PASSWORD` 同时设置才启用 |
| `WBH_AUTH_PASSWORD` | 空 | 内置 Basic Auth 密码；建议通过 Docker secret 或受保护的环境注入 |
| `TZ` | 容器内 `Asia/Shanghai` | 时区（定时任务按北京时间计算，内部固定使用 UTC+8） |

---

## 数据与加密

数据目录中的文件：

| 文件 | 说明 |
|---|---|
| `state.bin` | 账号、令牌、积分、日志（加密存储） |
| `key.bin` | 容器/Linux 端的 256 位随机密钥（`0600` 权限） |
| `proxy.conf` | 国际版代理地址；`direct` 表示直连（明文，可能包含代理用户名和密码） |
| `dns.conf` | 国内版固定 DNS 列表；`system` 表示使用系统 DNS |
| `instance.lock` | 进程锁，防止同一数据目录被多个进程同时写入 |

### 两种加密后端

`state.bin` 以**魔术前缀**自描述加密方式：

| 前缀 | 加密方式 | 使用场景 |
|---|---|---|
| `WBH1` | Windows DPAPI（绑定当前 Windows 用户/机器） | Windows 原生数据目录 |
| `WBK1` | AES-256-GCM + 目录内 `key.bin` | Linux / Docker 容器 / 可移植副本 |

程序**读取时两种前缀都支持**，写入时使用当前平台的默认后端（Windows → DPAPI，其他 → 密钥文件）。

> **重要**：`key.bin` 与 `state.bin` 必须成对备份。`key.bin` 丢失后 `state.bin` 将**无法解密**，只能重新添加账号。

> **代理凭据**：`proxy.conf` 为便于容器启动恢复而采用明文保存。若代理 URL 含 `user:pass@`，请限制 `data` 目录权限，不要提交到 Git、镜像或公开备份。
> Web/API 回显会移除代理用户名和密码；保存其他设置时不会覆盖文件中的认证信息，只有实际编辑代理字段才会更新它。

---

## 账号迁移（Windows ↔ 容器）

DPAPI 数据绑定 Windows 用户/机器，**Linux 容器无法直接解密**。迁移需先导出为可移植副本：

### 1. 在 Windows 上导出

```powershell
WorkBuddyHelper.exe --export-portable D:\wbh-data
```

该命令会解密当前 `state.bin`，用**新生成的 `key.bin`** 以 `WBK1` 格式重新加密写入 `D:\wbh-data`；如果源目录存在 `proxy.conf` 或 `dns.conf`，也会一并复制到可移植目录。

### 2. 在容器中挂载

```bash
docker run -d --name workbuddy-helper --restart unless-stopped \
  -p 18080:18080 \
  -e WBH_HOST=0.0.0.0 \
  -v /path/to/wbh-data:/data \
  workbuddy-helper:latest
```

### 3. 反向导入（容器数据 → Windows）

```powershell
# 在 Windows 上，把可移植目录导入到本地数据目录（按 UID 去重）
WorkBuddyHelper.exe --import D:\wbh-data
```

> 迁移时请确保 `key.bin` 与 `state.bin` **一同复制**，缺一不可。`proxy.conf` / `dns.conf` 是可选配置文件；其中 `proxy.conf` 可能包含代理认证信息，请按敏感凭据保护。

---

## 从源码构建

需要 **Go 1.26+**；构建时会按 `go.mod` 下载 `golang.org/x/net` 模块依赖。

```bash
# 运行测试与静态检查
go test ./...
go vet ./...

# Windows 构建
go build -trimpath -buildvcs=false -ldflags "-s -w" -o WorkBuddyHelper.exe ./cmd/workbuddy-helper

# Linux 构建
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -ldflags "-s -w" -o workbuddy-helper-linux ./cmd/workbuddy-helper
```

Windows 用户也可直接运行仓库内的 `build.ps1`。

---

## 项目结构

```text
workbuddy-helper/
├── cmd/workbuddy-helper/
│   ├── main.go                 # 入口：参数解析、服务启动、迁移子命令
│   └── web/                    # 内嵌前端资源（embed）
│       ├── index.html
│       ├── app.js
│       ├── styles.css
│       └── lucide.min.js       # 图标库（ISC 协议，见 lucide-LICENSE.txt）
├── internal/
│   ├── client/                 # WorkBuddy/CodeBuddy HTTP 客户端（登录、刷新、签到、积分）
│   ├── instance/               # 单实例锁（Windows 文件独占 / Unix flock）
│   ├── model/                  # 数据模型（账号、设置、日志、状态）
│   ├── server/                 # HTTP 服务、CSRF 校验、安全响应头
│   ├── service/                # 业务逻辑（签到、积分、定时、并发控制）
│   └── store/                  # 加密存储（DPAPI / AES-GCM 密钥文件 / 迁移）
├── Dockerfile                  # 多阶段构建
├── docker-compose.yml
├── build.ps1
└── LICENSE
```

### 接口一览

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | 看板页面（注入一次性 app-token） |
| GET | `/healthz` | 健康检查 |
| GET | `/api/state` | 账号与汇总（不含令牌；账号包含可选的 `tokenExpiresAt`，为 Unix 秒时间戳） |
| GET | `/api/logs?limit=` | 运行日志 |
| POST | `/api/login/start` | 开始登录（返回授权 URL） |
| POST | `/api/login/poll` | 轮询登录结果 |
| POST | `/api/run-all` | 执行当前版本全部账号任务（国内版签到 / 国际版活跃） |
| POST | `/api/refresh-points` | 刷新当前版本全部账号积分 |
| PUT | `/api/settings` | 更新定时设置 |
| POST | `/api/accounts/{id}/checkin` | 单账号签到 |
| POST | `/api/accounts/{id}/active` | 单账号发送国际版活跃会话 |
| POST | `/api/accounts/{id}/points` | 单账号积分刷新 |
| PUT | `/api/accounts/{id}` | 修改显示名 / 启停 |
| DELETE | `/api/accounts/{id}` | 删除账号 |

写操作需携带页面注入的 `X-App-Token`，且 `Origin` 必须与请求 `Host` 同源（或为本机回环），以防御 CSRF。

`tokenExpiresAt` 仅表示访问令牌的预计到期时间，服务端会优先读取 JWT 的 `exp` 声明，无法解析时回退到登录/刷新接口提供的 `expiresIn`。接口不会返回 JWT、Access Token 或 Refresh Token 本体；旧账号或上游未提供有效期时会省略该字段。

---

## 常见问题

**Q：界面提示"请求来源不受信任"？**
A：这是本地会话与 CSRF 保护。请通过浏览器访问页面（而非直接调 API），并确认 `Origin` 与访问地址同源。若通过反向代理访问，请确保代理正确透传 `Host` 头，并把外部域名加入 `WBH_ALLOWED_HOSTS`。

**Q：容器启动后宿主机访问不了？**
A：容器内 `WBH_HOST` 必须是 `0.0.0.0`。若为 `127.0.0.1`，Docker 端口映射无法投递。

**Q：登录失效怎么办？**
A：账号状态会变为 `needs_login`，在界面删除该账号后重新添加即可。令牌刷新失败不会影响其他账号。

**Q：国际版会话已发送，为什么没有收到积分？**
A：程序校验的是保存的服务端令牌和对话响应，无法检查另一台机器上的 WorkBuddy 桌面客户端是否正在登录；Docker 容器也无法读取桌面客户端本地会话。对话返回有效回答只表示请求被受理，不保证活动资格或积分发放。请查看“积分已到账”状态或运行日志中的复核结果；若多次复核仍未到账，应在官方客户端确认账号与活动规则。

**Q：能复制 `state.bin` 到别的机器直接用吗？**
A：不能。`WBH1`（DPAPI）绑定原 Windows 用户/机器；`WBK1` 需要配套的 `key.bin`。请使用 `--export-portable` 导出。

**Q：接口会不会变？**
A：签到与积分使用 WorkBuddy 客户端当前调用的 billing 接口；腾讯未公开面向个人用户的稳定 OpenAPI，**接口可能随时调整**。程序会在接口结构变化时提示错误并暂停该账号。

---

## 更新日志

### v1.0.8（2026-10-15）

- **活跃账号链路对齐真实客户端（本次核心修复）**：每次活跃都新开一个云端会话，先 `POST /console/as/conversations/v2` 登记会话，再以服务端返回的会话 ID 发送对话；会话 ID 每次不同，不再复用同一个旧会话。实测该改动后国际版活跃积分可正常到账。
- **活跃遥测事件补齐**：`chat_request_send` 事件按桌面端格式放在请求体 `extra_vars.growthEvent` 随对话一起上报（独立 `/v2/report` 降级为备用通道），事件字段与上报头（`X-Request-ID` / `X-Product` / 客户端 UA / 企业头）对齐真实客户端。
- **修复活跃会话被误判无效**：`hy3` / `hy4-preview` 均为 `onlyReasoning` 纯推理模型，推理内容同样占用输出预算；活跃请求 `max_tokens` 从 64/160 提升到 `4096`（与真实客户端同量级），修复推理耗尽预算导致 `content` 为空、进而被判定「未返回有效回答」的问题。校验同时接受 `reasoning_content`，并兼容整段 JSON 与 content 数组两种响应格式。
- **显示 JWT 令牌有效期**：账号列表展示访问令牌的预计到期时间（优先读取 JWT `exp`，回退上游 `expiresIn`），并按小时自动刷新，便于提前发现登录即将失效。
- **识别「登录处理中」状态**：上游在用户尚未完成授权时返回 HTTP 200 + `code=11217`（`login ing...`），现在会被正确识别为「等待授权」并继续轮询，不再直接报错。
- 预检失败不再发送活跃请求；只有收到有效回答才认定会话成功；界面区分「已发送待复核」「积分已到账」「复核未到账」。

### v1.0.7（2026-09-28）

- 国际版活跃前强制校验服务端登录态和积分查询；预检失败时不再发送活跃请求，账号会显示登录失效或具体错误。
- 解析流式对话响应，只有收到有效回答才认定会话成功；空流、业务错误和无效响应不再误报“活跃成功”。
- 检查备用遥测上报结果并记录警告；区分“已发送待复核”“积分已到账”和“复核未到账”。
- 明确说明 Docker 无法读取宿主机 WorkBuddy 桌面客户端的本地登录状态，积分发放仍由服务端活动规则决定。

### v1.0.6（2026-09-23）

- 修复代理保存后页面刷新回退、无法显式切回直连，以及 `proxy.conf=direct` 重启后旧代理复活的问题；设置接口现在返回实际持久化后的权威配置（代理认证信息不回显）。
- 国际版登录与后续请求统一复用代理链路，国内版登录与请求统一复用固定 DNS 直连链路；`dns.conf=system` 现在会真实使用系统 DNS。
- 代理热更新改为原子替换客户端，避免并发请求期间修改 `http.Client.Transport`；补充 HTTPS 同源访问、网络响应读取和代理地址校验。
- 统一积分复核默认值为 840 分钟（可调 60-1440），无变化最多复核 6 次。
- 登录轮询增加会话占用、UID 去重和账号上限原子检查，避免并发请求重复添加或覆盖账号。
- 积分查询支持多页套餐并校验分页完整性；静态前端资源禁用缓存，部署更新后立即加载新逻辑。
- 增加监听地址与 `Host` 白名单校验，通配监听默认只接受 IP/localhost，反向代理域名通过 `WBH_ALLOWED_HOSTS` 显式配置；批量任务增加全局互斥，避免重复并发执行。
- 国内/国际账号操作增加版本边界校验；启动时归一化旧设置；状态主文件损坏时校验并回退有效备份，未来版本状态不会被旧备份降级。
- 可选请求体保持向后兼容；登录授权会话限制为最多 32 个并自动清理过期会话；协议错误回显统一截断。
- 国际版直连在系统 DNS 返回不可达地址时会再次尝试固定 DNS；可移植导出会保留 `proxy.conf` / `dns.conf`；新增可选内置 Basic Auth 与 GitHub Actions / Docker 构建检查。

### v1.0.5（2026-09-17）

- **网络链路重写**：国内版独立 transport——固定公共 DNS 直连（`data/dns.conf` 可调，默认 223.5.5.5 / 119.29.29.29 / 1.1.1.1），完全不依赖容器 Docker DNS，容器 DNS 瘫痪时国内版依旧正常；国际版直连路径解析级两级兜底（系统解析失败→公共 DNS 重查→IP 直连）。
- **全部签到/活跃按钮按当前视图版本执行**：国内版界面=全部账号签到，国际版界面=全部账号活跃（按钮文案「全部活跃」），修复跨版本时空转不生效的问题。
- **扣费页请求内容展示**：活跃消息按真实客户端格式以 `<user_query>` 包装，plans-usage「请求」列正常显示提示词（此前显示 `-`）。
- 活跃会话完整对齐真实客户端：会话维度头（Conversation/Request ID）+ `chat_request_send` 遥测上报闭环。

### v1.0.4（2026-09-14）

- **活跃提示词自定义**：默认提示词改为实质提问「你是谁，谁开发你的，现在是什么时间，什么天气」（"hi" 会被判定为非有效会话）；国际版设置区可自定义，留空用默认，保存后每次活跃皆使用自定义。
- 手动「活跃」触发改为强制发送（绕过当日已活跃跳过），便于验证；定时任务仍按每日一次。

### v1.0.3（2026-09-14）

- **自定义网络代理**：设置面板新增代理配置（socks5/socks5h/http/https，支持认证），保存即热生效、重启持久化；解决服务器无法直连 workbuddy.ai 等国际域名的问题。
- Docker 构建基础镜像升级 golang:1.26，补齐 go.sum 拷贝。

### v1.0.2（2026-09-14）

- **版本切换 = 账号列表切换**：页头下拉在国内版 / 国际版间切换，两套账号容器独立管理；每日定时任务按「每日任务版本」执行对应列表（国内版签到 / 国际版活跃）。
- **国际版活跃获取积分**：以 WorkBuddy 桌面客户端完整特征（UA `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/2.137.1`、`X-IDE-Type/Name: WorkBuddy`、`X-Product: SaaS`）向 `workbuddy.ai/v2/chat/completions` 发送一次会话（hy3 / hy4-preview，默认 `max_tokens=64`，较长提示词为 `160`），当日即认定为活跃账号。
- **国际版登录授权**：「添加账户」按当前版本拉取对应授权页——国际版为 `https://www.workbuddy.ai/login?platform=workbuddy-ai&state=...`（state → 授权页 → token 轮询 → account 全链路对齐客户端）。
- **积分延迟复核**：活跃后按当时默认 60 分钟（可调 5-720）自动复查积分到账，无变化最多重试 3 次，日志记录积分增量；后续版本已调整默认值与范围，当前值见上方 v1.0.6 说明。
- **一键刷新积分**：位于「全部签到」左侧，立即刷新当前版本列表全部账号积分。
- `/healthz` 返回版本号（`ok 1.0.2`），便于部署验证。

### v1.0.0（2026-09-11）

- 首个公开版本：多账户管理、每日签到、积分看板、定时任务、Windows 免安装版与 Docker 部署。

---

## 开源协议

本项目基于 [MIT License](LICENSE) 开源。

```text
MIT License

Copyright (c) 2026 autumnsentiment

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

### 第三方组件

| 组件 | 协议 | 说明 |
|---|---|---|
| [Lucide](https://lucide.dev/) | ISC | 界面图标库，许可证见 `cmd/workbuddy-helper/web/lucide-LICENSE.txt` |

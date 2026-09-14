# WorkBuddy Helper

腾讯 WorkBuddy / CodeBuddy 的**多账户签到与积分看板**，支持 Windows 桌面版与 Docker/Linux 容器两种运行方式。

> ⚠️ **免责声明**：本项目仅用于**管理你本人有权操作的账号**。请遵守腾讯服务条款，不要用于批量注册、共享令牌、绕过验证码或风控。使用风险自负。

---

## 目录

- [功能特性](#功能特性)
- [界面预览](#界面预览)
- [下载免安装版（Windows）](#下载免安装版windows)
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
  - **国际版**（workbuddy.ai）：每日积分按"活跃账号"发放——当天至少发起一次对话即视为活跃；国际账号列表。请求完全对齐国际版客户端逆向结果：
    - 端点 `https://www.workbuddy.ai/v2/chat/completions`（流式，system 消息必须在前，否则 400 code=11128）
    - 客户端特征头：`WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/2.137.1` UA + `X-IDE-Type/Name: WorkBuddy` + `X-Product: SaaS` + `X-User-Id` + `X-Domain`（逆向自国际版客户端 CLIENT_INFO 注入链路，plans-usage 页识别为 WorkBuddy 客户端）
    - 模型可选 `hy3` / `hy4-preview`（`max_tokens=16`，几乎不消耗积分）
    - 积分查询走 `https://www.codebuddy.ai/v2/billing/meter/get-user-resource`（实测 credits 单位）
    - 国际版账号通过「添加账户」在 WorkBuddy 登录页完成浏览器授权——授权页为 `https://www.workbuddy.ai/login?platform=workbuddy-ai&state=...`（对齐国际版客户端登录链路：state → 授权页 → token 轮询 → account，全部走 workbuddy.ai 同域）。
  - **积分延迟复核**：国际版积分非即时到账，活跃成功后按设置延迟（默认 60 分钟，可调 5-720）自动复查积分变化，无变化最多重试 3 次；日志记录"活跃前 → 活跃后"的积分增量，可用于实测到账延迟。
- **一键刷新积分**：位于「全部签到」左侧，立即刷新**当前版本列表**全部启用账号的积分（不触发签到/活跃）。
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

## 下载免安装版（Windows）

不想自己编译的话，直接到 Releases 下载单文件程序（当前版本 **v1.0.2**）：

| 文件 | 说明 |
|---|---|
| [`WorkBuddyHelper-windows-amd64.exe`](https://github.com/autumnsentiment/workbuddy-helper/releases/latest) | Windows 64 位免安装单文件，双击即用，无需 Go 或任何运行库 |
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

### 1. 构建镜像

在仓库根目录（含 `go.mod` / `cmd/` / `internal/`）执行：

```bash
docker build -t workbuddy-helper:latest .
```

镜像采用多阶段构建：`golang:1.23-alpine` 编译 → `alpine:3.20` 运行，静态二进制、体积小、无外部 Go 依赖。

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

> **安全提示**：看板本身**没有登录口令**，且其中包含账号令牌。若映射到 `0.0.0.0`，请确保仅在**受信任的内网**使用，或自行在前置 Nginx 上加认证。

> **注意**：容器内 `WBH_HOST` 必须为 `0.0.0.0`，否则 Docker 端口映射（DNAT 到容器 eth0）无法投递到仅监听回环的进程。

### 4. 健康检查

容器内置健康检查，访问 `GET /healthz` 返回 `200 ok`：

```bash
docker inspect --format '{{.State.Health.Status}}' workbuddy-helper
curl -s http://127.0.0.1:18080/healthz
```

### 5. 每日自动签到

在界面右侧打开「自动签到」并设置北京时间（默认 `09:15`）。容器需保持运行；`restart: unless-stopped` 会随 Docker 自动重启。

也可用一次性模式（适合外部 cron）：

```bash
docker exec workbuddy-helper /app/workbuddy-helper --once
```

---

## 命令行选项

```text
workbuddy-helper [选项]

  --no-open                启动服务但不自动打开浏览器
  --port 18080             指定监听端口（默认 0 = 自动选择空闲端口）
  --host 127.0.0.1         监听地址（默认 127.0.0.1；容器内需设为 0.0.0.0）
  --data-dir DIR           指定数据目录
  --once                   对所有启用账号执行一次签到后退出
  --export-portable DIR    导出当前数据为可移植副本（供 Docker 使用）
  --import DIR             从 DIR 导入账号到数据目录（按 UID 去重）
```

---

## 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `WBH_DATA` | Windows：`%LOCALAPPDATA%\WorkBuddyHelper`<br>容器：`/data` | 数据目录 |
| `WBH_HOST` | `127.0.0.1` | 监听地址；容器内需为 `0.0.0.0` |
| `TZ` | 容器内 `Asia/Shanghai` | 时区（定时任务按北京时间计算，内部固定使用 UTC+8） |

---

## 数据与加密

数据目录中的文件：

| 文件 | 说明 |
|---|---|
| `state.bin` | 账号、令牌、积分、日志（加密存储） |
| `key.bin` | 容器/Linux 端的 256 位随机密钥（`0600` 权限） |
| `instance.lock` | 进程锁，防止同一数据目录被多个进程同时写入 |

### 两种加密后端

`state.bin` 以**魔术前缀**自描述加密方式：

| 前缀 | 加密方式 | 使用场景 |
|---|---|---|
| `WBH1` | Windows DPAPI（绑定当前 Windows 用户/机器） | Windows 原生数据目录 |
| `WBK1` | AES-256-GCM + 目录内 `key.bin` | Linux / Docker 容器 / 可移植副本 |

程序**读取时两种前缀都支持**，写入时使用当前平台的默认后端（Windows → DPAPI，其他 → 密钥文件）。

> **重要**：`key.bin` 与 `state.bin` 必须成对备份。`key.bin` 丢失后 `state.bin` 将**无法解密**，只能重新添加账号。

---

## 账号迁移（Windows ↔ 容器）

DPAPI 数据绑定 Windows 用户/机器，**Linux 容器无法直接解密**。迁移需先导出为可移植副本：

### 1. 在 Windows 上导出

```powershell
WorkBuddyHelper.exe --export-portable D:\wbh-data
```

该命令会解密当前 `state.bin`，用**新生成的 `key.bin`** 以 `WBK1` 格式重新加密写入 `D:\wbh-data`。

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

> 迁移时请确保 `key.bin` 与 `state.bin` **一同复制**，缺一不可。

---

## 从源码构建

需要 **Go 1.22+**，模块**无任何外部依赖**（仅标准库）。

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
| GET | `/api/state` | 账号与汇总（不含令牌） |
| GET | `/api/logs?limit=` | 运行日志 |
| POST | `/api/login/start` | 开始登录（返回授权 URL） |
| POST | `/api/login/poll` | 轮询登录结果 |
| POST | `/api/run-all` | 全部账号签到 |
| PUT | `/api/settings` | 更新定时设置 |
| POST | `/api/accounts/{id}/checkin` | 单账号签到 |
| POST | `/api/accounts/{id}/points` | 单账号积分刷新 |
| PUT | `/api/accounts/{id}` | 修改显示名 / 启停 |
| DELETE | `/api/accounts/{id}` | 删除账号 |

写操作需携带页面注入的 `X-App-Token`，且 `Origin` 必须与请求 `Host` 同源（或为本机回环），以防御 CSRF。

---

## 常见问题

**Q：界面提示"请求来源不受信任"？**
A：这是 CSRF 保护。请通过浏览器访问页面（而非直接调 API），并确认 `Origin` 与访问地址同源。若通过反向代理访问，请确保代理正确透传 `Host` 头。

**Q：容器启动后宿主机访问不了？**
A：容器内 `WBH_HOST` 必须是 `0.0.0.0`。若为 `127.0.0.1`，Docker 端口映射无法投递。

**Q：登录失效怎么办？**
A：账号状态会变为 `needs_login`，在界面删除该账号后重新添加即可。令牌刷新失败不会影响其他账号。

**Q：能复制 `state.bin` 到别的机器直接用吗？**
A：不能。`WBH1`（DPAPI）绑定原 Windows 用户/机器；`WBK1` 需要配套的 `key.bin`。请使用 `--export-portable` 导出。

**Q：接口会不会变？**
A：签到与积分使用 WorkBuddy 客户端当前调用的 billing 接口；腾讯未公开面向个人用户的稳定 OpenAPI，**接口可能随时调整**。程序会在接口结构变化时提示错误并暂停该账号。

---

## 更新日志

### v1.0.2（2026-09-14）

- **版本切换 = 账号列表切换**：页头下拉在国内版 / 国际版间切换，两套账号容器独立管理；每日定时任务按「每日任务版本」执行对应列表（国内版签到 / 国际版活跃）。
- **国际版活跃获取积分**：以 WorkBuddy 桌面客户端完整特征（UA `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/2.137.1`、`X-IDE-Type/Name: WorkBuddy`、`X-Product: SaaS`）向 `workbuddy.ai/v2/chat/completions` 发送一次极小会话（hy3 / hy4-preview，max_tokens=16），当日即认定为活跃账号。
- **国际版登录授权**：「添加账户」按当前版本拉取对应授权页——国际版为 `https://www.workbuddy.ai/login?platform=workbuddy-ai&state=...`（state → 授权页 → token 轮询 → account 全链路对齐客户端）。
- **积分延迟复核**：活跃后按设置延迟（默认 60 分钟，可调 5-720）自动复查积分到账，无变化最多重试 3 次，日志记录积分增量。
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

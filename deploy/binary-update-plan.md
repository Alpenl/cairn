# Cairn 二进制部署与一点更新

状态：`PLAN`。日期：2026-08-17。本文件是实施合同，不是当前生产事实。未改运行代码、未切生产。

授权：按 Sub2API 的方式重做部署；允许改掉现有 GHCR + Compose 应用路径。实施、打 tag、改主机仍要另一次明确授权。

对照上游：[Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) 的 `main`，当时 latest 为 `v0.1.177`（2026-08-15）。现场管理页侧边栏 `v0.1.177` / 「已是最新版本」与该 tag 一致。

---

## 0. 结论

生产从「钉死 digest 的应用容器 + 独立 Reader 静态站」改成「宿主机一个 `webtag` + systemd」。设置页出现版本徽章；持有 `ADMIN_AUTH_TOKEN` 的人点「立即更新」，从本仓库 GitHub Release 拉最新 Linux 包、校验、先迁移、再换文件、再重启。

交互、安装器、原子替换、自杀重启按 Sub2API。两处不抄：换文件前必须 `pg_dump` 且新二进制 migrate 成功；库比目标二进制新则拒绝回退。

PostgreSQL 继续用现有 Docker。不引入 Redis、账号系统、Setup Wizard。扩展 / Android / iOS 不在这个按钮里。

---

## 1. Sub2API 在做什么（一手事实）

### 1.1 产品形态

Sub2API 是自托管 AI API 网关：Go + 嵌入的 Vue 3 管理后台 + PostgreSQL + Redis。有用户、分组、计费、审计。许可证 LGPL-3.0。前端打进 `backend/internal/web/dist/`，`-tags=embed` 进同一个二进制。

Cairn 是单安装、无用户账号的阅读收藏服务。只有安装身份、httpOnly 会话、独立客户端（Reader / 扩展 / Android / iOS）。不能把它理解成「换皮 Sub2API」。本方案只搬**部署与一点更新**，不搬网关业务。

### 1.2 对方的四条安装路径

| 路径 | 入口 | 替换物 | 官方怎么升 |
| --- | --- | --- | --- |
| 脚本 / 二进制 | `deploy/install.sh` | `/opt/sub2api/sub2api` + systemd | 管理后台一点，或 `install.sh upgrade` |
| Docker Compose | `deploy/docker-deploy.sh` | 镜像 `weishaw/sub2api:latest` | `docker compose pull && up -d` |
| Apple container | `deploy/apple-container.sh` | 本地容器栈 | 脚本自己的 up |
| 源码编译 | README Method 4 | 自己 `go build` 的文件 | UI 不给一键更新，只提示 git pull |

根 README 把脚本安装标成 Method 1 Recommended。`deploy/README.md` 又把 Docker 标成 Recommended。一点更新只为 **CI 正式二进制** 设计。Docker 官方升级是 pull 镜像，不是 in-app 换文件。

现场实例装的是哪一种，没有登录主机核实。徽章行为与 `BuildType=release` + latest=`0.1.177` 一致。

### 1.3 对方发布产物

`.github/workflows/release.yml` 在 `v*` tag（或手工 dispatch）时：改 `backend/cmd/server/VERSION` → 建前端 → GoReleaser。

`.goreleaser.yaml`：`CGO_ENABLED=0`、`-tags=embed`、ldflags 写入 `Commit` / `Date` / **`BuildType=release`**。归档名：

```text
sub2api_{version}_{os}_{arch}.tar.gz    # Windows 为 zip
checksums.txt                           # SHA-256
```

`v0.1.177` 的资源：darwin/linux 的 amd64+arm64 tar.gz、windows amd64 zip、`checksums.txt`。linux amd64 约 36 MB。另发 DockerHub / GHCR 镜像。

安装脚本与进程内更新器都按这个名字找包：

```text
https://github.com/Wei-Shaw/sub2api/releases/download/${TAG}/sub2api_${VER}_${OS}_${ARCH}.tar.gz
https://github.com/Wei-Shaw/sub2api/releases/download/${TAG}/checksums.txt
```

更新器用 `runtime.GOOS` + `GOARCH` 拼 `linux_arm64` 这类片段去匹配 asset。

### 1.4 对方运行中的版本从哪来

`backend/cmd/server/main.go`：

- `//go:embed VERSION`（当时文件内容 `0.1.177`）
- ldflags 可覆盖 `Version` / `Commit` / `Date` / `BuildType`
- 默认 `BuildType=source`；只有 GoReleaser 包是 `release`
- `--version` 打印 `Sub2API %s (commit: %s, built: %s)`

前端两条通道：

1. 公开 `GET /api/v1/settings/public` 带 `version`。侧栏品牌来自 `site_name`（默认 Sub2API）。`AppSidebar.vue` 把 `siteVersion` 传给 `VersionBadge.vue`。
2. 管理员挂载时 `GET /api/v1/admin/system/check-updates`，结果进 Pinia：`current_version` / `latest_version` / `has_update` / `build_type`。

`isReleaseBuild = buildType === 'release'`。源码构建即使查到新版本，也只给 changelog，没有「立即更新」。非管理员只看到静态 `vX.Y.Z`。

### 1.5 对方页面上的徽章

用户选中的 DOM：

```html
<div class="sidebar-header">
  <a class="sidebar-logo" href="/admin/dashboard">...</a>
  <div class="sidebar-brand">
    <a class="sidebar-brand-title">Sub2API</a>
    <button title="已是最新版本">
      <span class="font-medium">v0.1.177</span>
    </button>
  </div>
</div>
```

这是收起态。`<!---->` 表示下拉没展开，所以看不见「立即更新」。

| 状态 | 按钮 | title（中文） |
| --- | --- | --- |
| `has_update=false` | 灰底 `v0.1.177` | 已是最新版本 |
| `has_update=true` | 琥珀底 + ping 圆点 | 有新版本可用！ |

点击只 `toggleDropdown`，不会直接升级。v0.1.111 曾给 `.sidebar-header` 加 `overflow: hidden`，下拉被裁掉（#1613 / #1631 / #1636）。绕过：手工 `POST /api/v1/admin/system/update`，或 `install.sh upgrade`。

### 1.6 对方一点更新的 API

全部在 `/api/v1/admin`，先过 `AdminAuthMiddleware`（管理员 JWT，或管理员 `x-api-key`）。更新没有 step-up 2FA。

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| GET | `/admin/system/version` | 当前版本 |
| GET | `/admin/system/check-updates?force=true` | 问 GitHub，缓存 20 分钟 |
| GET | `/admin/system/rollback-versions` | 最多 3 个更旧正式版 |
| POST | `/admin/system/update` | 下最新包并替换正在跑的文件 |
| POST | `/admin/system/rollback` | 本地 `.backup`，或再下指定旧 tag |
| POST | `/admin/system/restart` | 500ms 后 `os.Exit(0)` |

前端 `frontend/src/api/admin/system.ts` 给更新/回退 15 分钟超时，对齐后端 `systemUpdateTimeout`（他们的 #4504：axios 30s 会在下载中掐死）。

查更新：`UpdateService.CheckUpdate`（`backend/internal/service/update_service.go`）

1. 非 force 先读缓存（TTL 1200 秒）
2. `GET https://api.github.com/repos/Wei-Shaw/sub2api/releases/latest`
3. 只比三段数字 semver，去 `v`
4. 失败尽量返回缓存 + `warning`
5. 仓库名写死 `Wei-Shaw/sub2api`

GitHub 客户端（`backend/internal/repository/github_release_service.go`）：API 30s，下载 10 分钟，UA `Sub2API-Updater`。`UPDATE_GITHUB_TOKEN` 只加在 `api.github.com`；302 出域会拆掉 Authorization。代理初始化失败默认禁止直连回退。

### 1.7 对方真正替换的是哪个文件

`PerformUpdate` → 强制查更新 → 没有新版本则 `ErrNoUpdateAvailable`（handler 改写成 200 + `already_up_to_date`，修过 #2823）。

有更新则 `applyReleaseAssets`：

1. asset 名包含 `{GOOS}_{GOARCH}` 的包，以及 `checksums.txt`
2. URL 必须 https，host 只能是 `github.com` / `objects.githubusercontent.com` 及其子域
3. `os.Executable()` + `EvalSymlinks()`
4. 在同一目录建 `.sub2api-update-*`，保证 `rename` 同盘原子
5. 下载上限 500 MB
6. 有 checksum 就对 SHA-256；**没有 checksums.txt 则跳过**
7. tar 里只抽 `sub2api` / `sub2api.exe`；拒绝 `..`；`LimitReader` 防炸弹
8. `chmod 0755`
9. `current → current.backup`，`new → current`；第二步失败则改回

成功后进程仍是旧代码，必须再重启。下载用 `context.WithoutCancel`，浏览器断开后替换继续。有操作锁和幂等键。

systemd unit（`deploy/sub2api.service`）：

```ini
ExecStart=/opt/sub2api/sub2api
Restart=always
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/opt/sub2api
```

重启（`backend/internal/pkg/sysutil/restart.go`）：Linux 上 `os.Exit(0)`，不调 `systemctl`，不需要 sudo。前端倒计时 8 秒，轮询 `/health`，再 `location.reload()`。

回退：空 body 恢复 `.backup`；`{"version":"0.1.146"}` 必须在「比当前旧、非 draft/prerelease、最多 3 个」的名单里，再走同一套下载替换。UI 另给手工命令：

```bash
curl -sSL https://raw.githubusercontent.com/Wei-Shaw/sub2api/vX.Y.Z/deploy/install.sh \
  | sudo bash -s -- rollback vX.Y.Z
```

`install.sh` 的 `upgrade`：停服务 → 备份 → 拉 latest → checksum（缺文件只警告）→ 覆盖 → `systemctl start`。

### 1.8 对方安全边界

做得好的：管理员门、不能指定任意 URL、HTTPS + GitHub 域名、体积上限、zip-slip 防护、token 不跟 302、原子 rename、操作锁、`ProtectSystem=strict`。

仍然脆的：管理员 = 能换整个进程；跟 `releases/latest`；checksum 可跳过；无签名（#3839：GitHub 被劫持 = 一点即 RCE）；更新不做数据库备份；启动时自动前向迁移，回退二进制可能起不来；Docker 内替换改的是可写层，和下次 `compose up --force-recreate` 会分叉；Windows zip / 重启语义不同（#2397 / #2423）。

### 1.9 对方 Docker 与一点更新是两套世界

```bash
docker compose -f docker-compose.local.yml pull
docker compose -f docker-compose.local.yml up -d
```

默认 `image: weishaw/sub2api:latest`。容器启动时自己跑前向 SQL 迁移。回退库只能靠备份或手写补偿 SQL。

---

## 2. Cairn 现在是什么（当前事实）

### 2.1 生产拓扑

Oracle ARM64。主机路径沿用 `webtag`。

```text
Caddy
  reader.alpenl.com  → /var/www/reader        静态 SPA
  webtag.alpenl.com  → 127.0.0.1:8732         容器 webtag
compose @ /opt/webtag
  webtag     ghcr.io/<owner>/cairn:<VERSION>@sha256:...
  migrate    一次性 profile，容器内 /app/migrate
  webtag-db  postgres + /opt/webtag/pgdata
```

规则：只部署正式 tag 的不可变 digest；禁止 `latest` / `full` / `slim`；先备份 compose、`.env`、`pg_dump -Fc`；migrate 失败立即停，不重建应用；只重建 `webtag` 容器。独立 Reader 必须与后端同版本，否则 `representation_contract` 对不上会立刻断连。独立 Reader 发布脚本目前缺失。

### 2.2 版本身份

- `scripts/version.sh`：只有精确 `vX.Y.Z` 是正式号；开发构建是 `nearest-N-g<sha>[-dirty]`
- ldflags 注入 `webtag/internal/buildinfo.{Version,Commit,BuildTime}`
- `webtag --version` / `migrate --version` 在读配置之前打印
- 公开 `GET /health`：`status` / `version` / `commit` / `build_time`。没有独立 `/version`
- 正式镜像 commit 是 40 位；本地 Makefile 常用 12 位短哈希
- OpenAPI `info.version` 写死 `1.0.0`，那是文档版本，不是 Core SemVer
- Reader `bootstrap.ts` 只用 `/health` 判断同源是不是 Cairn，不展示给用户
- `SettingsSurface` 没有版本行
- 没有查 GitHub Release 的代码，没有 self-update。`internal/fetcher/github.go` 只用于抓内容

### 2.3 发布单元

| 单元 | Tag | 一点更新能不能动 |
| --- | --- | --- |
| Core（Go、迁移、镜像、内嵌 Reader） | `vX.Y.Z` | 本方案只动这个 |
| Extension | `extension/vX.Y.Z` | 否 |
| Android | `android/vX.Y.Z` | 否 |
| iOS | `ios/vX.Y.Z` | 否 |

`release-core.yml` 现在产出：linux amd64/arm64 的 `webtag`+`migrate` 压缩包、`SHA256SUMS`、GHCR full/slim、`IMAGE-DIGESTS.txt`、以及仍引用的独立 Reader 包（与 runbook「脚本已删」存在漂移）。

### 2.4 鉴权

- Reader：跨域 + httpOnly `webtag_session` + CSRF。token 不进 localStorage
- 公开 API：`EXTENSION_API_TOKEN` 或开放模式
- `/api/admin/*`：Bearer `ADMIN_AUTH_TOKEN`，非 dev 空 token fail-closed。现在只有 concept-merges
- 应用容器没有 docker socket，写不了 `/opt/webtag` 或 `/var/www/reader`

---

## 3. 对照表（Sub2API / Cairn 现在 / Cairn 目标）

### 3.1 产品与拓扑

| 项 | Sub2API | Cairn 现在 | Cairn 目标 |
| --- | --- | --- | --- |
| 产品 | 多用户 API 网关 | 单安装阅读收藏 | 不变 |
| 前端 | Vue 管理后台，嵌入二进制 | React Reader；生产是独立静态站，镜像内另有 `/reader/` | Reader 嵌入 `webtag`，`VITE_BASE=/`，Caddy 反代到进程 |
| 数据 | PostgreSQL + Redis | 只有 PostgreSQL | 只有 PostgreSQL，继续用 `webtag-db` 容器 |
| 反代 | 可选 Nginx/Caddy | Caddy | Caddy 保留，改 `reader.alpenl.com` 目标 |
| 生产主路径 | 二进制 + systemd（他们也推 Docker） | GHCR + Compose 应用容器 | 二进制 + systemd |
| 应用监听 | `0.0.0.0:8080` | `127.0.0.1:8732` | 仍是 `127.0.0.1:8732`，不对公网裸奔 |
| 主机路径 | `/opt/sub2api` | `/opt/webtag` | `/opt/webtag`，不改名 |
| 进程用户 | `sub2api` | 容器用户 | `webtag` |
| Setup Wizard | 有 | `.env` | 不要 |

### 3.2 发布与安装

| 项 | Sub2API | Cairn 现在 | Cairn 目标 |
| --- | --- | --- | --- |
| 正式触发 | `v*` tag + GoReleaser | `v*.*.*` + `release-core.yml` | 仍是 Core tag |
| 主产物 | `sub2api_{ver}_{os}_{arch}.tar.gz` | 镜像是主路径；二进制包已存在但不用来部署 | `cairn_{ver}_linux_{amd64,arm64}.tar.gz` 成为生产主产物 |
| 校验文件 | `checksums.txt` | `SHA256SUMS` | 继续 `SHA256SUMS`，更新器强制校验 |
| 镜像 | DockerHub `:latest` 与版本 tag、GHCR | GHCR 多架构，生产钉 digest | 降为可选第二路径；生产不用 |
| 独立前端包 | 无 | `cairn-reader-*.tar.gz`（流程漂移） | 停产 |
| 安装器 | `deploy/install.sh` | 无 | `deploy/install.sh`，子命令对齐对方 |
| 服务 unit | `sub2api.service` | 无（容器 `restart`） | `webtag.service`，`Restart=always`，`ReadWritePaths=/opt/webtag` |
| `BuildType` | `release` / `source` | 无此字段 | 正式包 `release`，本地 `source` |

### 3.3 版本展示与点击更新

| 项 | Sub2API | Cairn 现在 | Cairn 目标 |
| --- | --- | --- | --- |
| 展示位置 | 管理后台侧栏头 | 无。`/health` 有数据，UI 不用 | 设置 → 关于；可选壳上小字 |
| 收起态 | 灰 `vX.Y.Z` 或琥珀+圆点 | — | 同样文案：「已是最新版本」/「有新版本可用！」 |
| 点击徽章 | 只打开下拉 | — | 同样 |
| 立即更新 | 仅 `build_type=release` | 无 | 同样 |
| 源码构建 | 只给 git / Release 链接 | — | 同样 |
| 查更新 | `GET /api/v1/admin/system/check-updates` | 无 | `GET /api/admin/system/check-updates`（沿用现有 `/api/admin`，不造 `/api/v1`） |
| 执行更新 | `POST .../update` | 无 | `POST /api/admin/system/update` |
| 重启 | `POST .../restart` → `os.Exit(0)` | `docker compose up --force-recreate` | 同样自杀重启 |
| 回退 | 本地 `.backup` 或再下最多 3 个旧正式版 | 恢复 compose digest + 静态目录改名 | 对齐对方，外加迁移拒绝条件 |
| 跟哪个频道 | GitHub `releases/latest` | 禁止频道 tag，钉 digest | **进程内更新跟 `releases/latest`**。镜像路径若还在，仍禁止频道 tag |
| 仓库 | 写死 `Wei-Shaw/sub2api` | — | `Alpenl/cairn` 或构建期注入 |

### 3.4 更新时磁盘上发生什么

| 步骤 | Sub2API | Cairn 目标 |
| --- | --- | --- |
| 鉴权 | 管理员 JWT / admin API key | `ADMIN_AUTH_TOKEN`，内存解锁，不进 localStorage |
| 持锁 | 系统操作锁 + 幂等键 | 同样 |
| 查 latest | 强制 force | 同样 |
| 已最新 | 200 + `already_up_to_date` | 同样，不 500 |
| 数据库备份 | 无 | **先** `pg_dump -Fc`，非空才继续 |
| 下载 | GitHub asset，500 MB 上限 | 同样，域名白名单同样 |
| 校验 | checksum 可跳过 | **缺或错一律失败** |
| 解包 | 只抽 `sub2api` | 只抽 `webtag` |
| 迁移 | 换文件后，新进程启动时自己迁 | **抽出的新文件先 `webtag migrate`，失败则旧进程继续** |
| 替换 | rename 当前 → `.backup`，新 → 当前 | 同样，必须同盘 |
| 进程 | 仍跑旧代码直到重启 | 同样 |
| 重启 | `os.Exit(0)` + systemd | 同样 |
| 前端 | 8s 后探 `/health` 再 reload | 探 `/health` 的 **commit** 是否变成预期值再 reload |

这是唯一允许的必要偏差：先备份、先迁移、checksum 强制。其余按对方。

### 3.5 鉴权对照

| 项 | Sub2API | Cairn 现在 | Cairn 目标 |
| --- | --- | --- | --- |
| 谁能看见版本 | 所有登录用户看得到静态字；管理员可点 | 无人看见 | 能打开 Reader 的人看得见；未解锁只能看 |
| 谁能更新 | `user.IsAdmin()` 或 admin API key | 无人 | 输入 `ADMIN_AUTH_TOKEN` 解锁之后 |
| 会话能否更新 | 管理员 JWT 就是会话 | Reader 会话是跨域 Cookie | **不能**。会话与更新门分开 |
| 开放模式 | — | `PUBLIC_API_OPEN` 开放业务 API | 仍不开放更新接口 |
| 二次确认 | 更新无 step-up；回退有警告文案 | — | 回退保留警告；更新按钮本身不搞 2FA（对齐对方） |

### 3.6 迁移对照

| 项 | Sub2API | Cairn 现在 | Cairn 目标 |
| --- | --- | --- | --- |
| 何时迁 | 进程启动 | 单独 migrate 容器，先于重建 | 更新器在换主文件前调用新 `webtag migrate` |
| 失败 | 新进程起不来，systemd 可能反复拉 | HOLD，不重建应用 | 不换文件，旧进程继续 |
| 回退二进制 | 不检查 schema | 仅当迁移后向兼容才允许镜像回退 | 库比目标新则拒绝，指出 dump |
| 备份 | 无自动 dump | 人手 `pg_dump -Fc` | 更新器自动 dump 到 `/opt/webtag/backups/` |

### 3.7 抄与不抄

抄：

- 正式二进制 + systemd + `Restart=always` + `ProtectSystem=strict` + `ReadWritePaths`
- `install.sh` 的 install / upgrade / rollback / list-versions
- `BuildType` 分流按钮
- 徽章状态机与中文 title
- 跟 `releases/latest`、按 GOOS/GOARCH 下包
- 原子 rename、`.backup`、操作锁、15 分钟超时、请求与下载脱钩
- `os.Exit(0)` 重启、前端倒计时探活
- 最多 3 个更旧正式版回退
- Docker 降为第二路径，官方用 pull，不在容器里一点换文件

不抄：

- Redis、多用户、计费、Setup Wizard、管理后台整站
- 默认把应用镜像钉成 `:latest`
- checksum 可跳过
- 先换文件再靠启动迁移
- Windows 独立 exe 作为生产承诺
- 容器内替换可写层
- 用管理员 JWT 当唯一身份模型（Cairn 没有用户表）
- 把扩展和移动端塞进同一按钮

---

## 4. 目标拓扑

```text
Caddy
  reader.alpenl.com  → 127.0.0.1:8732     同一进程提供 UI + API
  webtag.alpenl.com  → 127.0.0.1:8732     API、/health、/ready；/reader/ 可作别名

systemd webtag.service
  ExecStart=/opt/webtag/webtag
  User=webtag
  WorkingDirectory=/opt/webtag
  EnvironmentFile=/opt/webtag/.env
  Restart=always
  RestartSec=5
  NoNewPrivileges=true
  ProtectSystem=strict
  ProtectHome=true
  PrivateTmp=true
  ReadWritePaths=/opt/webtag

docker
  webtag-db  不变
```

`.env`、`SESSION_SIGNING_KEY`、`CORS_ORIGINS`（必须显式列出 `https://reader.alpenl.com`，禁止 `*`）继续用。

为什么 Reader 必须进进程：现在静态站不随应用更新，契约一对不上就断。一点更新若只换后端，这个坑会变成默认操作。一次换文件必须等于 API + UI 同版本。`/var/www/reader` 切流后改名为备份，不再是发布目标。

---

## 5. 发布产物（目标）

继续只对 Core 的 `vX.Y.Z` 打正式包。

| 产物 | 角色 |
| --- | --- |
| `cairn_<ver>_linux_amd64.tar.gz` / `linux_arm64.tar.gz` | 生产主产物。内含已 embed Reader 的 `webtag`、`LICENSE` |
| `SHA256SUMS` | 安装器与更新器都要核 |
| GHCR full / slim | 可选、CI 冒烟、第二路径 |
| `IMAGE-DIGESTS.txt` | 只给还在用镜像的人 |
| `cairn-reader-*.tar.gz` | 停产 |

`webtag --version` 必须是精确 tag + 完整 commit + 构建时间。正式包 `BuildType=release`。

合并迁移入口，避免更新器处理两个文件：

```text
webtag
webtag migrate
webtag migrate --to ID
webtag --version
```

`cmd/migrate` 可暂时留给 CI。归档名必须稳定，更新器用 `linux_arm64` 匹配。生产是 ARM64，CI 必须继续验 arm64 包的 `--version`。

仓库写死 `Alpenl/cairn` 或构建期注入，不要出现 `Wei-Shaw/sub2api`。

---

## 6. 安装器

```bash
curl -sSL https://raw.githubusercontent.com/Alpenl/cairn/main/deploy/install.sh | sudo bash
sudo bash deploy/install.sh upgrade
sudo bash deploy/install.sh rollback vX.Y.Z
sudo bash deploy/install.sh list-versions
```

行为对齐对方 `install.sh`，收紧校验：

1. root、Bash 4+、linux amd64/arm64
2. latest 走 GitHub `releases/latest`；指定版本走 `releases/tags/vX.Y.Z`
3. 匿名下 tar.gz + `SHA256SUMS`；缺校验或对不上则停
4. 装到 `/opt/webtag/webtag`，属主 `webtag`
5. 已安装则先 `systemctl stop`，备份为 `webtag.backup` 或 `webtag.backup.<oldver>`
6. 写 unit；若本地改过，不要无提示覆盖
7. upgrade 后 `daemon-reload && start`
8. 不碰 `.env`、`pgdata`、不装 PostgreSQL

`UPDATE_GITHUB_TOKEN` 只用于 API 限流，下载匿名；不得进日志，不得跟 302 出 `api.github.com`。

---

## 7. 一点更新后端

模块对外只暴露：查 / 升 / 列回退 / 回退 / 重启。GitHub、解包、rename 藏在里面。

挂在现有 `/api/admin` 的 Bearer 门后，和 concept-merges 同一把 `ADMIN_AUTH_TOKEN`。

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| GET | `/api/admin/system/version` | 当前版本 + `build_type` |
| GET | `/api/admin/system/check-updates?force=` | latest、has_update、changelog、cached |
| GET | `/api/admin/system/rollback-versions` | 最多 3 个更旧正式版 |
| POST | `/api/admin/system/update` | dump → 下包 → 新文件 migrate → 换主文件 |
| POST | `/api/admin/system/rollback` | 空 body = `.backup`；带 version = 再下旧包（仍先迁） |
| POST | `/api/admin/system/restart` | 500ms 后 `os.Exit(0)` |

顺序：

```text
1. 持锁
2. CheckUpdate(force)
3. 无更新 → already_up_to_date
4. pg_dump -Fc → /opt/webtag/backups/<old>-to-<new>-<utc>.dump，非空才继续
5. 下载并校验到 /opt/webtag/.cairn-update-*/
6. 抽出 webtag，chmod 0755
7. 新文件 migrate（当前 DATABASE_URL）
     失败 → 删临时目录，旧进程继续
8. rename 当前 → webtag.backup；新 → webtag
     第二步失败 → 改回 backup
9. 返回 need_restart
10. 点重启 → os.Exit(0) → systemd 拉起新文件
```

新进程里的 migrate 必须幂等。`/ready` 仍要过。Linux 才允许自杀重启，否则提示 `systemctl restart webtag`。

回退指定版本走同一套。若 migrate 报告「库比目标新且无后向路径」，拒绝并指出 dump。

---

## 8. 前端

Cairn 没有管理侧栏。徽章放：

1. **设置 → 关于**（`SettingsSurface` 新一行）：完整下拉，状态机按第 3.3 节，对标 `VersionBadge.vue`
2. 可选：Reader 壳上小字，点击跳到关于

展示读公开 `/health` 的 `version` + 短 commit。禁止用 OpenAPI `1.0.0`。

运维解锁：

1. 在设置里输入 `ADMIN_AUTH_TOKEN`
2. 只放内存
3. 之后请求带 Bearer
4. 关标签页即失效

`build_type !== 'release'` 永不画「立即更新」。`PUBLIC_API_OPEN` 不开放这些 POST。

更新请求超时 15 分钟。重启后轮询 `/health` 的 commit，再 reload。

文案写清：只更新本机 Core，不管扩展和手机。

---

## 9. 现网切流

现网是 Compose 应用容器。切一次，之后靠徽章。

1. 备份 compose、`.env`、`pg_dump -Fc`、`/var/www/reader`
2. 选定已发布的 `vX.Y.Z` arm64 包，核 `SHA256SUMS`
3. 装 systemd 与 `/opt/webtag/webtag`；`.env` 留在原地，`DATABASE_URL` 仍指向 `webtag-db`
4. 停容器 `webtag`，用新二进制跑 `webtag migrate`
5. `systemctl start webtag`；`curl 127.0.0.1:8732/health` 对 commit
6. Caddy：`reader.alpenl.com` 从文件根改为反代 `127.0.0.1:8732`；`webtag.alpenl.com` 不变
7. 浏览器：会话仍是 `session`、读一篇真文、CORS 预检仍是具名源 + credentials
8. `docker compose stop webtag`（或从 compose 删应用服务）。不要动 `webtag-db`
9. `/var/www/reader` → `reader.backup-<release>`，先留着
10. 徽章走一遍「已是最新」。当天不拿生产试跨版本自动升

切流回退：Caddy 改回静态目录，compose 再起旧 digest，`systemctl stop webtag`。

之后日常升配：徽章或 `install.sh upgrade`。

---

## 10. 规则在实施后怎么变

| 旧 | 新 |
| --- | --- |
| 生产必须 GHCR digest | 生产必须正式 `webtag` + `SHA256SUMS` |
| 永不部署频道 tag | 镜像路径仍禁止。进程内更新故意跟 `releases/latest` |
| Reader 独立静态站与后端同版本发 | UI 在 Core 包里 |
| 迁移只在 compose profile | 换文件前 `webtag migrate`，失败不换 |
| 每次部署都是 R2 外部动作 | 切流、改 Caddy、停容器仍要授权。切完后一点更新是产品功能 |

四个发布单元不变。

---

## 11. 分期

每一期单独授权。

0. 本合同。冻结：`/opt/webtag`、admin token 门、先迁后换、PG 留在 Docker、不动扩展/移动端。
1. 单二进制 + `webtag migrate` + `BuildType` + `install.sh` + unit + 内嵌 `/` Reader。非生产机装起来。
2. 更新 API（无 UI）+ 假 GitHub 单测：checksum 失败、zip-slip、已最新、migrate 失败不换文件、rename 失败恢复 backup。
3. 设置页徽章 + 运维解锁。正式构建能点到重启；source 构建没有按钮。
4. 生产切流。另一次授权。
5. 把 deploy skill / 版本文档改成当前事实；compose 应用服务标成第二路径；观察一周再删 `reader.backup-*`。

建议下一次只授权第 1 期。

---

## 12. 测试与验收

第 2–3 期最少：版本比较与缓存；只抽 `webtag`；拒绝 `..` 与超大；缺/错 checksum 失败；migrate 失败则旧文件不变；非 admin / 空 token → 401；source 构建无更新按钮；回退名单不含当前、不含 prerelease、最多 3 个。

切流当天只记真做过的：`--version` = 目标 tag + 40 位 commit；`/health` 一致；`/ready` 200；真读一篇且会话是 `session`；CORS 预检正确；`systemctl is-active webtag`；应用容器已停、库容器仍在；dump 非空。

---

## 13. 风险

| 风险 | 处理 |
| --- | --- |
| GitHub latest 被劫持 = 换掉进程 | 与对方相同模型。强制 checksum；签名可后续加 |
| 迁移不可逆 | 先 dump；回退拒绝「库比二进制新」 |
| migrate 失败 + `Restart=always` 死循环 | 换文件前迁完；启动期 migrate 幂等；unit 可加 `StartLimitBurst` |
| 跨域会话被拿去更新 | 更新只用 admin token，且不进 localStorage |
| `ProtectSystem=strict` 但文件不在放行目录 | 二进制必须在 `/opt/webtag` |
| Caddy 切错 | 先留静态目录备份 |
| 对方 Windows / 容器内替换的坑 | 生产只承诺 Linux systemd |

---

## 14. 明确不做

- 不引入 Redis、多用户、Setup Wizard
- 不把生产应用镜像改成 `:latest`
- 不在 Reader 会话里静默带更新权限
- 不让按钮去改扩展、Android、iOS
- 本文件不修改运行代码、不切生产

---

## 15. Sub2API 源码索引

- [README.md](https://github.com/Wei-Shaw/sub2api/blob/main/README.md) / [deploy/README.md](https://github.com/Wei-Shaw/sub2api/blob/main/deploy/README.md)
- [deploy/install.sh](https://github.com/Wei-Shaw/sub2api/blob/main/deploy/install.sh)
- [deploy/sub2api.service](https://github.com/Wei-Shaw/sub2api/blob/main/deploy/sub2api.service)
- [deploy/docker-compose.local.yml](https://github.com/Wei-Shaw/sub2api/blob/main/deploy/docker-compose.local.yml)
- [backend/cmd/server/main.go](https://github.com/Wei-Shaw/sub2api/blob/main/backend/cmd/server/main.go)
- [backend/internal/service/update_service.go](https://github.com/Wei-Shaw/sub2api/blob/main/backend/internal/service/update_service.go)
- [backend/internal/handler/admin/system_handler.go](https://github.com/Wei-Shaw/sub2api/blob/main/backend/internal/handler/admin/system_handler.go)
- [backend/internal/repository/github_release_service.go](https://github.com/Wei-Shaw/sub2api/blob/main/backend/internal/repository/github_release_service.go)
- [backend/internal/pkg/sysutil/restart.go](https://github.com/Wei-Shaw/sub2api/blob/main/backend/internal/pkg/sysutil/restart.go)
- [backend/internal/server/routes/admin.go](https://github.com/Wei-Shaw/sub2api/blob/main/backend/internal/server/routes/admin.go)
- [frontend/src/components/common/VersionBadge.vue](https://github.com/Wei-Shaw/sub2api/blob/main/frontend/src/components/common/VersionBadge.vue)
- [frontend/src/api/admin/system.ts](https://github.com/Wei-Shaw/sub2api/blob/main/frontend/src/api/admin/system.ts)
- [frontend/src/stores/app.ts](https://github.com/Wei-Shaw/sub2api/blob/main/frontend/src/stores/app.ts)
- [.goreleaser.yaml](https://github.com/Wei-Shaw/sub2api/blob/main/.goreleaser.yaml)
- Issues：[#1613](https://github.com/Wei-Shaw/sub2api/issues/1613) [#1631](https://github.com/Wei-Shaw/sub2api/issues/1631) [#2823](https://github.com/Wei-Shaw/sub2api/issues/2823) [#2397](https://github.com/Wei-Shaw/sub2api/issues/2397) [#3839](https://github.com/Wei-Shaw/sub2api/issues/3839)

Cairn 对照入口：`internal/buildinfo/buildinfo.go`、`internal/app/router_static.go`、`reader/src/lib/bootstrap.ts`、`reader/src/components/reader-vnext/SettingsSurface.tsx`、`internal/app/router_admin.go`、`.github/workflows/release-core.yml`。

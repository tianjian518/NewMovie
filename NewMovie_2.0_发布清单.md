# NewMovie v2.0.0 — 与 139cas 合体：一个镜像，装完就能用

- 日期：2026-08-02
- 基线：v1.1.15（`6b2b79b`）
- 主题：**NewMovie 与用户自己的 OpenList 魔改版 [139cas](https://github.com/tianjian518/139cas) 整合为一个项目。**
  139cas 是后端（网盘挂载 / 直链 / `.cas` 解密），NewMovie 是套在外面的前端与播放器壳。
  部署方式从「装两个东西 + 复制 Token」变成「起一个容器」。

## 一、为什么要做这件事

1.x 的部署路径长得离谱：

```
装 OpenList → 建管理员 → 加网盘驱动 → 后台复制 Token → 装 NewMovie
→ 填 OpenList 地址 → 粘 Token → 填签名密钥 → 才能开始建媒体库
```

其中「复制 Token」「签名密钥」两步是纯粹的机械劳动，且极易出错（少个斜杠、
签名没开、地址填了公网 IP 导致回环失败……）。既然两个项目都是同一个人的，
那就没有理由让用户自己去搭桥。

**2.0 的部署路径：**

```
docker compose up -d → 打开 :8096/openlist/ 加网盘 → 回 :8096 建媒体库
```

Token、地址、签名密钥，NewMovie 自己搞定。

## 二、架构：双进程同容器

```
┌─────────────── 容器 newmovie:2.0 ────────────────┐
│                                                  │
│  supervisord                                     │
│   ├─ openlist(139cas)  127.0.0.1:5244  ← 不对外  │
│   │    网盘挂载 / 直链 / .cas 二次元数据          │
│   └─ newmovie          0.0.0.0:8096    ← 唯一入口 │
│        媒体库 / 刮削 / 播放决策 / remux            │
│        /openlist/*  ──反向代理──▶ 127.0.0.1:5244  │
└──────────────────────────────────────────────────┘
```

选双进程而不是把 139cas 编译进 NewMovie，理由很实在：

| 维度 | 双进程同容器 | 源码级合并 |
|---|---|---|
| 上游同步 | `scripts/sync-openlist.sh` 一键覆盖 | 每次都要手工合冲突 |
| 139cas 改动量 | **0 行** | 大量适配 |
| 崩溃隔离 | 后端挂了 supervisor 自动拉起，前端照常提供已扫描内容 | 一起挂 |
| 模块冲突 | 两个独立 go.mod，互不干扰 | 依赖地狱（gin/gorm/… vs 零依赖） |

**代价**：内存多 100~200MB，镜像大 ~110MB。对 NAS/软路由这类目标设备完全可接受，
换来的是后续能一直跟着 139cas 上游走。

## 三、改动清单

### 新增：内置后端源码树 `openlist/`
- 从 `github.com/tianjian518/139cas@main` 完整引入（2026-08-02 快照），**零修改**。
- 独立 Go 模块（`github.com/OpenListTeam/OpenList/v4`），不参与 `newmovie` 主模块构建。
- 清理了 `.github/`、`public/dist/`、`bin/`、`data/`。
- `openlist/UPSTREAM.md` 记录来源、139cas 相对上游 OpenList 的增量、同步方法。
- `scripts/sync-openlist.sh`：走 `ghproxy.net` / `gh-proxy.com` 镜像拉取上游，保留 `UPSTREAM.md`。

> 139cas 相对上游 OpenList 的增量：`internal/casmeta`（`.cas` 二次元数据）、
> `drivers/guangyapan`（广雅盘）、`server/handles/down.go` 里的 `.cas` 视频处理。

### 新增：自动接管 `internal/api/bootstrap.go`
NewMovie 启动后在后台完成这一串，用户全程无感：

1. 轮询 `/ping` 等后端就绪（500ms → 3s 退避，默认最多 90s）；
2. 取 Token —— 优先 `NEWMOVIE_BUNDLED_TOKEN`，其次 `NEWMOVIE_BUNDLED_PASS`，
   最后读 `/data/openlist_admin_pass`（entrypoint 首启生成的随机密码）；
3. 顺手读 `/api/admin/setting/get?key=token` 拿签名密钥，`/d/` 直链自己算 `sign=`，
   用户不需要去后台关签名；
4. 幂等写入存储表。

三个刻意的设计：

- **非阻塞**：跑在 goroutine 里。后端首启要初始化 DB + 加载几十个 driver，
  十几秒很正常，不能让 8096 端口跟着卡住。
- **无限重试**（30s 间隔，每 10 次记一条日志）：磁盘慢、supervisor 重启过后端，
  一次失败就永久放弃的话用户得手动重启整个容器。
- **保留用户改动**：已存在同 BaseURL 的存储时，只刷新 Token 和签名密钥，
  名称、限速、LocalRoot 原样保留 —— 重启不该冲掉手工调整。

### 新增：反向代理 `internal/api/bundledproxy.go`
- `/openlist/*` → `127.0.0.1:5244`，`httputil.ReverseProxy` 实现。
- `FlushInterval: -1` 关闭缓冲，保证下载/流式响应不被攒在内存里。
- 注入 `X-Forwarded-Prefix` / `X-Forwarded-Proto`；裸 `/openlist` 302 到 `/openlist/`。
- 结果：**只暴露一个端口**。5244 只监听回环，不对公网开放。

### 扩展：`internal/openlist/client.go`
新增 `Login()`（`POST /api/auth/login`）、`Ping()`、`SettingSignAll()`。

### 扩展：`internal/config/config.go`
| 变量 | 默认 | 说明 |
|---|---|---|
| `NEWMOVIE_BUNDLED` | `0`（镜像内为 `1`） | 是否启用内置后端 |
| `NEWMOVIE_BUNDLED_URL` | `http://127.0.0.1:5244` | 后端地址 |
| `NEWMOVIE_BUNDLED_NAME` | `内置网盘` | 自动注册的存储名 |
| `NEWMOVIE_BUNDLED_TOKEN` | 空 | 显式 Token（配了就跳过登录） |
| `NEWMOVIE_BUNDLED_USER` | `admin` | 后端管理员用户名 |
| `NEWMOVIE_BUNDLED_PASS` | 空 | 后端管理员密码（不填则读密码文件） |
| `NEWMOVIE_BUNDLED_TIMEOUT` | `90s` | 单轮就绪等待上限 |
| `NEWMOVIE_BUNDLED_PROXY` | 跟随 `BUNDLED` | 是否开 `/openlist/` 代理 |

### 容器：`Dockerfile` / `docker/` / `docker-compose.yml`
- 四阶段构建：NewMovie 前端 → OpenList 前端 dist（镜像站下载）→ 139cas 后端 → NewMovie 后端。
- **139cas 用 `CGO_ENABLED=0` 编译成功**（它用 `glebarez/sqlite` 纯 Go 驱动），
  意味着多架构交叉编译不需要 musl-cross 工具链，构建快得多。已验证产物 112MB 可正常运行。
- `docker/supervisord.conf`：openlist（priority 10）+ newmovie（priority 20），
  都 autorestart，日志统一到 stdout；`quit-on-fail` eventlistener 在子进程 FATAL 时干掉容器
  （避免 docker 层面看到"健康"但实际半死）。
- `docker/entrypoint.sh`：首启生成 20 位随机密码写 `/data/openlist_admin_pass`（600 权限），
  `openlist admin set` 幂等初始化，`PORT` → `VIDRIVE_ADDR` 适配（HF Space / Railway）。

> **踩到的坑（值得单独记一笔）**：OpenList 的 `config.json` 一旦生成，
> `OPENLIST_HTTP_PORT` 环境变量就**完全不生效**了，配置文件优先。
> 这在 E2E 测试里第一次暴露（设了随机端口，它还在 5244 上，测试干等 60s 超时），
> 但更要命的是它同样会坑到真实容器部署。entrypoint 现在用 python3 直接改写
> `config.json` 的 `scheme.address=127.0.0.1 / http_port=5244 / https_port=-1`
> （无 python3 时退化到 sed）。

### 前端：`web/src/App.tsx`
- `useBundled()` 轮询 `/api/health`，读 `bundled` / `bundled_ready` / `bundled_prefix`；
  就绪后停止轮询，不做无谓请求。
- 桌面侧栏与手机底栏新增「网盘挂载」入口，带状态点：**绿色=已接管，琥珀色脉冲=启动中**。
- 1.x 式裸跑部署（`NEWMOVIE_BUNDLED=0`）完全看不到这个入口，界面保持原样。

### `/api/health` 新增字段
```json
{ "bundled": true, "bundled_ready": true, "bundled_prefix": "/openlist/" }
```

## 四、验证

### 单元测试（22 个新增，全绿）
- `bootstrap_test.go`（9）：登录注册 / 幂等 / 保留用户改动但刷新 Token / 慢启动等待 /
  显式 Token 跳过登录 / 读密码文件 / 后端不可达优雅失败 / 密码错误 / 关闭时 no-op。
- `bundledproxy_test.go`（7）：剥前缀 / 裸根重定向 / 根映射 / 后端挂了返回 502 /
  非法目标返回 nil / POST body 转发 / query 保留。
- `config_test.go`（6）：默认关闭 / 开启时自动开代理 / 代理可单独关 /
  URL 去尾斜杠 / 超时解析 / boolenv 各种写法。

### 真实双进程 E2E（`bundled_e2e_test.go`）
不是 mock —— 起**真的 139cas 二进制**，跑完整链路：

```
139cas 启动 → admin set 密码 → 挂载 Local 驱动
  → BootstrapBundled 拿到真 JWT（203 字符）+ 真签名密钥（109 字符）
  → 自动注册存储（用户零配置）
  → 经 /openlist/ 代理 ping 通
  → 浏览 /local → 建媒体库 /local/电影
  → 扫描识别出「测试影片 (2024)」
  → 点播放，决策 L2，自动带上 sign=
  → 真实 ffmpeg 重封装 → 输出合法 MP4（bytes[4:8] == ftyp，av1+aac）
```

### 浏览器实测
真实 Chromium 跑桌面（1440×900）与移动（390×844）两种视口：
- 登录 → 侧栏「网盘挂载」入口存在，`href="/openlist/"`，状态点为绿色（已接管）；
- 点击 → 新标签打开 `/openlist/`，139cas 管理界面经代理正常渲染；
- 手机底栏「网盘」入口同样存在；
- **控制台零错误**。

截图：`2.0_ui_desktop.png` / `2.0_ui_mobile.png` / `2.0_openlist_proxy.png`。

## 五、升级说明

### 从 1.x 升级
`docker-compose.yml` 镜像换成 `tianjian518/newmovie:2.0` 即可。数据卷 `/data` 兼容，
原有媒体库、观看进度、外部 OpenList 存储配置全部保留 —— 内置网盘是**新增**一条存储，
不会动你已有的。

### 想继续用外部 OpenList
设 `NEWMOVIE_BUNDLED=0`，内置进程仍在跑但 NewMovie 不接管、不显示入口，
行为与 1.x 完全一致。

### 内存
双进程，建议至少 1G（`mem_limit: 1g`）。ARM 小内存 NAS 尤其注意。

## 六、已知限制

- 内置 139cas 的**管理界面登录是独立账号**（admin + 密码文件里的随机密码），
  与 NewMovie 的登录不打通。做 SSO 需要改 139cas 源码，与「零修改上游」原则冲突，
  暂不做。密码可用 `NEWMOVIE_BUNDLED_PASS` 自己指定。
- 沙箱环境里 `github.com` 直连被 TLS 拦截，`scripts/sync-openlist.sh` 与 Dockerfile
  的前端下载都内置了 `ghproxy.net` / `gh-proxy.com` 镜像回退。

## 七、2.0 代码审计与修复（PM + 高级程序员双视角走查）

以「高级产品经理」看体验闭环、「高级程序员」看实现正确性，对 2.0 核心代码做了一次
端到端走查，定位并修复了 **4 个真实 bug**（非风格问题）。全部带回归测试，已随
`7930645` 提交并推送 `main`，并重建镜像验证。

| # | 视角 | 文件 | 问题 | 修复 |
|---|------|------|------|------|
| 1 | 程序员 | `bundledproxy.go` | 反代不重写 3xx 的 `Location`/`Content-Location`。OpenList 的 `/@login`、根路径重定向若落到 `/openlist` 前缀之外，浏览器直接 404 白屏；指向后端自身的绝对 URL 会指向未暴露的 5244 端口。 | `ModifyResponse` 重写 `Location`：绝对路径加 `/openlist` 前缀；指向后端的绝对 URL 改写成同源根相对 `/openlist/...`（浏览器按当前源解析，绝不指回后端端口）；第三方绝对 URL 不动。 |
| 2 | 程序员/安全 | `netguard.go` | `guardedDial` 校验完 IP 后又用**主机名**二次解析去 dial，存在 DNS-rebind TOCTOU——攻击可在「校验」与「建连」之间把域名指向内网，绕过全部 SSRF 拦截。 | 解析后挑出「已校验通过」的 IP 直接 `DialContext(ip:port)`，不再二次解析。 |
| 3 | 产品经理 | `bootstrap.go` | `bundledReady` 首次成功后置 `true` 且**永不复位**。139cas 被 supervisor 重启 / Token 失效后，UI 仍显示「已挂载」，用户点进去全是报错——典型「状态假绿」。 | 增加 30s 保活循环：后端健康检查失败即复位状态并自动重新接管，UI 状态对齐真实情况。 |
| 4 | 程序员 | `handlers.go` | `/api/play/proxy` 把上游响应头**逐字全拷贝**，会把上游 `Set-Cookie` 落到 NewMovie 域名、逐跳头透传、`Content-Length` 与实际写出字节不符触发 `wrote more than declared` panic。 | 只透传端到端头，丢弃 `Connection/Te/Upgrade/Transfer-Encoding/Set-Cookie/Content-Length` 等；`Content-Length` 改由服务端按实际字节重算（`Content-Range` 保留，拖拽不受影响）。 |

### 验证

- `go test ./internal/api/` 全量通过（新增：反代 `Location` 重写三场景、SSRF 拦截清单 `isBlockedIP`、 `guardedDial` 校验路径）。
- 真实 Chromium 跑 `play_verify.js`：登录 → 打开播放页 → `<video>.src` 带 `token=`、
  `remux` 200、`readyState=4`、播到结尾（2.04s）、**无 401 / 无控制台错误**。4 处修复零回退。
- 镜像 `newmovie:2.0-test`（`82ca83bbb0`）重建并起容器 `nm2test`，`/api/health` 返回
  `bundled_ready=true`，OpenList 管理界面各路径正常。

### 交付物

- 源码：`7930645` 已推送 `origin/main`（同时触发 GitHub Actions 多架构构建，推 `ghcr.io` +
  Docker Hub）。
- 本地镜像包：`newmovie-2.0-image.tar.gz`（`docker load` 即得 `tianjian518/newmovie:2.0` 与 `:latest`）。


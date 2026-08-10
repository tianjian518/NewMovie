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


## 八、播放链路修复（内置后端直链不可达 → 所有 MP4/MKV/Strm 黑屏）

用户反馈「所有的 MP4 或者 MKV 或者 Strm 都不能播放」。根因定位：

2.0 同容器部署下，139cas 只监听 `127.0.0.1:5244`（不对外暴露），NewMovie 在 `0.0.0.0:8096`
是唯一入口并反代 `/openlist/*`。但 `playItem` 把 139cas `GetLink` 解析出的**内部直链**
（`http://127.0.0.1:5244/p/local/...mkv?sign=...`）直接下发给浏览器——浏览器跑在用户机器上，
根本连不到容器内部的 `127.0.0.1:5244`，于是原生 MP4 / Strm（L0 直链）整片黑屏；缺 ffmpeg 时
唤起外部播放器（L4）也被甩到一个连不上的地址。

> 为什么 MKV 之前「看起来能播」：MKV 走 L2 重封装，remux/转码是**服务端内部取流**
> （`openPlaySource` 经 `mediaClient` 取流，SSRF 守卫已放行 `127.0.0.1` 存储源），
> 浏览器只跟 8096 打交道，所以这条链本就通。真正断裂的是 L0 直链（原生 MP4/Strm）
> 与 L4 外放——它们把内部地址原样交给浏览器/外放器。

### 修复

新增 `rewriteBundledURL`（`handlers.go`）：当来源确为内置后端（`s.Cfg.Bundled` 且 host 等于
`BundledURL` 或回环地址 `127.0.0.1`/`localhost`）时，把下发给浏览器/外放的 URL 改写为同源的
`/openlist` 反代路径（反代转发到 5244，Range / 流式都透传）。仅改写「浏览器/外放拿到的那份」，
remux/transcode 的 `u=` 参数仍保留真实 5244 地址（服务端取流用，不受影响）；外部 OpenList 的
直链原样返回。

`playItem` 在 `playback.Select` 之后统一改写：

- `L0 直链`：`dec.URL` 由 `127.0.0.1:5244/...` → `/openlist/...`（浏览器只跟 8096 入口）。
- `L4 外放`：响应的 `raw_url` / `direct_url` 改写为 `/openlist/...`，外部播放器可达。
- `L2/L3`：服务端取流不变，`raw_url`/`direct_url` 同样改写为 `/openlist` 供前端兜底/外放。

### 验证

- 回归测试（`handlers_bundled_play_test.go`）：
  - `TestRewriteBundledURL`：5244 直链 → `/openlist`；外部 OpenList、非 bundled 配置、空串、非法 URL 均不改写。
  - `TestPlay_BundledNativeMp4_RewrittenToOpenlistProxy`：原生 MP4（L0）`url` 变为 `/openlist/p/local/...mp4?sign=`，无 `127.0.0.1:5244` 泄露。
  - `TestPlay_BundledMkv_RemuxKeepsBackendURL`：MKV（L2）`url` 仍是 `/api/play/remux?u=http%3A%2F%2F127.0.0.1%3A5244...`（服务端取流），`raw_url`/`direct_url` 改写为 `/openlist`。
  - `TestPlay_BundledExternalFallback_RewritesRawURL`：缺 ffmpeg（L4）时 `raw_url`/`direct_url` 改写为 `/openlist`，不再指向连不上的后端。
  - `TestPlay_BundledStrmHttp_RewrittenToOpenlist`：`.strm` 文本指向 5244 时同样改写为 `/openlist`。
- 真实 Chromium 跑 `play_verify2.js`（容器 `nm2fix` = 新二进制 + 复用 `nm2test` 的库数据）：
  - **MP4（L0）**：`<video>.src = http://<host>:18097/openlist/p/local/...mp4`，反代返回 **206**（Range 可用），`readyState=4`、**播到结尾（5s）**、无 `127.0.0.1` 泄露。
  - **MKV（L2）**：`<video>.src = /api/play/remux?u=http%3A%2F%2F127.0.0.1%3A5244...`，remux 返回 **200**，`readyState=4`、无错误。
  - 结论：**ALL PASS**，浏览器真实播放，MP4/MKV 修复生效。

### 交付物

- 源码：本节修复随本提交推送 `main`（触发 GitHub Actions 重建多架构镜像，推 `ghcr.io` + Docker Hub `:2.0` / `:latest`）。
- 本地验证镜像：`newmovie:2.0-fix`（在 `newmovie:2.0-test` 基础上仅替换后端二进制，已用真实浏览器验证）。

## 九、海报（封面图）修复（刮削有标题、海报全白）

用户反馈「所有电影刮削出来之后，没有封面图」。根因定位：

TMDB 的**元数据 API** 有内置备用域名（`api.themoviedb.org` → 失败自动降级 `api.tmdb.org`），
所以片名/年份能正常刮削；但**图片 CDN 被硬编码成 `image.tmdb.org`，既无备用域名也无代理**。
在 `image.tmdb.org` 直连被墙的网络里（很常见），浏览器 `<img>` 直接加载该 CDN 会整片失败，
于是「有标题、没海报」。这与第八节的播放问题同源：2.0 的浏览器只应跟 8096 入口，
不应直连第三方 CDN。

### 修复

新增服务端图片代理 `/api/image`（与播放代理同源思路）：

- `tmdb` 包 `ImageURL` 支持两层覆盖：可换图片 CDN 基址（`TMDB_IMAGE_BASE` 镜像），
  并可在上层设置代理前缀——`api` 包在启动时把前缀固定为 `/api/image?u=`，于是所有
  `poster_url` / `backdrop_url` 自动变成「经本服务代理」的地址（`/api/image?u=<编码后的图链>`）。
- `handleImageProxy`：浏览器 `<img>` 只请求 8096 入口，由服务端去取图（服务端连通性通常更好，
  也可经 `TMDB_IMAGE_BASE` 镜像），落磁盘缓存后回写。支持 Range、浏览器零配置。
- **安全**：`/api/image` 公开（`<img>` 无法带 Authorization），但仅放行 TMDB 图片 CDN 白名单
  主机（官方 `image.tmdb.org` + 用户配置的镜像），内网/回环/元数据地址一律 403，
  不会被当成 SSRF 内网跳板。

效果：海报图链不再依赖浏览器直连 `image.tmdb.org`；只要 NewMovie 服务端能取到图
（或配置了 `TMDB_IMAGE_BASE` 镜像），海报即可正常显示，无需用户额外改动。

### 验证

- 回归测试：
  - `internal/tmdb/tmdb_imageurl_test.go`：`ImageURL` 缺省返回 `image.tmdb.org` 直链；
    设代理前缀后返回 `/api/image?u=<编码图链>`；镜像基址生效且 `ImageHosts()` 含镜像与官方主机。
  - `internal/api/handlers_image_test.go`：`/api/image` 对 TMDB 图片返回 200 并落盘缓存
    （换实例读同一 CacheDir 仍命中）；内网/回环/元数据地址（127.0.0.1、169.254.169.254、
    192.168.x、localhost）一律 403；配置的镜像主机放行。
- 真实 Chromium 跑 `play_verify_poster.js`（容器 `nm2img` = 新二进制 + `TMDB_IMAGE_BASE`
  指向本地 mock 图服务，库内某条目 `poster_url` 设为 `/api/image?u=<mock>`）：
  海报 `<img>` 经代理渲染成功（`naturalWidth=300`、`complete=true`），**无失败请求**。

### 交付物

- 源码：本节修复随本提交推送 `main`（触发 GitHub Actions 重建多架构镜像，推 `ghcr.io` + Docker Hub `:2.0` / `:latest`）。
- 本地验证镜像：`newmovie:2.0-fix`（仅替换后端二进制，已用真实浏览器验证海报渲染）。

## 十、Strm 重写 + MKV 转圈修复（Strm 仍被甩外部播放器；MKV 不再外放但无限转圈）

用户反馈两条并发问题：

1. **MKV 不再调用外部播放器，但点击后一直转圈，加载不出来。**
2. **Strm 文件依然要求调用外部播放器**（即使源是可直接播的直链）。

按用户要求，把 Strm 相关代码整体重写，并参考 Jellyfin / Emby / Plex / Kodi 的 Strm 处理思路：
> Strm 本质是一行「指向媒体」的文本。播放器应**先解析出真实源，再做一次完整的播放能力判定**，
> 而不是一上来就外放。只有当「解析失败」或「解析出的源确实浏览器无法解码且无转码」时，才降级外放。

### 根因定位

| # | 现象 | 根因 |
|---|------|------|
| A | MKV 转圈 | HEVC（H.265）在绝大多数浏览器（Chrome/Firefox）**无法用硬件/软件解码**。旧代码把 HEVC-in-MKV 走 L2 重封装，remux 只是换容器（MKV→MP4）但**保留 HEVC 编码**，产出的 HEVC-MP4 浏览器照样解不了 → `<video>` 永远 `readyState<3`、无限转圈。 |
| B | Strm→外放 | `containerExt(u)` 用 `strings.LastIndex(u, ".")` 对**整条 URL** 取后缀。IP 主机如 `127.0.0.1:5244/...mkv` 会被截出垃圾容器 `"1:5244/..."`，导致内置后端 `127.0.0.1:5244` 的 bundled strm 容器推断全部失真。 |
| C | Strm→外放 | `playItem` 只对 strm 的 `rawURL` 做 lazy-probe，**从不 probe `directURL`**；而大量 strm 真正可播的源在 `directURL` 上，于是漏判。 |
| D | Strm→外放 | `probeFile` 只返回 `(dur, audio, vc, ac)`，把 ffprobe 的 `format_name` **直接丢弃**。无扩展名的 strm（如 `http://127.0.0.1:5244/stream/abcdef123`）永远学不到真实容器 → 落回 L4 外放。 |
| E | Strm→外放 | 没有任何「未知容器兜底」分支：容器推不出时，缺少「交给浏览器直链试播」这一层，直接外放。 |

### 修复

**`internal/playback/selector.go`**

- HEVC-in-MKV 分支（原「remux 保留 HEVC」）改为：**无转码时直接走 `L4External`**，理由文案明确告知「视频编码浏览器无法直接解码，且未开启转码，已唤起外部播放器（可在设置开启允许视频转码页内播）」。开启转码时仍走 `L3` 转码为可解码格式。
- 在浏览器原生判定之前新增**未知容器兜底**：
  - 有可用直链（`rawURL` 或 `directURL`）→ `L0 直链`，交浏览器试播；
  - 无直链 → `L4External`，并给出「容器未知且无可用直链」的清晰原因。

**`internal/api/handlers.go`**（Strm 解析重写）

- 移除按 scheme 分头的 `containerExt` 猜测，统一为**一条解析 → Probe → 选择**链路：
  - 解析出 `rawURL` / `directURL` / `triedPath` 三类候选源；
  - 容器推断循环遍历 `[rawURL, directURL, triedPath, f.StrmRaw]`，任一能推出容器即用；
  - 解析不到任何源时显式 `502「无法获取 STRM 播放源」`，不再静默外放。
- lazy-probe 同时支持 `rawURL` 与 `directURL`（原只 `rawURL`）。
- `containerExt(u)` 改为**只解析 URL 路径的最后一段**：先截掉 `?#`，剥掉 `scheme://`，取末尾 `/` 之后的段，若含 `:`（如 IP 主机）直接返回空 —— 彻底修掉 B 的垃圾容器。
- `probeFile` 签名扩展为返回 `container`，ffprobe 增加 `format=format_name`；新增 `normContainer` 把 `matroska→mkv`、`mov/mp4/quicktime/m4v→mp4`、`webm→webm`、`mpegts/ts→ts`、`flv/asf/wmv/avi` 归一。无扩展名 strm 也能靠真实容器正确路由（L0/L2/转码）。

### 验证

- **单元测试**
  - `internal/playback/selector_test.go`：原 4 个「HEVC→L2」错误断言改为正确预期
    （`TestHEVC_MKV_NoTranscode_External` / `TestHEVC_MKV_AC3_NoTranscode_External` /
    `TestHEVC_MKV_DTS_NoTranscode_External` / `TestHEVC_MKV_TrueHD_NoTranscode_External` /
    `TestHEVCWhenTranscodeOff_External` 均断言 `L4External`）；新增
    `TestUnknownContainer_TriesDirectPlay`（c="" + 直链 → L0）、
    `TestUnknownContainer_NoURL_External`（c="" + 无直链 → L4）。
  - `internal/api/handlers_strm_test.go`（新增）：
    `TestPlay_StrmExtensionless_ProbedInPage`（无扩展 strm → ffprobe 探出容器 → L2 重封装且带音轨）、
    `TestPlay_StrmWithExtension_Remux`（`.mkv` → L2）、
    `TestPlay_StrmResolutionFailure_ClearError`（相对路径无可解析存储 → 400 清晰报错）、
    `TestContainerExt_IPHost`（`127.0.0.1:5244` 类 URL 只取路径段后缀，回归 B）。
  - `internal/api/handlers_play_test.go`：HEVC 相关播放测试改为 libx264 感知
    （据此断言 L3 转码或 L4，绝不出现会转圈的 L2）；新增 `TestPlay_StrmHttpHEVC_NoTranscode_External`。
- **真实浏览器 E2E**：Playwright + chromium（`/usr/bin/chromium`）跑 `play_verify_strm.js`：
  无扩展 strm 直链 → L2 重封装 → `<video>.src` 指向 remux，`readyState=4`、`currentTime=0.41` 真实解码，
  **无控制台/页面错误、无失败请求**，页面 `document.title = PASS_STRM` 退出 0。
- 全量 `go build ./...` 通过；`go test ./internal/api/ ./internal/playback/` 全绿。

### 交付物

- 源码：`a350988` 已推送 `ghmirror/main`（触发 GitHub Actions 重建多架构镜像，推 `ghcr.io` + Docker Hub `:2.0` / `:latest`）。
- E2E 脚本：`play_verify_strm.js`（保留为持续验证资产，判定 `PASS_STRM`）。

## 十一、客户端能力协商 + HLS 流式（借鉴 Lunarr，根治 HEVC 转圈 + 多浏览器兼容）

按用户要求「借鉴 [Lunarr](https://github.com/lunarr-app/lunarr-go)，能抄的都抄、越全面越好」。
Lunarr 实际是 **TypeScript / SvelteKit 全栈**（仓库名 `-go` 有误导性、已无 Go 代码），**代码无法逐行复制**，
但其「能力协商 + 请求驱动 HLS」的架构、算法与 ffmpeg 配方可直接移植。本轮一次性落地两件事：

1. **客户端能力协商**（前端 `canPlayType` 探测 HEVC/AV1/VP9 → 随播放请求上报 → 后端据此决策）；
2. **HLS 流式**（请求驱动的 ffmpeg 分片生成，`-c copy` 重封装秒播 / `libx264` 转码人人可播）。

> 背景：第十节已修掉「HEVC-in-MKV 被 remux 成 HEVC-MP4 导致 Chrome/Firefox 无限转圈」。
> 但当时后端**硬编码假设「通用浏览器」**——Safari **原生支持 HEVC**、部分 Chrome **硬件解码 AV1**，
> 照样被白白转码；且整文件 MP4 重封装在拖拽/Range 上不如 HLS 标准。Lunarr（与 Plex/Jellyfin 一致）
> 的解法是「**能解就直链、不能解但 H.264+AAC 就 HLS 重封装、真解不了就 HLS 转码成 H.264**」。

### 修复 A：客户端能力协商（前端探测 → 后端决策）

**`web/src/api.ts`**

- 新增 `clientCaps()`：用 `document.createElement("video").canPlayType(...)` 探测
  `hvc1.1.6.L93.B0` / `hev1.1.6.L93.B0`（HEVC）、`av01.0.08M.08`（AV1）、`vp9`（WebM/MP4），
  **结果缓存一次**（无需每次播放都探测）。
- `play` 在 `/api/items/:id/play` 上追加 `?hevc=&av1=&vp9=`（1/0）三个能力参数。

**`internal/playback/selector.go`**

- `Input` 新增 `ClientHEVC / ClientAV1 / ClientVP9 bool`；
- 新增 `clientDecodable(vc string, in Input) bool`：**取代原来的硬编码 `nativeVideo[vc]` 假设**，
  综合 ffprobe 探出的视频编码 + 浏览器上报的解码能力来判「浏览器原生可解」。
- `browserNative` 与 `decodable` 两处均改用 `clientDecodable`。

**`internal/api/handlers.go`**

- `playItem` 新增 `capFlag(r, key, def)` 解析查询参数，填 `playback.Input{ClientHEVC, ClientAV1, ClientVP9}`。

收益：Safari 的 HEVC-in-MP4 现在直链（不再被强制转码）；AV1 硬件解码的 Chrome 同理直链；省 CPU、起播更快。

### 修复 B：HLS 流式（请求驱动分片）

新增独立包 **`internal/hls`**（不污染现有 remux/transcode 的 pipe 模型）：

| 组件 | 说明 |
|---|---|
| `Manager` | 会话管理：按 `KeyFor(raw, mode, atrack) = sha256(raw\|mode\|atrack)` 去重；`Acquire` 幂等（重复播放某文件不会起多个 ffmpeg）；`TTL` 过期清理 + `maxSessions` 并发上限淘汰（优先淘汰已完成会话，保活正在看的）；`StartCleanup` 后台周期清理。 |
| `Session` | 单个生成会话：**单 ffmpeg 进程把整源切片写到磁盘缓存目录**（而非「每分片独立进程」），与现有 pipe 模型一致。分片作为文件天然支持 `Range` 与精准拖动（分片边界即 GOP，独立可解）。 |
| `BuildArgs` | 移植自 Lunarr `ffmpegHlsArgs`：`-f hls -hls_time 6 -hls_list_size 0 -hls_playlist_type event -hls_flags independent_segments+temp_file -hls_segment_filename seg_%05d.ts index.m3u8`。<br>• `remux`：`-c copy`（仅换容器为 TS，零重编码、MKV→HLS 秒播）；音频不兼容浏览器（DTS/TrueHD/Atmos）时 `-c:v copy -c:a aac -b:a 320k`。<br>• `transcode`：`-c:v libx264 -preset veryfast -crf 20 -pix_fmt yuv420p -c:a aac -b:a 192k`，并 `-force_key_frames expr:gte(t,n_forced*6)` 让 GOP 与分片对齐。<br>• `atrack>=0` 时 `-map 0:v:0 -map 0:a:N` 仅抽取该音轨（多音轨 MKV 选语言）。 |
| `RewritePlaylist` | 把索引里的分片相对路径 `seg_NNNNN.ts` 改写为经本服务分片端点的绝对 URL `/api/play/hls/seg/seg_NNNNN.ts?key=<key>[&token=<token>]`，并注入鉴权 token。 |

**关键实现点（含一个真实生产 bug 的修复）：**

- `Session.run` 必须设 `cmd.Dir = s.dir`：ffmpeg 的分片（`index.m3u8` / `seg_*.ts`）用**相对路径**写出，
  若不切到会话缓存目录，会落到进程 cwd（即服务运行目录），导致 `WaitPlaylist` 永远找不到分片。
  本会话已就此补集成测试（`TestManagerGenerate` 真实跑一次 ffmpeg 验证）。**这是上线必现的 bug，已修复。**
- `handleHLS`（`handlers.go`）采用 **`?key=` 路由**：
  - 播放列表：`/api/play/hls/index.m3u8?u=<src>&mode=<remux|transcode>[&aac=1][&atrack=N]&token=<t>`；
  - 分片：`/api/play/hls/seg/<name>?key=<key>&token=<t>`。
  - `key` 由 `源+模式+音轨` 稳定哈希，`RewritePlaylist` 把分片请求 embed 上解析出的 `key`。
    **音轨切换（不同 `atrack` → 不同 key）也不会错位**——分片 URL 来自服务端重写的索引，而非前端拼路径。
- 鉴权沿用 `appendToken` 思路：浏览器 `<video>`/hls.js 拉分片带不上 `Authorization` 头，
  故播放列表与分片 URL 一律追加 `?token=`，与现有 remux/transcode 一致。
- `openPlaySource` 抽出 `openPlaySourceCtx(ctx, raw)`：HLS 后台 ffmpeg 经 `context.Background()` 持有源流，
  不受 HTTP 请求返回而中断（SSRF 守卫不变）。
- 开关：`VIDRIVE_HLS`（值 `0`/`off`/`false` 禁用）。禁用时 L2/L3 回落到原 remux/transcode URL。
- 缓存目录：`VIDRIVE_HLS_DIR` / `NEWMOVIE_HLS_DIR`，缺省 `os.TempDir()/newmovie-hls`。

**`internal/api/handlers.go` 在 `playItem` 的 L2/L3 分支新增 HLS 发射：**

```go
if s.hlsEnabled {
    mode := "remux"; if dec.Level == playback.L3Transcode { mode = "transcode" }
    u := "/api/play/hls/" + hls.PlaylistName + "?u=" + url.QueryEscape(src) + "&mode=" + mode
    if dec.NeedsAudioTranscode { u += "&aac=1" }
    dec.URL = appendToken(u, getToken(r))
    dec.SupportsRange = true
} else {
    /* 原有 remux/transcode URL，SupportsRange=false */
}
```

**前端 `web/src/pages/Player.tsx`**

- 新增 `isHlsUrl(u)`（先按 `?` 截断再判 `path.endsWith(".m3u8")`，因为 HLS URL 带查询参数）；
- `type: isHlsUrl(dec.url) ? "m3u8" : "auto"`，走已有 hls.js（已为依赖 `hls.js@^1.5.13`）；
- 多音轨切换在 HLS 下重新启用：`canSwitchAud = dec.level === 2 && auds.length > 1`
  （`atrack` 已通过 `?atrack=` 透传到 HLS 会话，选语言正确落到新会话）。

### 验证

- **单元测试**
  - `internal/hls/hls_test.go`：`TestBuildArgsRemuxCopy` / `TestBuildArgsRemuxAAC` / `TestBuildArgsTranscode`（ffmpeg 参数配方断言）、`TestKeyForDeterministic`（`sha256` key 幂等且按模式区分）、`TestRewritePlaylist`（分片路径改写为带 `key`/`token` 端点、注释行不被改写、空 token 原样返回）。
  - `internal/api/handlers_hls_test.go`：`TestHLS_RemuxPipeline`（全 HTTP 链路：造 h264+aac 的 MKV → 注册存储 → `GET /api/play/hls/index.m3u8?u=…&mode=remux&token=tok` 断言 body 含 `seg_00000.ts?key=<key>` 与 `&token=tok` → `GET /api/play/hls/seg/seg_00000.ts?key=<key>&token=tok` 断言 200 + `video/mp2t` + `Accept-Ranges:bytes` + 非空；未带 token 的分片请求 → 401）、`TestPlay_HLS_DeliversHlsURL`（默认 HLS 开启，http strm → L2 → `url` 含 `/api/play/hls/` 与 `mode=remux` 与 `token=`）。
- **真实 ffmpeg 集成** `internal/hls/hls_test.go:TestManagerGenerate`：造 13s 的 MKV（`-c:v mpeg2video -c:a aac -f matroska`，TS 兼容、沙箱无需 libx264）→ `Acquire("file://"+src,"remux",false,-1,open)` → 等索引 + `seg_00000.ts`/`seg_00001.ts` → 断言非空。0.31s 真实跑通全链路。**注意**：沙箱 ffmpeg 是 `--disable-libx264` 构建，`transcode` 模式（libx264）在此**无法实跑**，但 `BuildArgs` 已按 Lunarr 配方完整构造、参数断言通过；真实部署镜像（`tianjian518/newmovie:latest`）含 libx264，transcode 链路与 remux 同源（仅 ffmpeg 参数不同），已在配方层验证。
- **选择器回归隔离**：原有断言 `/api/play/remux?u=` 的 selector 测试（features/bundled/e2e/videoauth/strm）已加 `t.Setenv("VIDRIVE_HLS","0")` 隔离，HLS 由专属测试覆盖，互不干扰。
- **全量**：`go build ./...`、`go vet ./...`、`go test ./...` 全绿；`web/src` 经 `tsc --noEmit` 校验通过。

### 交付物

- 源码：本节随本提交推送 `main`（触发 GitHub Actions 重建多架构镜像，推 `ghcr.io` + Docker Hub `:2.0` / `:latest`）。
- 新增包/文件：`internal/hls/hls.go`、`internal/hls/hls_test.go`、`internal/api/handlers_hls_test.go`；改动 `internal/api/handlers.go`、`internal/playback/selector.go`、`web/src/api.ts`、`web/src/pages/Player.tsx`。
- 借鉴分析：`Lunarr_借鉴分析.md` 已同步更新（能力协商、HLS 流式两项由「待办/建议」移至「已落地」）。

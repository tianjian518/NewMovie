# NewMovie · 云盘媒体库播放器

> 一个自托管的媒体服务器：网盘当硬盘，自动刮削成**海报墙**，点开就能播，记得你看到哪儿。
> **2.0 起内置 [139cas](https://github.com/tianjian518/139cas) 网盘后端 —— 一个容器就是全部。**

NewMovie 把「挂载网盘 → 扫描 → 海报墙 → 播放 → 进度/收藏」压缩进**一个容器**，不必再拼 OpenList + strm 生成 + Emby 扫描 + MediaWarp 劫持 302 + Nginx 反代那一套。

---

## 2.0：与 139cas 合体

1.x 需要你先自己部署 OpenList、去后台复制 Token、再回来手填地址和签名密钥。
2.0 把 139cas 直接打进镜像，这些步骤全部消失：

```
┌─────────────── 容器 newmovie:2.0 ────────────────┐
│  supervisord                                     │
│   ├─ openlist(139cas)  127.0.0.1:5244  ← 不对外  │
│   └─ newmovie          0.0.0.0:8096    ← 唯一入口 │
│        /openlist/*  ──反向代理──▶ 127.0.0.1:5244  │
└──────────────────────────────────────────────────┘
```

NewMovie 启动后**自动**登录内置后端、取 Token 与签名密钥、注册成默认存储。
用户要做的只有两步：`:8096/openlist/` 加网盘 → 回 `:8096` 建媒体库。

想继续用外部 OpenList？设 `NEWMOVIE_BUNDLED=0`，行为与 1.x 完全一致。

详见 [`NewMovie_2.0_发布清单.md`](./NewMovie_2.0_发布清单.md)。

---

## 为什么不是「直接用 strm」

OpenList 的 strm 驱动会把网盘影视伪装成 `.strm` 文件（里面写着 OpenList 自己的 `/d/` 302 直链）。但官方文档写明它**只在访问目录时生成、不支持定时/监听自动生成**——这就是「strm 文件夹一直是空的」「新增剧集媒体库不更新」的根源。

NewMovie 的立场（详见 [`PLAN.md`](./PLAN.md)）：

- **生成 strm**：可选，非必需。NewMovie 自己就是媒体服务器，云端路径直接入库当一等公民。
- **播放 strm**：**必须，一等公民**。存量库无缝导入；且对 7 种 strm 方言 + 编码坑全自动适配，并把 OpenList 的 `/d/` 链接**归一回原生模式**（自行算签名、可拿 `raw_url` 直连网盘 CDN）。

两种媒体库模式并存：**原生模式**（推荐）与 **STRM 模式**，可一键切换、保留进度与收藏。

---

## 核心能力

- **OpenList 原生 API 驱动**：`/api/fs/list` 列目录、`/api/fs/get` 取直链、`refresh=false` 复用 OpenList 缓存（**几乎零风控风险**）、自行算 sign（**不必为了能用而关闭签名**）。
- **五级播放降级链**：`302 直链 → 代理转发 → Remux 重封装 → 真转码 → 外部播放器`。MKV 不再「不支持的视频格式」——H.264/AAC 的 MKV 走 L2 重封装秒播，H.265 唤起外部播放器或转码。
- **STRM 方言全适配**：完整直链 / 带签名 / URL 编码 / 纯内部路径 / 服务中转 / 本地绝对路径 / 相对路径 + BOM/CRLF/中文坑。
- **路径重写规则**：存量 strm 里写死 `http://localhost:5244/...`？一条正则搞定，不必重生成几万个文件。
- **风控友好**：令牌桶限速（默认 2 req/s）、增量扫描、指数退避、`refresh=false` 复用缓存。
- **海报墙 + 详情 + 播放进度 + 继续观看 + 收藏**：Emby 深色紧凑 + 大图海报墙。
- **刮削元数据更准**：
  - **NFO 优先**：同目录 `.nfo`（movie / tvshow / episodedetails）给出 tmdb id、标题、年份、简介、远程图。
  - **剧集 `tvshow.nfo` 递归父目录**：系列级 `tvshow.nfo` / `poster.jpg` / `fanart.jpg` 不在单集同目录、而在剧集根目录时，自动沿父目录向上查找（优先级高于单集 nfo）。
  - **`.vidrive.json` 手动锁定**：同目录或父目录放一个 `{"tmdb_id":27205,"type":"movie","title":"...","year":2010}`，手动锁定的 TMDB-ID / 标题 / 类型优先级最高，专治识别错乱。
  - **本地图代理 + 服务端缓存**：同目录 `poster.jpg` / `fanart.jpg` 与 NFO 远程图统一走服务端代理，并落磁盘缓存（命中直出、支持 Range），规避直链过期、减少回源。
  - **`.cas` 资源支持**：139cas（OpenList rebrand）的 `.cas` 指针文件按 strm 同类处理并归一化解析。
- **多架构镜像**：`linux/amd64` + `linux/arm64` + `linux/arm/v7`（树莓派等），NAS / ARM 设备通用。
- **稳定性护栏**（1.1.1）：目录环 / 超深嵌套检测、全链路 panic 恢复、合并落盘（写入快 ~370 倍）、原子写库、优雅退出、监听重试与容器健康检查 —— 彻底根治小内存 ARM 设备上的**无限重启**。
- **登录态修复**（1.1.3）：根因是 `App` 用 `useState(getToken())` 只在挂载时读一次 localStorage，登录成功后即便 token 已写入也无法重新渲染，表现为「点登录毫无反应、手动刷新才进得去」。改为 `onLogin` 回调 `setTok` 触发重渲染，并新增 `api` 在收到 `401` 时派发 `newmovie:unauthorized` 事件、`App` 监听后自动退回登录页并清 token —— 顺手解决了 token 失效后整站卡死的问题。
- **OpenList 连接修复**（1.1.4）：部分 OpenList / AList（尤其 139cas 等 rebrand）把 `size` / `modified` 序列化成字符串，导致「测试连接」报 `cannot unmarshal string into ... modified of type int64`。新增 `FlexInt64` 类型兼容数字 / 字符串数字 / 浮点 / ISO 日期串，解析不出回退 0，不再因单条脏数据让整次列表或测试连接失败。

---

## 快速开始（Docker）

```bash
docker compose up -d

# 1) http://<你的IP>:8096/openlist/   —— 网盘挂载管理（内置 139cas）
#    用 admin + ./data/openlist_admin_pass 里的密码登录，添加你的网盘
# 2) http://<你的IP>:8096/            —— 媒体库与播放器
#    用 VIDRIVE_ADMIN_PASS 登录，建媒体库、扫描、点开即播
```

内置网盘的存储条目由 NewMovie **自动注册**，不需要手填地址、Token 和签名密钥。
侧栏「网盘挂载」入口上的状态点：绿色=已接管，琥珀色脉冲=后端还在启动。

### 自行构建镜像

```bash
docker build -t newmovie:2.0 .

# 访问 dl-cdn.alpinelinux.org / proxy.golang.org / registry.npmjs.org 受限时，换镜像源：
docker build \
  --build-arg APK_MIRROR=https://mirrors.tencent.com/alpine \
  --build-arg GOPROXY=https://mirrors.tencent.com/go/,direct \
  --build-arg NPM_REGISTRY=https://mirrors.tencent.com/npm/ \
  -t newmovie:2.0 .
```

---

## 配置（环境变量）

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `VIDRIVE_ADDR` | `:8096` | 监听地址 |
| `VIDRIVE_DATA` | `/data` | 数据目录（SQLite/JSON、图片缓存、配置） |
| `VIDRIVE_ADMIN_USER` | `admin` | 管理员用户名 |
| `VIDRIVE_ADMIN_PASS` | `admin` | 管理员密码（**生产务必修改**） |
| `VIDRIVE_SCAN_RATE` | `2` | 全局默认扫描限速（req/s，风控友好） |
| `TMDB_API_KEY` | _(空)_ | TMDB API Key；**留空则跳过 TMDB**，仅靠 NFO（同目录 `.nfo` / 本地图）与文件名识别刮削。也可在「设置」页填入并持久化（环境变量优先） |

### 2.0 内置网盘

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `NEWMOVIE_BUNDLED` | 镜像内 `1` | 是否启用内置 139cas 后端。设 `0` 退回 1.x 的外部 OpenList 模式 |
| `NEWMOVIE_BUNDLED_URL` | `http://127.0.0.1:5244` | 内置后端地址 |
| `NEWMOVIE_BUNDLED_NAME` | `内置网盘` | 自动注册的存储名（可在界面改，重启不覆盖） |
| `NEWMOVIE_BUNDLED_TOKEN` | _(空)_ | 显式 Token，配了就跳过自动登录 |
| `NEWMOVIE_BUNDLED_USER` | `admin` | 内置后端管理员用户名 |
| `NEWMOVIE_BUNDLED_PASS` | _(空)_ | 内置后端管理员密码。不填则首启随机生成并写入 `/data/openlist_admin_pass` |
| `NEWMOVIE_BUNDLED_TIMEOUT` | `90s` | 单轮就绪等待上限（失败后仍会每 30s 重试） |
| `NEWMOVIE_BUNDLED_PROXY` | 跟随 `BUNDLED` | 是否开启 `/openlist/` 反向代理 |

---

## 本地开发

```bash
# 后端（零依赖，直接跑）
go run ./cmd/server

# 前端（另开终端，代理 /api 到 :8096）
cd web && npm install && npm run dev
```

构建镜像：

```bash
make docker IMAGE=tianjian518/newmovie   # 多架构构建并推送 Docker Hub
```

---

## API 速览

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/login` | 登录换 token |
| GET | `/api/storages` / POST | 存储源列表 / 新增 |
| POST | `/api/storages/test` | 测试连通性并返回已挂载网盘 |
| GET | `/api/storages/:id/drives` | 列出已挂载网盘 |
| GET/POST | `/api/libraries` | 媒体库列表 / 新增 |
| POST | `/api/libraries/:id/scan` | 启动扫描（异步） |
| GET | `/api/libraries/:id/items` | 海报墙条目 |
| GET | `/api/items/:id` | 条目详情 + 文件 |
| GET | `/api/items/:id/play` | 五级降级决策 + 直链 |
| POST | `/api/play/record` | 保存播放进度 |
| GET | `/api/continue` / `/api/favorites` | 继续观看 / 收藏 |
| GET/POST | `/api/rewrites` | strm 路径重写规则 |

---

## 项目结构

```
cmd/server/        入口 + embed 前端静态资源
internal/model/    领域模型（对应 PLAN 数据模型）
internal/store/    持久化层（JSON 文件实现，接口后易换 SQLite）
internal/openlist/ OpenList API 客户端 + 签名
internal/strm/     STRM 方言解析 + /d/ 归一化 + 重写
internal/playback/ 五级播放降级链
internal/parser/   文件名 → 标题/年份/季集
internal/scanner/  扫描（限速/增量/断点）
internal/api/      REST 接口
web/               React + Vite 前端
```

---

## 状态与路线

- **Phase 1（当前）**：原生/STRM 双模式、OpenList 驱动、扫描、海报墙、L0/L1 播放、进度/收藏、Docker 交付。
- **Phase 2**：通用 WebDAV、多用户/权限、搜索、L4 外部播放器进度回传、死链检测。
- **Phase 3**：L2 Remux + L3 转码（`:full` 镜像）、内封字幕/多音轨、反向导出 strm、PWA。
- **Phase 4**：自研原生网盘驱动（去 OpenList 依赖）、弹幕、多端客户端。

---

## 许可

本项目为**全新自研**，不复制 Emby / OpenList 代码。后端采用 MIT（如需）。仅以二者功能为产品隐喻。

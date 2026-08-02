# NewMovie v1.1.13 — MKV 页内播放（视频转码 HEVC→H.264）+ STRM 页内播放

- 日期：2026-08-02
- 基线：v1.1.12（`93333e2`）
- 主题：用户反馈「MP4 正常，但 MKV 提示视频不存在；STRM 无法页内播放，只弹外部播放器」。

## 一、问题根因

### 1) MKV「视频不存在」
v1.1.12 只修了**音轨**（DTS/TrueHD→AAC），让 MKV 不再弹外部播放器，但**视频仍是 HEVC**——
而 4K 蓝光原盘几乎都是 HEVC，多数浏览器（尤其 Firefox、多数 Chrome）**不带 HEVC 解码器**。
重封装成 MP4-HEVC 后浏览器照样放不出，前端表现为「视频不存在 / 加载失败」。

> 根因不是重封装管线（已验证 `-c copy` 对 mpeg4+DTS→MP4 正常），而是**视频编码浏览器解不了**，
> 缺一条「视频转码」的降级链（原 `TranscodeEnabled` 硬编码 `false`，且无转码端点）。

### 2) STRM 只能外部播放器
`playItem` 对 `.strm` 与原生文件一视同仁，用 `f.StorageID` + `f.Path` + `GetLink` 取源。
而 **http(s) 直链型 strm 的 `StorageID` 是空的**（resolver 只为 openlist scheme 设 StorageID），
于是 `GetStorage("")` 直接报「存储源不存在」，**根本进不了播放决策**，只能甩外部播放器。

## 二、改动

### 播放降级链（`internal/playback/selector.go`）
- 新增 `transcodableVideo` / `transcodableContainers` 映射（视频转码白名单）。
- 决策改为三段式：
  1. 容器不兼容、但**视频编码浏览器能解**（h264/vp9/av1）→ **L2 重封装**（音轨按需转 AAC）；
  2. 视频编码浏览器解不了（**HEVC 等**）且**开启转码**且可转 → **L3 视频转码**（HEVC→H.264 + AAC）；
  3. 视频可重封装但浏览器解不了（HEVC）→ 仍走 **L2**（保留给 HEVC 能力浏览器：Safari、带扩展的 Chrome）。
- 转码关闭时 HEVC 行为不变（仍 L2，未引入回归）。

### 取源与转码端点（`internal/api/handlers.go`）
- `playItem` 对 `Source==SrcStrm` 用 `strm.Resolver` 重新解析 `StrmRaw`，得到
  `http/https` / `openlist` / `file` 源并取到真实 URL（修复 http 直链 strm 报「存储源不存在」）。
- 新增 `TranscodeEnabled` 读取：环境变量 `VIDRIVE_TRANSCODE` 为默认，前端「设置」开关
  `transcode_enabled` 可覆盖（持久化）。
- 源为空时返回清晰错误（不再静默返回空 URL 导致浏览器「视频不存在」）。
- 新增 **`/api/play/transcode` 端点**：ffmpeg `libx264` + AAC → fragmented MP4 流式回写。
- `handleRemux`/`handleTranscode` 抽出 `openPlaySource` + `streamFFmpeg` 复用；支持本地文件
  （`file://`，供 CloudDrive2 等本地 strm），但**仅放行 `VIDRIVE_LOCAL_ROOTS` 配置目录**，
  防任意文件读取（SSRF）。

### 前端
- `Settings.tsx`：新增「允许视频转码（HEVC→H.264）」开关，持久化 `transcode_enabled`。
- `Player.tsx`：L3 也渲染为页内 ArtPlayer；视频加载/解码失败时给出明确引导
  （「可能是浏览器不支持 HEVC，可在设置开启视频转码重试，或用外部播放器」）。

## 三、安全

- `file://` 本地文件读取**仅当路径落在 `VIDRIVE_LOCAL_ROOTS` 内才放行**，否则 403。
  未配置则拒绝，保住原有 SSRF 防护（已用 `TestRemux_BlocksSSRF` 锁定：
  `file:///etc/passwd` 必须被拦截）。

## 四、验证

- `go test ./...` 全绿：
  - selector 新增 `TestL3TranscodeHEVCWhenEnabled`（HEVC+转码→L3）、`TestL2RemuxHEVCWhenTranscodeOff`（HEVC+转码关→L2）。
  - playItem 新增 `TestPlay_StrmHttpResolvesInPage`（http strm→L2 remux 页内播）、
    `TestPlay_StrmHttpHEVC_TranscodeWhenEnabled`（HEVC+转码→L3）。
  - `TestRemux_BlocksSSRF` 仍拦截 `file:///etc/passwd`。
- 功能：用 ffmpeg 造 h264+aac 的 MKV，`/api/play/remux` 返回合法 MP4（含 `ftyp`）。

## 五、部署注意（务必读）

- **必须重新构建/部署后才会生效**（拉镜像或 `go build` / `make build` 重启）。
- 视频转码默认**关闭**（CPU 开销大）。遇到 MKV/HEVC「视频不存在 / 无法播放」时，
  在「设置」开启「允许视频转码」即可；开启后 HEVC 会实时转成 H.264 页内播。
- 本地文件型 strm（CloudDrive2 等）需设置 `VIDRIVE_LOCAL_ROOTS=/mnt/cd2,...` 才放行读取。
- 前提：运行环境已装 `ffmpeg`（缺则转封装/转码端点明确报错）。

## 六、版本

`internal/api/handlers.go` 版本号 `1.1.12` → `1.1.13`。

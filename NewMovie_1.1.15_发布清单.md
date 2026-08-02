# NewMovie v1.1.15 — STRM/.cas 取链失败兜底（根除 502）+ ffmpeg 能力探测与优雅降级 + HEVC-in-MP4 回归修复

- 日期：2026-08-02
- 基线：v1.1.14（`43c0e2e`）
- 主题：用户反馈「MKV 和 Strm 还是不能播，而且被改过之后 MP4 也不能播了」。本版从源头把
  `*.cas` / OpenList `/d/` 型 STRM 的取链失败彻底兜底，并修掉 v1.1.13 引入的 MP4 回归、
  把「缺 ffmpeg」从「直接播不了」变成「优雅降级 + 明确提示」。

## 一、问题根因

### 1) STRM / `.cas` 报「网盘直链为空（502）」
CloudDrive2 的 `.cas` 经 OpenList 取链，返回的 `raw_url` 本身就是 OpenList 的
`/d/...cas?sign=` 中转直链——它自身 302 即跳真实网盘解密流（如天翼云盘 `ctyunxs.cn`），
remux/转码端点能直接跟随并产出 MP4（已用真实 4GB 源验证）。

但 `playItem` 对 `openlist` scheme 会**先用内部路径去 fs_get 重新取链**（`resolveOpenListLink`）。
当这次取链因「路径编码对不上 / CloudDrive2 模板占位符 `{season_episode}` / 签名态异常」等原因
返回空时，代码没有回退，直接 502「网盘直链为空」，于是 `.cas` 类 STRM 页内播不了。

> 关键洞察：**原始 `StrmRaw` 已经是一个可直连的 `/d/` 直链**，remux 端点拿着它就能成功。
> 所以取链失败时，直接回退用原始 `StrmRaw` 即可——零配置、零负担。

### 2) MP4 回归（v1.1.13 引入）
v1.1.13 把「容器需要重封装」的判定放宽，导致 **HEVC 封装在 MP4 里**也被错误地路由到
**L2 重封装**。但浏览器解不了 HEVC（无论容器是 MKV 还是 MP4），重封装出来的 MP4-HEVC
照样黑屏/「视频不存在」。正确做法：HEVC-in-MP4 必须走 **L3 转码（HEVC→H.264）** 或 **L4 外部播放器**，
绝不能走 L2。

### 3) 缺 ffmpeg 时体验崩坏
旧逻辑在「没 ffmpeg」时仍把 MKV/HEVC 推进重封装/转码路径，结果要么端点直接报错、要么返回空
200 让播放器黑屏，用户面对一堆看不懂的报错。

## 二、改动

### 取源兜底（`internal/api/handlers.go` · `playItem` 的 `openlist` 分支）
- 当 `resolveOpenListLink` 取链失败（`rawURL`/`directURL` 皆空）**且原始 `StrmRaw` 是 http(s) 直链**
  （含 OpenList `/d/` 中转链接）时，**直接回退用 `StrmRaw` 作为播放源**。
- 新增 `isStreamableURL()` 判断（`http://` / `https://`），不影响 `file://` 与相对路径的处理。
- 此兜底仅在「取链失败」时触发，正常取链（拿得到新鲜签名）路径完全不变，无回归。

### HEVC-in-MP4 回归修复（`internal/playback/selector.go`）
- 归一化容器分类：`nativeContainers = {mp4, webm, mov}`（浏览器原生可播）、
  `containerNeedsRemux = {mkv, ts, m2ts, flv}`（仅容器不兼容）。
- **关键**：HEVC-in-MP4 不再进 L2（浏览器解不了 HEVC）；转码开→L3，转码关→L4。
- 决策顺序：浏览器原生→直连；无 ffmpeg→L4 外部并给原因；容器需重封装且视频可解→L2；
  原生容器+视频可解但音轨不兼容→L2 音轨转 AAC；可转码且转码开→L3；MKV+HEVC 无转码→L2（保留 HEVC）。

### ffmpeg 能力探测与优雅降级（`internal/api/handlers.go` · `New()`）
- 启动即探测：
  - `exec.LookPath("ffmpeg")` 判断是否安装；
  - `ffmpeg -encoders` 是否含 `libx264`（视频转码 L3 必需）。
- 三态能力：
  - **ffmpeg + libx264**：重封装/转码都可用，**默认开启转码**（`VIDRIVE_TRANSCODE` 未显式
    关时）——HEVC 也能页内播，真正实现「无论什么 strm 都能播」。
  - **ffmpeg 无 libx264**：重封装(L2)可用，转码(L3)不可用；HEVC 改走 L2 保留 HEVC（仅 Safari 等
    HEVC 能力浏览器可播），不再返回空 200 黑屏。
  - **无 ffmpeg**：重封装/转码都不可用，MKV/HEVC 走 L4 外部播放器 + 明确提示「请部署含 ffmpeg
    的镜像」，不再抛出一堆底层报错。
- `api/health` 新增 `ffmpeg_ok` / `transcode_ok` / `transcode` 字段，便于运维自查。

### 前端提示（零配置负担）
- `Player.tsx`：无 ffmpeg 且本片需重封装/转码时，显示琥珀色横幅「服务端未安装 ffmpeg…请部署
  含 ffmpeg 的镜像」，替换原先误导性的「Phase 3 的 :full 镜像提供」。
- `Library.tsx`：全局顶部横幅——当服务端 `ffmpeg_ok == false` 时提示换镜像。
- `Settings.tsx`：转码开关旁显示 ffmpeg 绿/红点、`transcode_ok` 状态、libx264 缺失警告；
  无 libx264 时禁用开关。

### 部署镜像（`Dockerfile` / `.github/workflows/docker.yml`）
- 运行镜像 `apk add ... ffmpeg`（Alpine 官方 ffmpeg 自带 libx264），**`:latest` 与 `:full` 为同一镜像，均已含 ffmpeg**。
- CI 同时推送 `:full` 标签（同一镜像），历史 `:full` 引用仍可解析。
- `PLAN.md` 已更正旧文档里「`:latest` 精简、`:full` 才含 ffmpeg」的错误表述。

## 三、验证

- `go test ./...` 全绿：
  - **新增 `TestPlay_StrmOpenListDLink_FallbackWhenResolveFails`**：用 `httptest` 伪造一个
    「取链必 500」的 OpenList，STRM 文本本身是 `/d/` 直链，验证 `playItem` 回退到原始 `StrmRaw`、
    产出 **L2 remux、不再 502**（精确复现并锁死本次根因）。
  - selector 新增 `TestHEVCInMP4_RegressionFix`（HEVC-in-MP4→L3 转码 / L4 外部）、
    `TestNoFFmpeg_MKVH264ToExternal` / `TestNoFFmpeg_HEVCToExternal` / `TestNoFFmpeg_NativeMP4StillDirect` /
    `TestTranscodeUnavailable_HEVCFallsBack`。
  - playItem 既有 `TestPlay_StrmHttpResolvesInPage`、`TestPlay_StrmLocalPath_*` 仍通过。
  - `TestRemux_BlocksSSRF` 仍拦截 `file:///etc/passwd`。
- 真实源验证（v1.1.14 已完成，`handlers_live_test.go` 可选 live 测试，需设 `LIVE_OPENLIST_TOKEN`）：
  `TestLive_RemuxRealCas` 对真实 4GB `.cas`→MKV 直链跑完整 `/api/play/remux`，返回
  `200 + video/mp4 + ftyp` ✅。

## 四、部署注意（务必读）

- **必须重新构建/部署后才会生效**（重建 HF Space / 重拉镜像 / `make build` 重启）。
- 当前运行镜像**已含 ffmpeg（含 libx264）**；若仍报「未安装 ffmpeg」，说明跑的是旧构建，
  **重建一次 Space** 即可解决 MKV/STRM 播不了的问题。
- 视频转码（HEVC→H.264）在检测到 libx264 时**默认开启**；若想省 CPU，可在「设置」关闭
  （关闭后 HEVC 走 L2 保留 HEVC，仅 Safari 等可播）。
- 真实 OpenList 复测命令（可选）：
  `LIVE_OPENLIST_TOKEN=<你的 token> go test ./internal/api/ -run TestLive_ -v`

## 五、版本

`internal/api/handlers.go` 版本号 `1.1.14` → `1.1.15`（已打 tag `v1.1.15` 并推送）。

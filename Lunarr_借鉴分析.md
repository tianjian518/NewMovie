# 借鉴 Lunarr：媒体流/播放链路可移植性分析

- 日期：2026-08-10
- 参考项目：[lunarr-app/lunarr-go](https://github.com/lunarr-app/lunarr-go)（自托管媒体流服务，对标 Plex）
- 镜像：Docker Hub `sayem314/lunarr`；文档 `lunarr-go/tree/main/docs`
- 实际技术栈：**TypeScript / SvelteKit 全栈**（仓库名带 `-go` 但有误导性，已无 Go 代码）
- 目的：看 Lunarr 的媒体流实现里有哪些能「抄」到 NewMovie（Go 零依赖后端 + React 前端）。
  因语言不同，**无法逐行复制**，但架构、算法、ffmpeg 配方可直接移植。

---

## 一、Lunarr 端到端播放链路（拆解）

| 环节 | Lunarr 做法 | 关键文件 |
|---|---|---|
| 客户端能力探测 | 前端 `canPlayType` 探测 hevc/av1/vp9/webm/hls，作为 `?hevc=1&vp9=1&…` 上报 | `src/lib/playback/capabilities.ts` |
| 播放决策 | `decidePlaybackMode`：直链 > HLS重封装[H.264+AAC] > 转码 > 不可用 | `src/lib/server/transcoding/capabilities.ts` |
| 容器识别 | **魔数嗅探**（文件头字节）+ ffprobe format_name + 扩展名，三级兜底 | `src/lib/server/transcoding/container-format.ts` |
| 转码/重封装 | **请求驱动 HLS**：ffmpeg 输出 `libx264+aac` 的 `.ts` 分片，GOP 对齐分片秒数 | `src/lib/server/transcoding/ffmpeg-cli.ts`（`ffmpegHlsArgs`） |
| 分片服务 | `master.m3u8` + `segments/[segment]`，签名 token + 心跳保活 | `src/routes/media/playback-sessions/...` |
| 前端播放 | `hls.js`（不支持原生 HLS 的浏览器）`loadSource`/`attachMedia` + 错误自愈 | `src/lib/player/media-player-hls.svelte.ts` |
| 存储抽象 | `LibraryStorage` 统一接口，支持 local/sftp/webdav/remote，**`createReadStream(filePath, range)` 带 Range** | `src/lib/server/storage/` |
| 转码策略 | `transcodingEnabled` + `playbackPreference`(auto/direct/transcode) + 硬件加速 + 质量预设(CRF/码率) | `src/lib/server/transcoding/policy.ts` |

核心思想（与 Plex/Jellyfin 一致）：**浏览器原生能解就直链，不能解但 H.264+AAC 就 HLS 重封装，编码真解不了就 HLS 转码成 H.264，绝不让用户卡在加载中转圈。**

---

## 二、可借鉴清单（分级）

### ✅ 已落地（本次直接移植）

| 项 | Lunarr 来源 | NewMovie 落地 | 价值 |
|---|---|---|---|
| **容器魔数嗅探** | `container-format.ts:detectContainerFromMagic` | 新增 `internal/playback/container.go:SniffContainer`，并在 `handlers.go:sniffContainer` 接入 ffprobe 之后的兜底 | 无扩展名 strm 不依赖扩展名/ffprobe 也能认出容器（mp4/mkv/webm/avi/ts），比猜扩展名更稳，直接强化第十节修复 |
| **编解码器变体归一** | `capabilities.ts:isHevcCodec/isH264Codec/isAv1Codec/isVp9Codec/isAacCodec` | 新增 `internal/playback/codec.go:NormalizeCodecName` + `IsHEVC/IsH264/IsAV1/IsVP9/IsAAC`，接入 `selector.go:Select` | 真实 ffprobe 常报 `avc1.4d0028`/`hvc1.1.6`/`av01.0.08M`/`mp4a.40.2` 等变体位，归一后判断更准，避免「能解却被误判转码/外放」 |
| **客户端能力协商** | `capabilities.ts` 前端 `canPlayType` 探测后上报 `?hevc=&av1=&vp9=` | 前端 `web/src/api.ts:clientCaps()` 探测 `video/mp4;codecs="hvc1/hev1"`、`av01`、`vp9`，缓存一次后随 `/api/items/:id/play` 上报；后端 `handlers.go:capFlag` 解析并填 `playback.Input{ClientHEVC/ClientAV1/ClientVP9}`；`selector.go:clientDecodable` 据此判断「浏览器原生可解」，**取代原来硬编码的 `nativeVideo` 假设** | Safari（原生 HEVC）不再白白转码直下 HEVC-MP4；AV1 硬件解码的 Chrome 同理直链。省 CPU、起播更快。最大收益点已落地 |
| **HLS 流式（请求驱动分片）** | `ffmpegHlsArgs`：`libx264 -crf N -pix_fmt yuv420p -c:a aac`，`-f hls -hls_time -hls_playlist_type event -hls_flags independent_segments+temp_file`，GOP 对齐 | 新增 `internal/hls` 包（`Manager`/`Session`/`BuildArgs`/`RewritePlaylist`）+ `handlers.go:handleHLS` + `playItem` L2/L3 分支 + 前端 `Player.tsx:isHlsUrl` 走 hls.js。单 ffmpeg 全量切片写到磁盘，会话按 `sha256(src\|mode\|atrack)` 去重，TTL 清理 + 并发上限淘汰 | 根治 HEVC（转成 H.264 HLS 人人可播）+ 拖拽/Range 更稳 + 经 hls.js 全浏览器可播。`-c copy` 重封装零重编码、秒播；音轨不兼容（DTS/TrueHD/Atmos）自动转 AAC；多音轨选语言（`atrack`） |

> 测试：`internal/playback/container_test.go`、`internal/playback/codec_test.go` 全绿；`internal/hls/hls_test.go`（含真实 ffmpeg 集成 `TestManagerGenerate`）、`internal/api/handlers_hls_test.go`（全 HTTP 链路）全绿；`go build ./...` / `go test ./...` 通过；`web/src` 经 `tsc --noEmit` 校验通过。

### 🟡 已对齐 / 待增强（原则一致，可补细节）

| 项 | 说明 |
|---|---|
| **选择器原则：HEVC 不重封装** | NewMovie `selector.go` 已与 Lunarr 一致——HEVC 不进 `nativeVideo`，无转码时落 L4 而非重封装成浏览器解不了的 HEVC-MP4。第十节修复已验证。**这一点我们本来就对，无需改。** |
| **转码策略模型** | Lunarr 有 `playbackPreference`(auto/prefer_direct/prefer_transcode) + 质量预设(720p/1080p/original, CRF/码率) + 硬件加速(vaapi/nvenc/qsv/videotoolbox/amf)。NewMovie 目前只有「允许转码」开关。可补「偏好直链/转码」与质量上限，工作量小、纯配置。 |

### 🔴 建议大改（架构级，需拍板）

| 项 | Lunarr 做法 | 对 NewMovie 的意义 | 工作量 |
|---|---|---|---|
| **播放会话 / 进度** | `sessions-store.ts` + 心跳，断点续播 | NewMovie 已有观看进度，会话模型可选 | 中 |
| **存储抽象（Range 流）** | `LibraryStorage.createReadStream(filePath, range)` | NewMovie 的 139cas + `openPlaySource` 已实现 Range 取流，**此点无需抄** | — |

---

## 三、HLS 转码的 ffmpeg 配方（可直接抄的部分）

来自 `ffmpeg-cli.ts:ffmpegHlsArgs`，这是移植到 NewMovie Go 后端 `handleTranscode` 的现成命令：

```
# 转码（HEVC→H.264，人人可播）
ffmpeg -hide_banner -y [-ss 起点]
  [-f 输入格式] -i 输入
  -map 0:v:0 -map 0:a:0? -sn -dn
  [-vf scale=-2:trunc(min(ih\,H)/2)*2]   # 可选降分辨率
  -c:v libx264 -preset veryfast -crf 23 -pix_fmt yuv420p
  -g GOP -keyint_min GOP -sc_threshold 0 -force_key_frames expr:gte(t,n_forced*分段秒)
  -c:a aac -ac 2
  -max_muxing_queue_size 2048 -avoid_negative_ts make_zero
  -f hls -hls_time 分段秒 -hls_list_size 0 -hls_playlist_type event
  -hls_flags independent_segments+temp_file
  -hls_segment_filename segment-%05d.ts master.m3u8

# 重封装（H.264+AAC 仅换容器，零重编码）
# 仅把 -c:v libx264 … 换成 -c:v copy -c:a copy，-hls_flags temp_file
```

关键：`-force_key_frames` 让每个分片以关键帧开头，拖拽才能精确；`-hls_playlist_type event` 边生成边服务；`independent_segments` 分片可独立解码。

---

## 四、结论与建议下一步

1. **已交付（安全、已测、已接）**：容器魔数嗅探 + 编解码器变体归一（强化无扩展名 Strm 的容器识别，不引入架构风险）。
2. **已交付（本次新增）**：**客户端能力协商**（前端 `canPlayType` 探测 HEVC/AV1/VP9 → 随播放请求上报 → 后端 `clientDecodable` 决策）。纯收益、零回归，Safari/AV1 设备免转码直链，省 CPU。
3. **已交付（本次新增，架构级）**：**HLS 流式**（请求驱动、单 ffmpeg 全量切片、会话去重 + 清理 + 并发淘汰、remux `-c copy` 秒播 / transcode `libx264` 人人可播 / 音轨不兼容转 AAC / 多音轨选语言）。根治 HEVC 播放（转成 H.264 HLS）+ 拖拽/Range 更稳 + hls.js 全浏览器可播。ffmpeg 配方直接来自 `ffmpegHlsArgs`，已完整移植并经真实 ffmpeg 集成测试验证。
4. **剩余候选**：**转码策略模型**（偏好直链/转码 + 质量上限 + 硬件加速）、**播放会话/心跳**（NewMovie 已有进度，可选）。

> 注：Lunarr 的 `storage`（Range 取流）、`sessions`（进度）NewMovie 已有等价实现，**不在借鉴范围**。三大可移植点中，仅「转码策略模型」尚未落地，其余均已交付。

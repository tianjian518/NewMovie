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

> 测试：`internal/playback/container_test.go`、`internal/playback/codec_test.go` 全绿；`go build ./...` / `go test ./internal/playback/ ./internal/api/` 通过。

### 🟡 已对齐 / 待增强（原则一致，可补细节）

| 项 | 说明 |
|---|---|
| **选择器原则：HEVC 不重封装** | NewMovie `selector.go` 已与 Lunarr 一致——HEVC 不进 `nativeVideo`，无转码时落 L4 而非重封装成浏览器解不了的 HEVC-MP4。第十节修复已验证。**这一点我们本来就对，无需改。** |
| **转码策略模型** | Lunarr 有 `playbackPreference`(auto/prefer_direct/prefer_transcode) + 质量预设(720p/1080p/original, CRF/码率) + 硬件加速(vaapi/nvenc/qsv/videotoolbox/amf)。NewMovie 目前只有「允许转码」开关。可补「偏好直链/转码」与质量上限，工作量小、纯配置。 |
| **客户端能力协商** | Lunarr 前端探测 `canPlayType` 后上报，后端据此决定直链/转码。**最大收益点**：NewMovie 现在假设「通用浏览器」（HEVC 不算原生），于是在 **Safari（原生支持 HEVC）上也会白白转码**；AV1 硬件解码的 Chrome 同理。补上后：Safari 的 HEVC-in-MP4 可直链、AV1 可直链，省 CPU。需前端 + API 契约改动，中等工作量。 |

### 🔴 建议大改（架构级，需拍板）

| 项 | Lunarr 做法 | 对 NewMovie 的意义 | 工作量 |
|---|---|---|---|
| **HLS 流式（请求驱动分片）** | ffmpeg 输出 `libx264 -crf N -pix_fmt yuv420p -c:a aac -ac 2`，`-f hls -hls_time -hls_playlist_type event -hls_flags independent_segments+temp_file`，GOP 对齐分片 | 根治 HEVC（转成 H.264 HLS）+ 拖拽/Range 更稳 + 经 hls.js 全浏览器可播。当前 NewMovie 是「整文件/分片 MP4 重封装」，能用但 HLS 是更标准的 Plex/Jellyfin 解法 | 大：需 `master.m3u8`/`segments` 路由、分片生成（ffmpeg 配方已可从 `ffmpegHlsArgs` 直接抄）、前端 hls.js、播放会话/心跳 |
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

1. **本次已交付**（安全、已测、已接）：容器魔数嗅探 + 编解码器变体归一。直接强化无扩展名 Strm 的容器识别，不引入架构风险。
2. **强烈建议下一步**：**客户端能力协商**（前端 `canPlayType` 上报 → 后端决策）。纯收益、工作量中等，能让 Safari/AV1 设备免转码直链。
3. **架构级候选**：**HLS 流式**。收益最大（根治 HEVC + 拖拽），但改动面大，建议单独立项、先在小范围验证 ffmpeg 配方与 hls.js 前端，再全量替换现有 MP4 重封装路径。

> 注：Lunarr 的 `storage`（Range 取流）、`sessions`（进度）NewMovie 已有等价实现，**不在借鉴范围**。

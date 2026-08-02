# NewMovie v1.1.12 — 4K HEVC+DTS/TrueHD 页内播放（音轨转 AAC，不再甩外部）

- 日期：2026-08-02
- 基线：v1.1.11（`6f6177e`）
- 主题：用户反馈「解析播放源后仍弹『已为你选择外部播放器』」。根因是 4K 蓝光原盘的
  音轨是 DTS-HD / TrueHD / Atmos，此前被判定为无法重封装 → 直接 L4 外部播放器。

## 一、问题

v1.1.9 让 HEVC 走 L2 重封装页内播放，但 `remuxAudio` 只含 `aac/mp3/opus/ac3/eac3`，
**不含 DTS / TrueHD / Atmos**。而 4K 电影几乎都是「HEVC 视频 + DTS-HD/TrueHD 音轨」。
选择器里：

```go
remuxable := remuxVideo[vc] && remuxAudio[ac] && remuxContainers[c]
```

DTS 音轨使 `remuxable = false`，加上 `handlers.go` 里 `TranscodeEnabled` 硬编码为 `false`，
于是 HEVC+DTS 直接落到 **L4 外部播放器**——于是用户看到那句提示，点击就唤起外部播放器
（没外部播放器则变成浏览器下载）。v1.1.9 实际只修了 HEVC+AAC/AC3，没覆盖 DTS 音轨。

## 二、改动

`internal/playback/selector.go`：

- `Decision` 新增 `NeedsAudioTranscode` 字段。
- 重封装判定改为两段：
  - 视频可重封装（`remuxVideo`）且容器可重封装（`remuxContainers`）时即走 **L2**；
  - 音轨是 `aac/ac3/eac3/mp3/opus` → 纯 `-c copy` 重封装；
  - 音轨是 `dts/truehd/atmos/flac` 等装不进 MP4 的 → **视频保持拷贝、仅把音轨实时转成 AAC**
    （`NeedsAudioTranscode = true`），同样页内可播。
- 结果：HEVC + DTS/TrueHD/Atmos 现在走 L2 轻量音轨转码，原画质零损失、服务端几乎零开销。

`internal/api/handlers.go`：

- `handleRemux`：新增 `aac=1` 参数 → ffmpeg 用 `-c:v copy -c:a aac -b:a 320k`
  （视频不重编码，只转音轨）；否则维持原 `-c copy`。
- 构造播放决策时，若 `dec.NeedsAudioTranscode`，给 remux URL 追加 `&aac=1`。

`internal/playback/selector_test.go`：

- 原 `TestL4ExternalWhenTranscodeOff`（HEVC+DTS→L4）、`TestL3TranscodeWhenEnabled`（HEVC+DTS→L3）
  改为用「不支持重封装的视频编码（wmv）+ 未开转码」触发，分别对应 L4 / L3；
- 新增 `TestL2RemuxHEVC_MKV_DTS`、`TestL2RemuxHEVC_MKV_TrueHD`：确认 HEVC+DTS/TrueHD → L2 且 `NeedsAudioTranscode`。

## 三、验证

- `go test ./...` 全绿（含 playback / api）。
- 功能验证：用 `libopenh264 + dca` 造「视频 + DTS 音轨」的 MKV，跑 `aac=1` 那条 ffmpeg 管线，
  产物为 `视频流原样保留 + 音轨变为 AAC` 的合法 MP4（容器 `mov,mp4,...`），即页内可播格式。

## 四、部署注意（务必读）

- **本次是后端改动，必须重新构建/部署后才会生效**：拉取新镜像或 `go build` 重启服务。
  若仍跑着旧容器，会继续使用旧的 L4 逻辑、照弹外部播放器提示。
- 前提：运行环境已装 `ffmpeg`（`handleRemux` 会在缺失时明确报错）。
- 边界：本修复让「视频编码浏览器能解」的内容页内播。若浏览器**本身不解码 HEVC**
  （如 Firefox / 部分 Linux Chrome），MP4-HEVC 仍放不出——此时需开启转码（L3）把视频也转成 H.264。
  默认 `TranscodeEnabled=false`，如确需可在设置/配置里打开。

## 五、版本

`internal/api/handlers.go` 版本号 `1.1.11` → `1.1.12`。

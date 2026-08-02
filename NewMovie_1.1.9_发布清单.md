# NewMovie v1.1.9 — HEVC/4K 页内播放（不再强甩外部播放器）

- 日期：2026-08-02
- 基线：v1.1.8（`62ae1e9`）
- 主题：用户无外部播放器，希望 4K/HEVC 片源在页内直接播放，而非落到 L4 外部播放器提示。

## 一、问题

播放 HEVC/H.265（4K/HDR 几乎都是）片源时，播放页固定在 `Player.tsx` 显示
「已为你选择外部播放器（原画画质/音质）。」并交外部播放器。原因：

- 五级降级链（`internal/playback/selector.go`）**故意把 HEVC 排除在「重封装」之外**，
  且常见 4K 音轨（AC3/EAC3）也不在可重封装音频列表；
- `handlers.go` 中 `TranscodeEnabled` 硬编码为 `false`，故 HEVC 永远到不了 L3 转码，
  直接落 L4 外部播放器。

而 HEVC 其实可以**不改编码、只换容器**（`ffmpeg -c copy` 把 MKV/MP4 重封装为 MP4）在页内播放，
原画质零损失、服务端近乎零开销——这正是 L2 重封装干的事，只是当初保守地没纳入 HEVC。

## 二、改动

`internal/playback/selector.go`：

- `remuxVideo` 增加 `h265` / `hevc`；
- `remuxAudio` 增加 `ac3` / `eac3`（4K 常见音轨）；
- `remuxContainers` 增加 `mp4` / `mov`（使已是 MP4/MOV 但内含 HEVC 的片源也走一次同封装重混流，页内尝试播放）。

效果：HEVC +（AAC/AC3/EAC3）+（MKV/MP4/MOV/TS…）的片源，现在走 **L2 重封装页内播放**，
不再显示外部播放器提示。仅当音轨确实无法重封装（DTS / TrueHD / Atmos 装不进 MP4）
或客户端确无 HEVC 解码时，才退回 L4 外部播放器。

`internal/playback/selector_test.go`：

- 原 `TestL4ExternalWhenTranscodeOff`（HEVC+AAC）现应走 L2，改为用 **HEVC+DTS** 验证 L4 回退；
- 新增 `TestL2RemuxHEVC_MKV_AAC` / `TestL2RemuxHEVC_MKV_AC3` / `TestL2RemuxHEVC_MP4` 锁定页内重封装；
- `TestL3TranscodeWhenEnabled` 改为 **HEVC+DTS + 开转码** 触发 L3（重封装优先于转码）。

`internal/api/handlers.go`：版本号 `1.1.8` → `1.1.9`。

## 三、验证

- `go test ./...` 全绿（含 playback 全部用例）。
- 前端无需改动：`Player.tsx` 已对 L0/L2 用 ArtPlayer 页内播放 `dec.url`（`/api/play/remux?u=…`）。

## 四、已知边界（非本次回归）

- 客户端浏览器确无 HEVC 硬解（如 Firefox、缺解码器的 Chrome/Linux）时，重封装后的 MP4-HEVC
  仍可能播不出；此类情况属 L4 外部回退或需开启转码，后续可加「优先页内 / 优先外部」用户开关。
- 若 `GetLink` 失败且存储未配 `SignKey`，L4 卡片会只剩提示、无「唤起外部播放器」按钮（死胡同），
  本次未改，建议另立项加空 URL 诊断。

// Package playback 实现 PLAN.md 第六节的「五级播放降级链」。
// 无论资源来自原生挂载还是 strm 解析，都走同一条链。
//
// 纯标准库实现，可独立单测（见 selector_test.go）。
package playback

import (
	"net/url"
	"strings"
)

// Level 播放级别。
type Level int

const (
	L0Direct    Level = iota // 302 直链（零服务端开销）
	L1Proxy                  // 代理转发（补防盗链 header）
	L2Remux                  // 重封装 -c copy（修正容器不兼容）
	L3Transcode              // 真转码（资源黑洞，默认关）
	L4External               // 唤起外部播放器
)

// Input 决策输入。
type Input struct {
	Container         string // mp4/mkv/webm
	VideoCodec        string // h264/h265/av1/hevc...
	AudioCodec        string // aac/mp3/dts/truehd/opus...
	RawURL            string // 网盘真实直链（可为空）
	DirectURL         string // /d/ 302 链接（可为空）
	HotlinkProtection bool   // 是否有防盗链（需补 UA/Referer）
	SupportsRange     bool   // 源是否支持 Range（影响拖动）
	TranscodeEnabled  bool   // 是否允许 L3 转码
	PreferExternal    bool   // 用户偏好外部播放器
}

// Decision 决策结果（前端据此显示小标签）。
type Decision struct {
	Level          Level
	Label          string // 直链/代理/重封装/转码/外部
	Reason         string
	URL            string // L0/L1 直连或代理地址；L4 为空（交给外部播放器）
	UseRawURL      bool   // 是否优先用 raw_url（绕过 OpenList 中转）
	SupportsRange  bool
	NeedsTranscode bool
	// NeedsAudioTranscode：L2 重封装时，视频保持拷贝、仅把不兼容 MP4 的音轨
	//（DTS/TrueHD/Atmos/FLAC）实时转成 AAC。原画质零损失、几乎零开销。
	NeedsAudioTranscode bool
}

// 浏览器原生支持的白名单（保守，见 PLAN.md 第六节）。
var (
	nativeContainers = map[string]bool{"mp4": true, "webm": true, "mov": true}
	nativeVideo      = map[string]bool{"h264": true, "avc": true, "vp9": true, "av1": true}
	nativeAudio      = map[string]bool{"aac": true, "mp3": true, "opus": true}
)

// 可无损重封装（ffmpeg -c copy 换个壳）页内播放的编码/容器。
// 仅换容器、不重编码，原画质/音质零损失、服务端近乎零开销（见 L2Remux 与 /api/play/remux）。
// HEVC(h265) 多数现代浏览器（Safari 原生、Chrome/Edge 在有系统解码器时）能在 MP4 里解，
// 故纳入重封装，让用户能在页内直接看 4K/HDR，而不是被甩去外部播放器。
// 音轨分两类处理：
//   - aac/ac3/eac3/mp3/opus 可直接 -c copy 进 MP4（纯重封装）；
//   - dts/truehd/atmos/flac 等装不进 MP4，则走「视频拷贝 + 音轨转 AAC」的轻量重封装
//     （Decision.NeedsAudioTranscode=true），同样页内可播，不再甩外部播放器。
// 仅当视频编码本身浏览器无法解码、且未开转码时，才退回 L4 外部播放器。
var (
	remuxVideo = map[string]bool{
		"h264": true, "avc": true,
		"vp9": true, "av1": true,
		"h265": true, "hevc": true,
	}
	remuxAudio = map[string]bool{"aac": true, "mp3": true, "opus": true, "ac3": true, "eac3": true}
	// 这些容器「编码本身浏览器能解」但原容器浏览器不认，可被 -c copy 塞进 MP4。
	// mp4/mov 也纳入：当里面是 HEVC 时（浏览器原生白名单不含 h265），
	// 走一次同封装重混流即可在页内尝试播放，避免无谓地甩外部。
	remuxContainers = map[string]bool{"mkv": true, "ts": true, "m2ts": true, "flv": true, "mp4": true, "mov": true}
)

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Select 选择播放策略。
func Select(in Input) Decision {
	c := norm(in.Container)
	vc := norm(in.VideoCodec)
	ac := norm(in.AudioCodec)
	if vc == "hevc" {
		vc = "h265"
	}

	// L4 优先：用户明确偏好外部播放器
	if in.PreferExternal {
		return Decision{Level: L4External, Label: "外部", Reason: "用户偏好外部播放器", SupportsRange: in.SupportsRange}
	}

	browserNative := nativeContainers[c] && nativeVideo[vc] && nativeAudio[ac]

	if browserNative {
		if in.HotlinkProtection {
			url := pickURL(in)
			return Decision{Level: L1Proxy, Label: "代理", Reason: "有防盗链，服务端补 header 转发",
				URL: proxyPath(url), UseRawURL: in.RawURL != "", SupportsRange: in.SupportsRange}
		}
		// 直链：raw_url 优先（绕开 OpenList 中转）
		useRaw := in.RawURL != ""
		url := in.RawURL
		if url == "" {
			url = in.DirectURL
		}
		return Decision{Level: L0Direct, Label: "直链", Reason: "浏览器原生支持，直链零开销",
			URL: url, UseRawURL: useRaw, SupportsRange: in.SupportsRange}
	}

	// 非原生：先看是否只需修正容器（编码本身浏览器支持）→ L2 Remux
	videoRemuxable := remuxVideo[vc]
	containerRemuxable := remuxContainers[c]
	if videoRemuxable && containerRemuxable {
		// 音轨能否原样拷进 MP4：aac/ac3/eac3/mp3/opus 可直接 -c copy；
		// dts/truehd/atmos/flac 等装不进 MP4，必须转码音轨（见下方 NeedsAudioTranscode）。
		if remuxAudio[ac] {
			return Decision{Level: L2Remux, Label: "重封装", Reason: "容器不兼容但编码可用，服务端 -c copy 重封装为 MP4",
				URL: "", UseRawURL: in.RawURL != "", SupportsRange: in.SupportsRange}
		}
		// 视频保留（HEVC/H264 不变），仅把不兼容 MP4 的音轨实时转成 AAC：
		// 原画质零损失、仅极轻量音频转码，页内即可播放，不必甩外部播放器。
		// 这正是 4K 蓝光原盘（HEVC + DTS-HD/TrueHD/Atmos）能在页内播的关键。
		return Decision{Level: L2Remux, Label: "重封装", Reason: "视频保留、音轨转 AAC 重封装为 MP4（DTS/TrueHD 等不兼容 MP4）",
			URL: "", UseRawURL: in.RawURL != "", SupportsRange: in.SupportsRange, NeedsAudioTranscode: true}
	}

	// 编码不支持：转码 或 外部播放器
	if in.TranscodeEnabled {
		return Decision{Level: L3Transcode, Label: "转码", Reason: "编码浏览器不支持，服务端转码",
			URL: pickURL(in), NeedsTranscode: true, SupportsRange: in.SupportsRange}
	}
	// 默认不开转码 → 外部播放器（见 PLAN 4.5：绝不给用户一句"不支持"）
	return Decision{Level: L4External, Label: "外部",
		Reason: "编码不支持且未开启转码，唤起外部播放器", SupportsRange: in.SupportsRange}
}

func pickURL(in Input) string {
	if in.RawURL != "" {
		return in.RawURL
	}
	return in.DirectURL
}

// PickURL 导出给上层（handlers）取源地址，用于构造重封装链接。
func PickURL(in Input) string { return pickURL(in) }

// RemuxURL 把源直链转为 NewMovie 的实时重封装端点（ffmpeg -c copy → 分片 MP4）。
// 浏览器拿不到 MKV 直链，必须经过这道转封装才能页内播放。
func RemuxURL(raw string) string {
	if raw == "" {
		return ""
	}
	return "/api/play/remux?u=" + url.QueryEscape(raw)
}

// proxyPath 把直链转为 Vidrive 代理路径（前端统一请求本服务，由服务补 header）。
func proxyPath(raw string) string {
	if raw == "" {
		return ""
	}
	return "/api/play/proxy?u=" + raw
}

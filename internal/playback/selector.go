// Package playback 实现 PLAN.md 第六节的「五级播放降级链」。
// 无论资源来自原生挂载还是 strm 解析，都走同一条链。
//
// 纯标准库实现，可独立单测（见 selector_test.go）。
package playback

import "strings"

// Level 播放级别。
type Level int

const (
	L0Direct  Level = iota // 302 直链（零服务端开销）
	L1Proxy                // 代理转发（补防盗链 header）
	L2Remux                // 重封装 -c copy（修正容器不兼容）
	L3Transcode            // 真转码（资源黑洞，默认关）
	L4External             // 唤起外部播放器
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
}

// 浏览器原生支持的白名单（保守，见 PLAN.md 第六节）。
var (
	nativeContainers = map[string]bool{"mp4": true, "webm": true, "mov": true}
	nativeVideo      = map[string]bool{"h264": true, "avc": true, "vp9": true, "av1": true}
	nativeAudio      = map[string]bool{"aac": true, "mp3": true, "opus": true}
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
	remuxable := nativeVideo[vc] && nativeAudio[ac] && c == "mkv"
	if remuxable {
		return Decision{Level: L2Remux, Label: "重封装", Reason: "MKV 容器不兼容，编码可用，-c copy 重封装",
			URL: pickURL(in), UseRawURL: in.RawURL != "", SupportsRange: in.SupportsRange}
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

// proxyPath 把直链转为 Vidrive 代理路径（前端统一请求本服务，由服务补 header）。
func proxyPath(raw string) string {
	if raw == "" {
		return ""
	}
	return "/api/play/proxy?u=" + raw
}

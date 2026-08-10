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
	L2Remux                  // 重封装 -c copy（修正容器不兼容 / 音轨）
	L3Transcode              // 真转码（HEVC→H.264，人人可播）
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
	// TranscodeAvailable 服务端是否真的能转码：ffmpeg 在且带 libx264 编码器。
	// 仅 TranscodeEnabled 不够——若 ffmpeg 构建缺 libx264，转码会静默失败（空 200）。
	// 缺时不应走 L3，改 L2 重封装保留 HEVC（HEVC 能力浏览器可播）或 L4 外部。
	TranscodeAvailable bool
	// FFmpegAvailable 服务端是否安装了 ffmpeg。重封装(L2)/转码(L3)都依赖它；
	// 缺失时这两项无法实现，调用方应据此把本要走 L2/L3 的文件降级为 L4 外部播放器，
	// 而不是返回一个 500 报错让用户对着「视频不存在」发懵。
	FFmpegAvailable bool
	PreferExternal   bool // 用户偏好外部播放器
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
	// nativeContainers：浏览器「认识并能播放」的容器。mp4/webm/mov 直接 <video> 能播，
	// 不需要服务端重封装——重封装一个 mp4 成另一个 mp4 毫无意义（反而还要 ffmpeg、还更慢）。
	nativeContainers = map[string]bool{"mp4": true, "webm": true, "mov": true}
	// containerNeedsRemux：浏览器「根本不认」的容器，必须重封装成 MP4 才能页内播。
	// 这是 MKV / TS 类文件页内播放的唯一出路（浏览器读不了 MKV 容器）。
	containerNeedsRemux = map[string]bool{"mkv": true, "ts": true, "m2ts": true, "flv": true}
	nativeVideo          = map[string]bool{"h264": true, "avc": true, "vp9": true, "av1": true}
	nativeAudio          = map[string]bool{"aac": true, "mp3": true, "opus": true}
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
	// 可视频转码（重编码为 H.264）的编码与容器。转码是 CPU 黑洞，仅当浏览器真解不了
	// （如 HEVC）且用户开「允许视频转码」时才走。列表尽量宽：老编码/冷门容器都兜住。
	// 注意：HEVC(h265) 故意不进 nativeVideo（多数浏览器不解码），但进这里——开启转码后
	// 就能把 4K 蓝光原盘转成 H.264 页内播，不必依赖浏览器是否带 HEVC 解码器。
	transcodableVideo = map[string]bool{
		"h264": true, "avc": true, "hevc": true, "h265": true, "vp9": true, "av1": true,
		"mpeg4": true, "msmpeg4v2": true, "msmpeg4": true, "mpeg2video": true,
		"vc1": true, "wmv1": true, "wmv2": true, "wmv3": true, "wmv": true, "mjpeg": true,
		"mpeg1video": true, "theora": true, "vp8": true, "rv40": true, "rv20": true,
	}
	transcodableContainers = map[string]bool{
		"mkv": true, "mp4": true, "mov": true, "ts": true, "m2ts": true, "flv": true,
		"avi": true, "webm": true, "wmv": true, "rmvb": true, "ogv": true, "m4v": true,
		"3gp": true, "asf": true,
	}
)

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Select 选择播放策略。
//
// 核心原则（也回应了 v1.1.13 把 mp4 误送重封装导致 HEVC-in-MP4 报错的回归）：
//   - 重封装(L2) 只用于「修容器不兼容」或「修音轨不兼容」，绝不用于「让本就解不了的视频变可解」。
//     原生容器(mp4/mov/webm)里的 HEVC，重封装成 mp4 浏览器照样解不了，必须转码(L3)或外部(L4)。
//   - 没有 ffmpeg 时，L2/L3 都不可行，凡是需要它们的文件一律降级为 L4 外部播放器并给出明确原因，
//     而不是返回一个 500 让用户对着「视频不存在」发懵。
func Select(in Input) Decision {
	c := norm(in.Container)
	// 归一编解码器名：处理 avc1./hvc1./av01./mp4a. 等真实变体位（移植自 Lunarr 能力判定），
	// 避免「浏览器能解却被误判需转码/外放」。HEVC 一并归一到 h265 供后续分支使用。
	vc := NormalizeCodecName(in.VideoCodec)
	ac := NormalizeCodecName(in.AudioCodec)

	// L4 优先：用户明确偏好外部播放器
	if in.PreferExternal {
		return Decision{Level: L4External, Label: "外部", Reason: "用户偏好外部播放器", SupportsRange: in.SupportsRange}
	}

	// 容器未知：多为无扩展名的 strm 直链（签名链 / OpenList /d/ 链接）。不像其它实现那样
	// 直接甩外部，而是把直链交给浏览器试播——多数 strm 直链本就是可直接播放的流
	//（如直链 mp4），播不了再由前端报错 / 外部打开兜底。仅当确实没有任何可用直链时才外放。
	// （有 ffprobe 时容器会被探测出来走下面的正常分支，这里只是「探测不可用」的兜底。）
	if c == "" {
		if u := pickURL(in); u != "" {
			return Decision{Level: L0Direct, Label: "直链",
				Reason: "容器未知（多为无扩展名 strm 直链），交由浏览器直接试播",
				URL: u, UseRawURL: in.RawURL != "", SupportsRange: in.SupportsRange}
		}
		return Decision{Level: L4External, Label: "外部",
			Reason: "容器未知且无可用直链，唤起外部播放器", SupportsRange: in.SupportsRange}
	}

	browserNative := nativeContainers[c] && nativeVideo[vc] && nativeAudio[ac]

	// 浏览器原生支持：零开销直链（有防盗链则代理补 header）。
	if browserNative {
		if in.HotlinkProtection {
			url := pickURL(in)
			return Decision{Level: L1Proxy, Label: "代理", Reason: "有防盗链，服务端补 header 转发",
				URL: proxyPath(url), UseRawURL: in.RawURL != "", SupportsRange: in.SupportsRange}
		}
		useRaw := in.RawURL != ""
		url := in.RawURL
		if url == "" {
			url = in.DirectURL
		}
		return Decision{Level: L0Direct, Label: "直链", Reason: "浏览器原生支持，直链零开销",
			URL: url, UseRawURL: useRaw, SupportsRange: in.SupportsRange}
	}

	// 没有 ffmpeg：重封装/转码都做不了。到这里的都不是 browserNative，只能外部播放器。
	// 给出人话原因，前端据此提示用户「用含 ffmpeg 的镜像」。
	if !in.FFmpegAvailable {
		return Decision{Level: L4External, Label: "外部",
			Reason: "服务端未安装 ffmpeg，MKV/HEVC 等需重封装/转码才能页内播，已唤起外部播放器（请部署含 ffmpeg 的镜像）",
			SupportsRange: in.SupportsRange}
	}

	nativeC := nativeContainers[c]       // mp4/mov/webm：浏览器认容器
	needFix := containerNeedsRemux[c]    // mkv/ts/m2ts/flv：浏览器不认，必须重封装
	decodable := nativeVideo[vc]         // h264/vp9/av1：浏览器能解视频
	audioOK := nativeAudio[ac]           // aac/mp3/opus：浏览器原生音轨

	// 1) 容器浏览器不认、但视频它能解（h264/vp9/av1 在 MKV/TS 里）→ L2 重封装：
	//    换容器即可页内播；音轨按需转 AAC（DTS/TrueHD 等装不进 MP4 的情况）。
	//    这是 MKV+h264 秒播的主路径。
	if needFix && decodable {
		if remuxAudio[ac] {
			return Decision{Level: L2Remux, Label: "重封装", Reason: "容器不兼容但编码浏览器可解，服务端 -c copy 重封装为 MP4",
				URL: "", UseRawURL: in.RawURL != "", SupportsRange: in.SupportsRange}
		}
		return Decision{Level: L2Remux, Label: "重封装", Reason: "视频保留、音轨转 AAC 重封装为 MP4（DTS/TrueHD 等不兼容 MP4）",
			URL: "", UseRawURL: in.RawURL != "", SupportsRange: in.SupportsRange, NeedsAudioTranscode: true}
	}

	// 2) 原生容器、视频浏览器能解、但音轨不兼容（h264+DTS 装进 MP4 的怪胎）→ L2 重封装，
	//    仅把音轨转 AAC，视频原样拷贝。注意：这里仅针对「容器本就认」的情形，
	//    绝不会把 HEVC-in-MP4 误送进来（那种视频浏览器解不了，留给第 3/4 步）。
	if nativeC && decodable && !audioOK {
		return Decision{Level: L2Remux, Label: "重封装", Reason: "视频保留、音轨转 AAC 重封装为 MP4（音轨不兼容原生容器）",
			URL: "", UseRawURL: in.RawURL != "", SupportsRange: in.SupportsRange, NeedsAudioTranscode: true}
	}

	// 3) 视频编码浏览器解不了（HEVC 等）且开启了转码、且服务端真能转码（带 libx264）
	//    → L3 视频转码（HEVC→H.264 + 音轨 AAC）。产物是任何浏览器都能播的 MP4，
	//    彻底解决页内播放——这是「无论什么 strm 都能播」的通用路径。
	//    无论容器是 MKV 还是 MP4，只要视频解不了，就走这里（而非把 MP4+HEVC 误送重封装）。
	//    注意：必须 TranscodeAvailable（libx264 在）。若 ffmpeg 缺 libx264，转码会静默失败，
	//    此时跳过本步，落到第 4 步（MKV 保留 HEVC 重封装）或第 5 步（外部）。
	if in.TranscodeEnabled && in.TranscodeAvailable && transcodableVideo[vc] && transcodableContainers[c] {
		return Decision{Level: L3Transcode, Label: "转码", Reason: "编码浏览器不支持（如 HEVC），服务端转码为 H.264 人人可播",
			URL: "", UseRawURL: in.RawURL != "", NeedsTranscode: true, SupportsRange: in.SupportsRange}
	}

	// 4) 容器浏览器不认、视频编码浏览器也解不了（HEVC/h265 等）、且未开启/不能转码：
	//    此时「重封装」毫无意义——浏览器依然解不了视频，页内只会一直转圈（Chromium 对
	//    不认识的编码既不报错也不播放，卡在 loading）。正确做法是交给外部播放器
	//    （VLC/IINA 等自带 HEVC 解码），或提示用户开启转码（HEVC→H.264 人人可播）。
	//    这正是对标 Jellyfin/Emby 的处理：HEVC 默认转码，否则落到外部播放器，
	//    绝不把用户晾在加载中转圈。
	//    （原生容器里的 HEVC 不会进这里——needFix 为 false——已在第 3 步转码或落到底部外部。）
	if needFix && remuxVideo[vc] {
		return Decision{Level: L4External, Label: "外部",
			Reason: "视频编码（如 HEVC/H.265）浏览器无法直接解码，且未开启或不支持转码，已唤起外部播放器（可在「设置」开启「允许视频转码」页内播）",
			SupportsRange: in.SupportsRange}
	}

	// 5) 兜底：外部播放器（绝不给用户一句"不支持"）。
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

// TranscodeURL 把源直链转为 NewMovie 的实时视频转码端点（ffmpeg HEVC→H.264 + 音轨 AAC）。
// 用于浏览器本身解不了视频编码（如 HEVC）且开启了「允许视频转码」的场景，
// 产物是任何浏览器都能播的 MP4，彻底解决页内播放。
func TranscodeURL(raw string) string {
	if raw == "" {
		return ""
	}
	return "/api/play/transcode?u=" + url.QueryEscape(raw)
}

// proxyPath 把直链转为 Vidrive 代理路径（前端统一请求本服务，由服务补 header）。
func proxyPath(raw string) string {
	if raw == "" {
		return ""
	}
	return "/api/play/proxy?u=" + url.QueryEscape(raw)
}

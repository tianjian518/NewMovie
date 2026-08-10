// codec.go 提供编解码器名称的变体归一与判定，移植自 Lunarr 的客户端能力判定
// （src/lib/playback/capabilities.ts 的 isHevcCodec/isH264Codec/isAv1Codec/isVp9Codec/isAacCodec）。
//
// NewMovie 原有的 selector 用简单白名单 map 匹配 "h264"/"hevc" 等规范名，
// 但真实世界里 ffprobe 常报带点号的变体位：Safari 报 "hvc1.1.6.L93.B0"、
// MP4 里常见 "avc1.4d0028"、AV1 为 "av01.0.08M.08"、AAC 为 "mp4a.40.2"。
// 这些变体位若只做字符串相等匹配就会漏判，导致「明明浏览器能解却被判需要转码/外放」。
// 这里统一归一为规范名（hvc1.x→h265、avc1.x→h264、av01.x→av1、vp09.x→vp9、mp4a.x→aac），
// 让 Select 的判断更精确，也为后续「客户端能力协商」（Safari 原生 HEVC 直链）打地基。
package playback

// IsHEVC 是否 HEVC/H.265 族（含 hvc1./hev1. 变体位）。
func IsHEVC(c string) bool {
	c = norm(c)
	return c == "hevc" || c == "h265" || c == "hvc1" || c == "hev1" ||
		hasPrefix(c, "hvc1.") || hasPrefix(c, "hev1.")
}

// IsH264 是否 H.264/AVC 族（含 avc1./avc3. 变体位）。
func IsH264(c string) bool {
	c = norm(c)
	return c == "h264" || c == "avc" || c == "avc1" || c == "avc3" ||
		hasPrefix(c, "avc1.") || hasPrefix(c, "avc3.")
}

// IsAV1 是否 AV1 族（含 av01. 变体位）。
func IsAV1(c string) bool {
	c = norm(c)
	return c == "av1" || c == "av01" || hasPrefix(c, "av01.")
}

// IsVP9 是否 VP9 族（含 vp09. 变体位）。
func IsVP9(c string) bool {
	c = norm(c)
	return c == "vp9" || c == "vp09" || hasPrefix(c, "vp09.")
}

// IsAAC 是否 AAC 族（含 mp4a. 变体位）。
func IsAAC(c string) bool {
	c = norm(c)
	return c == "aac" || c == "mp4a" || hasPrefix(c, "mp4a.")
}

// NormalizeCodecName 把 ffprobe 报出的编解码器名归一为 NewMovie selector 使用的规范名。
// 规范名（h264/h265/av1/vp9/aac/mp3/opus/ac3/eac3/dts/truehd…）原样返回；
// 变体位（avc1.4d0028→h264、hvc1.1.6→h265、av01.0.08M→av1、vp09→vp9、mp4a.40.2→aac）
// 归一到对应规范名，使 Select 的既有白名单判定对真实数据同样准确。
func NormalizeCodecName(s string) string {
	c := norm(s)
	switch {
	case IsHEVC(c):
		return "h265"
	case IsH264(c):
		return "h264"
	case IsAV1(c):
		return "av1"
	case IsVP9(c):
		return "vp9"
	case IsAAC(c):
		return "aac"
	}
	return c
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

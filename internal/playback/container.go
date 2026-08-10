// container.go 提供「容器格式魔数嗅探」，移植自 Lunarr 的 detectContainerFromMagic
// （src/lib/server/transcoding/container-format.ts）。
//
// 为什么需要它：NewMovie 以往靠「URL 扩展名 + ffprobe format_name」推断容器。
// 对无扩展名的 strm 直链（签名链 / OpenList /d/ 中转链接），扩展名猜不出，
// 只能等 ffprobe。魔数嗅探读文件头几个字节即可判断容器，比猜扩展名更稳更快，
// 在 ffprobe 缺失或 format_name 为空时尤其有用——这正是此前 Strm 总被甩外部播放器的
// 对抗手段之一（与第十节修复同源）。
package playback

// SniffContainer 依据媒体文件头若干字节判断容器格式，识别不出返回空串。
//
// 魔数对照（与 Lunarr 一致）：
//   - EBML 头 0x1A 0x45 0xDF 0xA3 → matroska；若该头范围内出现 "webm" 字符串则为 webm
//   - ISO BMFF 'ftyp' 盒（偏移 4..8） → mp4
//   - RIFF....AVI  → avi
//   - MPEG-TS 同步字节 0x47（偏移 0 与 188） → ts
//
// head 至少需 4 字节（matroska/mp4/avi 在 12 字节内可判）；ts 需 ≥189 字节。
// 调用方传入足够长的头部即可，不足时按能判的返回或空串。
func SniffContainer(head []byte) string {
	// EBML：matroska 与 webm 共用同一文件头，靠 doctype 区分。
	if len(head) >= 4 &&
		head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3 {
		preview := string(head)
		if len(head) > 64 {
			preview = string(head[:64])
		}
		if contains(preview, "webm") {
			return "webm"
		}
		return "mkv"
	}
	// ISO Base Media File Format：'ftyp' 盒紧跟在 4 字节 size 之后。
	if len(head) >= 8 && string(head[4:8]) == "ftyp" {
		return "mp4"
	}
	// AVI：RIFF 头 + 'AVI ' 标识。
	if len(head) >= 12 &&
		string(head[0:4]) == "RIFF" && string(head[8:12]) == "AVI " {
		return "avi"
	}
	// MPEG-TS：固定 188 字节包，每包以 0x47 同步字节开头。
	if len(head) > 188 && head[0] == 0x47 && head[188] == 0x47 {
		return "ts"
	}
	return ""
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

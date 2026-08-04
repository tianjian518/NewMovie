package playback

import "testing"

func TestL0NativeDirectRawURL(t *testing.T) {
	d := Select(Input{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		RawURL:  "https://cdn.example.com/f.mp4",
		DirectURL: "http://openlist:5244/d/x/f.mp4",
	})
	if d.Level != L0Direct || !d.UseRawURL || d.URL != "https://cdn.example.com/f.mp4" {
		t.Fatalf("应 L0 直链且优先 raw_url，得到 %+v", d)
	}
}

func TestL0NoRawFallsToDirectURL(t *testing.T) {
	d := Select(Input{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		DirectURL: "http://openlist:5244/d/x/f.mp4",
	})
	if d.Level != L0Direct || d.UseRawURL {
		t.Fatalf("无 raw_url 应回退 /d/，得到 %+v", d)
	}
}

func TestL1HotlinkProtection(t *testing.T) {
	d := Select(Input{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mp4", HotlinkProtection: true,
	})
	if d.Level != L1Proxy || d.URL == "" {
		t.Fatalf("防盗链应走代理，得到 %+v", d)
	}
}

func TestL2RemuxMKV(t *testing.T) {
	// MKV 但编码浏览器支持 → 重封装秒播（需 ffmpeg）
	d := Select(Input{
		Container: "mkv", VideoCodec: "h264", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mkv", FFmpegAvailable: true,
	})
	if d.Level != L2Remux {
		t.Fatalf("MKV(H264/AAC) 应 L2 重封装，得到 %+v", d)
	}
}

func TestL3TranscodeWhenEnabled(t *testing.T) {
	// 视频编码本身浏览器无法重封装、且开了转码、且服务端真能转码（libx264 在）→ 真转码。
	d := Select(Input{
		Container: "mkv", VideoCodec: "wmv", AudioCodec: "dts",
		RawURL: "https://cdn.example.com/f.mkv", TranscodeEnabled: true, FFmpegAvailable: true, TranscodeAvailable: true,
	})
	if d.Level != L3Transcode || !d.NeedsTranscode {
		t.Fatalf("不支持重封装的视频编码且开转码应 L3，得到 %+v", d)
	}
}

func TestHEVC_MKV_NoTranscode_External(t *testing.T) {
	// HEVC 在 MKV 里、且未开启/不支持转码：重封装成 MP4 浏览器（尤其 Chrome/Firefox）
	// 依然解不了 HEVC，页内只会一直转圈。正确做法是唤起外部播放器（自带 HEVC 解码），
	// 或提示用户开启转码。这正是对标 Jellyfin/Emby：HEVC 默认转码，否则外部。
	d := Select(Input{
		Container: "mkv", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mkv", FFmpegAvailable: true,
	})
	if d.Level != L4External {
		t.Fatalf("HEVC+MKV 无转码应 L4 外部（不再重封装后转圈），得到 %+v", d)
	}
}

func TestHEVC_MKV_AC3_NoTranscode_External(t *testing.T) {
	// HEVC + AC3：同上，视频编码浏览器解不了 → 外部播放器。
	d := Select(Input{
		Container: "mkv", VideoCodec: "hevc", AudioCodec: "ac3",
		RawURL: "https://cdn.example.com/f.mkv", FFmpegAvailable: true,
	})
	if d.Level != L4External {
		t.Fatalf("HEVC+MKV(AC3) 无转码应 L4 外部，得到 %+v", d)
	}
}

// TestHEVCInMP4_RegressionFix 锁住 v1.1.13→1.1.14 的回归：
// HEVC 装在原生 MP4 里，重封装成 MP4 浏览器照样解不了 HEVC，绝不能走 L2 重封装
// （旧逻辑会误判为 remux 并因缺 ffmpeg 直接 500）。正确行为：
//   - 有 ffmpeg 且开转码 → L3 视频转码（人人可播）。
//   - 有 ffmpeg 但关转码 → L4 外部播放器（至少不报错）。
//   - 无 ffmpeg → L4 外部播放器。
func TestHEVCInMP4_RegressionFix(t *testing.T) {
	// 转码开：应 L3。
	d := Select(Input{Container: "mp4", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mp4", TranscodeEnabled: true, FFmpegAvailable: true, TranscodeAvailable: true})
	if d.Level != L3Transcode {
		t.Fatalf("HEVC 的 MP4 + 转码开应 L3 转码，得到 %+v", d)
	}
	// 转码关：应 L4（不再误走 L2 remux）。
	d2 := Select(Input{Container: "mp4", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mp4", FFmpegAvailable: true})
	if d2.Level != L4External {
		t.Fatalf("HEVC 的 MP4 + 转码关应 L4 外部（而非 L2 remux），得到 %+v", d2)
	}
	// 无 ffmpeg：应 L4。
	d3 := Select(Input{Container: "mp4", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mp4"})
	if d3.Level != L4External {
		t.Fatalf("HEVC 的 MP4 + 无 ffmpeg应 L4 外部，得到 %+v", d3)
	}
}

func TestL4ExternalWhenTranscodeOff(t *testing.T) {
	// 视频编码本身不支持重封装、且未开转码 → 回退外部播放器（而不是给一句"不支持"）。
	d := Select(Input{
		Container: "avi", VideoCodec: "wmv", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.avi", TranscodeEnabled: false, FFmpegAvailable: true,
	})
	if d.Level != L4External {
		t.Fatalf("不支持重封装的视频编码且未开转码应 L4 外部，得到 %+v", d)
	}
}

func TestHEVC_MKV_DTS_NoTranscode_External(t *testing.T) {
	// 4K 蓝光原盘典型组合：HEVC 视频 + DTS-HD 音轨。视频编码浏览器解不了 → 外部播放器
	// （DTS 转 AAC 无济于事，因为视频仍是 HEVC）。开启转码后才会走 L3 把视频也转掉。
	d := Select(Input{
		Container: "mkv", VideoCodec: "hevc", AudioCodec: "dts",
		RawURL: "https://cdn.example.com/f.mkv", FFmpegAvailable: true,
	})
	if d.Level != L4External {
		t.Fatalf("HEVC+DTS 无转码应 L4 外部，得到 %+v", d)
	}
}

func TestHEVC_MKV_TrueHD_NoTranscode_External(t *testing.T) {
	// TrueHD/Atmos 同理：视频编码浏览器解不了 → 外部播放器。
	d := Select(Input{
		Container: "mkv", VideoCodec: "h265", AudioCodec: "truehd",
		RawURL: "https://cdn.example.com/f.mkv", FFmpegAvailable: true,
	})
	if d.Level != L4External {
		t.Fatalf("HEVC+TrueHD 无转码应 L4 外部，得到 %+v", d)
	}
}

func TestL3TranscodeHEVCWhenEnabled(t *testing.T) {
	// 关键修复：HEVC 在多数浏览器（尤其 Firefox / 多数 Chrome）无法解码，
	// 即使重封装成 MP4 也放不出。开启转码后，HEVC→H.264 转码，任何浏览器都能页内播。
	d := Select(Input{
		Container: "mkv", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mkv", TranscodeEnabled: true, FFmpegAvailable: true, TranscodeAvailable: true,
	})
	if d.Level != L3Transcode || !d.NeedsTranscode {
		t.Fatalf("HEVC 开转码应 L3 视频转码，得到 %+v", d)
	}
}

// TestTranscodeUnavailable_HEVCFallsBack 锁住 libx264 缺失时的安全降级：
// ffmpeg 在、转码开关开，但服务端 ffmpeg 缺 libx264 → 绝不能走 L3（会静默失败空 200）。
// HEVC 视频浏览器本就解不了，缺转码时统一唤起外部播放器（MKV/MP4 皆然），
// 不再重封装出浏览器解不了的 HEVC-MP4 导致页内一直转圈。
func TestTranscodeUnavailable_HEVCFallsBack(t *testing.T) {
	// MKV+HEVC：缺 libx264 → 外部播放器（不再 L2 重封装保留 HEVC 后转圈）。
	d := Select(Input{Container: "mkv", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mkv", FFmpegAvailable: true, TranscodeAvailable: false})
	if d.Level == L3Transcode {
		t.Fatalf("缺 libx264 时 HEVC 不应走 L3 转码，得到 %+v", d)
	}
	if d.Level != L4External {
		t.Fatalf("缺 libx264 的 MKV+HEVC 应 L4 外部，得到 %+v", d)
	}
	// MP4+HEVC：原生容器无法靠重封装修解码 → 外部播放器。
	d2 := Select(Input{Container: "mp4", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mp4", FFmpegAvailable: true, TranscodeAvailable: false})
	if d2.Level == L3Transcode {
		t.Fatalf("缺 libx264 时 HEVC-in-MP4 不应走 L3，得到 %+v", d2)
	}
	if d2.Level != L4External {
		t.Fatalf("缺 libx264 的 HEVC-in-MP4 应 L4 外部，得到 %+v", d2)
	}
}

func TestHEVCWhenTranscodeOff_External(t *testing.T) {
	// 转码关闭时（ffmpeg 在），HEVC+MKV 不再重封装（浏览器解不了 HEVC 会一直转圈），
	// 而是唤起外部播放器（自带 HEVC 解码），或提示用户开启转码页内播。
	d := Select(Input{
		Container: "mkv", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mkv", TranscodeEnabled: false, FFmpegAvailable: true,
	})
	if d.Level != L4External {
		t.Fatalf("HEVC 未开转码应 L4 外部（不再重封装后转圈），得到 %+v", d)
	}
}

func TestL4PreferExternal(t *testing.T) {
	d := Select(Input{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		PreferExternal: true,
	})
	if d.Level != L4External {
		t.Fatalf("用户偏好应优先 L4，得到 %+v", d)
	}
}

// --- 无 ffmpeg 时的优雅降级：凡是需要重封装/转码的文件一律外部播放器，绝不给 500 ---

func TestNoFFmpeg_MKVH264ToExternal(t *testing.T) {
	d := Select(Input{Container: "mkv", VideoCodec: "h264", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mkv"})
	if d.Level != L4External {
		t.Fatalf("无 ffmpeg 时 MKV 应降级外部，得到 %+v", d)
	}
	if d.Reason == "" {
		t.Fatalf("降级应给出原因，得到 %+v", d)
	}
}

func TestNoFFmpeg_HEVCToExternal(t *testing.T) {
	d := Select(Input{Container: "mkv", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mkv"})
	if d.Level != L4External {
		t.Fatalf("无 ffmpeg 时 HEVC 应降级外部，得到 %+v", d)
	}
}

func TestNoFFmpeg_NativeMP4StillDirect(t *testing.T) {
	// 浏览器原生可播的 MP4 不需要 ffmpeg，无 ffmpeg 时仍应直链。
	d := Select(Input{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mp4"})
	if d.Level != L0Direct {
		t.Fatalf("无 ffmpeg 时原生 MP4 仍应 L0 直链，得到 %+v", d)
	}
}

// TestUnknownContainer_TriesDirectPlay 锁住「无扩展名 strm 直链」的兜底：
// 容器未知时不像其它实现那样直接甩外部，而是把直链交给浏览器试播（多数 strm 直链
// 本就是可直接播放的流）；仅当确实没有任何可用直链时才外部播放器。
func TestUnknownContainer_TriesDirectPlay(t *testing.T) {
	d := Select(Input{Container: "", VideoCodec: "", AudioCodec: "",
		RawURL: "https://cdn.example.com/secret-link?sign=abc", FFmpegAvailable: true})
	if d.Level != L0Direct {
		t.Fatalf("容器未知的直链应 L0 交由浏览器试播，得到 %+v", d)
	}
}

func TestUnknownContainer_NoURL_External(t *testing.T) {
	// 容器未知且无任何可用直链 → 外部播放器。
	d := Select(Input{Container: "", VideoCodec: "", AudioCodec: "", FFmpegAvailable: true})
	if d.Level != L4External {
		t.Fatalf("容器未知且无直链应 L4 外部，得到 %+v", d)
	}
}

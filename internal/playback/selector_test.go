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
	// MKV 但编码浏览器支持 → 重封装秒播
	d := Select(Input{
		Container: "mkv", VideoCodec: "h264", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mkv",
	})
	if d.Level != L2Remux {
		t.Fatalf("MKV(H264/AAC) 应 L2 重封装，得到 %+v", d)
	}
}

func TestL3TranscodeWhenEnabled(t *testing.T) {
	// 音轨无法重封装（DTS）且开了转码 → 真转码（重封装优先于转码，故用 DTS 触发）。
	d := Select(Input{
		Container: "mkv", VideoCodec: "hevc", AudioCodec: "dts",
		RawURL: "https://cdn.example.com/f.mkv", TranscodeEnabled: true,
	})
	if d.Level != L3Transcode || !d.NeedsTranscode {
		t.Fatalf("HEVC+DTS 且开转码应 L3 转码，得到 %+v", d)
	}
}

func TestL2RemuxHEVC_MKV_AAC(t *testing.T) {
	// HEVC + AAC 的 MKV：编码浏览器能解，仅容器不认 → 重封装页内播，不再甩外部。
	d := Select(Input{
		Container: "mkv", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mkv",
	})
	if d.Level != L2Remux {
		t.Fatalf("HEVC+MKV(AAC) 应 L2 重封装页内播，得到 %+v", d)
	}
}

func TestL2RemuxHEVC_MKV_AC3(t *testing.T) {
	// 4K 常见组合：HEVC + AC3（杜比数字）。AC3 可 -c copy 进 MP4 → 页内播。
	d := Select(Input{
		Container: "mkv", VideoCodec: "hevc", AudioCodec: "ac3",
		RawURL: "https://cdn.example.com/f.mkv",
	})
	if d.Level != L2Remux {
		t.Fatalf("HEVC+MKV(AC3) 应 L2 重封装，得到 %+v", d)
	}
}

func TestL2RemuxHEVC_MP4(t *testing.T) {
	// 已是 MP4 但里面是 HEVC：原容器浏览器虽认，但 h265 不在原生白名单，
	// 走同封装重混流即可页内尝试播放。
	d := Select(Input{
		Container: "mp4", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mp4",
	})
	if d.Level != L2Remux {
		t.Fatalf("HEVC 的 MP4 应 L2 重封装，得到 %+v", d)
	}
}

func TestL4ExternalWhenTranscodeOff(t *testing.T) {
	// HEVC 但音轨无法重封装（DTS/TrueHD/Atmos 装不进 MP4）→ 仍回退外部播放器，
	// 而不是给一句"不支持"。
	d := Select(Input{
		Container: "mkv", VideoCodec: "hevc", AudioCodec: "dts",
		RawURL: "https://cdn.example.com/f.mkv", TranscodeEnabled: false,
	})
	if d.Level != L4External {
		t.Fatalf("HEVC+DTS 无法重封装应 L4 外部，得到 %+v", d)
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

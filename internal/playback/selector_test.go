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
	d := Select(Input{
		Container: "mkv", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mkv", TranscodeEnabled: true,
	})
	if d.Level != L3Transcode || !d.NeedsTranscode {
		t.Fatalf("HEVC 应转码，得到 %+v", d)
	}
}

func TestL4ExternalWhenTranscodeOff(t *testing.T) {
	// HEVC 且未开转码 → 唤起外部播放器（绝不给"不支持"）
	d := Select(Input{
		Container: "mkv", VideoCodec: "hevc", AudioCodec: "aac",
		RawURL: "https://cdn.example.com/f.mkv", TranscodeEnabled: false,
	})
	if d.Level != L4External {
		t.Fatalf("HEVC 未开转码应 L4 外部，得到 %+v", d)
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

package playback

import "testing"

func TestNormalizeCodecName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// 规范名原样返回
		{"h264", "h264"},
		{"h265", "h265"},
		{"hevc", "h265"},
		{"av1", "av1"},
		{"vp9", "vp9"},
		{"aac", "aac"},
		{"dts", "dts"},
		{"truehd", "truehd"},
		// 真实变体位归一到规范名（移植自 Lunarr 能力判定）
		{"avc1.4d0028", "h264"},
		{"avc3.640028", "h264"},
		{"hvc1.1.6.L93.B0", "h265"},
		{"hev1.1.6.L93.B0", "h265"},
		{"av01.0.08M.08", "av1"},
		{"vp09.00.10.08", "vp9"},
		{"mp4a.40.2", "aac"},
		// 大小写/空白容错
		{"HVC1.1.6", "h265"},
		{"  avc1.4d0028  ", "h264"},
	}
	for _, c := range cases {
		if got := NormalizeCodecName(c.in); got != c.want {
			t.Fatalf("NormalizeCodecName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCodecMatchers(t *testing.T) {
	hevc := []string{"hevc", "h265", "hvc1", "hev1", "hvc1.1.6.L93.B0", "hev1.2.1"}
	h264 := []string{"h264", "avc", "avc1", "avc3", "avc1.4d0028"}
	av1 := []string{"av1", "av01", "av01.0.08M.08"}
	vp9 := []string{"vp9", "vp09", "vp09.00.10.08"}
	aac := []string{"aac", "mp4a", "mp4a.40.2"}
	for _, c := range hevc {
		if !IsHEVC(c) {
			t.Errorf("IsHEVC(%q) = false, want true", c)
		}
	}
	for _, c := range h264 {
		if !IsH264(c) {
			t.Errorf("IsH264(%q) = false, want true", c)
		}
	}
	for _, c := range av1 {
		if !IsAV1(c) {
			t.Errorf("IsAV1(%q) = false, want true", c)
		}
	}
	for _, c := range vp9 {
		if !IsVP9(c) {
			t.Errorf("IsVP9(%q) = false, want true", c)
		}
	}
	for _, c := range aac {
		if !IsAAC(c) {
			t.Errorf("IsAAC(%q) = false, want true", c)
		}
	}
	// 负例
	if IsHEVC("avc1.4d0028") || IsH264("hvc1.1.6") || IsAAC("dts") || IsAV1("vp9") {
		t.Fatal("编解码器判定出现误判（负例应为 false）")
	}
}

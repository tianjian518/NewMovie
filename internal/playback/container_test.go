package playback

import "testing"

func TestSniffContainer(t *testing.T) {
	// mp4：'ftyp' 盒位于偏移 4。
	mp4 := []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}
	// matroska：EBML 头，前 64 字节不含 "webm"。
	mkv := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x93, 0x42, 0x86, 0x81}
	// webm：EBML 头，且前 64 字节内出现 "webm" 字符串。
	webm := append([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x93, 0x42, 0x86, 0x81, 0x01}, []byte("somewebmmeta")...)
	// avi：RIFF 头 + 'AVI '。
	avi := []byte{'R', 'I', 'F', 'F', 0, 0, 0, 0, 'A', 'V', 'I', ' '}
	// ts：189 字节，偏移 0 与 188 为 0x47 同步字节。
	ts := make([]byte, 189)
	ts[0] = 0x47
	ts[188] = 0x47
	// 未知：全零。
	unknown := make([]byte, 16)

	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"mp4", mp4, "mp4"},
		{"matroska", mkv, "mkv"},
		{"webm", webm, "webm"},
		{"avi", avi, "avi"},
		{"mpegts", ts, "ts"},
		{"unknown", unknown, ""},
		{"too_short", []byte{0x1A, 0x45}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SniffContainer(c.head); got != c.want {
				t.Fatalf("SniffContainer(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

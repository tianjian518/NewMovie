package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"newmovie/internal/model"
)

// TestPlay_StrmHttpResolvesInPage 锁定 STRM 修复：http 直链型 strm 此前因 StorageID 为空，
// playItem 用 GetStorage("") 直接 400，只能甩外部播放器。修复后应解析出 http 源并走 L2 重封装页内播。
func TestPlay_StrmHttpResolvesInPage(t *testing.T) {
	st, do := featureFixture(t)
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "fstrm", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: "https://cdn.example.com/movie.mkv",
	})
	code, body := do(http.MethodGet, "/api/items/fstrm/play", "")
	if code != http.StatusOK {
		t.Fatalf("play 期望 200，得到 %d (%s)", code, body)
	}
	var d struct {
		Level int    `json:"level"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if d.Level != 2 {
		t.Fatalf("strm http 应 L2 重封装页内播，得到 level=%d (%s)", d.Level, body)
	}
	if !strings.Contains(d.URL, "/api/play/remux?u=") {
		t.Fatalf("strm 播放 URL 应指向 remux 端点，得到 %q", d.URL)
	}
}

// TestPlay_StrmHttpHEVC_TranscodeWhenEnabled 锁定视频转码：HEVC 的 strm 在开启
// 「允许视频转码」后应走 L3（HEVC→H.264），任何浏览器都能页内播，不再报「视频不存在」。
func TestPlay_StrmHttpHEVC_TranscodeWhenEnabled(t *testing.T) {
	st, do := featureFixture(t)
	_ = st.SaveSetting("transcode_enabled", "1")
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "fhevc", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: "https://cdn.example.com/movie-hevc.mkv",
		VideoCodec: "hevc", AudioCodec: "aac",
	})
	code, body := do(http.MethodGet, "/api/items/fhevc/play", "")
	if code != http.StatusOK {
		t.Fatalf("play 期望 200，得到 %d (%s)", code, body)
	}
	var d struct {
		Level int    `json:"level"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if d.Level != 3 {
		t.Fatalf("HEVC+转码应 L3 视频转码，得到 level=%d (%s)", d.Level, body)
	}
	if !strings.Contains(d.URL, "/api/play/transcode?u=") {
		t.Fatalf("HEVC 播放 URL 应指向 transcode 端点，得到 %q", d.URL)
	}
}

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/store"
)

// 造一个 vp9+aac 的 MKV（沙箱 ffmpeg 无 libx264，但 vp9/libvpx 可用）。
// vp9 属浏览器原生可解（nativeVideo），MKV 属需重封装容器（containerNeedsRemux），
// 故一个「无扩展名」的 vp9-MKV strm 经 ffprobe 探测后应走 L2 重封装页内播——
// 这正是此前「无扩展名 strm 因猜不出容器被甩外部」修复的端到端证明。
func makeVp9MKV(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mkv := filepath.Join(dir, "v.mkv")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:v", "vp9", "-c:a", "aac", "-shortest", mkv)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("造 vp9 MKV 失败: %v\n%s", err, b)
	}
	return mkv
}

// strmServerWithStore 起一个带「本地媒体服务」的存储源（127.0.0.1 放行 SSRF），
// 媒体服务把任意路径都当作该 MKV 返回，便于模拟无扩展名直链。
func strmServerWithStore(t *testing.T, mkv string) (*httptest.Server, store.Store, func(method, path, body string) (int, string)) {
	t.Helper()
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/x-matroska")
		http.ServeFile(w, r, mkv)
	}))
	t.Cleanup(media.Close)

	st, err := store.NewJSONStore(filepath.Join(t.TempDir(), "v.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	_ = st.SaveUser(model.User{ID: "u1", Username: "admin", Password: auth.HashPassword("admin"), IsAdmin: true})
	_ = st.UpsertToken("u1", "tok")
	_ = st.SaveStorage(model.Storage{ID: "s1", Name: "local", Type: model.StorageOpenList, BaseURL: media.URL})

	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache"}
	ts := httptest.NewServer(New(st, cfg).Handler())
	t.Cleanup(ts.Close)

	do := func(method, path, body string) (int, string) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("req %s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	return media, st, do
}

// TestPlay_StrmExtensionless_ProbedInPage 端到端锁定「无扩展名 strm 页内播」：
// strm 文本是一行无扩展名的直链（签名链 / OpenList /d/ 形态），播放时靠 ffprobe
// 从媒体内容认出容器/编码，走 L2 重封装页内播，不再因为文件名猜不出就被甩外部播放器。
func TestPlay_StrmExtensionless_ProbedInPage(t *testing.T) {
	mkv := makeVp9MKV(t)
	media, st, do := strmServerWithStore(t, mkv)
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "fext", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: media.URL + "/stream/abcdef123", // 无扩展名
		ProbeState: "pending",
	})
	code, body := do(http.MethodGet, "/api/items/fext/play", "")
	if code != http.StatusOK {
		t.Fatalf("play 期望 200，得到 %d (%s)", code, body)
	}
	var d struct {
		Level       int    `json:"level"`
		URL         string `json:"url"`
		VideoCodec  string `json:"video_codec"`
		AudioTracks []struct {
			Codec string `json:"codec"`
		} `json:"audio_tracks"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	// 探针确实跑了（拿到了音轨），且按真实编码决策 → L2 重封装页内播。
	if len(d.AudioTracks) == 0 {
		t.Fatalf("应已 ffprobe 探测出音轨，得到 %s", body)
	}
	if d.Level != 2 {
		t.Fatalf("无扩展名 strm 应经探测后 L2 页内播，得到 level=%d (%s)", d.Level, body)
	}
	if !strings.Contains(d.URL, "/api/play/remux?u=") {
		t.Fatalf("播放 URL 应指向 remux 端点，得到 %q", d.URL)
	}
}

// TestPlay_StrmWithExtension_Remux 有扩展名的 strm 直链：沿用扩展名推断（或探测）得到容器，
// MKV 走 L2 重封装页内播。
func TestPlay_StrmWithExtension_Remux(t *testing.T) {
	mkv := makeVp9MKV(t)
	media, st, do := strmServerWithStore(t, mkv)
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "fext2", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: media.URL + "/movie.mkv",
		ProbeState: "pending",
	})
	code, body := do(http.MethodGet, "/api/items/fext2/play", "")
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
		t.Fatalf("strm(.mkv) 应 L2 页内播，得到 level=%d (%s)", d.Level, body)
	}
	if !strings.Contains(d.URL, "/api/play/remux?u=") {
		t.Fatalf("播放 URL 应指向 remux 端点，得到 %q", d.URL)
	}
}

// TestPlay_StrmResolutionFailure_ClearError 锁定：strm 彻底解析不出源（相对路径且无存储）时，
// 返回明确 400，而不是让后面走到「拿不到源地址」的 502 或静默外放。
func TestPlay_StrmResolutionFailure_ClearError(t *testing.T) {
	st, do := featureFixture(t)
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "fbad", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: "/some/relative/path/no/storage", // 相对路径且无存储可解析
	})
	code, body := do(http.MethodGet, "/api/items/fbad/play", "")
	if code != http.StatusBadRequest {
		t.Fatalf("无法解析的 strm 应 400，得到 %d (%s)", code, body)
	}
}

// TestContainerExt_IPHost 锁定 containerExt 的回归：对 IP 型主机（如内置 139cas 的
// 127.0.0.1:5244），绝不可把主机里的点误当扩展名分隔符——否则容器会被解析成
// "1:5244/..." 这类垃圾值，导致所有内置源 strm 的容器推断全错、被错误甩外部播放器。
func TestContainerExt_IPHost(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:5244/d/local/movie.mkv": "mkv",
		"http://127.0.0.1:5244/stream/abcdef123":  "", // 无扩展名直链
		"https://cdn.example.com/movie.mkv":        "mkv",
		"https://cdn.example.com/path/movie.mp4":   "mp4",
		"/mnt/cloud/电影/movie.mkv":                  "mkv",
		"movie.mkv":                                 "mkv",
		"http://127.0.0.1:5244/d/x?sign=abc":       "",
	}
	for in, want := range cases {
		if got := containerExt(in); got != want {
			t.Fatalf("containerExt(%q)=%q, 期望 %q", in, got, want)
		}
	}
}

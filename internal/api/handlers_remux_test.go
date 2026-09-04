package api

import (
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

// newAuthedServerWithStore 起带登录态的服务端并暴露 store，便于注册存储源主机
// （remux/proxy 对已配置存储源放行内网，测试用的本地媒体服务器靠它才能被拉取）。
func newAuthedServerWithStore(t *testing.T) (*httptest.Server, store.Store, func(path string) (int, string)) {
	t.Helper()
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	u := model.User{ID: "u1", Username: "admin", Password: auth.HashPassword("admin"), IsAdmin: true}
	_ = st.SaveUser(u)
	_ = st.UpsertToken(u.ID, "tok")
	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache"}
	var s store.Store = st
	ts := httptest.NewServer(New(st, cfg).Handler())
	t.Cleanup(ts.Close)

	get := func(path string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("req: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	return ts, s, get
}

// TestRemux_MissingParam 缺 u 参数应是 400。
func TestRemux_MissingParam(t *testing.T) {
	_, _, get := newAuthedServerWithStore(t)
	code, _ := get("/api/play/remux")
	if code != http.StatusBadRequest {
		t.Errorf("缺参期望 400，得到 %d", code)
	}
}

// TestRemux_BlocksSSRF 重封装端点同样要挡内网/元数据，不能当跳板。
func TestRemux_BlocksSSRF(t *testing.T) {
	_, _, get := newAuthedServerWithStore(t)
	targets := []string{
		"http://169.254.169.254/latest/", // 云元数据
		"http://127.0.0.1:22/",           // 本机端口
		"http://10.0.0.1/",               // 内网
		"file:///etc/passwd",             // 非 http 协议
	}
	for _, target := range targets {
		code, _ := get("/api/play/remux?u=" + target)
		if code == http.StatusOK {
			t.Errorf("SSRF 未拦截: u=%s -> 200", target)
		}
	}
}

// TestRemux_StreamsMP4 用 ffmpeg 真实把 MKV 转封装为 MP4 流式返回。
// 需要运行环境安装 ffmpeg（沙箱/CI 镜像均满足）；缺失则跳过。
func TestRemux_StreamsMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 不可用，跳过转封装集成测试")
	}
	_, st, get := newAuthedServerWithStore(t)
	enc := requireH264Encoder(t)

	// 1) 造一个 h264+aac 的 MKV（浏览器原生不认容器）。
	mkv := filepath.Join(t.TempDir(), "sample.mkv")
	gen := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x180:rate=15",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", enc, "-c:a", "aac", "-pix_fmt", "yuv420p", mkv)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("生成 mkv 失败: %v (%s)", err, out)
	}

	// 2) 起本地媒体服务器，并把它登记为存储源（放行内网拉取）。
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, mkv)
	}))
	t.Cleanup(media.Close)
	_ = st.SaveStorage(model.Storage{ID: "st1", Type: model.StorageOpenList, BaseURL: media.URL})

	// 3) 请求转封装端点。
	code, body := get("/api/play/remux?u=" + media.URL + "/sample.mkv")
	if code != http.StatusOK {
		t.Fatalf("remux 期望 200，得到 %d (body=%s)", code, body)
	}
	// 4) 校验返回的是 MP4：开头应为 ftyp box。
	if !strings.Contains(body, "ftyp") {
		t.Errorf("返回内容不是 MP4（缺少 ftyp 标记）")
	}
}

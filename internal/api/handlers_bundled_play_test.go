package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/store"
)

// bundledFixture 建一个「内置 139cas」场景的测试服务端：Bundled=true，
// 内置后端地址固定为 http://127.0.0.1:5244。返回的 srv 可手动改 ffmpegOK 等字段。
func bundledFixture(t *testing.T) (store.Store, func(method, path, body string) (int, string), *Server) {
	t.Helper()
	// 同 featureFixture：关掉 HLS 以锁定 remux/transcode URL 旧断言（HLS 交付单独测）。
	t.Setenv("VIDRIVE_HLS", "0")
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	u := model.User{ID: "u1", Username: "admin", Password: auth.HashPassword("admin"), IsAdmin: true}
	_ = st.SaveUser(u)
	_ = st.UpsertToken(u.ID, "tok")
	_ = st.SaveMediaItem(model.MediaItem{ID: "m1", LibraryID: "lib1", Kind: model.KindMovie, Title: "测试片", Year: 2021})

	cfg := &config.Config{
		DataDir:    t.TempDir(),
		CacheDir:   t.TempDir() + "/cache",
		Bundled:    true,
		BundledURL: "http://127.0.0.1:5244",
		BundledProxy: true,
	}
	srv := New(st, cfg)
	ts := httptest.NewServer(srv.Handler())
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
	return st, do, srv
}

// mockBundledOpenList 模拟内置 139cas：无论请求什么路径，都返回指向 127.0.0.1:5244
// 的内部直链（与真实 2.0 容器里 139cas 的行为一致——浏览器在用户机器上连不到）。
func mockBundledOpenList(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"data": map[string]any{
				"raw_url": "http://127.0.0.1:5244/p/local/movie.mp4?sign=abc123",
				"url":     "http://127.0.0.1:5244/d/movie.mp4?sign=abc123",
			},
		})
	}))
}

// TestRewriteBundledURL 直接锁定核心修复：把内置后端（127.0.0.1:5244）的直链改写为
// 同源的 /openlist 反代路径；外部 OpenList、非 bundled 配置、以及其它 host 一律原样返回。
func TestRewriteBundledURL(t *testing.T) {
	s := &Server{Cfg: &config.Config{Bundled: true, BundledURL: "http://127.0.0.1:5244"}}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"原生 MP4 直链", "http://127.0.0.1:5244/p/local/movie.mp4?sign=abc123", "/openlist/p/local/movie.mp4?sign=abc123"},
		{"原生 MKV 直链", "http://127.0.0.1:5244/p/local/movie.mkv?sign=z", "/openlist/p/local/movie.mkv?sign=z"},
		{"localhost 变体", "http://localhost:5244/d/x.mp4?sign=1", "/openlist/d/x.mp4?sign=1"},
		{"外部 OpenList 直链不改写", "https://ol.example.com/d/x.mp4?sign=1", "https://ol.example.com/d/x.mp4?sign=1"},
		{"空串", "", ""},
		{"非法 URL 不改写", "::::not-a-url", "::::not-a-url"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.rewriteBundledURL(c.in); got != c.want {
				t.Fatalf("rewriteBundledURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	// 非 bundled 配置：即便 host 命中也不改写（此时源是用户自己的外部 OpenList，浏览器可达）。
	s2 := &Server{Cfg: &config.Config{Bundled: false, BundledURL: "http://127.0.0.1:5244"}}
	if got := s2.rewriteBundledURL("http://127.0.0.1:5244/p/local/movie.mp4?sign=1"); got != "http://127.0.0.1:5244/p/local/movie.mp4?sign=1" {
		t.Fatalf("非 bundled 不应改写，得到 %q", got)
	}
}

// TestPlay_BundledNativeMp4_RewrittenToOpenlistProxy 锁定「所有 MP4 黑屏」根因修复：
// 内置 139cas 返回的 MP4 直链指向 127.0.0.1:5244，浏览器连不到。playItem 必须把
// 下发给浏览器的 URL 改写为同源 /openlist/...，浏览器才只跟 8096 入口打交道。
func TestPlay_BundledNativeMp4_RewrittenToOpenlistProxy(t *testing.T) {
	st, do, _ := bundledFixture(t)
	ol := mockBundledOpenList(t)
	defer ol.Close()
	_ = st.SaveStorage(model.Storage{ID: "ol1", Name: "内置网盘", Type: model.StorageOpenList, BaseURL: ol.URL, Token: "t"})
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "fmp4", ItemID: "m1", Source: model.SrcNative, StorageID: "ol1", Path: "/movie.mp4",
		ProbeState: "done", Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
	})

	code, body := do(http.MethodGet, "/api/items/fmp4/play", "")
	if code != http.StatusOK {
		t.Fatalf("play 期望 200，得到 %d (%s)", code, body)
	}
	var d struct {
		Level     int    `json:"level"`
		URL       string `json:"url"`
		RawURL    string `json:"raw_url"`
		DirectURL string `json:"direct_url"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if d.Level != 0 {
		t.Fatalf("原生 MP4 应 L0 直链，得到 level=%d (%s)", d.Level, body)
	}
	// 浏览器拿到的直链必须是 /openlist 反代，绝不能还是 127.0.0.1:5244。
	if d.URL != "/openlist/p/local/movie.mp4?sign=abc123" {
		t.Fatalf("浏览器直链应改写为 /openlist 反代，得到 url=%q", d.URL)
	}
	if strings.Contains(d.URL, "127.0.0.1:5244") {
		t.Fatalf("浏览器直链泄露了内部后端地址：%q", d.URL)
	}
	if d.RawURL != "/openlist/p/local/movie.mp4?sign=abc123" {
		t.Fatalf("raw_url 也应改写，得到 %q", d.RawURL)
	}
	if d.DirectURL != "/openlist/d/movie.mp4?sign=abc123" {
		t.Fatalf("direct_url 也应改写，得到 %q", d.DirectURL)
	}
}

// TestPlay_BundledMkv_RemuxKeepsBackendURL 锁定 MKV 路径：浏览器不认 MKV 容器，
// 必须走服务端 remux。remux 的 u 参数里保留真实 5244 地址（由服务端内部取流，
// SSRF 守卫已放行 127.0.0.1 存储源），而给浏览器的 raw_url/direct_url 改写为 /openlist，
// 保证「外放播放器 / 前端兜底」也能用。
func TestPlay_BundledMkv_RemuxKeepsBackendURL(t *testing.T) {
	st, do, _ := bundledFixture(t)
	ol := mockBundledOpenList(t)
	defer ol.Close()
	_ = st.SaveStorage(model.Storage{ID: "ol1", Name: "内置网盘", Type: model.StorageOpenList, BaseURL: ol.URL, Token: "t"})
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "fmkv", ItemID: "m1", Source: model.SrcNative, StorageID: "ol1", Path: "/movie.mkv",
		ProbeState: "done", Container: "mkv", VideoCodec: "h264", AudioCodec: "aac",
	})

	code, body := do(http.MethodGet, "/api/items/fmkv/play", "")
	if code != http.StatusOK {
		t.Fatalf("play 期望 200，得到 %d (%s)", code, body)
	}
	var d struct {
		Level     int    `json:"level"`
		URL       string `json:"url"`
		RawURL    string `json:"raw_url"`
		DirectURL string `json:"direct_url"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if d.Level != 2 {
		t.Fatalf("MKV 应 L2 重封装，得到 level=%d (%s)", d.Level, body)
	}
	if !strings.Contains(d.URL, "/api/play/remux?u=") {
		t.Fatalf("MKV 应走 remux 端点，得到 url=%q", d.URL)
	}
	// remux 的 u 参数必须仍是 5244 内部地址（服务端取流用），不能改写成 /openlist。
	if !strings.Contains(d.URL, "127.0.0.1%3A5244") && !strings.Contains(d.URL, "127.0.0.1:5244") {
		t.Fatalf("remux 的 u 参数应保留 5244 内部地址，得到 url=%q", d.URL)
	}
	// 但给浏览器/外放的 raw_url/direct_url 必须改写成 /openlist。
	if d.RawURL != "/openlist/p/local/movie.mp4?sign=abc123" {
		t.Fatalf("raw_url 应改写为 /openlist，得到 %q", d.RawURL)
	}
	if d.DirectURL != "/openlist/d/movie.mp4?sign=abc123" {
		t.Fatalf("direct_url 应改写为 /openlist，得到 %q", d.DirectURL)
	}
}

// TestPlay_BundledExternalFallback_RewritesRawURL 锁定「缺 ffmpeg 唤起外部播放器」路径：
// 此时 dec.url 为空，前端用 raw_url/direct_url 唤起外部播放器。若源是内置后端，
// 这两个地址也必须是 /openlist 反代，否则外部播放器被甩到连不上的 127.0.0.1:5244。
func TestPlay_BundledExternalFallback_RewritesRawURL(t *testing.T) {
	st, do, srv := bundledFixture(t)
	srv.ffmpegOK = false // 模拟镜像未含 ffmpeg
	ol := mockBundledOpenList(t)
	defer ol.Close()
	_ = st.SaveStorage(model.Storage{ID: "ol1", Name: "内置网盘", Type: model.StorageOpenList, BaseURL: ol.URL, Token: "t"})
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "fmkvx", ItemID: "m1", Source: model.SrcNative, StorageID: "ol1", Path: "/movie.mkv",
		ProbeState: "done", Container: "mkv", VideoCodec: "h264", AudioCodec: "aac",
	})

	code, body := do(http.MethodGet, "/api/items/fmkvx/play", "")
	if code != http.StatusOK {
		t.Fatalf("play 期望 200，得到 %d (%s)", code, body)
	}
	var d struct {
		Level     int    `json:"level"`
		RawURL    string `json:"raw_url"`
		DirectURL string `json:"direct_url"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if d.Level != 4 {
		t.Fatalf("缺 ffmpeg 的 MKV 应 L4 外部，得到 level=%d (%s)", d.Level, body)
	}
	if !strings.HasPrefix(d.RawURL, "/openlist/") {
		t.Fatalf("L4 外放的 raw_url 必须改写为 /openlist，得到 %q", d.RawURL)
	}
	if !strings.HasPrefix(d.DirectURL, "/openlist/") {
		t.Fatalf("L4 外放的 direct_url 必须改写为 /openlist，得到 %q", d.DirectURL)
	}
	if strings.Contains(d.RawURL, "127.0.0.1:5244") {
		t.Fatalf("L4 外放地址泄露了内部后端地址：%q", d.RawURL)
	}
}

// TestPlay_BundledStrmHttp_RewrittenToOpenlist 锁定 STRM 场景：当 .strm 文本本身就是
// 指向内置 139cas（127.0.0.1:5244）的直链时，playItem 也必须把它改写成 /openlist 反代，
// 否则浏览器/外放播放器连不到容器内部地址。这正对应「所有 Strm 不能播放」的反馈。
func TestPlay_BundledStrmHttp_RewrittenToOpenlist(t *testing.T) {
	st, do, _ := bundledFixture(t)
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "fstrm5244", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: "http://127.0.0.1:5244/p/local/strm.mp4?sign=zzz",
		ProbeState: "done", Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
	})

	code, body := do(http.MethodGet, "/api/items/fstrm5244/play", "")
	if code != http.StatusOK {
		t.Fatalf("play 期望 200，得到 %d (%s)", code, body)
	}
	var d struct {
		Level     int    `json:"level"`
		URL       string `json:"url"`
		RawURL    string `json:"raw_url"`
		DirectURL string `json:"direct_url"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if d.Level != 0 {
		t.Fatalf("指向内置后端的原生 MP4 strm 应 L0 直链，得到 level=%d (%s)", d.Level, body)
	}
	if d.URL != "/openlist/p/local/strm.mp4?sign=zzz" {
		t.Fatalf("strm 浏览器直链应改写为 /openlist 反代，得到 url=%q", d.URL)
	}
	if strings.Contains(d.URL, "127.0.0.1:5244") {
		t.Fatalf("strm 浏览器直链泄露了内部后端地址：%q", d.URL)
	}
	if d.RawURL != "/openlist/p/local/strm.mp4?sign=zzz" {
		t.Fatalf("strm raw_url 应改写，得到 %q", d.RawURL)
	}
}

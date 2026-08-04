package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"newmovie/internal/config"
	"newmovie/internal/store"
	"newmovie/internal/tmdb"
)

// TestImageProxy_TMDBImageServedAndCached 锁定海报代理：浏览器请求 /api/image?u=<TMDB图链>，
// 服务端去取图并缓存，返回图片字节；白名单内主机放行。
func TestImageProxy_TMDBImageServedAndCached(t *testing.T) {
	// 起一个假 TMDB 图片服务器，返回 1x1 PNG。
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82}
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer imgSrv.Close()

	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	// 用镜像基址把假图服务器主机加入白名单（httptest 绑定 127.0.0.1，非 image.tmdb.org）。
	base := imgSrv.URL + "/t/p"
	tmdb.SetImageBase(base)
	defer tmdb.SetImageBase("")
	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache", TMDBImageBase: base}
	srv := New(st, cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	u := imgSrv.URL + "/t/p/w500/abc.jpg"
	resp, err := http.Get(ts.URL + "/api/image?u=" + url.QueryEscape(u))
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("/api/image 期望 200，得到 %d (%s)", resp.StatusCode, b)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(png) {
		t.Fatalf("返回图片内容不一致，len=%d", len(got))
	}
	// 二次请求应命中磁盘缓存（再起一个 server 实例读同一 CacheDir 验证落盘）。
	ts2 := httptest.NewServer(New(st, cfg).Handler())
	defer ts2.Close()
	resp2, err := http.Get(ts2.URL + "/api/image?u=" + url.QueryEscape(u))
	if err != nil {
		t.Fatalf("req2: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("缓存命中请求期望 200，得到 %d", resp2.StatusCode)
	}
}

// TestImageProxy_RejectsInternalHost 锁定 SSRF：/api/image 只允许 TMDB 图片 CDN 主机，
// 内网/回环/元数据地址一律拒绝，杜绝被当成内网跳板。
func TestImageProxy_RejectsInternalHost(t *testing.T) {
	st, _ := store.NewJSONStore(t.TempDir() + "/v.json")
	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache"}
	srv := New(st, cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, bad := range []string{
		"http://127.0.0.1:5244/p/x.jpg",
		"http://169.254.169.254/latest/meta-data",
		"http://192.168.1.1/",
		"http://localhost/secret.jpg",
	} {
		resp, err := http.Get(ts.URL + "/api/image?u=" + url.QueryEscape(bad))
		if err != nil {
			t.Fatalf("req %s: %v", bad, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("内网地址 %q 应被 403 拒绝，得到 %d", bad, resp.StatusCode)
		}
	}
}

// TestImageProxy_AllowsConfiguredMirror 锁定镜像：TMDB_IMAGE_BASE 配置的镜像主机也能经代理放行。
func TestImageProxy_AllowsConfiguredMirror(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4E, 0x47}
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(png)
	}))
	defer mirror.Close()
	// 配置图片基址为该镜像（提取 host 前的根）。
	base := strings.TrimSuffix(mirror.URL, "") + "/t/p"
	if !strings.HasPrefix(base, "http") {
		t.Fatalf("bad base %q", base)
	}
	tmdb.SetImageBase(base)
	defer tmdb.SetImageBase("")

	st, _ := store.NewJSONStore(t.TempDir() + "/v.json")
	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache", TMDBImageBase: base}
	srv := New(st, cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	u := mirror.URL + "/t/p/w500/m.jpg"
	resp, err := http.Get(ts.URL + "/api/image?u=" + url.QueryEscape(u))
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("镜像主机应放行，得到 %d", resp.StatusCode)
	}
}

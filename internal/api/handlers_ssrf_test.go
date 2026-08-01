package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/store"
)

// newAuthedServer 起一个带登录态的测试服务端。
func newAuthedServer(t *testing.T) (*httptest.Server, func(path string) (int, string)) {
	t.Helper()
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	u := model.User{ID: "u1", Username: "admin", Password: auth.HashPassword("admin"), IsAdmin: true}
	_ = st.SaveUser(u)
	_ = st.UpsertToken(u.ID, "tok")
	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache"}
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
	return ts, get
}

// TestPlayProxy_BlocksSSRF /api/play/proxy?u=... 会原样发起请求。
// 若不校验目标地址，任何登录用户（默认口令 admin/admin）都能把服务端当跳板
// 去打内网 —— 云环境元数据 169.254.169.254、宿主 127.0.0.1 上的其它服务、
// 内网 10./172.16./192.168. 网段全都暴露。这是典型 SSRF。
func TestPlayProxy_BlocksSSRF(t *testing.T) {
	_, get := newAuthedServer(t)

	// 起一个"内网服务"，代理若成功打到它就说明有 SSRF。
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("INTERNAL-SECRET"))
	}))
	defer secret.Close()

	targets := []string{
		secret.URL,                       // 127.0.0.1 上的本地服务
		"http://169.254.169.254/latest/", // 云元数据
		"http://127.0.0.1:22/",           // 本机端口探测
		"http://[::1]:8080/",             // IPv6 回环
		"http://10.0.0.1/",               // 内网
		"http://192.168.1.1/",            // 内网
		"file:///etc/passwd",             // 非 http 协议
		"http://localhost:9999/",         // localhost 域名
	}
	for _, target := range targets {
		code, body := get("/api/play/proxy?u=" + url.QueryEscape(target))
		if code == http.StatusOK {
			t.Errorf("SSRF 未拦截: u=%s -> 200 %q", target, body)
			continue
		}
		if body == "INTERNAL-SECRET" {
			t.Errorf("SSRF 未拦截且泄露内网响应: u=%s", target)
		}
	}
}

// TestPlayProxy_MissingParam 缺参数仍应是 400。
func TestPlayProxy_MissingParam(t *testing.T) {
	_, get := newAuthedServer(t)
	if code, _ := get("/api/play/proxy"); code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", code)
	}
}

// TestPlayProxy_AllowsConfiguredStorageHost 防护不能误伤正常用法：
// 家用部署里 OpenList 往往就在同一内网（192.168.x.x / docker 网段），
// 直链自然指向内网地址。只要目标主机属于用户已配置的存储源就必须放行。
func TestPlayProxy_AllowsConfiguredStorageHost(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("VIDEO-BYTES"))
	}))
	defer backend.Close()

	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	u := model.User{ID: "u1", Username: "admin", Password: auth.HashPassword("admin"), IsAdmin: true}
	_ = st.SaveUser(u)
	_ = st.UpsertToken(u.ID, "tok")
	// 关键：把这个 127.0.0.1 的地址登记成存储源
	_ = st.SaveStorage(model.Storage{ID: "s1", Type: model.StorageOpenList, BaseURL: backend.URL})

	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache"}
	ts := httptest.NewServer(New(st, cfg).Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/play/proxy?u="+url.QueryEscape(backend.URL+"/file.mp4"), nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(b) != "VIDEO-BYTES" {
		t.Fatalf("已配置存储源的直链被误拦: code=%d body=%q", resp.StatusCode, string(b))
	}
}

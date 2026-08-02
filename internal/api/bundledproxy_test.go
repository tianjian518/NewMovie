package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 反代要正确剥掉 /openlist 前缀，把 /openlist/api/fs/list 转成后端的 /api/fs/list。
func TestBundledProxy_StripsPrefix(t *testing.T) {
	var gotPath, gotPrefix string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotPrefix = r.Header.Get("X-Forwarded-Prefix")
		w.Write([]byte("backend-ok"))
	}))
	defer backend.Close()

	p := NewBundledProxy(backend.URL)
	if p == nil {
		t.Fatal("代理构造失败")
	}
	mux := http.NewServeMux()
	mux.Handle(BundledProxyPrefix, p)
	mux.Handle(BundledProxyPrefix+"/", p)
	front := httptest.NewServer(mux)
	defer front.Close()

	resp, err := http.Get(front.URL + "/openlist/api/fs/list")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if string(b) != "backend-ok" {
		t.Errorf("响应体 = %q", string(b))
	}
	if gotPath != "/api/fs/list" {
		t.Errorf("后端收到路径 = %q，期望 /api/fs/list（前缀应被剥掉）", gotPath)
	}
	if gotPrefix != BundledProxyPrefix {
		t.Errorf("X-Forwarded-Prefix = %q，期望 %q", gotPrefix, BundledProxyPrefix)
	}
}

// /openlist 根路径应重定向到 /openlist/，否则前端相对资源会解析错位导致白屏。
func TestBundledProxy_RedirectsBareRoot(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer backend.Close()

	p := NewBundledProxy(backend.URL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, BundledProxyPrefix, nil)
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("状态码 = %d，期望 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != BundledProxyPrefix+"/" {
		t.Errorf("Location = %q，期望 %q", loc, BundledProxyPrefix+"/")
	}
}

// /openlist/ 要映射到后端根路径 /。
func TestBundledProxy_RootMapsToSlash(t *testing.T) {
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer backend.Close()

	p := NewBundledProxy(backend.URL)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, BundledProxyPrefix+"/", nil))

	if gotPath != "/" {
		t.Errorf("后端收到路径 = %q，期望 /", gotPath)
	}
}

// 后端挂掉时返回 502 和可读中文提示，不能把 Go 的原始错误吐给用户。
func TestBundledProxy_BackendDownReturns502(t *testing.T) {
	p := NewBundledProxy("http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, BundledProxyPrefix+"/api/me", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "内置网盘暂不可用") {
		t.Errorf("响应体 = %q，期望可读中文提示", rec.Body.String())
	}
}

// 非法地址应返回 nil，让调用方跳过挂载而不是 panic。
func TestBundledProxy_InvalidTargetReturnsNil(t *testing.T) {
	for _, bad := range []string{"", "not-a-url", "://missing-scheme", "http://"} {
		if p := NewBundledProxy(bad); p != nil {
			t.Errorf("地址 %q 应返回 nil", bad)
		}
	}
}

// POST 请求体要完整透传（添加网盘驱动时是 POST JSON）。
func TestBundledProxy_ForwardsPostBody(t *testing.T) {
	var gotBody, gotMethod string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"code":200}`))
	}))
	defer backend.Close()

	p := NewBundledProxy(backend.URL)
	mux := http.NewServeMux()
	mux.Handle(BundledProxyPrefix+"/", p)
	front := httptest.NewServer(mux)
	defer front.Close()

	payload := `{"mount_path":"/aliyun","driver":"AliyundriveOpen"}`
	resp, err := http.Post(front.URL+"/openlist/api/admin/storage/create",
		"application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotMethod != http.MethodPost {
		t.Errorf("方法 = %q", gotMethod)
	}
	if gotBody != payload {
		t.Errorf("请求体 = %q，期望 %q", gotBody, payload)
	}
}

// 查询参数要保留（/d/ 直链带 sign）。
func TestBundledProxy_PreservesQuery(t *testing.T) {
	var gotQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
	}))
	defer backend.Close()

	p := NewBundledProxy(backend.URL)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		BundledProxyPrefix+"/d/movies/a.mkv?sign=abc:123", nil))

	if gotQuery != "sign=abc:123" {
		t.Errorf("查询串 = %q，期望 sign=abc:123", gotQuery)
	}
}

package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// deadBase 返回一个必定连不上的地址（监听后立即关闭，端口无人占用）。
func deadBase(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u := srv.URL
	srv.Close() // 端口随即空闲：后续请求必然 connection refused
	return u
}

// TestFallback_OnTransportError 主域名连不上时应自动切到备用域名。
// 对应真实场景：部分网络无法直连 api.themoviedb.org，但 api.tmdb.org 可达。
func TestFallback_OnTransportError(t *testing.T) {
	var hits int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"results":[{"id":83612,"name":"将夜","first_air_date":"2018-10-31","poster_path":"/p.jpg"}]}`))
	}))
	defer good.Close()

	c := &Client{APIKey: "k", BaseURL: deadBase(t), Fallbacks: []string{good.URL}, Lang: "zh-CN"}
	m, err := c.Search(context.Background(), "series", "将夜", 0)
	if err != nil {
		t.Fatalf("应自动降级到备用域名，却报错: %v", err)
	}
	if m == nil || m.TMDBID != 83612 || m.Title != "将夜" {
		t.Fatalf("meta 不正确: %+v", m)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("备用域名命中次数 = %d，期望 1", hits)
	}

	// 第二次请求应直接走已验证可用的根，不再重试死掉的主域名。
	if _, err := c.Search(context.Background(), "series", "将夜", 0); err != nil {
		t.Fatalf("二次搜索失败: %v", err)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("二次命中次数 = %d，期望 2", hits)
	}
	if c.goodBase != good.URL {
		t.Errorf("goodBase = %q，期望记住可用根", c.goodBase)
	}
}

// TestFallback_NotUsedOnHTTPError HTTP 状态码错误（如 Key 无效）是服务端明确答复，
// 不应换域名重试，而要把真实原因暴露给用户。
func TestFallback_NotUsedOnHTTPError(t *testing.T) {
	var fbHits int32
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fbHits, 1)
		_, _ = w.Write([]byte(`{"results":[{"id":1,"title":"x"}]}`))
	}))
	defer fb.Close()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer primary.Close()

	c := &Client{APIKey: "bad", BaseURL: primary.URL, Fallbacks: []string{fb.URL}, Lang: "zh-CN"}
	_, err := c.Search(context.Background(), "movie", "x", 0)
	if err == nil {
		t.Fatal("401 应直接返回错误")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("错误信息应含真实状态码，实际: %v", err)
	}
	if atomic.LoadInt32(&fbHits) != 0 {
		t.Errorf("不应回退到备用域名，实际命中 %d 次", fbHits)
	}
}

// TestByID_DetailEndpointShape 回归：/tv/{id}、/movie/{id} 返回的是单个对象，
// 不是 {"results":[...]}。旧实现复用 search 解析导致 ByID 永远返回 nil，
// 即「NFO 里写了 tmdb id 反而刮不到」。
func TestByID_DetailEndpointShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/tv/83612") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":83612,"name":"将夜","first_air_date":"2018-10-31",
			"overview":"大唐边军宁缺…","vote_average":7.9,"poster_path":"/p.jpg","backdrop_path":"/b.jpg"}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "k", BaseURL: srv.URL, HTTP: srv.Client(), Lang: "zh-CN"}
	m, err := c.ByID(context.Background(), true, 83612)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if m == nil {
		t.Fatal("ByID 返回 nil：详情端点响应未被正确解析")
	}
	if m.TMDBID != 83612 || m.Title != "将夜" || m.Year != 2018 || !m.IsTV {
		t.Errorf("meta 不正确: %+v", m)
	}
	if m.PosterPath != "/p.jpg" || m.Rating != 7.9 {
		t.Errorf("图片/评分未解析: %+v", m)
	}
}

// TestByID_NotFound 详情端点返回 404 时应报错而非静默成功。
func TestByID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := &Client{APIKey: "k", BaseURL: srv.URL, HTTP: srv.Client(), Lang: "zh-CN"}
	if _, err := c.ByID(context.Background(), false, 1); err == nil {
		t.Error("404 应返回错误")
	}
}

// TestNewWithBase 自定义根应作为首选，官方与内置备用仍保留兜底。
func TestNewWithBase(t *testing.T) {
	c := NewWithBase("k", "https://tmdb.example.com/3/")
	if c.BaseURL != "https://tmdb.example.com/3" {
		t.Errorf("BaseURL = %q（应去掉尾部斜杠）", c.BaseURL)
	}
	if len(c.Fallbacks) < 2 || c.Fallbacks[0] != BaseURL {
		t.Errorf("Fallbacks = %v，期望包含官方根与内置备用", c.Fallbacks)
	}
	d := NewWithBase("k", "")
	if d.BaseURL != BaseURL {
		t.Errorf("空 base 应回落官方根，实际 %q", d.BaseURL)
	}
	if len(d.Fallbacks) != len(FallbackBaseURLs) {
		t.Errorf("Fallbacks = %v", d.Fallbacks)
	}
}

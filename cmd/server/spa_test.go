package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 回归：根路径必须直接返回 index.html，而不是 embed 的目录列表。
//
// 历史 bug：`http.FileServer(http.FS(dist))` 的根是 dist 的父目录，
// 打开首页只会看到一个 "dist/" 链接；点进去后 index.html 里引用的
// /assets/xxx.js 又全部 404，React 挂载不上 → 整页空白。
func TestSPA_RootServesIndexNotDirListing(t *testing.T) {
	h := spaHandler(dist)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("根路径应返回 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `<a href="dist/">`) {
		t.Fatal("根路径返回了目录列表 —— fs.Sub 未生效")
	}
	if !strings.Contains(strings.ToLower(body), "<!doctype html") &&
		!strings.Contains(strings.ToLower(body), "<html") {
		t.Fatalf("根路径未返回 HTML 页面，实际内容: %.120s", body)
	}
}

// 回归：/dist/ 这层前缀不应该再存在（旧行为的残留入口）。
func TestSPA_NoDistPrefixExposed(t *testing.T) {
	h := spaHandler(dist)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dist/", nil))

	// 现在 /dist/ 只是一个不存在的前端路由，应回落 index.html（200 HTML），
	// 而绝不能是列目录。
	if strings.Contains(rec.Body.String(), `<a href="dist/">`) {
		t.Fatal("/dist/ 仍暴露目录列表")
	}
}

// 回归：前端路由（react-router）刷新时必须回落 index.html，而不是 404。
func TestSPA_FallbackForClientRoutes(t *testing.T) {
	h := spaHandler(dist)
	for _, p := range []string{"/library/abc", "/settings", "/item/xyz/play"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("前端路由 %s 应回落 index.html(200)，实际 %d", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("前端路由 %s 应返回 HTML，实际 Content-Type=%q", p, ct)
		}
	}
}

// /api/ 路径不能被前端 fallback 吞掉变成 HTML —— 否则接口错误会伪装成 200 页面。
func TestSPA_DoesNotSwallowAPI(t *testing.T) {
	h := spaHandler(dist)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/unknown", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/api/ 未命中应为 404，实际 %d（被前端 fallback 吞了）", rec.Code)
	}
}

// index.html 必须 no-cache，否则前端更新后用户一直拿到旧版本。
func TestSPA_IndexNotCached(t *testing.T) {
	h := spaHandler(dist)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("index.html 应为 no-cache，实际 %q", cc)
	}
}

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 回归：处理器 panic 必须被中间件兜住并返回 500，
// 而不是击穿进程（进程崩溃 + 容器 restart 策略 = 无限重启）。
func TestRecoverMW_CatchesPanic(t *testing.T) {
	h := recoverMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		m["boom"] = "1" // panic: assignment to entry in nil map
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/items/x/poster", nil)

	// 不 panic 逃逸即为通过
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d", rec.Code)
	}
}

// 正常请求不应受中间件影响。
func TestRecoverMW_PassThrough(t *testing.T) {
	h := recoverMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("正常请求被干扰: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

// ErrAbortHandler 是 net/http 约定的静默中止，应继续向上抛给 net/http 处理。
func TestRecoverMW_PropagatesAbortHandler(t *testing.T) {
	h := recoverMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if e := recover(); e != http.ErrAbortHandler {
			t.Fatalf("ErrAbortHandler 应被透传，实际: %v", e)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

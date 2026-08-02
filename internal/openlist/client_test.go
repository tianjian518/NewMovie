package openlist

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// OpenList 偶发返回 HTML 错误页（反向代理 502/504）时，客户端不应把
// `invalid character '<'...` 这种底层报错丢给用户，而要翻译成人话并重试。
func TestList_HTMLResponseIsFriendlyAndRetried(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}))
	defer srv.Close()

	cl := &Client{BaseURL: srv.URL}
	_, err := cl.List("/", false)
	if err == nil {
		t.Fatal("HTML 响应应返回错误")
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Errorf("不应暴露底层 JSON 解析报错，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "OpenList") {
		t.Errorf("应给出人话提示，实际: %v", err)
	}
	if hits < openlistMaxRetries {
		t.Errorf("瞬时错误应重试 %d 次，实际只请求了 %d 次", openlistMaxRetries, hits)
	}
}

// 反向代理偶发 5xx，但重试一次后恢复正常 → List 应成功而非整个扫描失败。
func TestList_RetryOnTransient5xx(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502</html>"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "data": map[string]any{"content": []any{}, "total": 0},
		})
	}))
	defer srv.Close()

	cl := &Client{BaseURL: srv.URL}
	objs, err := cl.List("/", false)
	if err != nil {
		t.Fatalf("重试后应成功，实际: %v（请求 %d 次）", err, hits)
	}
	if len(objs) != 0 {
		t.Errorf("期望空内容，实际 %+v", objs)
	}
}

// 持久错误（路径不存在，OpenList 返回 code=500 + 业务消息）不应重试，立刻失败。
func TestList_PersistentErrorNotRetried(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 500, "message": "object not found"})
	}))
	defer srv.Close()

	cl := &Client{BaseURL: srv.URL}
	if _, err := cl.List("/不存在", false); err == nil {
		t.Fatal("路径不存在应返回错误")
	}
	if hits != 1 {
		t.Errorf("持久错误不应重试，实际请求了 %d 次", hits)
	}
}

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestServeImageBytes_SuffixRange HTTP 规范里 `bytes=-N` 表示「最后 N 字节」，
// 不是「前 N 字节」。原实现把它当成 start=0/end=N 处理，返回的数据是错的。
func TestServeImageBytes_SuffixRange(t *testing.T) {
	b := []byte("0123456789") // 10 字节
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Range", "bytes=-3")
	w := httptest.NewRecorder()
	serveImageBytes(w, req, b, "image/jpeg")
	if w.Code != http.StatusPartialContent {
		t.Fatalf("code = %d, want 206", w.Code)
	}
	if got := w.Body.String(); got != "789" {
		t.Fatalf("body = %q, want %q（bytes=-3 应是最后 3 字节）", got, "789")
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 7-9/10" {
		t.Fatalf("Content-Range = %q, want %q", cr, "bytes 7-9/10")
	}
}

// TestServeImageBytes_OpenEndedRange `bytes=5-` 表示从 5 到结尾。
func TestServeImageBytes_OpenEndedRange(t *testing.T) {
	b := []byte("0123456789")
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Range", "bytes=5-")
	w := httptest.NewRecorder()
	serveImageBytes(w, req, b, "image/jpeg")
	if w.Code != http.StatusPartialContent {
		t.Fatalf("code = %d, want 206", w.Code)
	}
	if got := w.Body.String(); got != "56789" {
		t.Fatalf("body = %q, want %q", got, "56789")
	}
}

// TestServeImageBytes_SuffixLargerThanBody `bytes=-100` 超出总长时应退化为整体。
func TestServeImageBytes_SuffixLargerThanBody(t *testing.T) {
	b := []byte("0123456789")
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Range", "bytes=-100")
	w := httptest.NewRecorder()
	serveImageBytes(w, req, b, "image/jpeg")
	if w.Code != http.StatusPartialContent {
		t.Fatalf("code = %d, want 206", w.Code)
	}
	if got := w.Body.String(); got != "0123456789" {
		t.Fatalf("body = %q, want 全部内容", got)
	}
}

// TestServeImageBytes_UnsatisfiableRange start 超出总长应返回 416。
func TestServeImageBytes_UnsatisfiableRange(t *testing.T) {
	b := []byte("0123456789")
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Range", "bytes=50-60")
	w := httptest.NewRecorder()
	serveImageBytes(w, req, b, "image/jpeg")
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("code = %d, want 416", w.Code)
	}
}

package scanner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"newmovie/internal/model"
	"newmovie/internal/openlist"
	"newmovie/internal/store"
)

// 回归：目录成环（软链/循环挂载）时扫描必须终止，而不是无限下钻把内存耗尽。
// 这正是 ARM 设备上「无限重启」的根因：递归 → OOM Killer → 容器重启 → 再扫描。
func TestScan_DirectoryLoopTerminates(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/fs/list") {
			atomic.AddInt64(&calls, 1)
			// 永远返回一个同名子目录 —— 模拟自引用目录。
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 200,
				"data": map[string]interface{}{
					"content": []map[string]interface{}{
						{"name": "loop", "is_dir": true, "size": 0},
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	st, _ := store.NewJSONStore(t.TempDir() + "/db.json")
	cl := &openlist.Client{BaseURL: srv.URL}
	lib := model.Library{ID: "l1", StorageID: "s1", RootPath: "/", Mode: model.ModeNative}

	done := make(chan error, 1)
	go func() {
		done <- Scan(context.Background(), lib, st, cl, nil, nil, 100000, nil, nil)
	}()

	select {
	case <-done:
		// 必须在深度上限内收敛
		if n := atomic.LoadInt64(&calls); n > MaxScanDepth+2 {
			t.Fatalf("递归未被限制：列目录调用了 %d 次（上限应约为 %d）", n, MaxScanDepth)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("扫描未在 20s 内终止 —— 目录环防护失效")
	}
}

// 回归：自引用条目 "." / ".." 不应导致原地打转。
func TestScan_SelfReferenceEntriesSkipped(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/fs/list") {
			atomic.AddInt64(&calls, 1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 200,
				"data": map[string]interface{}{
					"content": []map[string]interface{}{
						{"name": ".", "is_dir": true, "size": 0},
						{"name": "..", "is_dir": true, "size": 0},
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	st, _ := store.NewJSONStore(t.TempDir() + "/db.json")
	cl := &openlist.Client{BaseURL: srv.URL}
	lib := model.Library{ID: "l1", StorageID: "s1", RootPath: "/media", Mode: model.ModeNative}

	done := make(chan error, 1)
	go func() {
		done <- Scan(context.Background(), lib, st, cl, nil, nil, 100000, nil, nil)
	}()

	select {
	case <-done:
		if n := atomic.LoadInt64(&calls); n != 1 {
			t.Fatalf("期望只列一次目录，实际 %d 次（. 与 .. 未被跳过）", n)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("扫描未终止 —— . / .. 自引用未被跳过")
	}
}

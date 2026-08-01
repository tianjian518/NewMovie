package scanner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newmovie/internal/model"
	"newmovie/internal/openlist"
	"newmovie/internal/store"
)

// mockFS 起一个假 OpenList：只实现 /api/fs/list，按 path 精确匹配返回目录内容。
// 未登记的 path 返回 code=500（真实 OpenList 对不存在路径就是这么回的）。
func mockFS(t *testing.T, tree map[string][]openlist.FsObj) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fs/list" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		content, ok := tree[req.Path]
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 500, "message": "object not found",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"data": map[string]any{"content": content, "total": len(content)},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Bug A：用户手填的路径没有前导斜杠 / 带尾斜杠 / 带空格时，直接透传给 OpenList → 整次扫描失败。
func TestRepro_RootPathNotNormalized(t *testing.T) {
	tree := map[string][]openlist.FsObj{
		"/Video": {{Name: "阿凡达 (2009).mp4", Size: 100}},
	}
	srv := mockFS(t, tree)
	cl := &openlist.Client{BaseURL: srv.URL}

	for _, raw := range []string{"Video", "/Video/", " /Video ", "//Video"} {
		st := newStore(t)
		lib := model.Library{ID: "lib1", Mode: model.ModeNative, StorageID: "st1", RootPath: raw}
		err := Scan(context.Background(), lib, st, cl, nil, nil, 1000, nil, nil)
		items, _ := st.ListMediaItems("lib1")
		if err != nil || len(items) != 1 {
			t.Errorf("root_path=%q 应被规范化后扫到 1 条，实际 err=%v items=%d", raw, err, len(items))
		}
	}
}

// Bug B：某个子目录列举失败（无权限/挂载掉线），整次扫描直接中断，
// 已经能扫到的兄弟目录内容全部丢失。
func TestRepro_OneBadDirAbortsWholeScan(t *testing.T) {
	tree := map[string][]openlist.FsObj{
		"/Video": {
			{Name: "坏目录", IsDir: true},
			{Name: "好目录", IsDir: true},
		},
		// "/Video/坏目录" 故意不登记 → List 报错
		"/Video/好目录": {{Name: "沙丘 (2021).mkv", Size: 100}},
	}
	srv := mockFS(t, tree)
	cl := &openlist.Client{BaseURL: srv.URL}
	st := newStore(t)
	lib := model.Library{ID: "lib1", Mode: model.ModeNative, StorageID: "st1", RootPath: "/Video"}

	_ = Scan(context.Background(), lib, st, cl, nil, nil, 1000, nil, nil)
	items, _ := st.ListMediaItems("lib1")
	if len(items) != 1 {
		t.Errorf("坏目录应被跳过、好目录仍要入库，期望 1 条，实际 %d 条", len(items))
	}
}

// Bug C：目录里全是 .strm，但库建成了 native 模式 → 一条都不入库，
// 且扫描任务报告 done/成功，用户完全看不出问题在哪。
func TestRepro_StrmFilesInNativeMode(t *testing.T) {
	tree := map[string][]openlist.FsObj{
		"/Video": {{Name: "教父 (1972).mkv.strm", Size: 50}},
	}
	srv := mockFS(t, tree)
	cl := &openlist.Client{BaseURL: srv.URL}
	st := newStore(t)
	lib := model.Library{ID: "lib1", Mode: model.ModeNative, StorageID: "st1", RootPath: "/Video"}

	_ = Scan(context.Background(), lib, st, cl, nil, nil, 1000, nil, nil)
	job, err := st.GetLatestScanJob("lib1")
	if err != nil {
		t.Fatal(err)
	}
	items, _ := st.ListMediaItems("lib1")
	if len(items) != 0 {
		t.Fatalf("native 模式不该收 strm，实际入库 %d 条", len(items))
	}
	if job.Skipped != 1 {
		t.Errorf("应统计到 1 个被跳过的 strm，实际 %d", job.Skipped)
	}
	if job.SkipHint == "" || !strings.Contains(job.SkipHint, "STRM 模式") {
		t.Errorf("应提示用户改库模式，实际 skip_hint=%q", job.SkipHint)
	}
	t.Logf("提示语：%s", job.SkipHint)
}

// Bug D：扫描失败时，失败原因没有任何地方记录，前端只能看到 status=failed。
func TestRepro_FailureReasonNotRecorded(t *testing.T) {
	srv := mockFS(t, map[string][]openlist.FsObj{})
	cl := &openlist.Client{BaseURL: srv.URL}
	st := newStore(t)
	lib := model.Library{ID: "lib1", Mode: model.ModeNative, StorageID: "st1", RootPath: "/不存在"}

	err := Scan(context.Background(), lib, st, cl, nil, nil, 1000, nil, nil)
	if err == nil {
		t.Fatal("路径不存在应报错")
	}
	job, _ := st.GetLatestScanJob("lib1")
	t.Logf("Scan 返回 err=%v，但 job 里只有 status=%s", err, job.Status)
	// ScanJob 目前没有 Error 字段，此处用反射式断言表达期望
	if job.Error == "" {
		t.Errorf("扫描失败原因应记录进 ScanJob.Error，供前端展示")
	}
}

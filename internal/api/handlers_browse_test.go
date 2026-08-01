package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/openlist"
	"newmovie/internal/store"
)

// mockFSList 假 OpenList，只实现 /api/fs/list。
func mockFSList(t *testing.T, tree map[string][]openlist.FsObj) *httptest.Server {
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
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 500, "message": "object not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "data": map[string]any{"content": content, "total": len(content)},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// setupBrowse 起一台带存储源的服务，返回带鉴权的 GET/POST 辅助函数。
func setupBrowse(t *testing.T, mock *httptest.Server) (func(string) (int, string), func(string, string) (int, string), store.Store) {
	t.Helper()
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatal(err)
	}
	u := model.User{ID: "u1", Username: "admin", Password: auth.HashPassword("admin"), IsAdmin: true}
	_ = st.SaveUser(u)
	_ = st.UpsertToken(u.ID, "tok")
	_ = st.SaveStorage(model.Storage{ID: "st1", Type: model.StorageOpenList, BaseURL: mock.URL})
	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache"}
	ts := httptest.NewServer(New(st, cfg).Handler())
	t.Cleanup(ts.Close)

	do := func(method, p, body string) (int, string) {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+p, rdr)
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	get := func(p string) (int, string) { return do(http.MethodGet, p, "") }
	post := func(p, body string) (int, string) { return do(http.MethodPost, p, body) }
	return get, post, st
}

type browseResp struct {
	Path   string `json:"path"`
	Parent string `json:"parent"`
	Dirs   []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"dirs"`
	VideoCount  int    `json:"video_count"`
	StrmCount   int    `json:"strm_count"`
	SuggestMode string `json:"suggest_mode"`
}

func TestBrowse_ListsDirsOnly(t *testing.T) {
	mock := mockFSList(t, map[string][]openlist.FsObj{
		"/": {
			{Name: "115_open", IsDir: true},
			{Name: "阿里云盘", IsDir: true},
		},
		"/115_open": {
			{Name: "电影", IsDir: true},
			{Name: "剧集", IsDir: true},
			{Name: "随手记.txt"},
			{Name: "预告片.mp4"},
		},
	})
	get, _, _ := setupBrowse(t, mock)

	code, body := get("/api/storages/st1/browse?path=/115_open")
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d body=%s", code, body)
	}
	var br browseResp
	if err := json.Unmarshal([]byte(body), &br); err != nil {
		t.Fatal(err)
	}
	if len(br.Dirs) != 2 {
		t.Fatalf("应只返回 2 个目录，实际 %+v", br.Dirs)
	}
	got := map[string]string{}
	for _, d := range br.Dirs {
		got[d.Name] = d.Path
	}
	if got["电影"] != "/115_open/电影" || got["剧集"] != "/115_open/剧集" {
		t.Errorf("子目录完整路径不对: %+v", got)
	}
	if br.VideoCount != 1 {
		t.Errorf("应统计到 1 个视频文件，实际 %d", br.VideoCount)
	}
	if br.Parent != "/" {
		t.Errorf("父目录应为 /，实际 %q", br.Parent)
	}
	if br.SuggestMode != "native" {
		t.Errorf("有视频无 strm 应推荐 native，实际 %q", br.SuggestMode)
	}
}

// 根目录必须能列出各挂载点，这是目录树的入口。
func TestBrowse_RootDefaultsToSlash(t *testing.T) {
	mock := mockFSList(t, map[string][]openlist.FsObj{
		"/": {{Name: "115_open", IsDir: true}},
	})
	get, _, _ := setupBrowse(t, mock)
	code, body := get("/api/storages/st1/browse")
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d body=%s", code, body)
	}
	var br browseResp
	_ = json.Unmarshal([]byte(body), &br)
	if br.Path != "/" || len(br.Dirs) != 1 {
		t.Errorf("缺省应列根目录挂载点，实际 %+v", br)
	}
	if br.Parent != "" {
		t.Errorf("根目录不该有父目录，实际 %q", br.Parent)
	}
}

// 用户手填的脏路径（无前导斜杠、尾斜杠、空格）浏览时也要能用。
func TestBrowse_NormalizesDirtyPath(t *testing.T) {
	mock := mockFSList(t, map[string][]openlist.FsObj{
		"/115_open": {{Name: "电影", IsDir: true}},
	})
	get, _, _ := setupBrowse(t, mock)
	for _, raw := range []string{"115_open", "/115_open/", "%20/115_open%20", "//115_open"} {
		code, body := get("/api/storages/st1/browse?path=" + raw)
		if code != http.StatusOK {
			t.Errorf("path=%q 应被规范化后成功，实际 %d %s", raw, code, body)
		}
	}
}

func TestBrowse_StrmDirSuggestsStrmMode(t *testing.T) {
	mock := mockFSList(t, map[string][]openlist.FsObj{
		"/strm": {{Name: "教父.mkv.strm"}, {Name: "教父.zh.srt"}},
	})
	get, _, _ := setupBrowse(t, mock)
	_, body := get("/api/storages/st1/browse?path=/strm")
	var br browseResp
	_ = json.Unmarshal([]byte(body), &br)
	if br.StrmCount != 1 || br.SuggestMode != "strm" {
		t.Errorf("strm 目录应推荐 strm 模式，实际 %+v", br)
	}
}

func TestBrowse_BadPathGivesReadableError(t *testing.T) {
	mock := mockFSList(t, map[string][]openlist.FsObj{"/": {}})
	get, _, _ := setupBrowse(t, mock)
	code, body := get("/api/storages/st1/browse?path=/不存在")
	if code != http.StatusBadGateway {
		t.Fatalf("期望 502，得到 %d", code)
	}
	if !strings.Contains(body, "找不到路径") {
		t.Errorf("错误信息应可读，实际 %s", body)
	}
}

func TestBrowse_UnknownStorage(t *testing.T) {
	mock := mockFSList(t, nil)
	get, _, _ := setupBrowse(t, mock)
	code, _ := get("/api/storages/nope/browse?path=/")
	if code != http.StatusNotFound {
		t.Errorf("未知存储源应 404，得到 %d", code)
	}
}

// 扫描预检：根路径读不到时必须同步返回 400 且带可读原因，
// 而不是先回 200 再让用户对着空白海报墙猜。
func TestStartScan_PreflightRejectsBadRoot(t *testing.T) {
	mock := mockFSList(t, map[string][]openlist.FsObj{"/": {}})
	_, post, st := setupBrowse(t, mock)
	_ = st.SaveLibrary(model.Library{ID: "lib1", Name: "测试库", Mode: model.ModeNative,
		StorageID: "st1", RootPath: "/根本不存在"})

	code, body := post("/api/libraries/lib1/scan", "")
	if code != http.StatusBadRequest {
		t.Fatalf("坏路径应同步返回 400，得到 %d body=%s", code, body)
	}
	if !strings.Contains(body, "找不到路径") {
		t.Errorf("应给出可读原因，实际 %s", body)
	}
	job, err := st.GetLatestScanJob("lib1")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "failed" || job.Error == "" {
		t.Errorf("失败原因应落到 ScanJob.Error，实际 %+v", job)
	}
}

// 建库时路径应被规范化后再落库，避免列表显示与实际扫描路径不一致。
func TestCreateLibrary_NormalizesRootPath(t *testing.T) {
	mock := mockFSList(t, nil)
	_, post, st := setupBrowse(t, mock)
	code, _ := post("/api/libraries", `{"name":"我的电影","mode":"native","storage_id":"st1","root_path":" 115_open/电影/ "}`)
	if code != http.StatusOK {
		t.Fatalf("建库失败 %d", code)
	}
	libs, _ := st.ListLibraries()
	if len(libs) != 1 || libs[0].RootPath != "/115_open/电影" {
		t.Errorf("root_path 未规范化: %+v", libs)
	}
}

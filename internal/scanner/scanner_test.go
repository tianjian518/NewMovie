package scanner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"newmovie/internal/model"
	"newmovie/internal/openlist"
	"newmovie/internal/store"
)

// fakeOpenList 实现一个最小 OpenList 兼容服务：/api/fs/list、/api/fs/get、/raw。
// 用于在不依赖真实 OpenList / 外网的情况下跑通「扫描 → NFO → 同目录图 → 刮削」整链。
// 返回 server 与 nfoReads 计数器，用于验证增量缓存（重复扫描不应再读 NFO）。
func fakeOpenList(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	files := map[string]string{
		"/movies/盗梦空间.2010.1080p.mkv":            "VIDEO",
		"/movies/盗梦空间.2010.1080p.nfo":            `<movie><uniqueid type="tmdb">27205</uniqueid><thumb>https://nfo/p.jpg</thumb></movie>`,
		"/movies/poster.jpg":                         "IMG",
	}
	nfoReads := 0

	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/fs/list":
			var b struct {
				Path string `json:"path"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			prefix := b.Path
			if prefix == "" {
				prefix = "/"
			}
			type obj struct {
				Name     string `json:"name"`
				Size     int64  `json:"size"`
				IsDir    bool   `json:"is_dir"`
				Modified int64  `json:"modified"`
			}
			var content []obj
			for f := range files {
				rest := strings.TrimPrefix(f, prefix)
				if !strings.HasPrefix(rest, "/") {
					continue
				}
				child := strings.TrimPrefix(rest, "/")
				if strings.Contains(child, "/") {
					continue // 非直接子项
				}
				content = append(content, obj{Name: child, Size: 1, IsDir: false, Modified: 1})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 200, "message": "",
				"data": map[string]interface{}{"content": content, "total": len(content)},
			})
		case r.URL.Path == "/api/fs/get":
			var b struct {
				Path string `json:"path"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			raw := base + "/raw?file=" + url.QueryEscape(b.Path)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 200, "message": "",
				"data": map[string]interface{}{"raw_url": raw, "url": "", "sign": ""},
			})
		case r.URL.Path == "/raw":
			f := r.URL.Query().Get("file")
			if strings.HasSuffix(f, ".nfo") {
				nfoReads++
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte(files[f]))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	base = srv.URL
	return srv, &nfoReads
}

func TestScan_NFOAndLocalPoster(t *testing.T) {
	srv, _ := fakeOpenList(t)
	defer srv.Close()

	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	lib := model.Library{ID: "lib1", Name: "电影", Mode: model.ModeNative, StorageID: "st1", RootPath: "/movies"}
	client := &openlist.Client{BaseURL: srv.URL, Token: "t"}

	// 限速调高，测试更快
	err = Scan(context.Background(), lib, st, client, nil, nil, 50, nil, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	items, _ := st.ListMediaItems("lib1")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	it := items[0]
	if it.TMDBID != 27205 {
		t.Errorf("tmdb id = %d, want 27205 (NFO 未被解析)", it.TMDBID)
	}
	// 同目录 poster.jpg 优先于 NFO 远程 thumb
	if it.PosterURL != "/api/items/"+it.ID+"/poster" {
		t.Errorf("poster_url = %q, want 本地代理路径", it.PosterURL)
	}
	if it.PosterPath != "/movies/poster.jpg" {
		t.Errorf("poster_path = %q, want /movies/poster.jpg", it.PosterPath)
	}

	files, _ := st.ListMediaFiles(it.ID)
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	if files[0].Container != "mkv" {
		t.Errorf("container = %q, want mkv", files[0].Container)
	}
}

// 验证增量缓存：第二次扫描不应再读 NFO / 再打 TMDB（已刮削的条目直接跳过）。
func TestScan_IncrementalSkipsRescraped(t *testing.T) {
	srv, nfoReads := fakeOpenList(t)
	defer srv.Close()

	st, err := store.NewJSONStore(t.TempDir() + "/v2.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	lib := model.Library{ID: "lib2", Name: "电影", Mode: model.ModeNative, StorageID: "st1", RootPath: "/movies"}
	client := &openlist.Client{BaseURL: srv.URL, Token: "t"}

	if err := Scan(context.Background(), lib, st, client, nil, nil, 50, nil, nil); err != nil {
		t.Fatalf("scan1: %v", err)
	}
	if *nfoReads != 1 {
		t.Fatalf("scan1 nfo reads = %d, want 1", *nfoReads)
	}
	// 第二次扫描：条目已有海报（缓存命中），不应再读 NFO
	if err := Scan(context.Background(), lib, st, client, nil, nil, 50, nil, nil); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	if *nfoReads != 1 {
		t.Errorf("scan2 不应再读 NFO，reads = %d, want 1（增量缓存失效）", *nfoReads)
	}
}

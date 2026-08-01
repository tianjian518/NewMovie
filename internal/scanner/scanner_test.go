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
// files 为「内部路径 -> 内容」映射；缺失路径的 /raw 返回 404（贴近真实 OpenList）。
func fakeOpenList(t *testing.T, files map[string]string) (*httptest.Server, *int) {
	t.Helper()
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
			seen := map[string]bool{}
			var content []obj
			// 仅考虑「真正位于本目录之下」的文件：严格前缀 prefix+"/"，再取相对路径。
			base := prefix
			if !strings.HasSuffix(base, "/") {
				base += "/"
			}
			for f := range files {
				if !strings.HasPrefix(f, base) {
					continue
				}
				rest := strings.TrimPrefix(f, base)
				if rest == "" {
					continue
				}
				if idx := strings.Index(rest, "/"); idx >= 0 {
					// 含 "/" 说明还有子目录，合成目录项
					dirName := rest[:idx]
					if !seen[dirName] {
						seen[dirName] = true
						content = append(content, obj{Name: dirName, Size: 1, IsDir: true, Modified: 1})
					}
				} else if !seen[rest] {
					seen[rest] = true
					content = append(content, obj{Name: rest, Size: 1, IsDir: false, Modified: 1})
				}
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
			data, ok := files[f]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if strings.HasSuffix(f, ".nfo") {
				nfoReads++
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte(data))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	base = srv.URL
	return srv, &nfoReads
}

func TestScan_NFOAndLocalPoster(t *testing.T) {
	srv, _ := fakeOpenList(t, map[string]string{
		"/movies/盗梦空间.2010.1080p.mkv": "VIDEO",
		"/movies/盗梦空间.2010.1080p.nfo": `<movie><uniqueid type="tmdb">27205</uniqueid><thumb>https://nfo/p.jpg</thumb></movie>`,
		"/movies/poster.jpg":              "IMG",
	})
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
	srv, nfoReads := fakeOpenList(t, map[string]string{
		"/movies/盗梦空间.2010.1080p.mkv": "VIDEO",
		"/movies/盗梦空间.2010.1080p.nfo": `<movie><uniqueid type="tmdb">27205</uniqueid><thumb>https://nfo/p.jpg</thumb></movie>`,
		"/movies/poster.jpg":              "IMG",
	})
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

// 验证 .vidrive.json 手动锁定：同目录 .vidrive.json 锁定的 tmdb_id/title 高于一切。
func TestScan_VidriveJSONLock(t *testing.T) {
	srv, _ := fakeOpenList(t, map[string]string{
		"/movies/某片.mkv": "VIDEO",
		// NFO 故意给错 id，验证手动锁优先
		"/movies/某片.nfo":       `<movie><uniqueid type="tmdb">1</uniqueid><title>错误标题</title></movie>`,
		"/movies/.vidrive.json": `{"tmdb_id":27205,"title":"正确标题","year":2010,"type":"movie"}`,
	})
	defer srv.Close()

	st, _ := store.NewJSONStore(t.TempDir() + "/v3.json")
	lib := model.Library{ID: "lib3", Name: "电影", Mode: model.ModeNative, StorageID: "st1", RootPath: "/movies"}
	client := &openlist.Client{BaseURL: srv.URL, Token: "t"}

	if err := Scan(context.Background(), lib, st, client, nil, nil, 50, nil, nil); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := st.ListMediaItems("lib3")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	it := items[0]
	if it.TMDBID != 27205 {
		t.Errorf("tmdb id = %d, want 27205（.vidrive.json 锁应覆盖 NFO 的 1）", it.TMDBID)
	}
	if it.Title != "正确标题" {
		t.Errorf("title = %q, want 正确标题", it.Title)
	}
	if it.Year != 2010 {
		t.Errorf("year = %d, want 2010", it.Year)
	}
}

// 验证剧集 tvshow.nfo / 系列海报 在父目录（剧集根目录）时的递归查找。
func TestScan_TVShowNFORecursive(t *testing.T) {
	srv, _ := fakeOpenList(t, map[string]string{
		"/tv/Show/tvshow.nfo":                       `<tvshow><uniqueid type="tmdb">1399</uniqueid><title>权力的游戏</title></tvshow>`,
		"/tv/Show/poster.jpg":                       "IMG",
		"/tv/Show/S01/权力的游戏.S01E01.mkv":        "VIDEO",
		"/tv/Show/S01/权力的游戏.S01E01.nfo":        `<episodedetails><title>凛冬的寒风</title></episodedetails>`,
	})
	defer srv.Close()

	st, _ := store.NewJSONStore(t.TempDir() + "/v4.json")
	lib := model.Library{ID: "lib4", Name: "剧集", Mode: model.ModeNative, StorageID: "st1", RootPath: "/tv"}
	client := &openlist.Client{BaseURL: srv.URL, Token: "t"}

	if err := Scan(context.Background(), lib, st, client, nil, nil, 50, nil, nil); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := st.ListMediaItems("lib4")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	it := items[0]
	if it.Kind != model.KindSeries {
		t.Errorf("kind = %q, want series", it.Kind)
	}
	if it.TMDBID != 1399 {
		t.Errorf("tmdb id = %d, want 1399（来自父目录 tvshow.nfo）", it.TMDBID)
	}
	if it.Title != "权力的游戏" {
		t.Errorf("title = %q, want 权力的游戏", it.Title)
	}
	if it.PosterPath != "/tv/Show/poster.jpg" {
		t.Errorf("poster_path = %q, want /tv/Show/poster.jpg（父目录系列海报）", it.PosterPath)
	}
}

// 验证 .cas（139cas 指针文件）按 strm 同类处理并正确解析。
func TestScan_CasStrm(t *testing.T) {
	srv, _ := fakeOpenList(t, map[string]string{
		"/movies/某片.cas": "/movies/某片.mkv", // .cas 内容为内部路径（139cas 风格）
	})
	defer srv.Close()

	st, _ := store.NewJSONStore(t.TempDir() + "/v5.json")
	lib := model.Library{ID: "lib5", Name: "cas", Mode: model.ModeMixed, StorageID: "st1", RootPath: "/movies"}
	client := &openlist.Client{BaseURL: srv.URL, Token: "t"}

	if err := Scan(context.Background(), lib, st, client, nil, nil, 50, nil, nil); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := st.ListMediaItems("lib5")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	files, _ := st.ListMediaFiles(items[0].ID)
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	if files[0].Source != model.SrcStrm {
		t.Errorf("source = %q, want strm（.cas 应视作 strm）", files[0].Source)
	}
	if files[0].Path != "/movies/某片.mkv" {
		t.Errorf("path = %q, want /movies/某片.mkv（.cas 内容解析）", files[0].Path)
	}
}

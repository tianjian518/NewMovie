package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"newmovie/internal/model"
	"newmovie/internal/openlist"
	"newmovie/internal/store"
)

// 本文件针对外部《NewMovie 项目 Bug 测试报告》逐条求证。
// 报告以「逻辑推演」方式撰写，引用的文件路径在本项目均不存在，
// 结论多数不成立。这里用可执行断言把现状钉死，防止日后误改。

// Bug-03 声称：多用户同时播放同一视频，续播记录会互相覆盖。
// 实际：SavePlayRecord 持锁且以 (UserID, FileID) 为复合键，不同用户各自成行。
func TestReport_Bug03_ConcurrentPlayRecordsDoNotOverwrite(t *testing.T) {
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	// 两个用户 × 各写 50 次同一个文件的进度
	for _, uid := range []string{"userA", "userB"} {
		for i := 1; i <= 50; i++ {
			wg.Add(1)
			go func(u string, pos int) {
				defer wg.Done()
				_ = st.SavePlayRecord(model.PlayRecord{
					UserID: u, FileID: "f1", Position: pos, Duration: 7200,
				})
			}(uid, i)
		}
	}
	wg.Wait()

	a, errA := st.GetPlayRecord("userA", "f1")
	b, errB := st.GetPlayRecord("userB", "f1")
	if errA != nil || errB != nil {
		t.Fatalf("两个用户的进度都应存在: errA=%v errB=%v", errA, errB)
	}
	if a.Position == 0 || b.Position == 0 {
		t.Errorf("进度不应为 0: A=%d B=%d", a.Position, b.Position)
	}
	// 关键：两条记录并存，没有互相覆盖
	all, _ := st.ListContinue("userA")
	if len(all) != 1 {
		t.Errorf("userA 应恰好有 1 条续播记录，实际 %d", len(all))
	}
}

// Bug-06 声称：重复点击收藏会插入多条重复记录。
// 实际：SaveFavorite 插入前已按 (UserID, ItemID, Kind) 查重。
func TestReport_Bug06_FavoriteIsIdempotent(t *testing.T) {
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatal(err)
	}
	fav := model.Favorite{UserID: "u1", ItemID: "m1", Kind: model.FavFavorite}
	// 连点 20 次，并发也算上
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = st.SaveFavorite(fav) }()
	}
	wg.Wait()

	list, _ := st.ListFavorites("u1")
	if len(list) != 1 {
		t.Errorf("重复收藏应只留 1 条，实际 %d 条", len(list))
	}
}

// Bug-05 声称：搜索只支持精确匹配，输入「复仇者」找不到「复仇者联盟」。
// 实际：searchItems 对 Title 与 Overview 做大小写无关的子串匹配。
func TestReport_Bug05_SearchSupportsPartialMatch(t *testing.T) {
	mock := mockFSList(t, nil)
	get, _, st := setupBrowse(t, mock)
	_ = st.SaveLibrary(model.Library{ID: "lib1", Name: "库", StorageID: "st1", RootPath: "/"})
	_ = st.UpsertMediaItemByTitle(model.MediaItem{
		ID: "m1", LibraryID: "lib1", Kind: model.KindMovie,
		Title: "复仇者联盟", Overview: "地球最强英雄集结",
	})
	_ = st.UpsertMediaItemByTitle(model.MediaItem{
		ID: "m2", LibraryID: "lib1", Kind: model.KindMovie, Title: "星际穿越",
	})

	for _, tc := range []struct{ q, want string }{
		{"复仇者", "复仇者联盟"}, // 片名前缀
		{"联盟", "复仇者联盟"},  // 片名后缀
		{"英雄集结", "复仇者联盟"}, // 命中简介
	} {
		code, body := get("/api/search?q=" + tc.q)
		if code != http.StatusOK {
			t.Fatalf("q=%s 期望 200，得到 %d", tc.q, code)
		}
		if !strings.Contains(body, tc.want) {
			t.Errorf("搜索 %q 应命中 %q，实际返回: %s", tc.q, tc.want, body)
		}
		if strings.Contains(body, "星际穿越") {
			t.Errorf("搜索 %q 不该命中无关条目", tc.q)
		}
	}
}

// Bug-08 声称：分页参数为负数或 0 时未校验，可能引发异常。
// 实际：本项目没有 page/pageSize，只有 limit，且已守卫 n>0；
// 这里覆盖各种畸形取值，确认既不 panic 也不返回错误数据。
func TestReport_Bug08_LimitParamBoundaries(t *testing.T) {
	mock := mockFSList(t, nil)
	get, _, st := setupBrowse(t, mock)
	_ = st.SaveLibrary(model.Library{ID: "lib1", Name: "库", StorageID: "st1", RootPath: "/"})
	for _, id := range []string{"m1", "m2", "m3"} {
		_ = st.UpsertMediaItemByTitle(model.MediaItem{
			ID: id, LibraryID: "lib1", Kind: model.KindMovie, Title: "片子" + id,
		})
	}
	for _, lim := range []string{"-1", "0", "abc", "999999", "", "9999999999999999999999", "2"} {
		code, body := get("/api/search?limit=" + lim)
		if code != http.StatusOK {
			t.Errorf("limit=%q 应正常返回 200，得到 %d body=%s", lim, code, body)
		}
		if strings.Contains(body, "panic") {
			t.Errorf("limit=%q 触发异常: %s", lim, body)
		}
	}
	// limit=2 必须真的截断
	_, body := get("/api/search?limit=2")
	if n := strings.Count(body, `"id"`); n != 2 {
		t.Errorf("limit=2 应返回 2 条，实际 %d 条: %s", n, body)
	}
}

// Bug-01 声称：代码用 /webdav/ 前缀调 OpenList，应改成 /dav/。
// 实际：本项目根本不用 WebDAV，走的是 REST（/api/fs/list、/api/fs/get）。
// 该测试记录客户端实际命中的路径，锁定只走 REST，顺带防止有人真去改成 WebDAV。
func TestReport_Bug01_UsesRestApiNotWebDAV(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/fs/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 200, "data": map[string]any{"content": []any{}, "total": 0},
			})
		case "/api/fs/get":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 200, "data": map[string]any{"raw_url": "http://x/f.mp4", "url": "http://x/d/f"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cl := &openlist.Client{BaseURL: srv.URL}
	if _, err := cl.List("/", false); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.GetLink("/电影/盗梦空间.mkv", false); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hits) == 0 {
		t.Fatal("没有记录到任何请求")
	}
	for _, p := range hits {
		if strings.Contains(p, "webdav") {
			t.Errorf("不应走 WebDAV，实际请求 %s", p)
		}
		if p != "/api/fs/list" && p != "/api/fs/get" {
			t.Errorf("只应走 REST list/get，实际请求 %s", p)
		}
	}
}

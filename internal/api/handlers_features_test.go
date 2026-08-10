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
	"newmovie/internal/store"
)

// featureFixture 建一个带数据的测试服务端。
func featureFixture(t *testing.T) (store.Store, func(method, path, body string) (int, string)) {
	t.Helper()
	// 选择器回归测试：HLS 是独立的交付层，这里关掉以锁定「决策→remux/transcode URL」的旧断言；
	// HLS 交付路径由 handlers_hls_test.go 单独覆盖。
	t.Setenv("VIDRIVE_HLS", "0")
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	u := model.User{ID: "u1", Username: "admin", Password: auth.HashPassword("admin"), IsAdmin: true}
	_ = st.SaveUser(u)
	_ = st.UpsertToken(u.ID, "tok")
	_ = st.SaveLibrary(model.Library{ID: "lib1", Name: "电影库"})
	_ = st.SaveLibrary(model.Library{ID: "lib2", Name: "剧集库"})
	_ = st.SaveMediaItem(model.MediaItem{ID: "m1", LibraryID: "lib1", Kind: model.KindMovie, Title: "沙丘", Year: 2021, Rating: 8.0, CreatedAt: 100})
	_ = st.SaveMediaItem(model.MediaItem{ID: "m2", LibraryID: "lib1", Kind: model.KindMovie, Title: "阿凡达", Year: 2009, Rating: 7.5, CreatedAt: 300, Overview: "潘多拉星球"})
	_ = st.SaveMediaItem(model.MediaItem{ID: "s1", LibraryID: "lib2", Kind: model.KindSeries, Title: "狂飙", Year: 2023, Rating: 9.0, CreatedAt: 200})

	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache"}
	ts := httptest.NewServer(New(st, cfg).Handler())
	t.Cleanup(ts.Close)

	do := func(method, path, body string) (int, string) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("req %s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	return st, do
}

func TestSearch_QueryKindSort(t *testing.T) {
	_, do := featureFixture(t)

	titles := func(body string) []string {
		var items []model.MediaItem
		if err := json.Unmarshal([]byte(body), &items); err != nil {
			t.Fatalf("解析响应: %v (%s)", err, body)
		}
		out := make([]string, len(items))
		for i, it := range items {
			out[i] = it.Title
		}
		return out
	}

	// 全量，默认按标题排（中文按码点序：沙丘 < 狂飙 < 阿凡达）
	code, body := do(http.MethodGet, "/api/search", "")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if got := titles(body); len(got) != 3 {
		t.Fatalf("全量条目数 = %d, want 3: %v", len(got), got)
	}

	// 关键词命中标题
	_, body = do(http.MethodGet, "/api/search?q=沙丘", "")
	if got := titles(body); len(got) != 1 || got[0] != "沙丘" {
		t.Fatalf("按标题搜索 = %v", got)
	}
	// 关键词命中简介
	_, body = do(http.MethodGet, "/api/search?q=潘多拉", "")
	if got := titles(body); len(got) != 1 || got[0] != "阿凡达" {
		t.Fatalf("按简介搜索 = %v", got)
	}
	// 类型筛选
	_, body = do(http.MethodGet, "/api/search?kind=series", "")
	if got := titles(body); len(got) != 1 || got[0] != "狂飙" {
		t.Fatalf("类型筛选 = %v", got)
	}
	// 库筛选
	_, body = do(http.MethodGet, "/api/search?library=lib1", "")
	if got := titles(body); len(got) != 2 {
		t.Fatalf("库筛选 = %v", got)
	}
	// 按年份倒序
	_, body = do(http.MethodGet, "/api/search?sort=year", "")
	if got := titles(body); got[0] != "狂飙" {
		t.Fatalf("按年份排序首项 = %v, want 狂飙(2023)", got)
	}
	// 按评分倒序
	_, body = do(http.MethodGet, "/api/search?sort=rating", "")
	if got := titles(body); got[0] != "狂飙" {
		t.Fatalf("按评分排序首项 = %v, want 狂飙(9.0)", got)
	}
	// 按最近添加倒序
	_, body = do(http.MethodGet, "/api/search?sort=recent", "")
	if got := titles(body); got[0] != "阿凡达" {
		t.Fatalf("按最近添加首项 = %v, want 阿凡达", got)
	}
}

// TestFavorites_AddListRemove 收藏必须能取消 —— 以前只能加不能删。
func TestFavorites_AddListRemove(t *testing.T) {
	_, do := featureFixture(t)

	if code, body := do(http.MethodPost, "/api/favorites", `{"item_id":"m1","kind":"favorite"}`); code != http.StatusOK {
		t.Fatalf("加收藏 code=%d %s", code, body)
	}
	// 列表要带上聚合后的条目信息，前端才画得出海报
	code, body := do(http.MethodGet, "/api/favorites", "")
	if code != http.StatusOK {
		t.Fatalf("列收藏 code=%d", code)
	}
	var rows []struct {
		ItemID string          `json:"item_id"`
		Item   model.MediaItem `json:"item"`
	}
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if len(rows) != 1 || rows[0].Item.Title != "沙丘" {
		t.Fatalf("收藏列表 = %+v，应聚合出条目标题", rows)
	}

	// 取消收藏
	if code, body := do(http.MethodDelete, "/api/favorites/m1", ""); code != http.StatusOK {
		t.Fatalf("取消收藏 code=%d %s", code, body)
	}
	_, body = do(http.MethodGet, "/api/favorites", "")
	if strings.Contains(body, "m1") {
		t.Fatalf("取消后仍在列表里: %s", body)
	}
	// 再删一次应报 404
	if code, _ := do(http.MethodDelete, "/api/favorites/m1", ""); code != http.StatusNotFound {
		t.Fatalf("重复取消 code = %d, want 404", code)
	}
	// 缺 item_id 应 400
	if code, _ := do(http.MethodPost, "/api/favorites", `{"kind":"favorite"}`); code != http.StatusBadRequest {
		t.Fatalf("缺 item_id code = %d, want 400", code)
	}
}

// TestFavorites_SkipsOrphans 条目随媒体库被删后，收藏页不该留空壳。
func TestFavorites_SkipsOrphans(t *testing.T) {
	st, do := featureFixture(t)
	_, _ = do(http.MethodPost, "/api/favorites", `{"item_id":"m1","kind":"favorite"}`)
	if err := st.DeleteLibrary("lib1"); err != nil {
		t.Fatalf("delete lib: %v", err)
	}
	_, body := do(http.MethodGet, "/api/favorites", "")
	if strings.Contains(body, "m1") {
		t.Fatalf("孤儿收藏未被过滤: %s", body)
	}
}

// TestContinue_EnrichedWithItem 继续观看要能认出是哪部片，
// 老实现只返回 file_id 和秒数，前端只能显示「1203s」。
func TestContinue_EnrichedWithItem(t *testing.T) {
	st, do := featureFixture(t)
	_ = st.SaveMediaFile(model.MediaFile{ID: "f1", ItemID: "s1", Path: "/tv/狂飙/第03集.mkv", SeasonNo: 1, EpisodeNo: 3})
	_ = st.SavePlayRecord(model.PlayRecord{ID: "rec-f1", UserID: "u1", FileID: "f1", Position: 600, Duration: 2700, UpdatedAt: 10})
	// 一条指向已删除文件的脏记录，不能让整个接口翻车
	_ = st.SavePlayRecord(model.PlayRecord{ID: "rec-zz", UserID: "u1", FileID: "gone", Position: 5, Duration: 100, UpdatedAt: 20})

	code, body := do(http.MethodGet, "/api/continue", "")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	var rows []struct {
		FileID    string          `json:"file_id"`
		Position  int             `json:"position"`
		EpisodeNo int             `json:"episode_no"`
		FileName  string          `json:"file_name"`
		Item      model.MediaItem `json:"item"`
	}
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if len(rows) != 1 {
		t.Fatalf("行数 = %d, want 1（脏记录应被跳过）: %s", len(rows), body)
	}
	if rows[0].Item.Title != "狂飙" || rows[0].EpisodeNo != 3 || rows[0].FileName != "第03集.mkv" {
		t.Fatalf("聚合信息不全: %+v", rows[0])
	}
}

// TestItemDetail_ProgressAndFavored 详情接口要一次带回进度和收藏态。
func TestItemDetail_ProgressAndFavored(t *testing.T) {
	st, do := featureFixture(t)
	_ = st.SaveMediaFile(model.MediaFile{ID: "f1", ItemID: "m1", Path: "/movie/沙丘.mkv"})
	_ = st.SavePlayRecord(model.PlayRecord{ID: "rec-f1", UserID: "u1", FileID: "f1", Position: 120, Duration: 9000})
	_, _ = do(http.MethodPost, "/api/favorites", `{"item_id":"m1","kind":"favorite"}`)

	_, body := do(http.MethodGet, "/api/items/m1", "")
	var r struct {
		Favored  bool                      `json:"favored"`
		Progress map[string]map[string]int `json:"progress"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if !r.Favored {
		t.Error("favored 应为 true")
	}
	if r.Progress["f1"]["position"] != 120 {
		t.Errorf("progress = %+v, want f1.position=120", r.Progress)
	}
}

// TestMatchItem_RequiresTMDBKey 未配 Key 时手动匹配应给出明确提示而不是静默失败。
func TestMatchItem_RequiresTMDBKey(t *testing.T) {
	_, do := featureFixture(t)
	code, body := do(http.MethodPost, "/api/items/m1/match", `{"tmdb_id":27205,"kind":"movie"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "TMDB API Key") {
		t.Fatalf("code=%d body=%s，应提示未配置 Key", code, body)
	}
	// 非法 ID
	if code, _ := do(http.MethodPost, "/api/items/m1/match", `{"tmdb_id":0}`); code != http.StatusBadRequest {
		t.Fatalf("非法 tmdb_id code = %d, want 400", code)
	}
	// 条目不存在
	if code, _ := do(http.MethodPost, "/api/items/nope/match", `{"tmdb_id":1}`); code != http.StatusNotFound {
		t.Fatalf("不存在条目 code = %d, want 404", code)
	}
}

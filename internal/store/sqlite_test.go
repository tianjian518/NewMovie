package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"newmovie/internal/model"
)

// 用 SQLite 实现跑一遍核心 CRUD + 去重 + 搜索 + 迁移回归。
// JSON store 的测试保留（兼容旧文件格式），SQLite 需要独立验证。

func newSQLite(t *testing.T) Store {
	t.Helper()
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatalf("sqlite store: %v", err)
	}
	return st
}

func TestSQLite_StorageCRUD(t *testing.T) {
	st := newSQLite(t)
	if err := st.SaveStorage(model.Storage{
		ID: "s1", Name: "内置", Type: model.StorageOpenList,
		BaseURL: "http://127.0.0.1:5244", Token: "tok", RateLimit: 2,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetStorage("s1")
	if err != nil || got.Name != "内置" || got.Type != model.StorageOpenList || got.Token != "tok" {
		t.Fatalf("GetStorage = %+v err=%v", got, err)
	}
	// 尾部斜杠归一化
	got2, err := st.GetStorageByBaseURL("http://127.0.0.1:5244/")
	if err != nil || got2.ID != "s1" {
		t.Fatalf("GetStorageByBaseURL = %+v err=%v", got2, err)
	}
	list, _ := st.ListStorages()
	if len(list) != 1 {
		t.Fatalf("ListStorages len = %d", len(list))
	}
	if err := st.DeleteStorage("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetStorage("s1"); err == nil {
		t.Fatal("删除后应取不到")
	}
}

func TestSQLite_LibraryCascadeDelete(t *testing.T) {
	st := newSQLite(t)
	_ = st.SaveLibrary(model.Library{ID: "lib", Name: "电影", Mode: model.ModeNative, StorageID: "s1"})
	_ = st.SaveMediaItem(model.MediaItem{ID: "it1", LibraryID: "lib", Title: "A", Year: 2020})
	_ = st.SaveMediaFile(model.MediaFile{ID: "f1", ItemID: "it1", StorageID: "s1", Path: "/a.mkv"})
	if err := st.DeleteLibrary("lib"); err != nil {
		t.Fatal(err)
	}
	if items, _ := st.ListMediaItems("lib"); len(items) != 0 {
		t.Fatalf("级联后 items = %d", len(items))
	}
	if _, err := st.GetMediaFile("f1"); err == nil {
		t.Fatal("级联后文件应删除")
	}
}

func TestSQLite_UpsertDedup(t *testing.T) {
	st := newSQLite(t)
	const id, lib = "m-abc", "lib1"
	if err := st.UpsertMediaItemByTitle(model.MediaItem{
		ID: id, LibraryID: lib, Title: "将夜", Year: 2026, Kind: model.KindSeries,
	}); err != nil {
		t.Fatal(err)
	}
	// 刮削回写（标题改为官方名 + 海报）
	if err := st.SaveMediaItem(model.MediaItem{
		ID: id, LibraryID: lib, Title: "将夜 第一季", Year: 2026,
		Kind: model.KindSeries, TMDBID: 282136, PosterURL: "https://img/p.jpg",
	}); err != nil {
		t.Fatal(err)
	}
	// 再次按原标题 upsert，不应新增
	if err := st.UpsertMediaItemByTitle(model.MediaItem{
		ID: id, LibraryID: lib, Title: "将夜", Year: 2026, Kind: model.KindSeries,
	}); err != nil {
		t.Fatal(err)
	}
	items, _ := st.ListMediaItems(lib)
	if len(items) != 1 {
		t.Fatalf("条目数 = %d, want 1", len(items))
	}
	got, _ := st.GetMediaItem(id)
	if got.Title != "将夜 第一季" || got.TMDBID != 282136 || got.PosterURL == "" {
		t.Fatalf("合并后元数据不完整: %+v", got)
	}
}

func TestSQLite_Search(t *testing.T) {
	st := newSQLite(t)
	_ = st.SaveMediaItem(model.MediaItem{ID: "a", LibraryID: "lib", Title: "无间道", Year: 2002, Rating: 8.5, Kind: model.KindMovie})
	_ = st.SaveMediaItem(model.MediaItem{ID: "b", LibraryID: "lib", Title: "无间道风云", Year: 2006, Rating: 9.0, Kind: model.KindMovie})
	_ = st.SaveMediaItem(model.MediaItem{ID: "c", LibraryID: "lib2", Title: "功夫", Year: 2004, Kind: model.KindMovie})

	searcher, ok := st.(interface {
		SearchMediaItems(q, kind, libID, sortBy string, offset, limit int) ([]model.MediaItem, error)
	})
	if !ok {
		t.Fatal("sqliteStore 应实现 SearchMediaItems")
	}
	// 子串匹配
	res, err := searcher.SearchMediaItems("无间道", "", "", "", 0, 0)
	if err != nil || len(res) != 2 {
		t.Fatalf("搜索[无间道] = %d err=%v", len(res), err)
	}
	// 按库过滤
	res, _ = searcher.SearchMediaItems("", "", "lib2", "", 0, 0)
	if len(res) != 1 || res[0].ID != "c" {
		t.Fatalf("按库过滤 = %+v", res)
	}
	// 排序 + limit
	res, _ = searcher.SearchMediaItems("", "", "lib", "rating", 0, 1)
	if len(res) != 1 || res[0].ID != "b" {
		t.Fatalf("排序limit = %+v", res)
	}
	// 无匹配
	res, _ = searcher.SearchMediaItems("不存在", "", "", "", 0, 0)
	if len(res) != 0 {
		t.Fatalf("无匹配应返回空, got %d", len(res))
	}
}

func TestSQLite_PlayRecordAndFav(t *testing.T) {
	st := newSQLite(t)
	_ = st.SavePlayRecord(model.PlayRecord{ID: "r1", UserID: "u1", FileID: "f1", Position: 120, Duration: 600})
	rec, err := st.GetPlayRecord("u1", "f1")
	if err != nil || rec.Position != 120 {
		t.Fatalf("GetPlayRecord = %+v err=%v", rec, err)
	}
	// 覆盖更新
	_ = st.SavePlayRecord(model.PlayRecord{ID: "r1", UserID: "u1", FileID: "f1", Position: 300, Duration: 600})
	rec, _ = st.GetPlayRecord("u1", "f1")
	if rec.Position != 300 {
		t.Fatalf("覆盖后 position = %d", rec.Position)
	}
	// 收藏
	_ = st.SaveFavorite(model.Favorite{ID: "fa", UserID: "u1", ItemID: "i1", Kind: model.FavFavorite})
	_ = st.SaveFavorite(model.Favorite{ID: "fb", UserID: "u1", ItemID: "i1", Kind: model.FavWishlist})
	favs, _ := st.ListFavorites("u1")
	if len(favs) != 2 {
		t.Fatalf("favorites = %d", len(favs))
	}
	// 删除指定 kind
	if err := st.DeleteFavorite("u1", "i1", model.FavWishlist); err != nil {
		t.Fatal(err)
	}
	favs, _ = st.ListFavorites("u1")
	if len(favs) != 1 || favs[0].Kind != model.FavFavorite {
		t.Fatalf("删除后 favorites = %+v", favs)
	}
}

func TestSQLite_UserAndSettings(t *testing.T) {
	st := newSQLite(t)
	_ = st.SaveUser(model.User{ID: "u1", Username: "alice", Password: "h", IsAdmin: true})
	u, err := st.GetUserByName("alice")
	if err != nil || !u.IsAdmin || u.ID != "u1" {
		t.Fatalf("GetUserByName = %+v err=%v", u, err)
	}
	_ = st.UpsertToken("u1", "tok-1")
	u2, err := st.GetUserByToken("tok-1")
	if err != nil || u2.ID != "u1" {
		t.Fatalf("GetUserByToken = %+v err=%v", u2, err)
	}
	_ = st.SaveSetting("tmdb_key", "abc")
	v, err := st.GetSetting("tmdb_key")
	if err != nil || v != "abc" {
		t.Fatalf("GetSetting = %q err=%v", v, err)
	}
	settings, _ := st.ListSettings()
	if settings["tmdb_key"] != "abc" {
		t.Fatalf("ListSettings = %v", settings)
	}
}

// 测试 JSON → SQLite 自动迁移：把 legacy JSON 写到临时目录，打开 SQLite 后应自动导入。
func TestSQLite_MigrateFromJSON(t *testing.T) {
	dir := t.TempDir()
	legacy := legacyJSON{
		Storages: []model.Storage{{ID: "s1", Name: "旧存储", Type: model.StorageOpenList, BaseURL: "http://x:5244"}},
		Libraries: []model.Library{{ID: "lib", Name: "旧库", Mode: model.ModeStrm}},
		Items:     []model.MediaItem{{ID: "it", LibraryID: "lib", Title: "旧片", Year: 2019}},
		Files:     []model.MediaFile{{ID: "f", ItemID: "it", Path: "/old.mkv"}},
		Users:     []model.User{{ID: "u1", Username: "olduser", Password: "h", IsAdmin: false}},
		Settings:  map[string]string{"k": "v"},
	}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dir, "newmovie.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := NewSQLiteStore(filepath.Join(dir, "newmovie.db"))
	if err != nil {
		t.Fatal(err)
	}
	// 验证导入
	if libs, _ := st.ListLibraries(); len(libs) != 1 || libs[0].ID != "lib" {
		t.Fatalf("迁移后 libraries = %+v", libs)
	}
	if items, _ := st.ListMediaItems("lib"); len(items) != 1 || items[0].Title != "旧片" {
		t.Fatalf("迁移后 items = %+v", items)
	}
	if files, _ := st.ListMediaFiles("it"); len(files) != 1 || files[0].Path != "/old.mkv" {
		t.Fatalf("迁移后 files = %+v", files)
	}
	if _, err := st.GetUserByName("olduser"); err != nil {
		t.Fatalf("迁移后用户缺失: %v", err)
	}
	// 二次打开不重复迁移
	st2, err := NewSQLiteStore(filepath.Join(dir, "newmovie.db"))
	if err != nil {
		t.Fatal(err)
	}
	if items, _ := st2.ListMediaItems("lib"); len(items) != 1 {
		t.Fatalf("重复迁移 items = %d", len(items))
	}
	_ = st.Close()
	_ = st2.Close()
}

// 大写入性能冒烟：SQLite 写入 3000 条应亚秒级（替代旧的 O(n²) JSON 全量重写）。
func TestSQLite_LargeWritePerformance(t *testing.T) {
	st := newSQLite(t)
	const n = 3000
	for i := 0; i < n; i++ {
		if e := st.SaveMediaItem(model.MediaItem{
			ID: "it-" + itoa(i), LibraryID: "lib1", Title: "标题" + itoa(i), Year: 2020,
		}); e != nil {
			t.Fatal(e)
		}
	}
	items, err := st.ListMediaItems("lib1")
	if err != nil || len(items) != n {
		t.Fatalf("items = %d err=%v", len(items), err)
	}
	// 逐条查询也应快（走主键索引）
	if _, err := st.GetMediaItem("it-2999"); err != nil {
		t.Fatal(err)
	}
}

// 分页：offset+limit 应不重不漏地覆盖全部条目（字符串序下分页切片正确即可）。
func TestSQLite_SearchPagination(t *testing.T) {
	st := newSQLite(t)
	for i := 0; i < 25; i++ {
		_ = st.SaveMediaItem(model.MediaItem{ID: "p" + itoa(i), LibraryID: "lib", Title: "页" + itoa(i), Year: 2020})
	}
	searcher, ok := st.(interface {
		SearchMediaItems(q, kind, libID, sortBy string, offset, limit int) ([]model.MediaItem, error)
	})
	if !ok {
		t.Fatal("missing searcher")
	}
	seen := map[string]bool{}
	total := 0
	for off := 0; ; off += 10 {
		res, err := searcher.SearchMediaItems("", "", "lib", "title", off, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) == 0 {
			break
		}
		for _, it := range res {
			if seen[it.ID] {
				t.Fatalf("重复条目 %s (off=%d)", it.ID, off)
			}
			seen[it.ID] = true
		}
		total += len(res)
		if len(res) < 10 {
			break
		}
	}
	if total != 25 || len(seen) != 25 {
		t.Fatalf("total=%d seen=%d, want 25", total, len(seen))
	}
	// 超出范围返回空
	page4, _ := searcher.SearchMediaItems("", "", "lib", "title", 30, 10)
	if len(page4) != 0 {
		t.Fatalf("page4 = %d, want 0", len(page4))
	}
}

// TestSQLite_UpsertEmptyID 回归：按标题命中时，若新条目 ID 为空，不得清空已有条目的主键。
// 旧代码无条件 old.ID = m.ID，会把已有条目 ID 抹成空串，后续按 ID 查找全部落空。
func TestSQLite_UpsertEmptyID(t *testing.T) {
	st := newSQLite(t)
	const oldID = "m-old-hash"
	// 先存一条带旧 ID 的条目（模拟旧库按文件名 hash 生成的 ID）
	if err := st.SaveMediaItem(model.MediaItem{
		ID: oldID, LibraryID: "lib", Title: "将夜", Year: 2026, Kind: model.KindSeries,
	}); err != nil {
		t.Fatal(err)
	}
	// 用空 ID + 同标题同年份 upsert（模拟某调用方未生成 ID）
	if err := st.UpsertMediaItemByTitle(model.MediaItem{
		ID: "", LibraryID: "lib", Title: "将夜", Year: 2026, Kind: model.KindSeries,
		Overview: "新简介",
	}); err != nil {
		t.Fatal(err)
	}
	// 旧 ID 必须还在
	got, err := st.GetMediaItem(oldID)
	if err != nil {
		t.Fatalf("旧 ID 应仍可查询: %v", err)
	}
	if got.Overview != "新简介" {
		t.Fatalf("元数据应合并: Overview=%q", got.Overview)
	}
	// 全库只有一条
	items, _ := st.ListMediaItems("lib")
	if len(items) != 1 {
		t.Fatalf("条目数 = %d, want 1", len(items))
	}
	if items[0].ID != oldID {
		t.Fatalf("条目 ID = %q, want %q（不应被空串覆盖）", items[0].ID, oldID)
	}
}

package store

import (
	"testing"

	"newmovie/internal/model"
)

// TestUpsertMediaItemByTitle_NoDupAfterRetitle 真实缺陷回归。
//
// 场景：扫描先以解析出的「将夜」建条目，刮削后标题被 TMDB 改写成官方名；
// 下次扫描同一部剧仍以「将夜」来 upsert，旧实现只按标题匹配 → 匹配不上 →
// append 出第二条同 ID 记录。用户看到的是海报墙上同一部剧两个方块，
// 其中一个永远没有海报。
func TestUpsertMediaItemByTitle_NoDupAfterRetitle(t *testing.T) {
	st, err := NewJSONStore(t.TempDir() + "/u.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const id, lib = "m-abc", "lib1"

	if err := st.UpsertMediaItemByTitle(model.MediaItem{
		ID: id, LibraryID: lib, Title: "将夜", Year: 2026, Kind: model.KindSeries,
	}); err != nil {
		t.Fatalf("首次 upsert: %v", err)
	}

	// 刮削回写：标题变成 TMDB 官方名，并带上海报。
	if err := st.SaveMediaItem(model.MediaItem{
		ID: id, LibraryID: lib, Title: "将夜 第一季", Year: 2026,
		Kind: model.KindSeries, TMDBID: 282136, PosterURL: "https://img/p.jpg",
	}); err != nil {
		t.Fatalf("刮削回写: %v", err)
	}

	// 再次扫描：仍以解析出的原标题 upsert。
	if err := st.UpsertMediaItemByTitle(model.MediaItem{
		ID: id, LibraryID: lib, Title: "将夜", Year: 2026, Kind: model.KindSeries,
	}); err != nil {
		t.Fatalf("二次 upsert: %v", err)
	}

	items, _ := st.ListMediaItems(lib)
	if len(items) != 1 {
		var got []string
		for _, x := range items {
			got = append(got, x.Title)
		}
		t.Fatalf("条目数 = %d，期望 1（同 ID 不应重复）；实际: %v", len(items), got)
	}
	if items[0].PosterURL == "" {
		t.Error("重扫后海报被清空了，应保留刮削结果")
	}
	if items[0].TMDBID != 282136 {
		t.Errorf("tmdb id = %d，应保留 282136", items[0].TMDBID)
	}
}

// TestUpsertMediaItemByTitle_MergesByTitle 无 ID 命中时仍按 标题+年份 合并。
func TestUpsertMediaItemByTitle_MergesByTitle(t *testing.T) {
	st, err := NewJSONStore(t.TempDir() + "/u2.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	_ = st.UpsertMediaItemByTitle(model.MediaItem{ID: "m-1", LibraryID: "lib", Title: "庆余年", Year: 2019})
	_ = st.UpsertMediaItemByTitle(model.MediaItem{ID: "m-2", LibraryID: "lib", Title: "庆余年", Year: 2019, TMDBID: 89901})

	items, _ := st.ListMediaItems("lib")
	if len(items) != 1 {
		t.Fatalf("条目数 = %d，期望 1（同名同年应合并）", len(items))
	}
	if items[0].TMDBID != 89901 {
		t.Errorf("tmdb id = %d，期望合并进 89901", items[0].TMDBID)
	}
}

// TestUpsertMediaItemByTitle_DifferentLibrariesIsolated 不同媒体库互不干扰。
func TestUpsertMediaItemByTitle_DifferentLibrariesIsolated(t *testing.T) {
	st, err := NewJSONStore(t.TempDir() + "/u3.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	_ = st.UpsertMediaItemByTitle(model.MediaItem{ID: "a", LibraryID: "libA", Title: "同名剧", Year: 2020})
	_ = st.UpsertMediaItemByTitle(model.MediaItem{ID: "b", LibraryID: "libB", Title: "同名剧", Year: 2020})

	if a, _ := st.ListMediaItems("libA"); len(a) != 1 {
		t.Errorf("libA 条目数 = %d，期望 1", len(a))
	}
	if b, _ := st.ListMediaItems("libB"); len(b) != 1 {
		t.Errorf("libB 条目数 = %d，期望 1", len(b))
	}
}

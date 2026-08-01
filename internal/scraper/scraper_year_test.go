package scraper

import (
	"context"
	"testing"

	"newmovie/internal/model"
	"newmovie/internal/tmdb"
)

// TestScrape_NFOYearBeatsTMDB 元数据优先级文档写的是 NFO > TMDB，
// 标题确实是 NFO 优先，但年份被 TMDB 无条件覆盖了 —— 前后不一致。
// 后果：TMDB 匹配到同名不同年的作品时，用户 NFO 里写死的年份被冲掉，
// 条目 ID（libID|title|year）随之变化，扫描会产生重复条目。
func TestScrape_NFOYearBeatsTMDB(t *testing.T) {
	st := newStore(t)
	item := model.MediaItem{ID: "y-1", LibraryID: "lib1", Kind: model.KindMovie, Title: "原始"}
	lib := model.Library{ID: "lib1", StorageID: "st1"}
	reader := stubReader{text: `<movie><title>无间道</title><year>2002</year></movie>`}
	searcher := stubSearcher{meta: &tmdb.Meta{Title: "无间道风云", Year: 2006, Overview: "翻拍版"}}

	if err := Scrape(context.Background(), item, lib, st, reader, searcher, "dir/m.nfo", "", "", nil); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	got, _ := st.GetMediaItem("y-1")
	if got.Title != "无间道" {
		t.Errorf("title = %q, want 无间道（NFO 优先）", got.Title)
	}
	if got.Year != 2002 {
		t.Errorf("year = %d, want 2002（NFO 有年份时不应被 TMDB 覆盖）", got.Year)
	}
}

// TestScrape_TMDBYearFillsWhenMissing NFO 没写年份时，TMDB 年份仍要能补上。
func TestScrape_TMDBYearFillsWhenMissing(t *testing.T) {
	st := newStore(t)
	item := model.MediaItem{ID: "y-2", LibraryID: "lib1", Kind: model.KindMovie, Title: "原始"}
	lib := model.Library{ID: "lib1", StorageID: "st1"}
	reader := stubReader{text: `<movie><title>某片</title></movie>`}
	searcher := stubSearcher{meta: &tmdb.Meta{Title: "某片", Year: 2019}}

	if err := Scrape(context.Background(), item, lib, st, reader, searcher, "dir/m.nfo", "", "", nil); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	got, _ := st.GetMediaItem("y-2")
	if got.Year != 2019 {
		t.Errorf("year = %d, want 2019（NFO 缺年份时用 TMDB 补）", got.Year)
	}
}

// TestScrape_ParsedYearKeptOverTMDB 文件名解析出的年份也应优先于 TMDB 搜索结果，
// 因为 item.Year 参与了条目 ID 计算，被改会导致重复条目。
func TestScrape_ParsedYearKeptOverTMDB(t *testing.T) {
	st := newStore(t)
	item := model.MediaItem{ID: "y-3", LibraryID: "lib1", Kind: model.KindMovie, Title: "沙丘", Year: 2021}
	lib := model.Library{ID: "lib1", StorageID: "st1"}
	searcher := stubSearcher{meta: &tmdb.Meta{Title: "沙丘", Year: 1984}}

	if err := Scrape(context.Background(), item, lib, st, nil, searcher, "", "", "", nil); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	got, _ := st.GetMediaItem("y-3")
	if got.Year != 2021 {
		t.Errorf("year = %d, want 2021（已解析出的年份不应被 TMDB 覆盖）", got.Year)
	}
}

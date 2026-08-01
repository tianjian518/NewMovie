package scraper

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"newmovie/internal/model"
	"newmovie/internal/store"
	"newmovie/internal/tmdb"
)

// ---- 桩 ----

type stubReader struct{ text string; err error }

func (s stubReader) ReadText(path string) (string, error) { return s.text, s.err }

type stubSearcher struct{ meta *tmdb.Meta; err error }

func (s stubSearcher) Search(ctx context.Context, kind, q string, y int) (*tmdb.Meta, error) {
	return s.meta, s.err
}
func (s stubSearcher) ByID(ctx context.Context, isTV bool, id int64) (*tmdb.Meta, error) {
	return s.meta, s.err
}

func newStore(t *testing.T) store.Store {
	t.Helper()
	p := filepath.Join(t.TempDir(), "v.json")
	st, err := store.NewJSONStore(p)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return st
}

// ---- NFO 解析 ----

func TestParseNFO_Movie(t *testing.T) {
	nfo := `<?xml version="1.0" encoding="UTF-8"?>
<movie>
  <title>盗梦空间</title>
  <year>2010</year>
  <uniqueid type="tmdb">27205</uniqueid>
  <uniqueid type="imdb">tt1375666</uniqueid>
  <plot>关于梦境与潜意识的科幻故事。</plot>
  <thumb>https://image.tmdb.org/t/p/original/poster.jpg</thumb>
  <fanart><thumb>https://image.tmdb.org/t/p/original/fanart.jpg</thumb></fanart>
</movie>`
	id, year, title, ov, poster, backdrop := parseNFO(nfo)
	if id != 27205 {
		t.Errorf("tmdb id = %d, want 27205", id)
	}
	if year != 2010 {
		t.Errorf("year = %d, want 2010", year)
	}
	if title != "盗梦空间" {
		t.Errorf("title = %q", title)
	}
	if ov == "" {
		t.Errorf("overview empty")
	}
	if poster != "https://image.tmdb.org/t/p/original/poster.jpg" {
		t.Errorf("poster = %q", poster)
	}
	if backdrop != "https://image.tmdb.org/t/p/original/fanart.jpg" {
		t.Errorf("backdrop = %q", backdrop)
	}
}

func TestParseNFO_TV(t *testing.T) {
	nfo := `<tvshow>
  <title>权力的游戏</title>
  <premiered>2011-04-17</premiered>
  <uniqueid type="tmdb">1399</uniqueid>
  <plot>维斯特洛大陆的权谋。</plot>
</tvshow>`
	id, year, title, _, _, _ := parseNFO(nfo)
	if id != 1399 || title != "权力的游戏" || year != 2011 {
		t.Errorf("got id=%d title=%q year=%d", id, title, year)
	}
}

func TestParseNFO_Garbage(t *testing.T) {
	id, year, title, ov, poster, backdrop := parseNFO("not xml at all <<<")
	if id != 0 || title != "" || poster != "" || backdrop != "" || ov != "" {
		t.Errorf("expected empty parse, got %d %q %q %q %q", id, title, poster, backdrop, ov)
	}
	_ = year
}

// ---- 刮削优先级 ----

// 用例：同目录本地图 必须 高于 NFO 远程图 与 TMDB 图。
func TestScrape_LocalImageWins(t *testing.T) {
	st := newStore(t)
	item := model.MediaItem{ID: "m-1", LibraryID: "lib1", Kind: model.KindMovie, Title: "电影"}
	lib := model.Library{ID: "lib1", StorageID: "st1"}
	reader := stubReader{text: `<movie><uniqueid type="tmdb">1</uniqueid><thumb>https://nfo/poster.jpg</thumb></movie>`}
	searcher := stubSearcher{meta: &tmdb.Meta{PosterPath: "/tmdb.jpg", Rating: 8.5}}

	err := Scrape(context.Background(), item, lib, st, reader, searcher, "dir/m.nfo", "dir/poster.jpg", "", nil)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	got, _ := st.GetMediaItem("m-1")
	if got.PosterURL != "/api/items/m-1/poster" {
		t.Errorf("poster_url = %q, want proxied local path", got.PosterURL)
	}
	if got.PosterPath != "dir/poster.jpg" {
		t.Errorf("poster_path = %q", got.PosterPath)
	}
	if got.PosterStorageID != "st1" {
		t.Errorf("poster_storage_id = %q", got.PosterStorageID)
	}
	// TMDB rating 仍应被采用（文本/评分兜底）
	if got.Rating != 8.5 {
		t.Errorf("rating = %v, want 8.5", got.Rating)
	}
}

// 用例：无本地图时，NFO 远程图 高于 TMDB 图。
func TestScrape_NFOImageOverTMDB(t *testing.T) {
	st := newStore(t)
	item := model.MediaItem{ID: "m-2", LibraryID: "lib1", Kind: model.KindMovie, Title: "电影"}
	lib := model.Library{ID: "lib1", StorageID: "st1"}
	reader := stubReader{text: `<movie><uniqueid type="tmdb">2</uniqueid><thumb>https://nfo/p.jpg</thumb></movie>`}
	searcher := stubSearcher{meta: &tmdb.Meta{PosterPath: "/tmdb.jpg"}}

	if err := Scrape(context.Background(), item, lib, st, reader, searcher, "dir/m.nfo", "", "", nil); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	got, _ := st.GetMediaItem("m-2")
	if got.PosterURL != "https://nfo/p.jpg" {
		t.Errorf("poster_url = %q, want NFO remote", got.PosterURL)
	}
}

// 用例：无 NFO、有 searcher → TMDB 兜底填充 poster/title/year/rating。
func TestScrape_TMDBFallback(t *testing.T) {
	st := newStore(t)
	item := model.MediaItem{ID: "m-3", LibraryID: "lib1", Kind: model.KindMovie, Title: "原始标题"}
	lib := model.Library{ID: "lib1", StorageID: "st1"}
	reader := stubReader{err: os.ErrNotExist} // 无 NFO
	searcher := stubSearcher{meta: &tmdb.Meta{
		TMDBID: 99, Title: "官方标题", Year: 2021, Overview: "简介", Rating: 7.0, PosterPath: "/abc.jpg",
	}}

	if err := Scrape(context.Background(), item, lib, st, reader, searcher, "", "", "", nil); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	got, _ := st.GetMediaItem("m-3")
	if got.PosterURL != tmdb.ImageURL("/abc.jpg", "w500") {
		t.Errorf("poster_url = %q", got.PosterURL)
	}
	if got.Title != "官方标题" {
		t.Errorf("title = %q", got.Title)
	}
	if got.Year != 2021 {
		t.Errorf("year = %d", got.Year)
	}
	if got.TMDBID != 99 || got.Rating != 7.0 {
		t.Errorf("tmdb/rating = %d/%v", got.TMDBID, got.Rating)
	}
}

// 用例：无 NFO、无 searcher（未配 TMDB Key）→ 仅保留解析器已有信息，不报错。
func TestScrape_NoTMDBKey(t *testing.T) {
	st := newStore(t)
	item := model.MediaItem{ID: "m-4", LibraryID: "lib1", Kind: model.KindMovie, Title: "原始", Year: 1999}
	lib := model.Library{ID: "lib1", StorageID: "st1"}
	reader := stubReader{err: os.ErrNotExist}

	if err := Scrape(context.Background(), item, lib, st, reader, nil, "", "", "", nil); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	got, _ := st.GetMediaItem("m-4")
	if got.PosterURL != "" {
		t.Errorf("poster_url should be empty, got %q", got.PosterURL)
	}
	if got.Year != 1999 {
		t.Errorf("year = %d", got.Year)
	}
}

// 用例：.vidrive.json 手动锁定（ManualMeta）优先级高于 NFO 与 TMDB。
func TestScrape_ManualLockWins(t *testing.T) {
	st := newStore(t)
	item := model.MediaItem{ID: "m-5", LibraryID: "lib1", Kind: model.KindMovie, Title: "原始"}
	lib := model.Library{ID: "lib1", StorageID: "st1"}
	reader := stubReader{text: `<movie><uniqueid type="tmdb">1</uniqueid><thumb>https://nfo/p.jpg</thumb><title>NFO标题</title></movie>`}
	searcher := stubSearcher{meta: &tmdb.Meta{TMDBID: 999, Title: "TMDB标题", PosterPath: "/tmdb.jpg", Rating: 9.0}}
	manual := &ManualMeta{TMDBID: 27205, Title: "锁定标题", Year: 2010}

	if err := Scrape(context.Background(), item, lib, st, reader, searcher, "dir/m.nfo", "", "", manual); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	got, _ := st.GetMediaItem("m-5")
	if got.TMDBID != 27205 {
		t.Errorf("tmdb id = %d, want 27205 (manual lock 应覆盖 NFO/TMDB)", got.TMDBID)
	}
	if got.Title != "锁定标题" {
		t.Errorf("title = %q, want 锁定标题", got.Title)
	}
	if got.Year != 2010 {
		t.Errorf("year = %d, want 2010", got.Year)
	}
	// 即便锁定了 id，TMDB 仍可按 id 拉到 rating 兜底
	if got.Rating != 9.0 {
		t.Errorf("rating = %v, want 9.0 (TMDB 按锁定 id 补全)", got.Rating)
	}
}

// 用例：ManualMeta 可强制条目类型（movie <-> tv）。
func TestScrape_ManualKindOverride(t *testing.T) {
	st := newStore(t)
	item := model.MediaItem{ID: "m-6", LibraryID: "lib1", Kind: model.KindMovie, Title: "剧集X"}
	lib := model.Library{ID: "lib1", StorageID: "st1"}
	searcher := stubSearcher{meta: &tmdb.Meta{TMDBID: 7, Title: "剧集X官方"}}
	manual := &ManualMeta{Kind: model.KindSeries, TMDBID: 7}

	if err := Scrape(context.Background(), item, lib, st, nil, searcher, "", "", "", manual); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	got, _ := st.GetMediaItem("m-6")
	if got.Kind != model.KindSeries {
		t.Errorf("kind = %q, want series (manual lock 应覆盖)", got.Kind)
	}
}

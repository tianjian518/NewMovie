package scanner

import (
	"context"
	"testing"

	"newmovie/internal/model"
	"newmovie/internal/openlist"
	"newmovie/internal/store"
)

// TestScan_StrmSeriesMergedIntoOneItem 真实结构回归。
//
// 用户网盘里的实际布局（OpenList /strm 驱动，AutoFilm 生成）：
//
//	/strm/国漫/将夜 (2026)/将夜（2026）/第1集.mp4.strm
//	                                  /第2集.mp4.strm
//	                                  ...
//
// 旧实现只解析文件名，结果：13 集 → 13 个条目，标题全是「第N集」，
// TMDB 拿「第15集」去搜还会错配成别的剧。
// 正确行为：合并为 1 个条目、标题「将夜」、13 个分集文件。
func TestScan_StrmSeriesMergedIntoOneItem(t *testing.T) {
	files := map[string]string{}
	for _, n := range []string{"第1集", "第2集", "第3集", "第10集", "第13集"} {
		files["/strm/将夜 (2026)/将夜（2026）/"+n+".mp4.strm"] = "http://ol/d/movie/" + n + ".mp4"
	}
	srv, _ := fakeOpenList(t, files)
	defer srv.Close()

	st, err := store.NewJSONStore(t.TempDir() + "/s.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	lib := model.Library{ID: "libS", Name: "国漫", Mode: model.ModeStrm, StorageID: "st1",
		RootPath: "/strm/将夜 (2026)"}
	client := &openlist.Client{BaseURL: srv.URL, Token: "t"}

	if err := Scan(context.Background(), lib, st, client, nil, nil, 200, nil, nil); err != nil {
		t.Fatalf("scan: %v", err)
	}

	items, _ := st.ListMediaItems("libS")
	if len(items) != 1 {
		var titles []string
		for _, x := range items {
			titles = append(titles, x.Title)
		}
		t.Fatalf("条目数 = %d（期望 1，同一部剧应合并）；标题: %v", len(items), titles)
	}
	it := items[0]
	if it.Title != "将夜" {
		t.Errorf("标题 = %q，期望「将夜」", it.Title)
	}
	if it.Year != 2026 {
		t.Errorf("年份 = %d，期望 2026", it.Year)
	}
	if it.Kind != model.KindSeries {
		t.Errorf("类型 = %q，期望 series", it.Kind)
	}

	mf, _ := st.ListMediaFiles(it.ID)
	if len(mf) != len(files) {
		t.Fatalf("分集文件数 = %d，期望 %d", len(mf), len(files))
	}
	eps := map[int]bool{}
	for _, f := range mf {
		eps[f.EpisodeNo] = true
		if f.SeasonNo != 1 {
			t.Errorf("集 %d 的季号 = %d，期望 1", f.EpisodeNo, f.SeasonNo)
		}
	}
	for _, want := range []int{1, 2, 3, 10, 13} {
		if !eps[want] {
			t.Errorf("缺少第 %d 集（集号解析失败）", want)
		}
	}
}

// TestScan_TwoSeriesNotCollide 两部剧各有「第1集.mp4.strm」，
// 旧实现的文件 ID 只 hash 文件名 → 相互覆盖，后扫的剧会吃掉先扫那部的分集。
func TestScan_TwoSeriesNotCollide(t *testing.T) {
	srv, _ := fakeOpenList(t, map[string]string{
		"/strm/剧集/将夜 (2026)/第1集.mp4.strm":   "http://ol/d/a1.mp4",
		"/strm/剧集/将夜 (2026)/第2集.mp4.strm":   "http://ol/d/a2.mp4",
		"/strm/剧集/庆余年 (2019)/第1集.mp4.strm": "http://ol/d/b1.mp4",
		"/strm/剧集/庆余年 (2019)/第2集.mp4.strm": "http://ol/d/b2.mp4",
	})
	defer srv.Close()

	st, err := store.NewJSONStore(t.TempDir() + "/s2.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	lib := model.Library{ID: "libT", Mode: model.ModeStrm, StorageID: "st1", RootPath: "/strm/剧集"}
	client := &openlist.Client{BaseURL: srv.URL, Token: "t"}
	if err := Scan(context.Background(), lib, st, client, nil, nil, 200, nil, nil); err != nil {
		t.Fatalf("scan: %v", err)
	}

	items, _ := st.ListMediaItems("libT")
	if len(items) != 2 {
		var titles []string
		for _, x := range items {
			titles = append(titles, x.Title)
		}
		t.Fatalf("条目数 = %d，期望 2；标题: %v", len(items), titles)
	}
	for _, it := range items {
		mf, _ := st.ListMediaFiles(it.ID)
		if len(mf) != 2 {
			t.Errorf("《%s》分集数 = %d，期望 2（不同剧的同名文件互相覆盖了）", it.Title, len(mf))
		}
	}
}

// TestScan_SeasonDirGrouping 带季目录的结构应仍归为一个条目，并解析出正确季号。
func TestScan_SeasonDirGrouping(t *testing.T) {
	srv, _ := fakeOpenList(t, map[string]string{
		"/strm/庆余年 (2019)/Season 1/第1集.mp4.strm": "http://ol/d/s1e1.mp4",
		"/strm/庆余年 (2019)/Season 1/第2集.mp4.strm": "http://ol/d/s1e2.mp4",
		"/strm/庆余年 (2019)/Season 2/第1集.mp4.strm": "http://ol/d/s2e1.mp4",
	})
	defer srv.Close()

	st, err := store.NewJSONStore(t.TempDir() + "/s3.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	lib := model.Library{ID: "libSS", Mode: model.ModeStrm, StorageID: "st1", RootPath: "/strm/庆余年 (2019)"}
	client := &openlist.Client{BaseURL: srv.URL, Token: "t"}
	if err := Scan(context.Background(), lib, st, client, nil, nil, 200, nil, nil); err != nil {
		t.Fatalf("scan: %v", err)
	}

	items, _ := st.ListMediaItems("libSS")
	if len(items) != 1 {
		t.Fatalf("条目数 = %d，期望 1（跨季仍是同一部剧）", len(items))
	}
	mf, _ := st.ListMediaFiles(items[0].ID)
	if len(mf) != 3 {
		t.Fatalf("分集数 = %d，期望 3", len(mf))
	}
	seasons := map[int]int{}
	for _, f := range mf {
		seasons[f.SeasonNo]++
	}
	if seasons[1] != 2 || seasons[2] != 1 {
		t.Errorf("季分布 = %v，期望 {1:2, 2:1}", seasons)
	}
}

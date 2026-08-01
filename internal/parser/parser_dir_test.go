package parser

import "testing"

// TestParseInDir_StrmEpisodeOnly 真实场景回归：
// AutoFilm/strm 生成的文件名只有集数（「第15集.mp4.strm」），剧名在父目录上。
// 旧实现只看文件名 → 标题变成「第15集」，海报刮不到且每集各成一条。
func TestParseInDir_StrmEpisodeOnly(t *testing.T) {
	dirs := []string{"将夜（2026）", "将夜 (2026)", "国漫"}
	r := ParseInDir("第15集.mp4.strm", dirs)
	if r.Title != "将夜" {
		t.Errorf("标题 = %q，期望「将夜」（应从父目录取剧名）", r.Title)
	}
	if !r.IsSeries || r.Episode != 15 {
		t.Errorf("应识别为剧集第 15 集，实际 %+v", r)
	}
	if r.Season != 1 {
		t.Errorf("季号 = %d，期望默认 1", r.Season)
	}
	if r.Year != 2026 {
		t.Errorf("年份 = %d，期望 2026（从目录名取）", r.Year)
	}
}

// TestParseInDir_SameSeriesSameTitle 同一部剧的各集必须解析出一致的标题+年份，
// 这是它们能合并成一个媒体库条目的前提。
func TestParseInDir_SameSeriesSameTitle(t *testing.T) {
	dirs := []string{"将夜（2026）", "将夜 (2026)"}
	var first Result
	for i, name := range []string{"第1集.mp4.strm", "第7集.mp4.strm", "第13集.mp4.strm"} {
		r := ParseInDir(name, dirs)
		if i == 0 {
			first = r
			continue
		}
		if r.Title != first.Title || r.Year != first.Year {
			t.Errorf("%s 解析出 (%q,%d)，与首集 (%q,%d) 不一致 → 会拆成多个条目",
				name, r.Title, r.Year, first.Title, first.Year)
		}
	}
}

// TestParseInDir_SkipSeasonDir 季目录不含剧名，应继续向上找。
func TestParseInDir_SkipSeasonDir(t *testing.T) {
	cases := []struct {
		dirs   []string
		season int
	}{
		{[]string{"Season 2", "庆余年 (2019)"}, 2},
		{[]string{"S03", "庆余年 (2019)"}, 3},
		{[]string{"第二季", "庆余年 (2019)"}, 2},
		{[]string{"第 12 季", "庆余年 (2019)"}, 12},
	}
	for _, c := range cases {
		r := ParseInDir("第5集.mkv", c.dirs)
		if r.Title != "庆余年" {
			t.Errorf("dirs=%v 标题 = %q，期望「庆余年」", c.dirs, r.Title)
		}
		if r.Season != c.season {
			t.Errorf("dirs=%v 季号 = %d，期望 %d", c.dirs, r.Season, c.season)
		}
		if r.Episode != 5 {
			t.Errorf("dirs=%v 集号 = %d，期望 5", c.dirs, r.Episode)
		}
	}
}

// TestParseInDir_FileNameWins 文件名自带剧名时不应被目录名覆盖。
func TestParseInDir_FileNameWins(t *testing.T) {
	r := ParseInDir("狂飙.S01E05.1080p.mkv", []string{"乱七八糟的目录名", "电视剧"})
	if r.Title != "狂飙" {
		t.Errorf("标题 = %q，期望「狂飙」（文件名优先）", r.Title)
	}
	if r.Season != 1 || r.Episode != 5 {
		t.Errorf("季集 = S%dE%d，期望 S1E5", r.Season, r.Episode)
	}
}

// TestParseStripsEpisodeMarker 标题里不能残留集数标记，
// 否则拿「庆余年 第12集」去搜 TMDB 必然搜不到 → 没海报。
func TestParseStripsEpisodeMarker(t *testing.T) {
	cases := map[string]string{
		"庆余年 第12集 4K 内封字幕.mp4":          "庆余年",
		"狂飙.S01E05.1080p.WEB-DL.mkv":   "狂飙",
		"三体 EP03 1080p.mp4":            "三体",
		"[01] 城市之光.mp4":                "城市之光",
	}
	for name, want := range cases {
		if got := Parse(name).Title; got != want {
			t.Errorf("Parse(%q).Title = %q，期望 %q", name, got, want)
		}
	}
}

// TestIsEpisodeOnlyTitle 纯集数标题识别。
func TestIsEpisodeOnlyTitle(t *testing.T) {
	yes := []string{"第15集", "第 3 话", "EP12", "E5", "[01]", "07", "S01E02", ""}
	for _, s := range yes {
		if !IsEpisodeOnlyTitle(s) {
			t.Errorf("%q 应判定为纯集数标题", s)
		}
	}
	no := []string{"将夜", "庆余年", "Inception", "三体 第二部"}
	for _, s := range no {
		if IsEpisodeOnlyTitle(s) {
			t.Errorf("%q 不应判定为纯集数标题", s)
		}
	}
}

// TestIsSeasonDir 季目录识别。
func TestIsSeasonDir(t *testing.T) {
	yes := []string{"Season 1", "season 12", "S02", "第一季", "第 3 季", "Specials", "特别篇", "压缩包版（未加密）"}
	for _, s := range yes {
		if !IsSeasonDir(s) {
			t.Errorf("%q 应判定为季目录", s)
		}
	}
	no := []string{"将夜 (2026)", "国漫", "庆余年"}
	for _, s := range no {
		if IsSeasonDir(s) {
			t.Errorf("%q 不应判定为季目录", s)
		}
	}
}

// TestParseMovieInDir 电影结构（剧名目录 + 单文件）不应被误判成剧集。
func TestParseMovieInDir(t *testing.T) {
	r := ParseInDir("流浪地球2.2023.2160p.mkv", []string{"流浪地球2 (2023)", "华语电影"})
	if r.IsSeries {
		t.Errorf("不应识别为剧集: %+v", r)
	}
	if r.Year != 2023 {
		t.Errorf("年份 = %d，期望 2023", r.Year)
	}
}

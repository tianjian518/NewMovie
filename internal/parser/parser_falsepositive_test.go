package parser

import "testing"

// TestParse_NotSeriesFalsePositive 电影名里带数字不应被误判成剧集。
// 典型踩坑：「第9区」被 `第(\d+)` 命中 → Episode=9；「Se7en」被 `E(\d+)` 命中 → Episode=7。
// 一旦误判成剧集，条目会按「剧名+季集」聚合，海报和详情全乱。
func TestParse_NotSeriesFalsePositive(t *testing.T) {
	cases := []string{
		"第9区.2009.1080p.BluRay.mkv",
		"Se7en.1995.2160p.mkv",
		"第一滴血4.mkv",
		"速度与激情7 (2015).mp4",
		"银翼杀手2049.2017.mkv",
		"E.T. 外星人 (1982).mkv",
	}
	for _, name := range cases {
		r := Parse(name)
		if r.IsSeries {
			t.Errorf("%q 被误判为剧集: Season=%d Episode=%d Title=%q", name, r.Season, r.Episode, r.Title)
		}
	}
}

// TestParse_StillDetectsRealSeries 修正误判后，真正的剧集仍要能识别出来。
func TestParse_StillDetectsRealSeries(t *testing.T) {
	cases := []struct {
		name string
		ep   int
	}{
		{"狂飙.S01E05.1080p.mkv", 5},
		{"庆余年 第12集 4K.mp4", 12},
		{"[01] 城市之光.mp4", 1},
		{"三体 EP03.mkv", 3},
		{"漫长的季节 E07.mkv", 7},
		{"某剧 第8话.mp4", 8},
	}
	for _, c := range cases {
		r := Parse(c.name)
		if !r.IsSeries || r.Episode != c.ep {
			t.Errorf("%q 剧集解析失败: IsSeries=%v Episode=%d（想要 %d）", c.name, r.IsSeries, r.Episode, c.ep)
		}
	}
}

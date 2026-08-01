package parser

import "testing"

func TestParseMovie(t *testing.T) {
	r := Parse("再见，李可乐 (2023) - 2160p.mkv")
	if r.Title == "" || r.Year != 2023 {
		t.Fatalf("电影解析失败: %+v", r)
	}
	if r.IsSeries {
		t.Fatalf("不应识别为剧集: %+v", r)
	}
}

func TestParseSeriesSxxExx(t *testing.T) {
	r := Parse("狂飙.S01E05.1080p.WEB-DL.H264.AAC.mkv")
	if !r.IsSeries || r.Season != 1 || r.Episode != 5 {
		t.Fatalf("剧集 SxxExx 解析失败: %+v", r)
	}
	if r.Title == "" || r.Title == "狂飙.S01E05.1080p.WEB-DL.H264.AAC" {
		t.Fatalf("标题清理失败: %q", r.Title)
	}
}

func TestParseChineseEpisode(t *testing.T) {
	r := Parse("庆余年 第12集 4K 内封字幕.mp4")
	if !r.IsSeries || r.Episode != 12 {
		t.Fatalf("中文集数解析失败: %+v", r)
	}
}

func TestParseBracketEpisode(t *testing.T) {
	r := Parse("[01] 城市之光.mp4")
	if !r.IsSeries || r.Episode != 1 {
		t.Fatalf("方括号集数解析失败: %+v", r)
	}
}

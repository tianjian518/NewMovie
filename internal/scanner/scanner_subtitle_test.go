package scanner

import (
	"testing"

	"newmovie/internal/model"
)

func TestDetectSubtitles(t *testing.T) {
	names := []string{"Movie.mkv", "Movie.zh.srt", "Movie.eng.ass", "poster.jpg", "readme.txt"}
	subs := detectSubtitles("/media", "Movie", names, "st1")
	if len(subs) != 2 {
		t.Fatalf("期望 2 条字幕，得到 %d: %+v", len(subs), subs)
	}
	byLang := map[string]model.Subtitle{}
	for _, s := range subs {
		byLang[s.Lang] = s
	}
	zh, ok := byLang["zh"]
	if !ok || zh.Ext != "srt" || zh.Title != "简体中文" {
		t.Errorf("zh 字幕异常: %+v", zh)
	}
	en, ok := byLang["en"]
	if !ok || en.Ext != "ass" || en.Title != "English" {
		t.Errorf("en 字幕异常: %+v", en)
	}
	if subs[0].Path != "/media/Movie.zh.srt" {
		t.Errorf("字幕路径应为同目录: %s", subs[0].Path)
	}
	// 无关文件不应被当作字幕
	if len(detectSubtitles("/media", "Movie", []string{"Movie.mkv", "x.jpg"}, "st1")) != 0 {
		t.Errorf("不应识别到字幕")
	}
}

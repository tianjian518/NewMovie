package subtitle

import (
	"bytes"
	"strings"
	"testing"
)

func TestDetectLang(t *testing.T) {
	cases := []struct {
		name  string
		lang  string
		title string
	}{
		{"Movie.zh.srt", "zh", "简体中文"},
		{"Movie.chi.ass", "zh", "简体中文"},
		{"Movie.简体中文.srt", "zh", "简体中文"},
		{"Movie.eng.srt", "en", "English"},
		{"Movie.jpn.ass", "ja", "日本語"},
		{"Movie.srt", "und", "默认"},
		{"Movie.1080p.zh.srt", "zh", "简体中文"},
	}
	for _, c := range cases {
		lang, title := DetectLang(c.name)
		if lang != c.lang || title != c.title {
			t.Errorf("DetectLang(%q) = (%q,%q), want (%q,%q)", c.name, lang, title, c.lang, c.title)
		}
	}
}

func TestConvertSRT_UTF8(t *testing.T) {
	in := "1\n00:00:01,000 --> 00:00:04,000\n你好，世界\n\n2\n00:00:05,000 --> 00:00:06,000\n第二行\n"
	var out bytes.Buffer
	if err := ConvertSRT(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.HasPrefix(s, "WEBVTT") {
		t.Errorf("缺少 WEBVTT 头: %q", s)
	}
	if strings.Contains(s, ",") {
		t.Errorf("时间戳逗号未替换为点: %q", s)
	}
	if !strings.Contains(s, "你好，世界") {
		t.Errorf("正文丢失: %q", s)
	}
}

func TestIsSubtitleExt(t *testing.T) {
	for _, ok := range []string{"srt", "vtt", "ass", "ssa"} {
		if !IsSubtitleExt(ok) {
			t.Errorf("%s 应识别为字幕", ok)
		}
	}
	for _, no := range []string{"mkv", "jpg", "txt"} {
		if IsSubtitleExt(no) {
			t.Errorf("%s 不应识别为字幕", no)
		}
	}
}

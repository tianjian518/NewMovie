// Package parser 从混乱的中文资源命名中识别 标题/年份/季/集。
// 见 PLAN.md 第七节。NFO 优先于本解析器（在 scanner 中处理）。
package parser

import (
	"regexp"
	"strconv"
	"strings"
)

// Result 解析结果。
type Result struct {
	Title    string
	Year     int
	Season   int // 0 表示电影/未知
	Episode  int // 0 表示电影/未知
	IsSeries bool
}

// 常见噪音词，刮削前清理。
var noise = regexp.MustCompile(`(?i)\[[^\]]*\]|\([^)]*\)|\{[^}]*\}|（[^）]*）|【[^】]*】|WEB-DL|WEBRip|BluRay|BDRip|HDTV|HDR|DV|REMUX|HYSUB|字幕组|国语|英语|中字|双语|官译|特效|内封|外挂|4K|2160p|1080p|720p|480p|H264|H265|HEVC|x264|x265|AVC|10bit|8bit|FLAC|AAC|DTS|TrueHD|Atmos|MP4|MKV|WEB|全集|合集`)

// 年份
var yearRe = regexp.MustCompile(`(19|20)\d{2}`)

// 季集形态
var seasonEpRe = regexp.MustCompile(`(?i)S(\d{1,2})[.\s]*E(\d{1,3})`)
var epRe = regexp.MustCompile(`(?i)(?:EP|第|E|第)(\d{1,3})(?:集|话|EP|E)?`)
var chineseEPRe = regexp.MustCompile(`第\s*(\d{1,3})\s*集`)
var bracketEPRe = regexp.MustCompile(`\[(\d{1,3})\]`)

// Parse 解析文件名（不含扩展名最佳）。
func Parse(name string) Result {
	base := name
	// 去扩展名
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}

	r := Result{}

	// 季集
	if m := seasonEpRe.FindStringSubmatch(base); m != nil {
		r.IsSeries = true
		r.Season, _ = strconv.Atoi(m[1])
		r.Episode, _ = strconv.Atoi(m[2])
	} else if m := chineseEPRe.FindStringSubmatch(base); m != nil {
		r.IsSeries = true
		r.Episode, _ = strconv.Atoi(m[1])
	} else if m := bracketEPRe.FindStringSubmatch(base); m != nil {
		r.IsSeries = true
		r.Episode, _ = strconv.Atoi(m[1])
	} else if m := epRe.FindStringSubmatch(base); m != nil {
		r.IsSeries = true
		r.Episode, _ = strconv.Atoi(m[1])
	}
	if r.Season == 0 && r.IsSeries {
		r.Season = 1
	}

	// 年份
	if m := yearRe.FindString(base); m != "" {
		r.Year, _ = strconv.Atoi(m)
		// 年份太离谱（如 2099）当噪音忽略
		if r.Year < 1900 || r.Year > 2100 {
			r.Year = 0
		}
	}

	// 标题：先去噪音，再砍掉年份/季集残留
	title := noise.ReplaceAllString(base, " ")
	if r.Year > 0 {
		title = strings.ReplaceAll(title, strconv.Itoa(r.Year), " ")
	}
	title = strings.ReplaceAll(title, ".", " ")
	title = strings.ReplaceAll(title, "_", " ")
	fields := strings.Fields(title)
	r.Title = strings.Join(fields, " ")
	r.Title = strings.TrimSpace(r.Title)

	// 标题清理后若为空，回退用原名
	if r.Title == "" {
		r.Title = base
	}
	return r
}

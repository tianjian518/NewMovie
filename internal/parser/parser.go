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
var noise = regexp.MustCompile(`(?i)\[[^\]]*\]|\([^)]*\)|\{[^}]*\}|（[^）]*）|【[^】]*】|WEB-DL|WEBRip|BluRay|BDRip|HDTV|HDR|DV|REMUX|HYSUB|字幕组|字幕|国语|英语|中字|双语|官译|特效|内封|内嵌|外挂|4K|2160p|1080p|720p|480p|H264|H265|HEVC|x264|x265|AVC|10bit|8bit|FLAC|AAC|DTS|TrueHD|Atmos|MP4|MKV|WEB|全集|合集`)

// 年份
var yearRe = regexp.MustCompile(`(19|20)\d{2}`)

// 季集形态
var seasonEpRe = regexp.MustCompile(`(?i)S(\d{1,2})[.\s]*E(\d{1,3})`)

// chineseEPRe 中文集数，**必须**带「集/话/話/期」量词。
// 不能只认 `第(\d+)`：「第9区」「第一滴血4」这类片名会被当成第 9 集/第 4 集，
// 于是电影被塞进剧集聚合逻辑，海报、详情、条目 ID 全部错乱。
var chineseEPRe = regexp.MustCompile(`第\s*(\d{1,3})\s*[集话話期]`)

// epRe 英文集数 EP03 / E07。E 前后都要求 ASCII 词边界，
// 否则 "Se7en" 里的 `e7` 会被识别成第 7 集。
var epRe = regexp.MustCompile(`(?i)\bEP?\s*(\d{1,3})\b`)

var bracketEPRe = regexp.MustCompile(`\[(\d{1,3})\]`)

// 季目录名：Season 1 / S01 / 第一季 / 第 2 季 / 特别篇 等，这类目录不含剧名，
// 推导剧名时要跳过，继续往上一层找。
var seasonDirRe = regexp.MustCompile(`(?i)^(season\s*\d{1,2}|s\d{1,2}|第\s*[0-9一二三四五六七八九十]{1,3}\s*[季部]|specials?|特别篇|番外|正片|全集|合集|压缩包版[^/]*)$`)

// 纯集数标题：把集数标记去掉后什么都不剩，说明文件名里根本没有剧名
// （典型如 AutoFilm/strm 生成的「第15集.mp4.strm」「E05.strm」「[01].strm」）。
var epOnlyRe = regexp.MustCompile(`(?i)^(第\s*\d{1,3}\s*[集话話期]|ep?\s*\d{1,3}|\[\d{1,3}\]|\d{1,3}|s\d{1,2}\s*e\d{1,3})$`)

// IsSeasonDir 判断目录名是否是「季/特辑」这类不含剧名的中间层。
func IsSeasonDir(name string) bool {
	return seasonDirRe.MatchString(strings.TrimSpace(name))
}

// IsEpisodeOnlyTitle 判断标题是否只是个集数标记，不含真正的剧名。
func IsEpisodeOnlyTitle(title string) bool {
	t := strings.TrimSpace(title)
	if t == "" {
		return true
	}
	return epOnlyRe.MatchString(t)
}

// ParseInDir 结合所在目录解析。
//
// 为什么需要它：网盘里剧集的剧名通常在目录上，文件名只有集数
// （`/国漫/将夜 (2026)/将夜（2026）/第15集.mp4.strm`）。只看文件名会得到
// 「第15集」这种伪标题——既刮不到海报，还会让每一集各自变成一个媒体库条目。
//
// dirs 为从库根到文件所在目录的各层目录名（由近及远，即 dirs[0] 是文件的直接父目录）。
// 文件名能自带剧名时以文件名为准，否则逐层向上取第一个「不是季目录」的目录名当剧名。
func ParseInDir(fileName string, dirs []string) Result {
	r := Parse(fileName)
	if !IsEpisodeOnlyTitle(r.Title) {
		return r // 文件名自带剧名，无需借助目录
	}
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d == "" || IsSeasonDir(d) {
			continue
		}
		dr := Parse(d)
		if IsEpisodeOnlyTitle(dr.Title) {
			continue // 目录名本身也只是集数，继续向上
		}
		r.Title = dr.Title
		if r.Year == 0 {
			r.Year = dr.Year
		}
		// 目录层能提供剧名，说明这是「剧名/(季)/集」结构 → 必然是剧集。
		// 但仅当文件名确实解析出了集数时才断定；否则可能是「电影名/电影文件」结构。
		if r.Episode > 0 {
			r.IsSeries = true
			// 文件名没有显式 SxxExx 时，季号以目录为准（Parse 只会保守地补 1）。
			if !seasonEpRe.MatchString(fileName) {
				r.Season = seasonFromDirs(dirs)
			}
		}
		return r
	}
	return r
}

// seasonFromDirs 从目录层里提取季号（Season 2 / S02 / 第二季），找不到按第 1 季。
func seasonFromDirs(dirs []string) int {
	for _, d := range dirs {
		if !IsSeasonDir(d) {
			continue
		}
		if m := regexp.MustCompile(`(?i)(?:season\s*|s)(\d{1,2})`).FindStringSubmatch(d); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n
			}
		}
		if m := regexp.MustCompile(`第\s*([0-9一二三四五六七八九十]{1,3})\s*[季部]`).FindStringSubmatch(d); m != nil {
			if n := cnNum(m[1]); n > 0 {
				return n
			}
		}
	}
	return 1
}

// cnNum 解析「2」「二」「十二」这类季号（只需覆盖个位到二十几）。
func cnNum(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	digits := map[rune]int{'一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	rs := []rune(s)
	if len(rs) == 1 {
		if rs[0] == '十' {
			return 10
		}
		return digits[rs[0]]
	}
	if len(rs) == 2 {
		if rs[0] == '十' { // 十一 ~ 十九
			return 10 + digits[rs[1]]
		}
		if rs[1] == '十' { // 二十
			return digits[rs[0]] * 10
		}
	}
	if len(rs) == 3 && rs[1] == '十' { // 二十一
		return digits[rs[0]]*10 + digits[rs[2]]
	}
	return 0
}

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
	title = strings.TrimSpace(strings.Join(strings.Fields(title), " "))

	// 砍掉季集标记：「庆余年 第12集」应以「庆余年」去搜 TMDB，
	// 带着集数搜必然搜不到或搜岔（这正是海报刮不出来的元凶之一）。
	if r.IsSeries {
		stripped := strings.TrimSpace(strings.Join(strings.Fields(epMarkerRe.ReplaceAllString(title, " ")), " "))
		stripped = strings.Trim(stripped, " -–—·、,，")
		if stripped != "" {
			r.Title = stripped
		} else {
			// 整个文件名就只是集数（如「第15集」）：保留它，
			// 好让上层 ParseInDir 识别出「需要去目录名里找剧名」。
			r.Title = title
		}
	} else {
		r.Title = title
	}

	// 标题清理后若为空，回退用原名
	if r.Title == "" {
		r.Title = base
	}
	return r
}

// epMarkerRe 标题中残留的季集标记（S01E05 / 第12集 / EP03 / E07 / [01]）。
// 与 epRe 保持一致：单字母 E 形态也要能被剥掉，否则「漫长的季节 E07」
// 会带着 E07 去搜 TMDB，必然搜不到。
var epMarkerRe = regexp.MustCompile(`(?i)S\d{1,2}\s*E\d{1,3}|第\s*\d{1,3}\s*[集话話期]|\bEP?\s*\d{1,3}\b|\[\d{1,3}\]`)

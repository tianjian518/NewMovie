// Package scraper 媒体元数据刮削：NFO 优先，TMDB 兜底，同目录本地图最高优先级。
//
// 设计原则（见 PLAN / 用户要求「NFO 同目录逻辑一起做掉」）：
//
//  1. NFO 优先：同目录 .nfo（tinyMediaManager / Emby / AutoFilm / Kodi 格式）给出
//     tmdb id、标题、年份、简介、远程 thumb/fanart 地址。
//  2. 同目录本地图（poster.jpg / fanart.jpg 等）优先级高于一切远程图——
//     用户自己放的图永远最准，且经服务端代理不会因直链过期失效。
//  3. TMDB 兜底/丰富：无 NFO 或 NFO 信息不全时，按标题+年份搜索；
//     有 NFO 的 tmdb id 时直接 ByID 拉详情（更准，补 rating）。
//
// 全部纯标准库，无外部依赖；Searcher 接口可注入桩，便于单测。
package scraper

import (
	"context"
	"encoding/xml"
	"strconv"
	"strings"

	"newmovie/internal/model"
	"newmovie/internal/store"
	"newmovie/internal/tmdb"
)

// NFOReader 读 NFO 文本的能力（*openlist.Client 即满足）。
type NFOReader interface {
	ReadText(path string) (string, error)
}

// Searcher 元数据搜索能力（由 TMDB 客户端实现，便于单测注入桩）。
type Searcher interface {
	Search(ctx context.Context, kind, query string, year int) (*tmdb.Meta, error)
	ByID(ctx context.Context, isTV bool, id int64) (*tmdb.Meta, error)
}

// tmdbSearcher 把 *tmdb.Client 适配为 Searcher。
type tmdbSearcher struct{ c *tmdb.Client }

// NewTMDBSearcher 用 API Key 构造 Searcher；key 为空时调用方应传 nil 跳过 TMDB。
func NewTMDBSearcher(apiKey string) Searcher { return tmdbSearcher{c: tmdb.New(apiKey)} }

func (t tmdbSearcher) Search(ctx context.Context, kind, query string, year int) (*tmdb.Meta, error) {
	return t.c.Search(ctx, kind, query, year)
}
func (t tmdbSearcher) ByID(ctx context.Context, isTV bool, id int64) (*tmdb.Meta, error) {
	return t.c.ByID(ctx, isTV, id)
}

// Scrape 刮削单个 MediaItem 并回写。
//
// 参数：
//   - item      必须已 upsert（含 ID / LibraryID / Kind / Title / Year）
//   - lib       提供存储上下文（本地图归属）
//   - st        持久化（回写）
//   - reader    读 NFO（可为 nil，表示跳过 NFO）
//   - searcher  TMDB 搜索（可为 nil，表示跳过 TMDB）
//   - nfoPath / posterPath / backdropPath  同目录候选文件的 OpenList 内部路径（空表示无）
//
// 图片优先级：同目录本地图 > NFO 远程 thumb/fanart > TMDB。
// 元数据优先级：NFO(tmdb id/标题/年份/简介) > TMDB(简介/rating/year) > 解析器结果(已在 item 中)。
func Scrape(ctx context.Context, item model.MediaItem, lib model.Library, st store.Store,
	reader NFOReader, searcher Searcher, nfoPath, posterPath, backdropPath string) error {

	var (
		tmdbID      int64
		title       string
		year        int
		overview    string
		nfoPoster   string
		nfoBackdrop string
		hasNFO      bool
	)
	if nfoPath != "" && reader != nil {
		if data, err := reader.ReadText(nfoPath); err == nil {
			if t, y, ti, ov, po, ba := parseNFO(data); t != 0 || ti != "" || po != "" || ba != "" {
				tmdbID, year, title, overview, nfoPoster, nfoBackdrop = t, y, ti, ov, po, ba
				hasNFO = true
			}
		}
	}

	// TMDB 兜底 / 丰富
	var tm *tmdb.Meta
	if searcher != nil {
		var err error
		if tmdbID != 0 {
			tm, err = searcher.ByID(ctx, item.Kind == model.KindSeries, tmdbID)
		} else {
			q := title
			if q == "" {
				q = item.Title
			}
			y := year
			if y == 0 {
				y = item.Year
			}
			tm, err = searcher.Search(ctx, string(item.Kind), q, y)
		}
		if err != nil {
			tm = nil
		}
	}

	// ---- 图片合并（优先级：本地同目录 > NFO 远程 > TMDB）----
	posterURL, backdropURL := "", ""
	if posterPath != "" {
		posterURL = "/api/items/" + item.ID + "/poster"
		item.PosterPath = posterPath
		item.PosterStorageID = lib.StorageID
	} else if nfoPoster != "" {
		posterURL = nfoPoster
	} else if tm != nil && tm.PosterPath != "" {
		posterURL = tmdb.ImageURL(tm.PosterPath, "w500")
	}

	if backdropPath != "" {
		backdropURL = "/api/items/" + item.ID + "/backdrop"
		item.BackdropPath = backdropPath
		item.BackdropStorageID = lib.StorageID
	} else if nfoBackdrop != "" {
		backdropURL = nfoBackdrop
	} else if tm != nil && tm.BackdropPath != "" {
		backdropURL = tmdb.ImageURL(tm.BackdropPath, "w1280")
	}

	// ---- 元数据合并 ----
	if title != "" {
		item.Title = title
	} else if tm != nil && tm.Title != "" {
		item.Title = tm.Title
	}
	if year != 0 {
		item.Year = year
	}
	if tm != nil && tm.Year != 0 {
		item.Year = tm.Year
	}
	if overview != "" {
		item.Overview = overview
	} else if tm != nil && tm.Overview != "" {
		item.Overview = tm.Overview
	}
	if tmdbID != 0 {
		item.TMDBID = tmdbID
	} else if tm != nil && tm.TMDBID != 0 {
		item.TMDBID = tm.TMDBID
	}
	if tm != nil && tm.Rating > 0 {
		item.Rating = tm.Rating
	}
	item.PosterURL = posterURL
	item.BackdropURL = backdropURL
	_ = hasNFO
	return st.SaveMediaItem(item)
}

// ---- NFO 解析（纯函数，可单测，无网络）----

// nfoDoc 兼容 movie / tvshow / episodedetails 的常见字段。
// 根元素名不影响解析：title/year/plot/uniqueid/fanart 子元素名一致。
type nfoDoc struct {
	XMLName     xml.Name `xml:"-"`
	Title       string   `xml:"title"`
	Year        int      `xml:"year"`
	Premiered   string   `xml:"premiered"`
	Released    string   `xml:"released"`
	Plot        string   `xml:"plot"`
	Outline     string   `xml:"outline"`
	Thumb       string   `xml:"thumb"` // 顶层 thumb = 海报
	UniqueIDs   []nfoUID `xml:"uniqueid"`
	FanartThumbs []string `xml:"fanart>thumb"` // fanart 内 thumb = 背景
}

type nfoUID struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

// parseNFO 从 NFO 文本解析出 (tmdbID, year, title, overview, posterURL, backdropURL)。
func parseNFO(data string) (tmdbID int64, year int, title, overview, posterURL, backdropURL string) {
	var d nfoDoc
	if err := xml.Unmarshal([]byte(data), &d); err != nil {
		return
	}
	for _, u := range d.UniqueIDs {
		if strings.EqualFold(strings.TrimSpace(u.Type), "tmdb") {
			if id, err := strconv.ParseInt(strings.TrimSpace(u.Value), 10, 64); err == nil && id > 0 {
				tmdbID = id
			}
		}
	}
	title = strings.TrimSpace(d.Title)
	overview = strings.TrimSpace(firstNonEmpty(d.Plot, d.Outline))
	posterURL = strings.TrimSpace(d.Thumb)
	if len(d.FanartThumbs) > 0 {
		backdropURL = strings.TrimSpace(d.FanartThumbs[0])
	}
	if d.Year == 0 {
		if y := yearOfNFO(d.Premiered); y != 0 {
			d.Year = y
		} else if y := yearOfNFO(d.Released); y != 0 {
			d.Year = y
		}
	}
	year = d.Year
	return
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func yearOfNFO(s string) int {
	if len(s) >= 4 {
		if y, err := strconv.Atoi(s[:4]); err == nil && y >= 1900 && y <= 2100 {
			return y
		}
	}
	return 0
}

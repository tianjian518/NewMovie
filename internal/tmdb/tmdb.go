// Package tmdb 极简 TMDB（The Movie Database）客户端。
//
// 纯标准库实现（net/http），无外部 SDK 依赖，满足 Vidrive 零依赖约束。
// 默认中文结果（language=zh-CN）。HTTP 客户端可注入，便于单测用 httptest 桩。
//
// 刮削策略见 PLAN 与 scraper 包：TMDB 作为「NFO 优先」之后的兜底/丰富来源。
package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// BaseURL TMDB API v3 根。
const BaseURL = "https://api.themoviedb.org/3"

// Client TMDB 客户端。
type Client struct {
	APIKey  string
	BaseURL string
	Lang    string
	HTTP    *http.Client
}

// New 构造客户端，APIKey 为空时后续请求会失败（调用方据此跳过 TMDB）。
func New(apiKey string) *Client {
	return &Client{APIKey: apiKey, BaseURL: BaseURL, Lang: "zh-CN"}
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Meta TMDB 返回的精简元数据。
type Meta struct {
	TMDBID       int64
	Title        string
	Year         int
	Overview     string
	Rating       float64
	PosterPath   string // 形如 /xxx.jpg（需拼 image.tmdb.org）
	BackdropPath string
	IsTV         bool
}

// searchResult 与 TMDB search 响应对齐（只取需要的字段）。
type searchResult struct {
	Results []resultItem `json:"results"`
}

// resultItem 单条搜索/详情结果。
type resultItem struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Title        string `json:"title"`
	MediaType    string `json:"media_type"`
	ReleaseDate  string `json:"release_date"`
	FirstAirDate string `json:"first_air_date"`
	Overview     string `json:"overview"`
	VoteAverage  float64 `json:"vote_average"`
	PosterPath   string `json:"poster_path"`
	BackdropPath string `json:"backdrop_path"`
}

func (c *Client) get(ctx context.Context, endpoint string, extra url.Values) (*searchResult, error) {
	q := url.Values{}
	q.Set("api_key", c.APIKey)
	q.Set("language", c.Lang)
	for k, v := range extra {
		q[k] = v
	}
	u := c.BaseURL + "/" + endpoint + "?" + q.Encode()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb %s: http %d", endpoint, resp.StatusCode)
	}
	var r searchResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Search 按标题+年份搜索。kind 取 model.KindMovie/KindSeries 时走精确端点，
// 其它（如空串）走 /search/multi 兼容电影与剧集。
func (c *Client) Search(ctx context.Context, kind, query string, year int) (*Meta, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("tmdb: 缺少 API Key")
	}
	extra := url.Values{}
	extra.Set("query", query)
	if year > 0 {
		extra.Set("year", strconv.Itoa(year))
		extra.Set("first_air_date_year", strconv.Itoa(year))
	}
	var endpoint string
	switch kind {
	case "movie":
		endpoint = "search/movie"
	case "series":
		endpoint = "search/tv"
	default:
		endpoint = "search/multi"
	}
	r, err := c.get(ctx, endpoint, extra)
	if err != nil {
		return nil, err
	}
	if len(r.Results) == 0 {
		return nil, nil
	}
	m := toMeta(r.Results[0])
	return &m, nil
}

// ByID 已知 tmdb id 时直接拉详情，结果最准（NFO 给出 tmdb id 时走这条）。
func (c *Client) ByID(ctx context.Context, isTV bool, id int64) (*Meta, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("tmdb: 缺少 API Key")
	}
	endpoint := "movie/" + strconv.FormatInt(id, 10)
	if isTV {
		endpoint = "tv/" + strconv.FormatInt(id, 10)
	}
	r, err := c.get(ctx, endpoint, url.Values{})
	if err != nil {
		return nil, err
	}
	if len(r.Results) == 0 {
		return nil, nil
	}
	m := toMeta(r.Results[0])
	return &m, nil
}

func toMeta(x resultItem) Meta {
	m := Meta{
		TMDBID:       x.ID,
		Title:        strings.TrimSpace(firstNonEmpty(x.Title, x.Name)),
		Overview:     x.Overview,
		Rating:       x.VoteAverage,
		PosterPath:   x.PosterPath,
		BackdropPath: x.BackdropPath,
		IsTV:         x.MediaType == "tv",
	}
	if x.ReleaseDate != "" {
		m.Year = yearOf(x.ReleaseDate)
	}
	if x.FirstAirDate != "" {
		m.Year = yearOf(x.FirstAirDate)
		m.IsTV = true
	}
	return m
}

// ImageURL 拼接 TMDB 图片完整地址。size 形如 "w500"/"w1280"/"original"。
func ImageURL(path, size string) string {
	if path == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/" + size + path
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func yearOf(s string) int {
	if len(s) >= 4 {
		if y, err := strconv.Atoi(s[:4]); err == nil {
			return y
		}
	}
	return 0
}

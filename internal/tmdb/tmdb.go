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
	"sync"
	"time"
)

// BaseURL TMDB API v3 根。
const BaseURL = "https://api.themoviedb.org/3"

// FallbackBaseURLs 备用 API 根。
//
// 背景：不少网络环境（含中国大陆常见家宽 / NAS 环境）无法直连
// api.themoviedb.org（表现为 TCP 连接超时，而非 HTTP 错误码），
// 但 api.tmdb.org 指向同一套 TMDB v3 API 且通常可达。
// 主域名发生「传输层错误」时自动切到备用域名，用户零配置即可刮削成功；
// 若返回的是 HTTP 状态码错误（如 401 Key 无效），则不再重试，直接暴露原因。
var FallbackBaseURLs = []string{"https://api.tmdb.org/3"}

// Client TMDB 客户端。
type Client struct {
	APIKey    string
	BaseURL   string
	Fallbacks []string
	Lang      string
	HTTP      *http.Client

	mu       sync.Mutex
	goodBase string // 已验证可用的根，避免每次请求都重试不通的主域名
}

// New 构造客户端，APIKey 为空时后续请求会失败（调用方据此跳过 TMDB）。
func New(apiKey string) *Client {
	return NewWithBase(apiKey, "")
}

// NewWithBase 允许指定自定义 API 根（用户自建反代 / 镜像站）。
// base 为空时用官方根，并保留内置备用域名自动兜底。
func NewWithBase(apiKey, base string) *Client {
	c := &Client{APIKey: apiKey, Lang: "zh-CN"}
	if b := strings.TrimRight(strings.TrimSpace(base), "/"); b != "" {
		// 用户显式指定则以其为主，官方域名与内置备用作为兜底。
		c.BaseURL = b
		c.Fallbacks = append([]string{BaseURL}, FallbackBaseURLs...)
	} else {
		c.BaseURL = BaseURL
		c.Fallbacks = append([]string(nil), FallbackBaseURLs...)
	}
	return c
}

// defaultHTTP 带超时，避免主域名不可达时每次请求都挂满 TCP 默认超时（可达 2 分钟），
// 拖垮整库扫描。10s 足够，超时即触发备用域名。
var defaultHTTP = &http.Client{Timeout: 10 * time.Second}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

// bases 返回本次请求要依次尝试的 API 根：已验证可用的排最前。
func (c *Client) bases() []string {
	c.mu.Lock()
	good := c.goodBase
	c.mu.Unlock()
	out := make([]string, 0, len(c.Fallbacks)+2)
	if good != "" {
		out = append(out, good)
	}
	if c.BaseURL != good {
		out = append(out, c.BaseURL)
	}
	for _, f := range c.Fallbacks {
		if f != good {
			out = append(out, f)
		}
	}
	return out
}

func (c *Client) markGood(base string) {
	c.mu.Lock()
	c.goodBase = base
	c.mu.Unlock()
}

// ActiveBase 返回最近一次请求成功所用的 API 根；尚未成功过则返回首选根。
func (c *Client) ActiveBase() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.goodBase != "" {
		return c.goodBase
	}
	return c.BaseURL
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
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Title         string `json:"title"`
	OriginalName  string `json:"original_name"`  // 中文剧集常只在原名里与查询一致
	OriginalTitle string `json:"original_title"`
	MediaType     string `json:"media_type"`
	ReleaseDate   string `json:"release_date"`
	FirstAirDate  string `json:"first_air_date"`
	Overview     string `json:"overview"`
	VoteAverage  float64 `json:"vote_average"`
	PosterPath   string `json:"poster_path"`
	BackdropPath string `json:"backdrop_path"`
}

// getRaw 发起请求并返回原始响应体；主域名传输层失败时自动降级到备用域名。
func (c *Client) getRaw(ctx context.Context, endpoint string, extra url.Values) ([]byte, error) {
	q := url.Values{}
	q.Set("api_key", c.APIKey)
	q.Set("language", c.Lang)
	for k, v := range extra {
		q[k] = v
	}
	qs := q.Encode()

	var lastErr error
	for _, base := range c.bases() {
		req, err := http.NewRequest(http.MethodGet, base+"/"+endpoint+"?"+qs, nil)
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		resp, err := c.http().Do(req)
		if err != nil {
			// 传输层错误（DNS/连接超时/被重置）→ 换下一个根重试
			lastErr = fmt.Errorf("tmdb %s (%s): %w", endpoint, base, err)
			if ctx.Err() != nil {
				return nil, lastErr // 调用方主动取消，不再重试
			}
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			// HTTP 层错误（401 Key 无效 / 404 不存在）是服务端明确答复，
			// 换域名也一样，直接返回让用户看到真实原因。
			return nil, fmt.Errorf("tmdb %s: http %d", endpoint, resp.StatusCode)
		}
		c.markGood(base)
		return b, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("tmdb %s: 无可用 API 地址", endpoint)
	}
	return nil, lastErr
}

func (c *Client) get(ctx context.Context, endpoint string, extra url.Values) (*searchResult, error) {
	b, err := c.getRaw(ctx, endpoint, extra)
	if err != nil {
		return nil, err
	}
	var r searchResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// getItem 解析「详情端点」响应。/movie/{id}、/tv/{id} 返回的是单个对象，
// 而非 {"results":[...]}，因此不能复用 get（否则永远解析出 0 条结果）。
func (c *Client) getItem(ctx context.Context, endpoint string, extra url.Values) (*resultItem, error) {
	b, err := c.getRaw(ctx, endpoint, extra)
	if err != nil {
		return nil, err
	}
	var it resultItem
	if err := json.Unmarshal(b, &it); err != nil {
		return nil, err
	}
	if it.ID == 0 {
		return nil, nil
	}
	return &it, nil
}

// Search 按标题+年份搜索。kind 取 model.KindMovie/KindSeries 时走精确端点，
// 其它（如空串）走 /search/multi 兼容电影与剧集。
func (c *Client) Search(ctx context.Context, kind, query string, year int) (*Meta, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("tmdb: 缺少 API Key")
	}
	extra := url.Values{}
	extra.Set("query", query)
	var endpoint string
	switch kind {
	case "movie":
		endpoint = "search/movie"
		if year > 0 {
			extra.Set("year", strconv.Itoa(year))
		}
	case "series":
		endpoint = "search/tv"
		// search/tv 不认 year，只认 first_air_date_year；传错参数会被忽略，也可能导致排名劣化。
		if year > 0 {
			extra.Set("first_air_date_year", strconv.Itoa(year))
		}
	default:
		endpoint = "search/multi"
		if year > 0 {
			extra.Set("year", strconv.Itoa(year))
		}
	}
	r, err := c.get(ctx, endpoint, extra)
	if err != nil {
		return nil, err
	}
	// 带年份搜不到时退一步：只按标题再搜一次。
	// TMDB 的 year 过滤对国产剧常年不准（首播年 / 上线年 / 目录里写的年份三者不一致），
	// 直接返回空会让整部剧刮不到海报。
	if len(r.Results) == 0 && year > 0 {
		extra.Del("year")
		extra.Del("first_air_date_year")
		if r2, err2 := c.get(ctx, endpoint, extra); err2 == nil {
			r = r2
		}
	}
	if len(r.Results) == 0 {
		return nil, nil
	}
	best := pickBest(r.Results, query, year)
	if best < 0 {
		return nil, nil
	}
	m := toMeta(r.Results[best])
	return &m, nil
}

// pickBest 从搜索结果里挑最匹配的一条，返回下标；没有可信结果时返回 -1。
//
// 为什么不能直接取 results[0]：TMDB 的相关性排序对中文标题相当不可靠。
// 真实案例——搜「将夜」(2026) 返回的第一条是《昨夜将至》（只因为共享「夜」「将」字），
// 真正的《将夜》排在第二。盲取首条就会给用户配上完全不相干的海报。
// 这里按「完全相同 > 前缀/包含 > 年份吻合」打分，且要求达到最低可信度。
func pickBest(rs []resultItem, query string, year int) int {
	q := normTitle(query)
	if q == "" {
		return 0
	}
	bestIdx, bestScore := -1, 0
	for i, x := range rs {
		score := 0
		for _, name := range []string{x.Title, x.Name, x.OriginalTitle, x.OriginalName} {
			n := normTitle(name)
			if n == "" {
				continue
			}
			switch {
			case n == q:
				score = maxInt(score, 100)
			case strings.HasPrefix(n, q) || strings.HasPrefix(q, n):
				score = maxInt(score, 60)
			case strings.Contains(n, q) || strings.Contains(q, n):
				score = maxInt(score, 40)
			}
		}
		if score == 0 {
			continue // 标题毫无关系，宁可不刮也不要配错
		}
		if year > 0 {
			y := yearOf(firstNonEmpty(x.ReleaseDate, x.FirstAirDate))
			switch {
			case y == year:
				score += 25
			case y > 0 && abs(y-year) <= 1: // 跨年上线常见，容忍 1 年
				score += 10
			}
		}
		score -= i // 同分时保持 TMDB 原有相关性顺序
		if score > bestScore {
			bestIdx, bestScore = i, score
		}
	}
	return bestIdx
}

// normTitle 归一化标题用于比较：转小写、去空白与常见标点，
// 让「将夜（2026）」「将夜 2026」「将夜」能对上。
func normTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '.', '-', '_', '·', ':', '：', '!', '！', '?', '？', ',', '，', '\'', '"', '’':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
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
	it, err := c.getItem(ctx, endpoint, url.Values{})
	if err != nil || it == nil {
		return nil, err
	}
	m := toMeta(*it)
	m.IsTV = isTV // 详情端点无 media_type 字段，用调用方已知的类型
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

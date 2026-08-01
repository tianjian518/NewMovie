// Package openlist 封装 OpenList/AList V3 API：列目录、取直链、算签名、列已挂载网盘。
// 纯标准库实现，无需外部依赖。
//
// 关键设计（见 PLAN.md 第二节/第五节）：
//   - 默认 refresh=false 复用 OpenList 自身缓存，几乎零风控风险
//   - 拿到 token 后自己算 sign，不必让用户在后台关闭签名（安全性不降级）
//   - /api/fs/get 返回的 raw_url 是网盘 CDN 真实直链，可绕开 OpenList 中转
package openlist

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Client OpenList 客户端。
type Client struct {
	BaseURL string // 例: http://openlist:5244
	Token   string // API token（Authorization: Bearer）
	SignKey string // 后台 "签名所有" 密钥，默认为空
	HTTP    *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// ---- 请求/响应结构 ----

type fsListReq struct {
	Path     string `json:"path"`
	Password string `json:"password,omitempty"`
	Refresh  bool   `json:"refresh"`
}

type FsObj struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	IsDir    bool   `json:"is_dir"`
	Modified int64  `json:"modified"` // 秒级时间戳
	Thumb    string `json:"thumb"`
	Type     string `json:"type"`
	Sign     string `json:"sign"`
}

type fsListResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Content []FsObj `json:"content"`
		Total   int     `json:"total"`
	} `json:"data"`
}

type fsGetReq struct {
	Path     string `json:"path"`
	Password string `json:"password,omitempty"`
	Refresh  bool   `json:"refresh"`
}

type fsGetResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		RawURL  string            `json:"raw_url"`  // 网盘真实直链
		URL     string            `json:"url"`      // /d/ 302 链接
		Sign    string            `json:"sign"`
		Headers map[string]string `json:"headers"`
	} `json:"data"`
}

// ---- API 方法 ----

// List 列出某路径下的内容。refresh=false 复用 OpenList 缓存（风控友好）。
func (c *Client) List(path string, refresh bool) ([]FsObj, error) {
	body, _ := json.Marshal(fsListReq{Path: path, Refresh: refresh})
	resp, err := c.do("/api/fs/list", body)
	if err != nil {
		return nil, err
	}
	var r fsListResp
	if err := json.Unmarshal(resp, &r); err != nil {
		return nil, err
	}
	if r.Code != 200 {
		return nil, fmt.Errorf("openlist list %q: code=%d %s", path, r.Code, r.Message)
	}
	return r.Data.Content, nil
}

// GetLink 取某文件的直链信息（raw_url + /d/ url + 所需 headers）。
func (c *Client) GetLink(path string, refresh bool) (*fsGetResp, error) {
	body, _ := json.Marshal(fsGetReq{Path: path, Refresh: refresh})
	resp, err := c.do("/api/fs/get", body)
	if err != nil {
		return nil, err
	}
	var r fsGetResp
	if err := json.Unmarshal(resp, &r); err != nil {
		return nil, err
	}
	if r.Code != 200 {
		return nil, fmt.Errorf("openlist get %q: code=%d %s", path, r.Code, r.Message)
	}
	return &r, nil
}

// ListDrives 列出已挂载的网盘（根目录即各挂载点）。
func (c *Client) ListDrives() ([]FsObj, error) {
	return c.List("/", false)
}

// RawURL 取某内部路径的真实直链（raw_url），失败则退回带签名的 /d/ 中转链接。
// 用于经服务端代理读取 NFO / 同目录图片等小文件。
func (c *Client) RawURL(p string) (string, error) {
	link, err := c.GetLink(p, false)
	if err == nil && link.Data.RawURL != "" {
		return link.Data.RawURL, nil
	}
	return c.SignedDURL(p), nil
}

// ReadText 通过 raw_url 读取一个小文本文件（如 .nfo），上限 1MB，避免网盘大文件撑爆内存。
func (c *Client) ReadText(p string) (string, error) {
	u, err := c.RawURL(p)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openlist read %q: http %d", p, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// do 通用带鉴权请求。
func (c *Client) do(api string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(c.BaseURL, "/")+api, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", c.Token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ---- 签名 ----

// Sign 计算 OpenList/AList 的 sign 参数。
//
// 算法（社区广泛记录的 AList 实现）：
//   sign = md5( ts + path + key )，其中 ts = floor(now/60)（分钟），key 为后台 "签名所有" 密钥。
// 返回形如 "hash:ts" —— 注意 OpenList 实际把 ts 拼在 sign 后，用 ?sign=hash:ts 传递。
//
// 重要：若你的 OpenList 版本 401，对照官方实现微调此函数即可（版本间算法可能微调）。
func Sign(path, key string) string {
	ts := time.Now().Unix() / 60
	raw := fmt.Sprintf("%d%s%s", ts, path, key)
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:]) + ":" + fmt.Sprint(ts)
}

// SignedDURL 生成带签名的 /d/ 直链（302 中转路径）。
func (c *Client) SignedDURL(internalPath string) string {
	u := strings.TrimRight(c.BaseURL, "/") + "/d/" + strings.TrimLeft(internalPath, "/")
	if c.SignKey != "" {
		u += "?sign=" + Sign(internalPath, c.SignKey)
	}
	return u
}

// ---- 工具 ----

// IsOpenListD 判断一个 URL 是否指向某 OpenList 实例的 /d/ 接口。
// 返回 (是否, 该实例 baseURL, 内部路径)。
//
// 注意：真实 OpenList strm 输出的 URL 常含未编码的空白/中文（默认 Site URL 形态），
// 故这里用手写解析而非 url.Parse，避免空格/非 ASCII 直接让解析失败。
func IsOpenListD(rawURL, candidateBase string) (bool, string, string) {
	rawURL = strings.TrimSpace(rawURL)
	idx := strings.Index(rawURL, "://")
	if idx < 0 {
		return false, "", ""
	}
	scheme := rawURL[:idx]
	if scheme != "http" && scheme != "https" {
		return false, "", ""
	}
	rest := rawURL[idx+3:]
	hidx := strings.Index(rest, "/")
	if hidx < 0 {
		return false, "", ""
	}
	host := rest[:hidx]
	path := rest[hidx:]
	if q := strings.Index(path, "?"); q >= 0 { // 忽略 ?sign= 等查询（我们会重算）
		path = path[:q]
	}
	if !strings.HasPrefix(path, "/d/") {
		return false, "", ""
	}
	// 候选 base 形如 http://openlist:5244[/...]
	cidx := strings.Index(candidateBase, "://")
	if cidx < 0 {
		return false, "", ""
	}
	chost := candidateBase[cidx+3:]
	if ch := strings.Index(chost, "/"); ch >= 0 {
		chost = chost[:ch]
	}
	if host != chost {
		return false, "", ""
	}
	internal := "/" + strings.TrimPrefix(path, "/d/")
	internal, _ = url.PathUnescape(internal) // 解码 Encode Path
	return true, candidateBase, internal
}

// SortByName 辅助：按名称排序。
func SortByName(objs []FsObj) {
	sort.Slice(objs, func(i, j int) bool { return objs[i].Name < objs[j].Name })
}

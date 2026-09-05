// Package webdav 实现 WebDAV 存储驱动（通用网盘/云盘/NAS 的 WebDAV 接口）。
//
// 满足 openlist.FSClient 接口，使 Vidrive 的扫描 / 取链 / 读 NFO 逻辑
// 在 WebDAV 存储上原样可用（model.Storage.Type == "webdav"）。
//
// 协议说明：
//   - 列目录：PROPFIND Depth:1，解析 <D:response>/<D:href> 与 <D:getcontentlength> 等；
//   - 直链：WebDAV 没有 OpenList 的「签名 /d/ 中转」，GET 本身即取流，
//     所以 SignedDURL/RawURL 都返回服务端 URL（可带 Basic Auth 的授权直链）。
//   - 鉴权：支持 HTTP Basic（用户名/密码）；Token 字段兼容存放 basic:user:pass。
package webdav

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"newmovie/internal/openlist"
)

// Client WebDAV 客户端。
type Client struct {
	BaseURL string // 例: https://dav.example.com/dav
	User    string
	Pass    string
	HTTP    *http.Client
}

// New 从存储配置构造 WebDAV 客户端。
// Token 字段若为 "basic:user:pass" 形式则拆出账号密码（前端「测试连接」时方便）。
func New(baseURL, token string) *Client {
	c := &Client{BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/")}
	if strings.HasPrefix(token, "basic:") {
		rest := strings.TrimPrefix(token, "basic:")
		if i := strings.Index(rest, ":"); i >= 0 {
			c.User, c.Pass = rest[:i], rest[i+1:]
		} else {
			c.User = rest
		}
	}
	return c
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// abs 把 WebDAV 内部路径拼成完整 URL。
func (c *Client) abs(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return c.BaseURL + "/"
	}
	return c.BaseURL + "/" + p
}

// auth 返回 Basic 鉴权请求头（有账号才加）。
func (c *Client) auth() string {
	if c.User == "" {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.User+":"+c.Pass))
}

// ---- PROPFIND 解析 ----

type propfindMultiStatus struct {
	XMLName xml.Name    `xml:"DAV: multistatus"`
	Responses []propfindResponse `xml:"response"`
}

type propfindResponse struct {
	Href  string `xml:"href"`
	Props struct {
		ResourceType struct {
			Collection *struct{} `xml:"collection"`
		} `xml:"resourcetype"`
		ContentLength string `xml:"getcontentlength"`
		LastModified  string `xml:"getlastmodified"`
	} `xml:"propstat>prop"`
}

// List 用 PROPFIND Depth:1 列出目录内容，归一化为 openlist.FsObj。
func (c *Client) List(p string, refresh bool) ([]openlist.FsObj, error) {
	u := c.abs(p)
	req, err := http.NewRequest("PROPFIND", u, bytes.NewReader([]byte(`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:getcontentlength/><d:getlastmodified/></d:prop></d:propfind>`)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	if a := c.auth(); a != "" {
		req.Header.Set("Authorization", a)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("webdav 连接失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("webdav PROPFIND %q: http %d", p, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var ms propfindMultiStatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return nil, fmt.Errorf("webdav 响应解析失败: %v", err)
	}
	// href 可能是「完整 URL」或「纯路径」两种形态；取它的路径部分再与 BaseURL 的路径前缀比对。
	basePathPrefix := ""
	if u, err := url.Parse(c.BaseURL); err == nil {
		basePathPrefix = strings.TrimRight(u.Path, "/")
	}
	out := []openlist.FsObj{}
	for _, r := range ms.Responses {
		href, err := url.PathUnescape(r.Href)
		if err != nil {
			href = r.Href
		}
		// 归一化 href 为纯路径
		hrefPath := href
		if u, err := url.Parse(href); err == nil {
			hrefPath = u.Path
		}
		rel := strings.TrimPrefix(hrefPath, basePathPrefix)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue // 目录自身（根条目）
		}
		name := strings.TrimSuffix(rel, "/")
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if name == "" {
			continue
		}
		obj := openlist.FsObj{Name: name}
		obj.IsDir = r.Props.ResourceType.Collection != nil
		if n, err := strconv.ParseInt(strings.TrimSpace(r.Props.ContentLength), 10, 64); err == nil {
			obj.Size = openlist.FlexInt64(n)
		}
		if t, err := time.Parse(time.RFC1123, strings.TrimSpace(r.Props.LastModified)); err == nil {
			obj.Modified = openlist.FlexInt64(t.Unix())
		}
		out = append(out, obj)
	}
	return out, nil
}

// GetLink 返回 WebDAV 直链（raw_url 即服务端 URL）。
// 关键：WebDAV 通常需要 Basic Auth，浏览器直接访问直链会 401。
// 把 Authorization header 放进响应，播放时经 /api/play/proxy 反代带上鉴权，
// 或前端用 fetch + headers 取流。这是 WebDAV 存储能页内播放的前提。
func (c *Client) GetLink(p string, refresh bool) (*openlist.FsGetResp, error) {
	u := c.abs(p)
	headers := map[string]string{}
	if a := c.auth(); a != "" {
		headers["Authorization"] = a
	}
	return &openlist.FsGetResp{
		Data: struct {
			RawURL  string            `json:"raw_url"`
			URL     string            `json:"url"`
			Sign    string            `json:"sign"`
			Headers map[string]string `json:"headers"`
		}{RawURL: u, URL: u, Headers: headers},
	}, nil
}

// RawURL 返回直链；WebDAV 无 /d/ 兜底，直接返回。
func (c *Client) RawURL(p string) (string, error) {
	return c.abs(p), nil
}

// SignedDURL 对 WebDAV 而言即直链本身。
func (c *Client) SignedDURL(p string) string {
	return c.abs(p)
}

// ListDrives 列出根目录（即 BaseURL 根集合）。
func (c *Client) ListDrives() ([]openlist.FsObj, error) {
	return c.List("/", false)
}

// ReadText 通过直链读取小文本文件。
func (c *Client) ReadText(p string) (string, error) {
	u := c.abs(p)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	if a := c.auth(); a != "" {
		req.Header.Set("Authorization", a)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("webdav read %q: http %d", p, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Head 探测路径是否存在（PROPFIND 0-depth，比 GET 便宜）。
func (c *Client) Head(p string) (bool, error) {
	req, err := http.NewRequest("PROPFIND", c.abs(p), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Depth", "0")
	if a := c.auth(); a != "" {
		req.Header.Set("Authorization", a)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusMultiStatus, nil
}

// normalize 规范化路径（复用 OpenList 的路径清理逻辑）。
func normalize(p string) string {
	return path.Clean("/" + strings.TrimSpace(p))
}

// 编译期断言：*Client 实现 openlist.FSClient。
var _ openlist.FSClient = (*Client)(nil)

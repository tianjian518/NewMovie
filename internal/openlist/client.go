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
	"strconv"
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
	Name     string    `json:"name"`
	Size     FlexInt64 `json:"size"`
	IsDir    bool      `json:"is_dir"`
	Modified FlexInt64 `json:"modified"` // 秒级时间戳
	Thumb    string     `json:"thumb"`
	Type     FlexString `json:"type"`
	Sign     string     `json:"sign"`
}

// FlexInt64 兼容 OpenList 系接口把数值字段（size / modified）有时传数字、
// 有时传字符串（甚至 ISO 日期串）的不一致行为，反序列化时统一归一为 int64。
// 解析不出时回退 0，绝不因单条脏数据让整次列表 / 测试连接失败。
type FlexInt64 int64

func (f *FlexInt64) UnmarshalJSON(b []byte) error {
	*f = 0
	s := strings.TrimSpace(string(b))
	if len(s) == 0 || s == "null" {
		return nil
	}
	// 数字：先试整型，再试浮点（如 1699... .0 或 1.5e3）
	if s[0] == '-' || (s[0] >= '0' && s[0] <= '9') {
		var n int64
		if err := json.Unmarshal(b, &n); err == nil {
			*f = FlexInt64(n)
			return nil
		}
		var fl float64
		if err := json.Unmarshal(b, &fl); err == nil {
			*f = FlexInt64(int64(fl))
			return nil
		}
	}
	// 字符串：先去引号，再试整型 / 浮点
	str := strings.Trim(strings.TrimSpace(s), `"`)
	if str == "" {
		return nil
	}
	if v, err := strconv.ParseInt(str, 10, 64); err == nil {
		*f = FlexInt64(v)
		return nil
	}
	if v, err := strconv.ParseFloat(str, 64); err == nil {
		*f = FlexInt64(int64(v))
		return nil
	}
	// ISO 日期串：转成 unix 秒（优先 RFC3339Nano，兼容 OpenList 返回的纳秒精度时间戳，
	// 如 "2026-07-29T14:12:07.778495506Z"）
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, str); err == nil {
			*f = FlexInt64(t.Unix())
			return nil
		}
	}
	return nil
}

func (f FlexInt64) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(f))
}

// FlexString 兼容 OpenList 把 type 等字段有时传字符串、有时传数字（真实 139cas 返回 int）的
// 不一致行为，反序列化时统一归一为 string（该字段仅用于展示，不参与业务逻辑）。
type FlexString string

func (f *FlexString) UnmarshalJSON(b []byte) error {
	*f = ""
	s := strings.TrimSpace(string(b))
	if len(s) == 0 || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err == nil {
			*f = FlexString(str)
		}
		return nil
	}
	if s[0] == '-' || (s[0] >= '0' && s[0] <= '9') {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			*f = FlexString(strconv.FormatInt(v, 10))
			return nil
		}
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			*f = FlexString(strconv.FormatFloat(v, 'f', -1, 64))
			return nil
		}
	}
	var v interface{}
	if err := json.Unmarshal(b, &v); err == nil {
		*f = FlexString(fmt.Sprintf("%v", v))
	}
	return nil
}

func (f FlexString) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(f))
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

// NormalizePath 把用户手填的网盘路径整理成 OpenList 能接受的形式。
//
// OpenList 的 /api/fs/list 对路径相当挑剔：缺前导斜杠、多一个尾斜杠、
// 前后带空格（从别处复制粘贴时极常见）都会直接返回 code=500 object not found，
// 表现给用户就是「路径明明是对的，却一个文件都扫不出来」。
// 这里统一收口：去首尾空白与不可见字符、反斜杠转正斜杠、折叠重复斜杠、
// 补前导斜杠、去尾斜杠（根目录除外）。
func NormalizePath(p string) string {
	p = strings.TrimSpace(p)
	// 去掉从网页复制常带的零宽字符与 NBSP
	p = strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\ufeff', '\u00a0':
			return -1
		}
		return r
	}, p)
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" {
		return "/"
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		return "/"
	}
	return p
}

// List 列出某路径下的内容。refresh=false 复用 OpenList 缓存（风控友好）。
func (c *Client) List(path string, refresh bool) ([]FsObj, error) {
	path = NormalizePath(path)
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
	path = NormalizePath(path)
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

// do 通用带鉴权请求，并对瞬时故障自动重试。
//
// OpenList 在扫描大目录时可能因网络抖动、反向代理（Nginx/Caddy）502/504、或
// TLS 中途断开而偶发返回非 JSON 内容（HTML 错误页、空响应）。旧实现把这类响应
// 直接喂给 json.Unmarshal，于是用户看到一串莫名其妙的
// `invalid character '<' looking for beginning of value`。这里：
//   - 识别「非 JSON / 空 / 5xx」并翻译成人话，不再暴露底层解析报错；
//   - 对瞬时类错误（连接重置、TLS 中断、代理抽风、5xx）重试几次再放弃，
//     让偶发断连自愈，而不是一抖就整个媒体库扫描失败。
func (c *Client) do(api string, body []byte) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= openlistMaxRetries; attempt++ {
		raw, err := c.tryOnce(api, body)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		// 持久错误（路径错、鉴权失败、OpenList 业务码非 200）不重试，立刻返回。
		if !isTransient(err) {
			return nil, err
		}
		if attempt < openlistMaxRetries {
			time.Sleep(time.Duration(attempt) * openlistRetryBackoff)
		}
	}
	return nil, lastErr
}

// tryOnce 执行单次请求并读取响应；返回可读错误，瞬时类错误带 transient 标记以便 do() 重试。
func (c *Client) tryOnce(api string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(c.BaseURL, "/")+api, bytes.NewReader(body))
	if err != nil {
		return nil, &olErr{msg: "构造请求失败：" + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", c.Token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		// 网络层瞬时错误（connection reset / TLS 中断 / 代理抽风）应重试。
		return nil, &olErr{
			msg:       "OpenList 连接失败（" + err.Error() + "），请检查地址是否可达、网络是否稳定",
			transient: true,
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return nil, &olErr{
			msg:       fmt.Sprintf("OpenList 返回 %d，服务可能临时不可用或被反向代理拦截", resp.StatusCode),
			transient: true,
		}
	}
	// 加读取上限：目录列表接口的响应大小由对端决定，一个塞了几十万文件的
	// 目录（或一台被替换掉的恶意 OpenList）能直接把内存吃光。
	// 32MB 足够放下任何正常的 fs/list 响应。
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, &olErr{msg: "读取 OpenList 响应失败：" + err.Error(), transient: true}
	}
	if int64(len(b)) >= maxRespBytes {
		return nil, fmt.Errorf("openlist %s: 响应超过 %dMB 上限，疑似目录过大或对端异常", api, maxRespBytes>>20)
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return nil, &olErr{msg: "OpenList 返回了空响应，可能是连接中途断开", transient: true}
	}
	// 响应体以 '<' 开头（HTML 错误页 / 代理报错页）或明显不是 JSON →
	// 视为瞬时错误重试，避免把 `invalid character '<'...` 这种底层报错丢给用户。
	if trimmed[0] == '<' {
		return nil, &olErr{
			msg:       "OpenList 返回的不是 JSON（可能是连接中断、被反向代理拦截，或返回了错误页），请检查 OpenList 服务是否在线",
			transient: true,
		}
	}
	return b, nil
}

// olErr 是客户端的可读错误；transient=true 表示瞬时故障、do() 应重试。
type olErr struct {
	msg       string
	transient bool
}

func (e *olErr) Error() string { return e.msg }
func (e *olErr) Temporary() bool { return e.transient }

func isTransient(err error) bool {
	if e, ok := err.(*olErr); ok {
		return e.transient
	}
	return false
}

// maxRespBytes 单次 API 响应最大读取字节数。
const maxRespBytes int64 = 32 << 20

// openlistMaxRetries / openlistRetryBackoff 瞬时故障的重试次数与退避步长。
// 扫描大目录时 OpenList 偶发断连很常见，重试 3 次（累计约 0.8~1.2s）即可自愈，
// 又不至于在 OpenList 真挂掉时无限拖延。
const (
	openlistMaxRetries  = 3
	openlistRetryBackoff = 400 * time.Millisecond
)

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

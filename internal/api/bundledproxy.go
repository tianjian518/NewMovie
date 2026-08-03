// bundledproxy.go —— 把内置 OpenList（139cas）的管理界面挂到 NewMovie 同一个端口下。
//
// 2.0 的容器里 139cas 只监听 127.0.0.1:5244，不对外暴露端口。用户要添加网盘、
// 配置驱动时仍然需要它的后台，于是这里做一层反代：
//
//	http://<host>:8096/openlist/   →   http://127.0.0.1:5244/
//
// 这样整个 2.0 对外只有一个端口、一个入口。
package api

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// BundledProxyPrefix 反代挂载路径。
const BundledProxyPrefix = "/openlist"

// NewBundledProxy 构造指向内置 OpenList 的反向代理 handler。
// target 解析失败时返回 nil，调用方应跳过挂载。
func NewBundledProxy(target string) http.Handler {
	u, err := url.Parse(strings.TrimRight(target, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		log.Printf("[bundled] 内置网盘地址无效，跳过反代挂载：%q", target)
		return nil
	}

	rp := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			// 剥掉 /openlist 前缀：OpenList 自身以 / 为根提供服务。
			p := strings.TrimPrefix(r.URL.Path, BundledProxyPrefix)
			if p == "" {
				p = "/"
			}
			r.URL.Path = p
			r.Host = u.Host

			// 让 OpenList 生成的绝对链接带上正确的外部前缀。
			// 它读 X-Forwarded-* 来拼 Site URL，不传的话后台里的链接会指向 5244。
			r.Header.Set("X-Forwarded-Prefix", BundledProxyPrefix)
			if r.Header.Get("X-Forwarded-Proto") == "" {
				if r.TLS != nil {
					r.Header.Set("X-Forwarded-Proto", "https")
				} else {
					r.Header.Set("X-Forwarded-Proto", "http")
				}
			}
		},
		// 不设 FlushInterval 的话，OpenList 的下载/预览流会被缓冲住。
		// -1 表示立即透传每一次写入，适合 /d/ 这类流式响应。
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[bundled] 反代 %s 失败：%v", r.URL.Path, err)
			http.Error(w, "内置网盘暂不可用，请稍后重试", http.StatusBadGateway)
		},
		// OpenList 的 3xx 重定向（/@login、/ 等）如果在反代层不重写 Location，
		// 浏览器会直接跳到 /@login 而不是 /openlist/@login，落在外层前缀之外
		// → 404 白屏。同样要把指向后端自身（127.0.0.1:5244）的绝对 URL 改写成
		// 客户端可见的前缀，否则浏览器去直连未对外暴露的后端端口 → 连不上。
		ModifyResponse: func(resp *http.Response) error {
			if loc := resp.Header.Get("Location"); loc != "" {
				if newLoc, ok := rewriteProxyLocation(loc, u); ok {
					resp.Header.Set("Location", newLoc)
				}
			}
			if cl := resp.Header.Get("Content-Location"); cl != "" {
				if newLoc, ok := rewriteProxyLocation(cl, u); ok {
					resp.Header.Set("Content-Location", newLoc)
				}
			}
			return nil
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /openlist 不带斜杠时重定向到 /openlist/，否则 OpenList 前端的
		// 相对路径资源（./assets/...）会解析到根目录去，白屏。
		if r.URL.Path == BundledProxyPrefix {
			http.Redirect(w, r, BundledProxyPrefix+"/", http.StatusMovedPermanently)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// rewriteProxyLocation 把 OpenList 在反代之后产生的「相对/绝对」重定向位置，
// 重写回客户端可见的 /openlist 前缀之下，避免浏览器跳出前缀导致 404。
//
// 规则：
//   - 已经是带前缀的路径：原样返回，避免重复加前缀。
//   - 绝对路径（/@login、/）：加上 /openlist 前缀。
//   - 绝对 URL 且指向后端自身（target.Host，如 127.0.0.1:5244）：
//     改写成客户端请求使用的 scheme+host + /openlist 前缀 + 原 path/query/fragment，
//     否则浏览器会去直连未对外暴露的后端端口。
//   - 其它绝对 URL（第三方登录回调等）：不动，交给浏览器原样跳转。
func rewriteProxyLocation(loc string, target *url.URL) (string, bool) {
	if loc == "" {
		return "", false
	}
	// 已是带前缀路径：原样返回。
	if loc == BundledProxyPrefix || strings.HasPrefix(loc, BundledProxyPrefix+"/") {
		return loc, true
	}
	// 绝对路径重定向：加前缀即可。
	if strings.HasPrefix(loc, "/") {
		return BundledProxyPrefix + loc, true
	}
	// 仅当指向内置后端自身时才改写绝对 URL。
	u, err := url.Parse(loc)
	if err != nil || !u.IsAbs() {
		return "", false
	}
	if u.Host != target.Host {
		return "", false
	}
	// 关键：不能用 resp.Request.Host 拼绝对 URL——那是反代发往后端的请求，
	// Host 是后端自身（如 127.0.0.1:5244），拼出来会把浏览器指回未暴露的后端端口。
	// 改成同源根相对路径 /openlist/...，由浏览器按当前页面源解析即可。
	out := BundledProxyPrefix + u.Path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.Fragment
	}
	return out, true
}

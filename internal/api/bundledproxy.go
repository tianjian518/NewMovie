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

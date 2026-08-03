// netguard.go 出站请求安全：SSRF 防护 + 统一超时。
//
// 背景：/api/play/proxy?u=<任意URL> 会由服务端原样发起请求并把响应回吐给调用方。
// 若不加限制，任何登录用户（默认口令还是 admin/admin）都能把 NewMovie 当成
// 内网跳板：读云厂商元数据（169.254.169.254）、探测宿主机端口、访问同一
// docker 网络里的其它容器。这是标准的 SSRF。
//
// 防护分两层：
//  1. URL 层：只允许 http/https，拒绝其它协议（file:// gopher:// 等）。
//  2. 连接层：自定义 DialContext，在 TCP 建连前检查**实际解析出的 IP**。
//     放在连接层而不是解析后校验一次，是为了同时挡住 DNS rebinding 和
//     302 跳转到内网这两种绕过手法。
//
// 例外：家用场景里 OpenList 常部署在同一内网（192.168.x.x / docker 网段），
// 直链自然指向内网。所以「目标主机名属于用户已配置的存储源」时放行内网地址，
// 既保住可用性，又不给出任意内网访问能力。
package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// errBlockedTarget 目标地址被 SSRF 防护拦截。
var errBlockedTarget = errors.New("目标地址不允许访问（内网/回环地址已被安全策略拦截）")

// isBlockedIP 判断 IP 是否属于「不该由服务端主动去访问」的范围。
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if ip.IsPrivate() { // 10/8, 172.16/12, 192.168/16, fc00::/7
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0: // 0.0.0.0/8
			return true
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64/10 运营商级 NAT
			return true
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24 IETF 协议分配
			return true
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18/15 基准测试
			return true
		case v4[0] == 255: // 广播
			return true
		}
		return false
	}
	// IPv6：站点本地（已废弃但仍可能出现）
	return ip[0] == 0xfe && ip[1]&0xc0 == 0xc0
}

// guardedDial 在建立 TCP 连接前校验对端 IP。
func guardedDial(allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if allowPrivate {
			return d.DialContext(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			// Transport 传进来的可能是主机名（自定义 DialContext 下不会预解析）。
			// 这里自己解析、逐个校验，**并挑出一个通过校验的 IP 直接去 dial**，
			// 绝不能再用原始主机名 dial —— 否则会二次解析，DNS rebinding 能在
			// 「校验」和「建连」之间把域名指向内网，绕过全部校验（TOCTOU）。
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			var chosen net.IP
			for _, cand := range ips {
				if isBlockedIP(cand) {
					return nil, errBlockedTarget
				}
				if chosen == nil {
					chosen = cand
				}
			}
			if chosen == nil {
				return nil, errBlockedTarget
			}
			return d.DialContext(ctx, network, net.JoinHostPort(chosen.String(), port))
		}
		if isBlockedIP(ip) {
			return nil, errBlockedTarget
		}
		// host 本身就是 IP，addr 已是 ip:port，不会再解析，直接 dial。
		return d.DialContext(ctx, network, addr)
	}
}

func newTransport(allowPrivate bool) *http.Transport {
	return &http.Transport{
		DialContext:           guardedDial(allowPrivate),
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
}

var (
	// mediaClient 访问用户自己配置的存储源（OpenList 直链、同目录图片）。
	// 地址来源可信，只需要超时；不设 Client.Timeout 是因为视频流可能持续很久，
	// 卡死的情况交给 ResponseHeaderTimeout 兜。
	mediaClient = &http.Client{Transport: newTransport(true)}

	// proxyClientStrict 代理任意外部 URL：拦内网。
	proxyClientStrict = &http.Client{Transport: newTransport(false)}

	// imageClient 拉图片，整体超时 30s 足够。
	imageClient = &http.Client{Transport: newTransport(true), Timeout: 30 * time.Second}
)

// parseOutboundURL 校验协议与主机，返回解析后的 URL。
func parseOutboundURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("URL 格式错误")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, errors.New("只允许 http/https 协议")
	}
	if u.Hostname() == "" {
		return nil, errors.New("URL 缺少主机名")
	}
	return u, nil
}

// storageHosts 收集用户已配置存储源的主机名，用于放行内网直链。
func (s *Server) storageHosts() map[string]bool {
	hosts := map[string]bool{}
	list, err := s.Store.ListStorages()
	if err != nil {
		return hosts
	}
	for _, st := range list {
		if u, err := url.Parse(strings.TrimSpace(st.BaseURL)); err == nil {
			if h := strings.ToLower(u.Hostname()); h != "" {
				hosts[h] = true
			}
		}
	}
	return hosts
}

// proxyClientFor 按目标主机挑选出站客户端：
// 属于已配置存储源的主机放行内网，其余一律走严格模式。
func (s *Server) proxyClientFor(u *url.URL) *http.Client {
	if s.storageHosts()[strings.ToLower(u.Hostname())] {
		return mediaClient
	}
	return proxyClientStrict
}

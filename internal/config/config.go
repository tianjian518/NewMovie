// Package config 读取运行配置（环境变量优先，带默认值）。
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr         string // HTTP 监听地址
	DataDir      string // 数据根目录 (/data)
	DBFile       string // JSON 存储文件路径
	CacheDir     string // 图片/直链缓存根目录 (DataDir/cache)
	AdminUser    string
	AdminPass    string
	TMDBKey      string
	TMDBBase     string // TMDB API 根地址覆盖（自建反代/镜像），空则用官方 + 内置备用域名
	TMDBImageBase string // TMDB 图片 CDN 根地址覆盖（镜像）。空则用官方 image.tmdb.org。
	                    // 部分网络 image.tmdb.org 被墙，可用自建图片反代/镜像，或由本服务
	                	// /api/image 服务端取图（浏览器只跟 8096 入口，无需直连 CDN）。
	ProxyRefresh bool   // 默认 refresh=false 复用 OpenList 缓存
	DefaultRate  float64
	// TranscodeEnabled 是否允许视频转码（HEVC→H264 等）。默认关：转码是 CPU 黑洞。
	// 仅当浏览器本身解不了某编码（多数 Firefox / 部分 Chrome 不解码 HEVC）且用户
	// 又想页内播放时才开。前端「设置」里的开关会覆盖此项（持久化 transcode_enabled）。
	TranscodeEnabled bool
	// LocalRoots 允许服务端直接读盘播放的本地目录白名单（CloudDrive2 等本地 strm）。
	// 仅当 strm 指向 file:// 且路径落在这些目录下才放行，否则拒绝——防任意文件读取（SSRF）。
	LocalRoots []string

	// ---- 2.0 内置 OpenList（139cas）----
	// 2.0 把 139cas 作为同容器的后端进程一起打包。NewMovie 启动后自动登录它、
	// 把它注册成默认存储，用户不用再手填地址和 Token。

	// Bundled 是否启用内置 OpenList 接管。容器镜像里默认开；裸跑二进制默认关。
	Bundled bool
	// BundledURL 内置 OpenList 的地址，默认 http://127.0.0.1:5244（仅回环，不对外暴露）。
	BundledURL string
	// BundledName 自动注册的存储显示名。
	BundledName string
	// BundledToken 直接指定 Token；留空则用下面的账号密码登录换取。
	BundledToken string
	// BundledUser / BundledPass 内置 OpenList 的管理员账号，用于自动换取 Token。
	BundledUser string
	BundledPass string
	// BundledTimeout 等待内置 OpenList 就绪的最长时间。
	BundledTimeout time.Duration
	// BundledProxy 是否把 /openlist/* 反代到内置 OpenList，让用户在同一端口管理网盘挂载。
	BundledProxy bool
}

// Load 从环境变量加载配置，缺省则用合理默认值。
func Load() *Config {
	c := &Config{
		Addr:         getenv("VIDRIVE_ADDR", ":8096"),
		DataDir:      getenv("VIDRIVE_DATA", "/data"),
		AdminUser:    getenv("VIDRIVE_ADMIN_USER", "admin"),
		AdminPass:    getenv("VIDRIVE_ADMIN_PASS", "admin"),
		TMDBKey:      os.Getenv("TMDB_API_KEY"),
		TMDBBase:     os.Getenv("TMDB_API_BASE"),
		TMDBImageBase: os.Getenv("TMDB_IMAGE_BASE"),
		ProxyRefresh: false,
		DefaultRate:  2.0, // 保守限速，风控友好
	}
	if v := os.Getenv("VIDRIVE_TRANSCODE"); v == "1" || v == "true" || v == "on" {
		c.TranscodeEnabled = true
	}
	if v := os.Getenv("VIDRIVE_LOCAL_ROOTS"); v != "" {
		for _, r := range strings.Split(v, ",") {
			if r = strings.TrimSpace(r); r != "" {
				c.LocalRoots = append(c.LocalRoots, r)
			}
		}
	}
	if r := os.Getenv("VIDRIVE_SCAN_RATE"); r != "" {
		if v, err := strconv.ParseFloat(r, 64); err == nil && v > 0 {
			c.DefaultRate = v
		}
	}

	// ---- 2.0 内置 OpenList ----
	// 镜像的 supervisord 会注入 NEWMOVIE_BUNDLED=1；裸跑时默认关闭，行为与 1.x 完全一致。
	c.Bundled = boolenv("NEWMOVIE_BUNDLED", false)
	c.BundledURL = strings.TrimRight(getenv("NEWMOVIE_BUNDLED_URL", "http://127.0.0.1:5244"), "/")
	c.BundledName = getenv("NEWMOVIE_BUNDLED_NAME", "内置网盘")
	c.BundledToken = os.Getenv("NEWMOVIE_BUNDLED_TOKEN")
	c.BundledUser = getenv("NEWMOVIE_BUNDLED_USER", "admin")
	c.BundledPass = os.Getenv("NEWMOVIE_BUNDLED_PASS")
	c.BundledTimeout = 90 * time.Second
	if v := os.Getenv("NEWMOVIE_BUNDLED_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.BundledTimeout = d
		}
	}
	// 反代默认跟随 Bundled：开了内置就默认能在同端口访问挂载管理页。
	c.BundledProxy = boolenv("NEWMOVIE_BUNDLED_PROXY", c.Bundled)

	c.DBFile = c.DataDir + "/newmovie.json"
	c.CacheDir = c.DataDir + "/cache"
	return c
}

// boolenv 解析布尔环境变量：1/true/on/yes 为真，0/false/off/no 为假，未设置或无法识别用 def。
func boolenv(k string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	switch v {
	case "1", "true", "on", "yes", "y":
		return true
	case "0", "false", "off", "no", "n":
		return false
	}
	return def
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

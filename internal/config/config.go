// Package config 读取运行配置（环境变量优先，带默认值）。
package config

import (
	"os"
	"strconv"
	"strings"
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
	ProxyRefresh bool   // 默认 refresh=false 复用 OpenList 缓存
	DefaultRate  float64
	// TranscodeEnabled 是否允许视频转码（HEVC→H264 等）。默认关：转码是 CPU 黑洞。
	// 仅当浏览器本身解不了某编码（多数 Firefox / 部分 Chrome 不解码 HEVC）且用户
	// 又想页内播放时才开。前端「设置」里的开关会覆盖此项（持久化 transcode_enabled）。
	TranscodeEnabled bool
	// LocalRoots 允许服务端直接读盘播放的本地目录白名单（CloudDrive2 等本地 strm）。
	// 仅当 strm 指向 file:// 且路径落在这些目录下才放行，否则拒绝——防任意文件读取（SSRF）。
	LocalRoots []string
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
	c.DBFile = c.DataDir + "/newmovie.json"
	c.CacheDir = c.DataDir + "/cache"
	return c
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

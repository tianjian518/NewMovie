// Package config 读取运行配置（环境变量优先，带默认值）。
package config

import (
	"os"
	"strconv"
)

type Config struct {
	Addr         string // HTTP 监听地址
	DataDir      string // 数据根目录 (/data)
	DBFile       string // JSON 存储文件路径
	CacheDir     string // 图片/直链缓存根目录 (DataDir/cache)
	AdminUser    string
	AdminPass    string
	TMDBKey      string
	ProxyRefresh bool // 默认 refresh=false 复用 OpenList 缓存
	DefaultRate  float64
}

// Load 从环境变量加载配置，缺省则用合理默认值。
func Load() *Config {
	c := &Config{
		Addr:         getenv("VIDRIVE_ADDR", ":8096"),
		DataDir:      getenv("VIDRIVE_DATA", "/data"),
		AdminUser:    getenv("VIDRIVE_ADMIN_USER", "admin"),
		AdminPass:    getenv("VIDRIVE_ADMIN_PASS", "admin"),
		TMDBKey:      os.Getenv("TMDB_API_KEY"),
		ProxyRefresh: false,
		DefaultRate:  2.0, // 保守限速，风控友好
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

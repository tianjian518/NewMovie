// Package strm 解析 .strm 文件的一行文本，归一化为统一资源描述。
//
// 设计见 PLAN.md 第四节：strm 没有统一规范，方言极多。Vidrive 的策略是
// 「先应用重写规则 → 再嗅探方言 → OpenList 的 /d/ 链接归一回原生模式」。
//
// 这是纯标准库实现，无外部依赖，可独立单测（见 resolver_test.go）。
package strm

import (
	"os"
	"regexp"
	"strings"

	"newmovie/internal/model"
	"newmovie/internal/openlist"
)

// ResolvedSource 归一化后的统一资源描述。
// Scheme 取值：
//
//	openlist  — 解析为某个已配置 OpenList 存储的内部路径（可算 sign、取 raw_url）
//	http/https — 直链模式，直接拿去播
//	file      — 本地/挂载点绝对路径
//	relative  — 相对路径，需调用方结合 strm 文件所在目录解析
type ResolvedSource struct {
	Scheme      string
	StorageID   string // Scheme=openlist 时命中
	Path        string // openlist 内部路径 或 本地绝对路径
	RawURL      string // Scheme=http/https 时的完整 URL
	IsOpenListD bool   // 源自 /d/ 链接（已归一化回原生）
}

// Resolver 解析器，持有候选存储与重写规则。
type Resolver struct {
	Storages []model.Storage
	Rewrites []model.PathRewrite
}

func NewResolver(storages []model.Storage, rewrites []model.PathRewrite) *Resolver {
	return &Resolver{Storages: storages, Rewrites: rewrites}
}

// Resolve 解析一行 strm 文本。
func (r *Resolver) Resolve(raw string) ResolvedSource {
	line := clean(raw)
	line = r.applyRewrites(line)

	// 1) 重写后可能已是 openlist:// 方案
	if strings.HasPrefix(line, "openlist://") {
		name, path := splitOpenListScheme(line)
		return ResolvedSource{
			Scheme:      "openlist",
			StorageID:   r.findStorageID(name),
			Path:        path,
			IsOpenListD: true,
		}
	}

	// 2) 直链方言：http(s)://
	if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
		scheme := "http"
		if strings.HasPrefix(line, "https://") {
			scheme = "https"
		}
		// 尝试归一化为 OpenList 的 /d/ 链接
		for _, s := range r.Storages {
			if s.Type != model.StorageOpenList {
				continue
			}
			if ok, base, internal := openlist.IsOpenListD(line, s.BaseURL); ok {
				_ = base
				return ResolvedSource{
					Scheme:      "openlist",
					StorageID:   s.ID,
					Path:        internal,
					IsOpenListD: true,
				}
			}
		}
		return ResolvedSource{Scheme: scheme, RawURL: line}
	}

	// 3) 以 / 开头：本地文件 或 OpenList 内部路径（Without Url 模式）
	if strings.HasPrefix(line, "/") {
		if _, err := os.Stat(line); err == nil {
			return ResolvedSource{Scheme: "file", Path: line}
		}
		// 默认当作首个 OpenList 存储的内部路径（无 URL 模式）
		if sid := r.firstOpenListID(); sid != "" {
			return ResolvedSource{Scheme: "openlist", StorageID: sid, Path: line, IsOpenListD: true}
		}
		// 实在无存储可用：按内部路径兜底
		return ResolvedSource{Scheme: "openlist", Path: line, IsOpenListD: true}
	}

	// 4) 其余：相对路径
	return ResolvedSource{Scheme: "relative", Path: line}
}

// ---- 内部辅助 ----

// clean 去 BOM、CR、首尾空白，跳过 # 注释行。
func clean(line string) string {
	line = strings.TrimPrefix(line, "\ufeff") // BOM
	line = strings.ReplaceAll(line, "\r", "") // CRLF -> LF
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return ""
	}
	return line
}

// applyRewrites 按优先级依次应用正则重写规则，结果串接传递。
// 例：^http://localhost:5244/d/(.*)$ -> openlist://main/$1
func (r *Resolver) applyRewrites(line string) string {
	for _, rw := range r.Rewrites { // Rewrites 已按优先级升序
		re, err := regexp.Compile(rw.Pattern)
		if err != nil {
			continue
		}
		if re.MatchString(line) {
			line = re.ReplaceAllString(line, rw.Replacement)
		}
	}
	return line
}

// splitOpenListScheme 解析 openlist://<storageName>/<path>。
func splitOpenListScheme(s string) (name, path string) {
	rest := strings.TrimPrefix(s, "openlist://")
	idx := strings.Index(rest, "/")
	if idx < 0 {
		return rest, "/"
	}
	return rest[:idx], rest[idx:] // 保留前导 /
}

func (r *Resolver) findStorageID(name string) string {
	for _, s := range r.Storages {
		if s.Name == name || s.ID == name {
			return s.ID
		}
	}
	return ""
}

func (r *Resolver) firstOpenListID() string {
	for _, s := range r.Storages {
		if s.Type == model.StorageOpenList {
			return s.ID
		}
	}
	return ""
}

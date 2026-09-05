// Package api 用标准库 net/http 暴露 Vidrive 的 REST 接口。
// 路由手写（零依赖）。前端通过 Bearer token 鉴权。
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/hls"
	"newmovie/internal/model"
	"newmovie/internal/openlist"
	"newmovie/internal/playback"
	"newmovie/internal/scanner"
	"newmovie/internal/scraper"
	"newmovie/internal/store"
	"newmovie/internal/strm"
	"newmovie/internal/subtitle"
	"newmovie/internal/tmdb"
	"newmovie/internal/webdav"
)

// Version 是当前服务版本，健康检查与前端一并使用。
// 2.0：内置 139cas 后端，一个容器完成「挂网盘 → 刮削 → 播放」。
const Version = "2.0.0"

// Server 持有依赖。
type Server struct {
	Store store.Store
	Cfg   *config.Config

	// ffmpegOK 服务端是否安装了 ffmpeg。重封装(L2)/转码(L3)都依赖它；
	// 缺失时这些路径不可用，playItem 会把相关文件降级为外部播放器并提示用户换镜像。
	ffmpegOK bool
	// transcodeOK ffmpeg 是否带 libx264 编码器（视频转码 L3 必需）。部分精简 ffmpeg
	// 构建不含 libx264，此时重封装(L2)仍可用，但转码(L3)不可用——缺则 HEVC 改走
	// L2 重封装（保留 HEVC，仅 HEVC 能力浏览器可播）而非返回空 200 让播放器黑屏。
	transcodeOK bool

	// hlsMgr 管理 HLS 按需切片会话（见 internal/hls）。L2/L3 播放经它切 HLS 分片，
	// 浏览器用 hls.js 拉取，拖动精准、起播快、通用性强（对标 Plex/Jellyfin/Lunarr）。
	hlsMgr *hls.Manager
	// hlsEnabled 是否用 HLS 交付 L2/L3（默认开；VIDRIVE_HLS=0/off/false 关闭，退回单 MP4 流）。
	hlsEnabled bool

	// scanMu/scanning 保证同一媒体库同时只有一个扫描协程。
	// 没有这把锁时，前端连点两下「扫描」（或页面轮询期间用户手抖）就会起两个
	// 协程同时遍历同一个库：两边各自 upsert 同一批条目、互相覆盖 FileCount，
	// 还会把 OpenList 的请求量翻倍，正好撞上网盘风控。
	scanMu   sync.Mutex
	scanning map[string]bool
}

// tryLockScan 抢占某个媒体库的扫描权，已有扫描在跑时返回 false。
func (s *Server) tryLockScan(libID string) bool {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if s.scanning == nil {
		s.scanning = map[string]bool{}
	}
	if s.scanning[libID] {
		return false
	}
	s.scanning[libID] = true
	return true
}

// unlockScan 释放扫描权。
func (s *Server) unlockScan(libID string) {
	s.scanMu.Lock()
	delete(s.scanning, libID)
	s.scanMu.Unlock()
}

// ScanRunning 报告某个媒体库当前是否正在扫描（内存态，进程重启即清空，
// 因此不会像持久化的 job.Status 那样被崩溃残留的 "running" 永久卡住）。
func (s *Server) ScanRunning(libID string) bool {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	return s.scanning[libID]
}

// New 构造 Server。
func New(st store.Store, cfg *config.Config) *Server {
	s := &Server{Store: st, Cfg: cfg}
	// 启动即探测 ffmpeg 能力与 libx264 编码器（视频转码 L3 必需）。
	// - ffmpeg 在 + 带 libx264：重封装/转码都可用，默认开启转码（HEVC 也能页内播，
	//   真正实现「无论什么 strm 都能播」）；仅当用户显式 VIDRIVE_TRANSCODE=0/off/false 时关闭。
	// - ffmpeg 在但无 libx264：重封装(L2)可用，转码(L3)不可用，HEVC 改走 L2 保留 HEVC。
	// - ffmpeg 不在：重封装/转码都不可用，MKV/HEVC 只能唤起外部播放器 + 明确提示换镜像。
	if bin, err := exec.LookPath("ffmpeg"); err == nil {
		s.ffmpegOK = true
		if ffmpegHasEncoder(bin, "libx264") {
			s.transcodeOK = true
			if v := os.Getenv("VIDRIVE_TRANSCODE"); v != "0" && v != "off" && v != "false" {
				s.Cfg.TranscodeEnabled = true
			}
			log.Printf("[info] ffmpeg 已就绪（含 libx264）：重封装/转码可用，HEVC 等将默认转码页内播")
		} else {
			log.Printf("[warn] ffmpeg 存在但缺少 libx264 编码器：重封装(L2)可用，视频转码(L3)不可用。" +
				"HEVC 将走重封装保留 HEVC（仅 HEVC 能力浏览器可播）。请换含 libx264 的 ffmpeg 构建。")
		}
	} else {
		s.ffmpegOK = false
		log.Printf("[warn] 未找到 ffmpeg：重封装/转码不可用，MKV/HEVC 将只能唤起外部播放器。" +
			"请用含 ffmpeg 的镜像（tianjian518/newmovie:latest 已含 ffmpeg）")
	}

	// HLS 按需切片：默认开启（hls.js 已内置前端，L2/L3 切 HLS 分片更稳更通用）。
	// 可用 VIDRIVE_HLS=0/off/false 关闭，退回原有「单 MP4 流」重封装/转码。
	s.hlsEnabled = true
	hlsEnv := os.Getenv("VIDRIVE_HLS")
	if hlsEnv == "0" || hlsEnv == "off" || hlsEnv == "false" {
		s.hlsEnabled = false
	}
	hlsDir := os.Getenv("VIDRIVE_HLS_DIR")
	if hlsDir == "" {
		hlsDir = os.Getenv("NEWMOVIE_HLS_DIR")
	}
	s.hlsMgr = hls.New(hlsDir)
	s.hlsMgr.StartCleanup(time.Minute)
	if s.hlsEnabled {
		log.Printf("[info] HLS 按需切片已启用（缓存目录 %s）", s.hlsMgr.Dir())
	} else {
		log.Printf("[info] HLS 按需切片已关闭（VIDRIVE_HLS=%s），L2/L3 退回单 MP4 流", hlsEnv)
	}

	// 图片代理：把 TMDB 图片直链改写成经本服务 /api/image 的地址，浏览器只跟 8096 入口，
	// 由服务端去取图并缓存。解决「image.tmdb.org 被墙 → 海报全白」的问题（与 2.0 单端口模型一致）。
	tmdb.SetImageProxyPrefix("/api/image?u=")
	if s.Cfg.TMDBImageBase != "" {
		tmdb.SetImageBase(s.Cfg.TMDBImageBase)
	}
	return s
}

// ffmpegHasEncoder 探测 ffmpeg 是否带某个编码器（如 libx264），用于判断转码能力。
func ffmpegHasEncoder(bin, name string) bool {
	out, err := exec.Command(bin, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		return false
	}
	return bytes.Contains(out, []byte(name))
}

// --- 通用辅助 ---

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	writeJSON(w, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v interface{}) error { return json.NewDecoder(r.Body).Decode(v) }

func getToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

// appendToken 给内部媒体 URL 附上 ?token=。
//
// 浏览器的 <video src>、<track src> 由浏览器自己发起请求，**没有任何办法带上
// Authorization 头**。而 /api/play/remux、/api/play/transcode、/api/play/subtitle
// 都要求鉴权，于是页内播放会直接 401 黑屏 —— 用 Go 客户端做的 E2E 因为手动设了
// header，恰好绕过了这个坑，属于典型的「测试通过但用户用不了」。
//
// 后端 getToken 本就支持 ?token= 兜底，这里在下发 URL 时补上即可，前端零改动。
func appendToken(u, tok string) string {
	if u == "" || tok == "" || strings.Contains(u, "token=") {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "token=" + url.QueryEscape(tok)
}

// capFlag 解析客户端能力上报参数（?hevc=1 / ?av1=true / ?vp9=probably）。
// 未上报时返回默认值 def：vp9/av1 默认 true（旧前端/直接 API 不报也按可解），hevc 默认 false。
func capFlag(r *http.Request, key string, def bool) bool {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "probably"
}

func (s *Server) requireUser(r *http.Request) (model.User, bool) {
	tok := getToken(r)
	if tok == "" {
		return model.User{}, false
	}
	u, err := s.Store.GetUserByToken(tok)
	if err != nil {
		return model.User{}, false
	}
	return u, true
}

// childModeMaxRating 儿童模式的安全评级上限：超出即过滤。
const childModeMaxRating = 12.0

// filterItemsForUser 按用户权限过滤条目列表：
//   - 库白名单：条目所属媒体库不在用户可访问范围则移除（管理员的 AllowedLibs 空=全放行，已在 CanAccess 处理）；
//   - 儿童模式：仅保留低龄安全内容。当前按「评分上限 + 无剧情评级字段则放行」的保守策略——
//     rating 未知(<=0) 时放行（可能是动画/纪录片尚未刮到评分），rating>12 时移除。
func (s *Server) filterItemsForUser(u model.User, items []model.MediaItem) []model.MediaItem {
	if u.IsAdmin {
		return items
	}
	// 先按库白名单过滤
	var out []model.MediaItem
	for _, it := range items {
		if !u.CanAccess(it.LibraryID) {
			continue
		}
		out = append(out, it)
	}
	if !u.ChildMode {
		return out
	}
	// 儿童模式二次过滤：只看低龄内容
	filtered := out[:0]
	for _, it := range out {
		if it.Rating > childModeMaxRating {
			continue
		}
		filtered = append(filtered, it)
	}
	return filtered
}

func (s *Server) clientFor(storage model.Storage) openlist.FSClient {
	if storage.Type == model.StorageWebDAV {
		return webdav.New(storage.BaseURL, storage.Token)
	}
	return &openlist.Client{BaseURL: storage.BaseURL, Token: storage.Token, SignKey: storage.SignKey}
}

// resolveOpenListLink 在 OpenList 存储里定位 strm 指向的文件。
// 先用完整路径取链；若失败（常见于「本地路径型 strm」前缀与存储内部路径不一致），
// 逐级剥掉路径前缀重试（/mnt/cloud/媒体/A.mkv → /cloud/媒体/A.mkv → /媒体/A.mkv → /A.mkv），
// 命中即用。返回最终命中的 link 与用于后续（签名/容器探测）的 actual 路径。
// 这是「无论什么 strm 都能页内播放」的零配置兜底：用户不必为路径前缀差异做任何配置。
func (s *Server) resolveOpenListLink(cl openlist.FSClient, path string) (*openlist.FsGetResp, string) {
	p := path
	last := p
	for {
		link, err := cl.GetLink(p, s.Cfg.ProxyRefresh)
		if err == nil && link != nil && (link.Data.RawURL != "" || link.Data.URL != "") {
			return link, p
		}
		last = p
		// 剥掉第一段（保留前导 /）
		rest := strings.TrimPrefix(p, "/")
		idx := strings.Index(rest, "/")
		if idx < 0 {
			break
		}
		p = "/" + rest[idx+1:]
		if p == "/" {
			break
		}
	}
	return nil, last
}

// tmdbKey 解析 TMDB Key：环境变量优先，其次用户在前端自填并持久化的设置。
func (s *Server) tmdbKey() string {
	if s.Cfg.TMDBKey != "" {
		return s.Cfg.TMDBKey
	}
	if v, err := s.Store.GetSetting("tmdb_api_key"); err == nil && v != "" {
		return v
	}
	return ""
}

// tmdbBase 解析 TMDB API 根地址覆盖（自建反代/镜像）：环境变量优先，其次前端设置。
// 返回空串表示用官方地址 + 内置备用域名自动兜底。
func (s *Server) tmdbBase() string {
	if s.Cfg.TMDBBase != "" {
		return s.Cfg.TMDBBase
	}
	if v, err := s.Store.GetSetting("tmdb_api_base"); err == nil && v != "" {
		return v
	}
	return ""
}

// newSearcher 按当前配置构造刮削搜索器；未配置 Key 时返回 nil（调用方据此跳过 TMDB）。
func (s *Server) newSearcher() scraper.Searcher {
	key := s.tmdbKey()
	if key == "" {
		return nil
	}
	return scraper.NewTMDBSearcherWithBase(key, s.tmdbBase())
}

// Handler 返回 http.Handler（外层包一层 panic 恢复中间件）。
//
// net/http 自带的 per-connection recover 只能兜住处理器同步栈上的 panic，
// 且默认会静默断开连接。这里显式恢复并回 500，同时打出堆栈便于定位；
// 任何一个坏条目/坏数据都不会再把服务打挂。
func (s *Server) Handler() http.Handler { return recoverMW(http.HandlerFunc(s.route)) }

// recoverMW 捕获处理器 panic，避免单个请求击穿整个服务。
func recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if e := recover(); e != nil {
				// ErrAbortHandler 是 net/http 约定的"静默中止"（如客户端断开），不当作错误。
				if e == http.ErrAbortHandler {
					panic(e)
				}
				log.Printf("[http] 处理 %s %s 时 panic 已恢复: %v\n%s", r.Method, r.URL.Path, e, debug.Stack())
				defer func() { _ = recover() }() // 响应头可能已写出，忽略二次失败
				writeErr(w, http.StatusInternalServerError, "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(p, "/")

	// 免鉴权
	switch {
	case p == "api/health":
		bundledPrefix := ""
		if s.Cfg.BundledProxy {
			bundledPrefix = BundledProxyPrefix + "/"
		}
		writeJSON(w, map[string]interface{}{
			"ok": true, "version": Version, "name": "NewMovie",
			"ffmpeg_ok":    s.ffmpegOK,
			"transcode_ok": s.transcodeOK,
			"transcode":    s.Cfg.TranscodeEnabled,
			// 2.0：内置 139cas 状态。前端据此决定是否显示「网盘挂载」入口，
			// 以及在接管尚未完成时给出「正在连接内置网盘」的提示。
			"bundled":        s.Cfg.Bundled,
			"bundled_ready":  BundledReady(),
			"bundled_prefix": bundledPrefix,
		})
		return
	case p == "api/login" && r.Method == http.MethodPost:
		s.login(w, r)
		return
	}
	// /api/image 是海报/背景图代理：浏览器 <img> 无法携带 Authorization，故公开。
	// 仅放行 TMDB 图片 CDN 白名单主机（见 handleImageProxy），不构成 SSRF 跳板。
	if len(parts) >= 2 && parts[0] == "api" && parts[1] == "image" && r.Method == http.MethodGet {
		s.handleImageProxy(w, r)
		return
	}

	if _, ok := s.requireUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "未登录或 token 失效")
		return
	}

	if len(parts) >= 2 && parts[0] == "api" {
		switch parts[1] {
		case "storages":
			s.handleStorages(w, r, parts)
			return
		case "libraries":
			s.handleLibraries(w, r, parts)
			return
		case "scan":
			s.handleScan(w, r, parts)
			return
		case "items":
			s.handleItems(w, r, parts)
			return
		case "search":
			s.searchItems(w, r)
			return
		case "continue":
			s.handleContinue(w, r)
			return
		case "favorites":
			s.handleFavorites(w, r, parts)
			return
		case "rewrites":
			s.handleRewrites(w, r, parts)
			return
		case "users":
			s.handleUsers(w, r, parts)
			return
		case "settings":
			if len(parts) == 4 && parts[2] == "tmdb" && parts[3] == "test" && r.Method == http.MethodPost {
				s.testTMDB(w, r)
				return
			}
			s.handleSettings(w, r)
			return
		case "play":
			s.handlePlay(w, r, parts)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "not found: "+p)
}

// --- 鉴权 ---

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体错误")
		return
	}
	u, err := s.Store.GetUserByName(body.Username)
	if err != nil || !auth.CheckPassword(body.Password, u.Password) {
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	tok := auth.NewToken()
	_ = s.Store.UpsertToken(u.ID, tok)
	writeJSON(w, map[string]interface{}{"token": tok, "is_admin": u.IsAdmin, "username": u.Username})
}

// --- 存储源 ---

// sanitizeStorage 对普通用户脱敏存储源的敏感字段（Token / SignKey）。
// 建库时前端只需要 id/name/base_url/type，不需要也不应该拿到网盘 API 凭证。
func sanitizeStorage(s model.Storage) model.Storage {
	s.Token = ""
	s.SignKey = ""
	return s
}

func (s *Server) handleStorages(w http.ResponseWriter, r *http.Request, parts []string) {
	cur, _ := s.requireUser(r)
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		list, _ := s.Store.ListStorages()
		// 普通用户只能看到脱敏后的存储源（用于建库时选择），管理员看到完整信息。
		if !cur.IsAdmin {
			for i := range list {
				list[i] = sanitizeStorage(list[i])
			}
		}
		writeJSON(w, list)
	case len(parts) == 2 && r.Method == http.MethodPost:
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可创建存储源")
			return
		}
		var st model.Storage
		if err := readJSON(r, &st); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体错误")
			return
		}
		// 同一 Base URL 已存在则视为「更新」，避免重复保存越攒越多。
		if st.ID == "" {
			if ex, e := s.Store.GetStorageByBaseURL(st.BaseURL); e == nil {
				st.ID = ex.ID
			}
		}
		if st.ID == "" {
			st.ID = auth.GenID("st")
			st.CreatedAt = model.NowMillis()
		}
		if st.RateLimit <= 0 {
			st.RateLimit = s.Cfg.DefaultRate
		}
		_ = s.Store.SaveStorage(st)
		writeJSON(w, st)
	case len(parts) == 3 && r.Method == http.MethodPut:
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可修改存储源")
			return
		}
		// 编辑已有存储源：按 ID 更新。
		var st model.Storage
		if err := readJSON(r, &st); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体错误")
			return
		}
		st.ID = parts[2]
		if _, e := s.Store.GetStorage(st.ID); e != nil {
			writeErr(w, http.StatusNotFound, "存储源不存在")
			return
		}
		if st.RateLimit <= 0 {
			st.RateLimit = s.Cfg.DefaultRate
		}
		_ = s.Store.SaveStorage(st)
		writeJSON(w, st)
	case len(parts) == 3 && r.Method == http.MethodDelete:
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可删除存储源")
			return
		}
		if err := s.Store.DeleteStorage(parts[2]); err != nil {
			writeErr(w, http.StatusNotFound, "存储源不存在")
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	case len(parts) == 3 && parts[2] == "test" && r.Method == http.MethodPost:
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可测试存储源连接")
			return
		}
		s.testStorage(w, r)
	case len(parts) == 4 && parts[3] == "drives" && r.Method == http.MethodGet:
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可列出存储源驱动器")
			return
		}
		st, err := s.Store.GetStorage(parts[2])
		if err != nil {
			writeErr(w, http.StatusNotFound, "存储源不存在")
			return
		}
		drives, err := s.clientFor(st).ListDrives()
		if err != nil {
			writeErr(w, http.StatusBadGateway, "连接 OpenList 失败: "+err.Error())
			return
		}
		writeJSON(w, drives)
	case len(parts) == 4 && parts[3] == "browse" && r.Method == http.MethodGet:
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可浏览存储源目录")
			return
		}
		s.browseStorage(w, r, parts[2])
	default:
		writeErr(w, http.StatusNotFound, "unknown storages route")
	}
}

// browseStorage 列出某存储源指定路径下的**子目录**，供前端目录树逐级展开。
// GET /api/storages/{id}/browse?path=/115/影视
//
// 顺带统计每个目录内的视频/字幕文件数量：用户挑目录时最想知道的就是
// 「这个文件夹里到底有没有片子」，光给个文件夹名等于让人瞎猜。
// 统计只针对**当前层**（不递归），所以零额外请求。
func (s *Server) browseStorage(w http.ResponseWriter, r *http.Request, storageID string) {
	st, err := s.Store.GetStorage(storageID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "存储源不存在")
		return
	}
	p := openlist.NormalizePath(r.URL.Query().Get("path"))
	objs, err := s.clientFor(st).List(p, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeErr(w, http.StatusBadGateway, browseErrMsg(p, err))
		return
	}
	openlist.SortByName(objs)

	dirs := make([]map[string]interface{}, 0, len(objs))
	var videoCount, strmCount int
	for _, o := range objs {
		if o.Name == "." || o.Name == ".." || o.Name == "" {
			continue
		}
		if o.IsDir {
			dirs = append(dirs, map[string]interface{}{
				"name":     o.Name,
				"path":     path.Join(p, o.Name),
				"modified": int64(o.Modified),
			})
			continue
		}
		switch {
		case isStrmName(o.Name):
			strmCount++
		case isVideoName(o.Name):
			videoCount++
		}
	}
	writeJSON(w, map[string]interface{}{
		"path":        p,
		"parent":      parentPath(p),
		"dirs":        dirs,
		"video_count": videoCount,
		"strm_count":  strmCount,
		// suggest_mode 给前端一个建库模式的默认值，省得用户选错模式扫出个空库。
		"suggest_mode": suggestMode(videoCount, strmCount),
	})
}

// browseErrMsg 与扫描器共用一套人话翻译，保证「浏览失败」和「扫描失败」口径一致。
func browseErrMsg(p string, err error) string {
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "not found"), strings.Contains(low, "404"):
		return "网盘里找不到路径「" + p + "」"
	case strings.Contains(low, "401"), strings.Contains(low, "unauthorized"):
		return "OpenList 鉴权失败，请检查 Token"
	}
	return "读取目录失败: " + err.Error()
}

func parentPath(p string) string {
	if p == "/" || p == "" {
		return ""
	}
	return openlist.NormalizePath(path.Dir(p))
}

func isVideoName(name string) bool {
	switch strings.ToLower(extOf(name)) {
	case "mp4", "mkv", "webm", "mov", "avi", "ts", "m2ts", "flv":
		return true
	}
	return false
}

func isStrmName(name string) bool {
	e := strings.ToLower(extOf(name))
	return e == "strm" || e == "cas"
}

func extOf(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return ""
}

// suggestMode 依据目录里实际有什么，推荐建库模式。
func suggestMode(video, strm int) string {
	switch {
	case video > 0 && strm > 0:
		return string(model.ModeMixed)
	case strm > 0:
		return string(model.ModeStrm)
	case video > 0:
		return string(model.ModeNative)
	}
	return ""
}

// testStorage 不落盘，仅验证连通性并返回已挂载网盘列表。
func (s *Server) testStorage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type    string `json:"type"`
		BaseURL string `json:"base_url"`
		Token   string `json:"token"`
		SignKey string `json:"sign_key"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体错误")
		return
	}
	var cl openlist.FSClient
	if body.Type == "webdav" {
		cl = webdav.New(body.BaseURL, body.Token)
	} else {
		cl = &openlist.Client{BaseURL: body.BaseURL, Token: body.Token, SignKey: body.SignKey}
	}
	drives, err := cl.ListDrives()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "drives": drives})
}

// --- 媒体库 ---

func (s *Server) handleLibraries(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		list, _ := s.Store.ListLibraries()
		u, _ := s.requireUser(r)
		// 权限隔离：非管理员只返回自己可访问的媒体库。
		if !u.IsAdmin && len(u.AllowedLibs) > 0 {
			filtered := list[:0]
			for _, l := range list {
				if u.CanAccess(l.ID) {
					filtered = append(filtered, l)
				}
			}
			list = filtered
		}
		writeJSON(w, list)
	case len(parts) == 2 && r.Method == http.MethodPost:
		cur, _ := s.requireUser(r)
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可创建媒体库")
			return
		}
		var lib model.Library
		if err := readJSON(r, &lib); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体错误")
			return
		}
		lib.ID = auth.GenID("lib")
		lib.CreatedAt = model.NowMillis()
		if lib.Mode == "" {
			lib.Mode = model.ModeNative
		}
		// 存之前就把路径洗干净，避免「列表里显示的路径」和「实际扫描用的路径」不一致。
		lib.RootPath = openlist.NormalizePath(lib.RootPath)
		_ = s.Store.SaveLibrary(lib)
		writeJSON(w, lib)
	case len(parts) == 3 && r.Method == http.MethodDelete:
		cur, _ := s.requireUser(r)
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可删除媒体库")
			return
		}
		if err := s.Store.DeleteLibrary(parts[2]); err != nil {
			writeErr(w, http.StatusNotFound, "媒体库不存在")
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	case len(parts) == 3 && r.Method == http.MethodPut:
		cur, _ := s.requireUser(r)
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可编辑媒体库")
			return
		}
		existing, err := s.Store.GetLibrary(parts[2])
		if err != nil {
			writeErr(w, http.StatusNotFound, "媒体库不存在")
			return
		}
		var patch struct {
			Name      *string `json:"name"`
			Icon      *string `json:"icon"`
			Color     *string `json:"color"`
			SortOrder *int    `json:"sort_order"`
			ScanRate  *float64 `json:"scan_rate"`
		}
		if err := readJSON(r, &patch); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体错误")
			return
		}
		if patch.Name != nil {
			existing.Name = *patch.Name
		}
		if patch.Icon != nil {
			existing.Icon = *patch.Icon
		}
		if patch.Color != nil {
			existing.Color = *patch.Color
		}
		if patch.SortOrder != nil {
			existing.SortOrder = *patch.SortOrder
		}
		if patch.ScanRate != nil {
			existing.ScanRate = *patch.ScanRate
		}
		_ = s.Store.SaveLibrary(existing)
		writeJSON(w, existing)
	case len(parts) == 4 && parts[3] == "items" && r.Method == http.MethodGet:
		lib, err := s.Store.GetLibrary(parts[2])
		if err != nil {
			writeErr(w, http.StatusNotFound, "媒体库不存在")
			return
		}
		u, _ := s.requireUser(r)
		if !u.CanAccess(lib.ID) {
			writeErr(w, http.StatusForbidden, "无权访问该媒体库")
			return
		}
		items, _ := s.Store.ListMediaItems(lib.ID)
		writeJSON(w, s.filterItemsForUser(u, items))
	case len(parts) == 4 && parts[3] == "scan" && r.Method == http.MethodGet:
		// 返回该媒体库最近一次扫描任务，供海报墙自动轮询进度。
		job, err := s.Store.GetLatestScanJob(parts[2])
		if err != nil {
			writeErr(w, http.StatusNotFound, "尚无扫描任务")
			return
		}
		writeJSON(w, job)
	case len(parts) == 4 && parts[3] == "scan" && r.Method == http.MethodPost:
		s.startScan(w, r, parts[2])
	default:
		writeErr(w, http.StatusNotFound, "unknown libraries route")
	}
}

// --- 扫描 ---

func (s *Server) startScan(w http.ResponseWriter, r *http.Request, libID string) {
	cur, _ := s.requireUser(r)
	if !cur.IsAdmin {
		writeErr(w, http.StatusForbidden, "仅管理员可触发扫描")
		return
	}
	lib, err := s.Store.GetLibrary(libID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "媒体库不存在")
		return
	}
	st, err := s.Store.GetStorage(lib.StorageID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "媒体库未绑定存储源")
		return
	}
	storages, _ := s.Store.ListStorages()
	rewrites, _ := s.Store.ListPathRewrites()
	rate := lib.ScanRate
	if rate <= 0 {
		rate = st.RateLimit
	}
	if rate <= 0 {
		rate = s.Cfg.DefaultRate
	}
	if !s.tryLockScan(lib.ID) {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]interface{}{
			"job_id": "job-" + lib.ID, "status": "running",
			"error": "该媒体库正在扫描中，请等待本次扫描结束",
		})
		return
	}
	cl := s.clientFor(st)

	// 预检：扫描是异步的，根目录读不到的话用户要等到轮询才知道，
	// 中间那段时间只会看到一个空白页面在转圈。这里先同步探一次，
	// 路径错了立刻退 400 并说清楚错在哪。
	lib.RootPath = openlist.NormalizePath(lib.RootPath)
	if _, err := cl.List(lib.RootPath, false); err != nil {
		s.unlockScan(lib.ID)
		job := model.ScanJob{
			ID: "job-" + lib.ID, LibraryID: lib.ID, Status: "failed",
			StartedAt: model.NowMillis(), FinishedAt: model.NowMillis(),
			Error: scanner.FriendlyErr(lib.RootPath, err),
		}
		_ = s.Store.SaveScanJob(job)
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]interface{}{
			"job_id": job.ID, "status": "failed", "error": job.Error,
		})
		return
	}
	searcher := s.newSearcher()
	// 用脱离请求的 context：避免 HTTP 响应返回后请求 ctx 被取消而中断后台扫描。
	bg := context.Background()
	go func() {
		defer s.unlockScan(lib.ID)
		// 后台协程必须自带 recover：Go 里任何 goroutine panic 都会终止**整个进程**，
		// 而容器 restart 策略会立刻把它拉起来，前端再次触发扫描 → 又 panic，
		// 于是形成无限重启。这里兜住，把故障限制在这一次扫描内。
		defer func() {
			if e := recover(); e != nil {
				log.Printf("[scan] 扫描协程 panic 已恢复，媒体库=%s: %v\n%s", lib.ID, e, debug.Stack())
				if job, err := s.Store.GetScanJob("job-" + lib.ID); err == nil {
					job.Status = "failed"
					job.FinishedAt = model.NowMillis()
					_ = s.Store.UpdateScanJob(job)
				}
			}
		}()
		_ = scanner.Scan(bg, lib, s.Store, cl, storages, rewrites, rate, searcher, nil)
	}()
	writeJSON(w, map[string]interface{}{"job_id": "job-" + lib.ID, "status": "running"})
}

// --- 条目 / 海报墙 ---

func (s *Server) handleItems(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 3 && r.Method == http.MethodGet:
		item, err := s.Store.GetMediaItem(parts[2])
		if err != nil {
			writeErr(w, http.StatusNotFound, "条目不存在")
			return
		}
		u, _ := s.requireUser(r)
		// 权限隔离：非管理员无权查看不可访问库的条目；儿童模式下限制级条目不可见。
		if !u.CanAccess(item.LibraryID) {
			writeErr(w, http.StatusForbidden, "无权访问该条目")
			return
		}
		if u.ChildMode && item.Rating > childModeMaxRating {
			writeErr(w, http.StatusForbidden, "儿童模式下不可查看该条目")
			return
		}
		files, _ := s.Store.ListMediaFiles(item.ID)
		// 顺手带上每个文件的观看进度和该条目的收藏状态，
		// 省得前端为了画个进度条再打 N 次请求。
		progress := map[string]map[string]int{}
		for _, f := range files {
			if rec, err := s.Store.GetPlayRecord(u.ID, f.ID); err == nil && rec.Position > 0 {
				progress[f.ID] = map[string]int{"position": rec.Position, "duration": rec.Duration}
			}
		}
		favored := false
		if favs, err := s.Store.ListFavorites(u.ID); err == nil {
			for _, f := range favs {
				if f.ItemID == item.ID {
					favored = true
					break
				}
			}
		}
		writeJSON(w, map[string]interface{}{
			"item": item, "files": files, "progress": progress, "favored": favored,
		})
	case len(parts) == 4 && parts[3] == "play" && r.Method == http.MethodGet:
		s.playItem(w, r, parts[2])
	case len(parts) == 4 && parts[3] == "poster" && r.Method == http.MethodGet:
		s.serveItemImage(w, r, parts[2], "poster")
	case len(parts) == 4 && parts[3] == "backdrop" && r.Method == http.MethodGet:
		s.serveItemImage(w, r, parts[2], "backdrop")
	case len(parts) == 4 && parts[3] == "rescrape" && r.Method == http.MethodPost:
		s.rescrapeItem(w, r, parts[2])
	case len(parts) == 4 && parts[3] == "match" && r.Method == http.MethodPost:
		s.matchItem(w, r, parts[2])
	default:
		writeErr(w, http.StatusNotFound, "unknown items route")
	}
}

// searchItems 全局搜索 + 筛选 + 排序：GET /api/search?q=&kind=&library=&sort=&limit=&offset=
//
// 返回 {items: [...], total: N}，前端据此做分页与「共 N 条」展示。
// limit 上限 500，防止恶意请求一次性拉全库。
// sort 支持 title / year / rating / recent，默认按标题。
func (s *Server) searchItems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	u, _ := s.requireUser(r)
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	libFilter := strings.TrimSpace(r.URL.Query().Get("library"))
	sortBy := strings.TrimSpace(r.URL.Query().Get("sort"))
	lim := 0
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lim = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	// limit 上限保护：一次性拉全库既浪费带宽又可能把弱设备拖垮。
	const maxLimit = 500
	if lim == 0 || lim > maxLimit {
		lim = maxLimit
	}

	// SQLite 存储：搜索 + 计数都下沉到 SQL，避免全库线性扫描。
	if searcher, ok := s.Store.(interface {
		SearchMediaItems(q, kind, libID, sortBy string, offset, limit int) ([]model.MediaItem, error)
		CountMediaItems(q, kind, libID string) (int, error)
	}); ok {
		items, err := searcher.SearchMediaItems(q, kind, libFilter, sortBy, offset, lim)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "搜索失败")
			return
		}
		total, err := searcher.CountMediaItems(q, kind, libFilter)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "计数失败")
			return
		}
		filtered := s.filterItemsForUser(u, items)
		// 非管理员有库白名单/儿童模式过滤时，total 也需要按过滤后口径重算。
		// 保守做法：过滤后数量少于本页 limit 时 total 取 offset+len(filtered)，
		// 足够前端判断「是否还有下一页」；管理员无过滤则 total 精确。
		if !u.IsAdmin && total > offset+len(filtered) {
			total = offset + len(filtered)
		}
		writeJSON(w, map[string]interface{}{"items": filtered, "total": total})
		return
	}

	// JSON store 回退：全量过滤排序后再分页
	libs, _ := s.Store.ListLibraries()
	out := []model.MediaItem{}
	for _, lib := range libs {
		if libFilter != "" && lib.ID != libFilter {
			continue
		}
		items, _ := s.Store.ListMediaItems(lib.ID)
		for _, it := range items {
			if kind != "" && string(it.Kind) != kind {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(it.Title), q) &&
				!strings.Contains(strings.ToLower(it.Overview), q) {
				continue
			}
			out = append(out, it)
		}
	}
	out = s.filterItemsForUser(u, out)
	sort.SliceStable(out, func(i, j int) bool {
		switch sortBy {
		case "year":
			if out[i].Year != out[j].Year {
				return out[i].Year > out[j].Year
			}
		case "rating":
			if out[i].Rating != out[j].Rating {
				return out[i].Rating > out[j].Rating
			}
		case "recent":
			if out[i].CreatedAt != out[j].CreatedAt {
				return out[i].CreatedAt > out[j].CreatedAt
			}
		}
		return out[i].Title < out[j].Title
	})
	total := len(out)
	if offset > 0 && offset < len(out) {
		out = out[offset:]
	}
	if lim > 0 && lim < len(out) {
		out = out[:lim]
	}
	writeJSON(w, map[string]interface{}{"items": out, "total": total})
}

// matchItem 手动把条目绑定到指定 TMDB ID 并立即重刮削：
// POST /api/items/:id/match  {"tmdb_id":27205,"kind":"movie"}
//
// 自动匹配再准也有认错的时候（同名翻拍、中文译名不一致、系列续作），
// 之前只能去网盘目录里手写 .vidrive.json 才能纠正 —— 对普通用户等于没救。
// 仅管理员：会修改全局条目的元数据。
func (s *Server) matchItem(w http.ResponseWriter, r *http.Request, id string) {
	cur, _ := s.requireUser(r)
	if !cur.IsAdmin {
		writeErr(w, http.StatusForbidden, "仅管理员可手动匹配")
		return
	}
	item, err := s.Store.GetMediaItem(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "条目不存在")
		return
	}
	var body struct {
		TMDBID int64  `json:"tmdb_id"`
		Kind   string `json:"kind"`
	}
	if err := readJSON(r, &body); err != nil || body.TMDBID <= 0 {
		writeErr(w, http.StatusBadRequest, "需要有效的 tmdb_id")
		return
	}
	searcher := s.newSearcher()
	if searcher == nil {
		writeErr(w, http.StatusBadRequest, "未配置 TMDB API Key，无法手动匹配")
		return
	}
	if body.Kind == string(model.KindMovie) || body.Kind == string(model.KindSeries) {
		item.Kind = model.MediaKind(body.Kind)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	meta, err := searcher.ByID(ctx, item.Kind == model.KindSeries, body.TMDBID)
	if err != nil || meta == nil {
		writeErr(w, http.StatusBadGateway, "按 ID 拉取 TMDB 详情失败，请检查 ID 与类型是否匹配")
		return
	}
	item.TMDBID = body.TMDBID
	if meta.Title != "" {
		item.Title = meta.Title
	}
	if meta.Year != 0 {
		item.Year = meta.Year
	}
	if meta.Overview != "" {
		item.Overview = meta.Overview
	}
	if meta.Rating > 0 {
		item.Rating = meta.Rating
	}
	// 本地图仍然最高优先级，没有本地图才用 TMDB 的。
	if item.PosterPath == "" && meta.PosterPath != "" {
		item.PosterURL = tmdb.ImageURL(meta.PosterPath, "w500")
	}
	if item.BackdropPath == "" && meta.BackdropPath != "" {
		item.BackdropURL = tmdb.ImageURL(meta.BackdropPath, "w1280")
	}
	if err := s.Store.SaveMediaItem(item); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "item": item})
}

// --- 扫描任务状态 ---

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 3 || r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "unknown scan route")
		return
	}
	job, err := s.Store.GetScanJob(parts[2])
	if err != nil {
		writeErr(w, http.StatusNotFound, "扫描任务不存在")
		return
	}
	writeJSON(w, job)
}

// --- 继续观看 ---

// handleContinue 返回「继续观看」列表。
//
// 老实现只吐裸 PlayRecord（file_id + 秒数），前端拿不到片名和海报，
// 只能显示「进度 37% / 1203s」这种东西——用户根本认不出是哪部片。
// 这里顺带把所属条目和文件补齐，前端一次请求就能渲染完整卡片。
func (s *Server) handleContinue(w http.ResponseWriter, r *http.Request) {
	u, _ := s.requireUser(r)
	recs, _ := s.Store.ListContinue(u.ID)
	out := make([]map[string]interface{}, 0, len(recs))
	for _, rec := range recs {
		row := map[string]interface{}{
			"id": rec.ID, "file_id": rec.FileID,
			"position": rec.Position, "duration": rec.Duration, "updated_at": rec.UpdatedAt,
		}
		f, err := s.Store.GetMediaFile(rec.FileID)
		if err != nil {
			continue // 文件已被删库清理，跳过这条历史
		}
		row["season_no"], row["episode_no"] = f.SeasonNo, f.EpisodeNo
		row["file_name"] = path.Base(f.Path)
		if item, err := s.Store.GetMediaItem(f.ItemID); err == nil {
			row["item"] = item
		}
		out = append(out, row)
	}
	writeJSON(w, out)
}

// --- 收藏 ---

func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request, parts []string) {
	u, _ := s.requireUser(r)

	// DELETE /api/favorites/:itemID —— 以前只能收藏不能取消，收错了只能手改 JSON。
	if len(parts) == 3 && r.Method == http.MethodDelete {
		if err := s.Store.DeleteFavorite(u.ID, parts[2], model.FavoriteKind(r.URL.Query().Get("kind"))); err != nil {
			writeErr(w, http.StatusNotFound, "未收藏该条目")
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
		return
	}

	switch r.Method {
	case http.MethodGet:
		list, _ := s.Store.ListFavorites(u.ID)
		out := make([]map[string]interface{}, 0, len(list))
		for _, f := range list {
			row := map[string]interface{}{"id": f.ID, "item_id": f.ItemID, "kind": f.Kind}
			item, err := s.Store.GetMediaItem(f.ItemID)
			if err != nil {
				continue // 条目已随媒体库删除，别在收藏页留空壳
			}
			row["item"] = item
			out = append(out, row)
		}
		writeJSON(w, out)
	case http.MethodPost:
		var f model.Favorite
		if err := readJSON(r, &f); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体错误")
			return
		}
		if f.ItemID == "" {
			writeErr(w, http.StatusBadRequest, "缺少 item_id")
			return
		}
		f.ID = auth.GenID("fav")
		f.UserID = u.ID
		if f.Kind == "" {
			f.Kind = model.FavFavorite
		}
		_ = s.Store.SaveFavorite(f)
		writeJSON(w, f)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// --- 路径重写规则 ---

// handleUsers 用户管理（仅管理员）：GET 列表 / POST 新建 / PUT 修改 / DELETE 删除。
// 子账号支持：媒体库白名单（AllowedLibs）+ 儿童模式（ChildMode）。
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, parts []string) {
	cur, ok := s.requireUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "未登录")
		return
	}
	// 所有写操作仅管理员
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可查看用户列表")
			return
		}
		list, _ := s.Store.ListUsers()
		// 不下发密码哈希，避免泄露
		sanitized := make([]map[string]interface{}, 0, len(list))
		for _, u := range list {
			sanitized = append(sanitized, map[string]interface{}{
				"id": u.ID, "username": u.Username, "is_admin": u.IsAdmin,
				"child_mode": u.ChildMode, "allowed_libs": u.AllowedLibs,
			})
		}
		writeJSON(w, sanitized)
		return

	case len(parts) == 2 && r.Method == http.MethodPost:
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可创建用户")
			return
		}
		var body struct {
			Username    string   `json:"username"`
			Password    string   `json:"password"`
			IsAdmin     bool     `json:"is_admin"`
			ChildMode   bool     `json:"child_mode"`
			AllowedLibs []string `json:"allowed_libs"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体错误")
			return
		}
		body.Username = strings.TrimSpace(body.Username)
		if body.Username == "" || body.Password == "" {
			writeErr(w, http.StatusBadRequest, "用户名和密码不能为空")
			return
		}
		if _, err := s.Store.GetUserByName(body.Username); err == nil {
			writeErr(w, http.StatusConflict, "用户名已存在")
			return
		}
		nu := model.User{
			ID:          auth.GenID("u"),
			Username:    body.Username,
			Password:    auth.HashPassword(body.Password),
			IsAdmin:     body.IsAdmin,
			ChildMode:   body.ChildMode,
			AllowedLibs: body.AllowedLibs,
		}
		if err := s.Store.SaveUser(nu); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存用户失败")
			return
		}
		writeJSON(w, map[string]interface{}{"id": nu.ID, "username": nu.Username, "ok": true})
		return

	case len(parts) == 3 && r.Method == http.MethodPut:
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可修改用户")
			return
		}
		u, err := s.Store.GetUserByName(parts[2])
		if err != nil {
			writeErr(w, http.StatusNotFound, "用户不存在")
			return
		}
		var body struct {
			Password    string   `json:"password"` // 空 = 不改密码
			IsAdmin     *bool    `json:"is_admin"`
			ChildMode   *bool    `json:"child_mode"`
			AllowedLibs []string `json:"allowed_libs"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体错误")
			return
		}
		if body.Password != "" {
			u.Password = auth.HashPassword(body.Password)
		}
		if body.IsAdmin != nil {
			u.IsAdmin = *body.IsAdmin
		}
		if body.ChildMode != nil {
			u.ChildMode = *body.ChildMode
		}
		u.AllowedLibs = body.AllowedLibs
		if err := s.Store.SaveUser(u); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存用户失败")
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "username": u.Username})
		return

	case len(parts) == 3 && r.Method == http.MethodDelete:
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可删除用户")
			return
		}
		if cur.Username == parts[2] {
			writeErr(w, http.StatusBadRequest, "不能删除自己")
			return
		}
		target, err := s.Store.GetUserByName(parts[2])
		if err != nil {
			writeErr(w, http.StatusNotFound, "用户不存在")
			return
		}
		if err := s.Store.DeleteUser(target.ID); err != nil {
			writeErr(w, http.StatusNotFound, "用户不存在")
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
		return
	}
	writeErr(w, http.StatusNotFound, "unknown users route")
}

func (s *Server) handleRewrites(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		list, _ := s.Store.ListPathRewrites()
		writeJSON(w, list)
	case len(parts) == 2 && r.Method == http.MethodPost:
		cur, _ := s.requireUser(r)
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可创建路径重写")
			return
		}
		var rw model.PathRewrite
		if err := readJSON(r, &rw); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体错误")
			return
		}
		rw.ID = auth.GenID("rw")
		if rw.Priority == 0 {
			rw.Priority = 100
		}
		_ = s.Store.SavePathRewrite(rw)
		writeJSON(w, rw)
	default:
		writeErr(w, http.StatusNotFound, "unknown rewrites route")
	}
}

// sensitiveSettings 普通用户不应看到的设置 key（含 API Key / 密钥等）。
var sensitiveSettings = map[string]bool{
	"tmdb_api_key": true,
}

// handleSettings 读写全局设置（如用户自填的 TMDB Key）。
// GET 返回全部；PUT/POST 接收 {key:value} 增量保存。
// 普通用户 GET 时敏感 key 会被脱敏（返回空串），写操作仅管理员。
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	cur, _ := s.requireUser(r)
	switch r.Method {
	case http.MethodGet:
		list, _ := s.Store.ListSettings()
		if !cur.IsAdmin {
			for k := range list {
				if sensitiveSettings[k] {
					list[k] = ""
				}
			}
		}
		writeJSON(w, list)
	case http.MethodPut, http.MethodPost:
		if !cur.IsAdmin {
			writeErr(w, http.StatusForbidden, "仅管理员可修改全局设置")
			return
		}
		var body map[string]string
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体错误")
			return
		}
		for k, v := range body {
			_ = s.Store.SaveSetting(k, v)
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// testTMDB 用当前（或请求体里临时指定的）Key/根地址做一次真实搜索，
// 让用户在扫描前就能确认「Key 有效 + 网络可达」，而不是扫完发现全是灰底占位图。
//
// 请求体可选：{"api_key":"...","api_base":"..."}；缺省则用已保存的配置。
// 仅管理员：会消耗 TMDB API 配额，且能探测已配置 Key 的有效性。
func (s *Server) testTMDB(w http.ResponseWriter, r *http.Request) {
	cur, _ := s.requireUser(r)
	if !cur.IsAdmin {
		writeErr(w, http.StatusForbidden, "仅管理员可测试 TMDB 连接")
		return
	}
	var body struct {
		APIKey  string `json:"api_key"`
		APIBase string `json:"api_base"`
	}
	_ = readJSON(r, &body)
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		key = s.tmdbKey()
	}
	if key == "" {
		writeErr(w, http.StatusBadRequest, "未配置 TMDB API Key")
		return
	}
	base := strings.TrimSpace(body.APIBase)
	if base == "" {
		base = s.tmdbBase()
	}

	c := tmdb.NewWithBase(key, base)
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	m, err := c.Search(ctx, "movie", "Inception", 2010)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "TMDB 不可用: "+err.Error())
		return
	}
	if m == nil {
		writeErr(w, http.StatusBadGateway, "TMDB 返回空结果（Key 可能受限）")
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok":       true,
		"endpoint": c.ActiveBase(), // 实际生效的地址，便于用户判断是否走了备用域名
		"sample":   m.Title,
		"poster":   tmdb.ImageURL(m.PosterPath, "w500"),
	})
}

// --- 播放 ---

// proxySkipHeaders 反代上游响应时应丢弃的头：逐跳头、Set-Cookie，
// 以及 Content-Length（由服务端按实际写出字节重算）。键为 CanonicalHeaderKey 形式。
var proxySkipHeaders = map[string]bool{
	"Connection":         true,
	"Proxy-Connection":   true,
	"Keep-Alive":         true,
	"Proxy-Authenticate": true,
	"Proxy-Authorization": true,
	"Te":                 true,
	"Trailer":            true,
	"Transfer-Encoding":  true,
	"Upgrade":            true,
	"Set-Cookie":         true,
	"Content-Length":     true,
}

// handlePlay 仅处理 /api/play/proxy（直链代理转发，L1）。
// 单文件播放决策走 /api/items/:id/play（见 playItem）。
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) >= 3 && parts[2] == "remux" && r.Method == http.MethodGet {
		s.handleRemux(w, r)
		return
	}
	if len(parts) >= 3 && parts[2] == "transcode" && r.Method == http.MethodGet {
		s.handleTranscode(w, r)
		return
	}
	if len(parts) >= 3 && parts[2] == "subtitle" && r.Method == http.MethodGet {
		s.handleSubtitle(w, r)
		return
	}
	if len(parts) >= 3 && parts[2] == "hls" && r.Method == http.MethodGet {
		s.handleHLS(w, r, parts)
		return
	}
	if len(parts) >= 3 && parts[2] == "proxy" && r.Method == http.MethodGet {
		raw := r.URL.Query().Get("u")
		if raw == "" {
			writeErr(w, http.StatusBadRequest, "缺少 u 参数")
			return
		}
		target, err := parseOutboundURL(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "构造请求失败")
			return
		}
		// 透传 Range，播放器拖进度条要用。
		if rg := r.Header.Get("Range"); rg != "" {
			req.Header.Set("Range", rg)
		}
		resp, err := s.proxyClientFor(target).Do(req)
		if err != nil {
			if errors.Is(err, errBlockedTarget) {
				writeErr(w, http.StatusForbidden, errBlockedTarget.Error())
				return
			}
			writeErr(w, http.StatusBadGateway, "代理失败: "+err.Error())
			return
		}
		defer resp.Body.Close()
		// 只透传「端到端」头，丢弃逐跳头与可能造成问题的头：
		//   - Connection/Keep-Alive/Te/Trailer/Transfer-Encoding/Upgrade 等逐跳头
		//     绝不该原样转发。
		//   - Set-Cookie 落到 NewMovie 域名会让后端随意给前端种 Cookie。
		//   - Content-Length 不转发，改由服务端按实际写出字节重算：否则上游的
		//     声明长度与实际 io.Copy 写出不符会触发 "wrote more than declared" panic；
		//     Range 响应里 Content-Range 仍保留，播放器拖拽不受影响。
		for k, vs := range resp.Header {
			if proxySkipHeaders[http.CanonicalHeaderKey(k)] {
				continue
			}
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	if len(parts) >= 3 && parts[2] == "record" && r.Method == http.MethodPost {
		s.saveRecord(w, r)
		return
	}
	writeErr(w, http.StatusNotFound, "unknown play route")
}

// allowedLocalRoot 判断本地文件路径是否落在允许目录内。
// 允许目录 = 各存储配置的 local_root（自动纳入，对应其本地挂载根）+ 可选的
// VIDRIVE_LOCAL_ROOTS（全局兜底）。这样用户只需在存储配置里填 local_root，
// file:// 直读盘即自动放行，不必再单独维护全局白名单。根目录 / 永远不允许作为放行范围。
func (s *Server) allowedLocalRoot(p string) bool {
	clean := path.Clean(p)
	roots := append([]string{}, s.Cfg.LocalRoots...)
	if sts, err := s.Store.ListStorages(); err == nil {
		for _, st := range sts {
			if st.LocalRoot != "" {
				roots = append(roots, st.LocalRoot)
			}
		}
	}
	for _, root := range roots {
		r := path.Clean(root)
		if r == "/" {
			continue // 不允许用根目录作为放行范围
		}
		if clean == r || strings.HasPrefix(clean, r+"/") {
			return true
		}
	}
	return false
}

// openPlaySource 打开播放源，返回可读流。支持两类来源：
//   - http/https 远程直链：受 SSRF 守卫约束（netguard）。
//   - file:// 本地绝对路径：供 CloudDrive2 等本地 strm 直接读盘，不经过网络。
//
// 返回值：(流, HTTP 状态码, 错误)。成功时状态码为 0；失败时流为 nil、状态码用于回写。
// 取流用的 context 来自请求；HLS 等长耗时切片的场景改用 openPlaySourceCtx 传入
// context.Background()，让源流在 HTTP 请求返回后依然存活（见 handleHLS）。
func (s *Server) openPlaySource(r *http.Request, raw string) (io.ReadCloser, int, error) {
	return s.openPlaySourceCtx(r.Context(), raw)
}

// openPlaySourceCtx 是 openPlaySource 的 context 可控版本，供后台任务（如 HLS 切片）
// 用独立的生命周期持有源流。
func (s *Server) openPlaySourceCtx(ctx context.Context, raw string) (io.ReadCloser, int, error) {
	if strings.HasPrefix(raw, "file://") {
		p := strings.TrimPrefix(raw, "file://")
		if !path.IsAbs(p) {
			return nil, http.StatusBadRequest, errors.New("本地文件路径必须绝对路径")
		}
		// 安全：本地文件读取仅放行配置目录（VIDRIVE_LOCAL_ROOTS）内的路径，
		// 防任意文件读取（SSRF 的一种）。未配置则一律拒绝。
		if !s.allowedLocalRoot(p) {
			return nil, http.StatusForbidden, errors.New("本地文件路径不在允许目录内（配置 VIDRIVE_LOCAL_ROOTS）")
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, http.StatusBadGateway, fmt.Errorf("读取本地文件失败: %w", err)
		}
		return f, 0, nil
	}
	target, err := parseOutboundURL(raw)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	client := s.proxyClientFor(target)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("构造请求失败")
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errBlockedTarget) {
			return nil, http.StatusForbidden, errBlockedTarget
		}
		return nil, http.StatusBadGateway, fmt.Errorf("拉取源失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, http.StatusBadGateway, fmt.Errorf("源返回 %d", resp.StatusCode)
	}
	return resp.Body, 0, nil
}

// handleRemux 实时重封装：拉取源（受 SSRF 守卫约束，或本地文件），用 ffmpeg -c copy
// 转成 fragmented MP4 流式回写给浏览器。仅做容器转换、不重新编码，开销极低。
// 这样 MKV（h264/aac、hevc/aac 等）也能在页内 ArtPlayer 直接播，不必甩外部播放器。
func (s *Server) handleRemux(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("u")
	if raw == "" {
		writeErr(w, http.StatusBadRequest, "缺少 u 参数")
		return
	}
	src, status, err := s.openPlaySource(r, raw)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	defer src.Close()

	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "服务端未安装 ffmpeg，无法转封装（请在镜像中安装 ffmpeg）")
		return
	}

	args := []string{"-loglevel", "error", "-i", "pipe:0"}
	// aac=1：视频保持拷贝（HEVC/H264 不变），仅把不兼容 MP4 的音轨（DTS/TrueHD/Atmos/FLAC）
	// 实时转成 AAC。4K 蓝光原盘（HEVC + DTS-HD/TrueHD/Atmos）借此页内可播，且几乎不耗服务端算力。
	if r.URL.Query().Get("aac") == "1" {
		args = append(args, "-c:v", "copy", "-c:a", "aac", "-b:a", "320k")
	} else {
		args = append(args, "-c", "copy")
	}
	args = append(args, "-f", "mp4", "-movflags", "+frag_keyframe+empty_moov+delay_moov")
	// atrack：仅抽取指定音轨（多音轨 MKV 切换语言用）。
	// 指定后不再 copy 全部流，而是显式映射视频 + 该音轨。
	if at := r.URL.Query().Get("atrack"); at != "" {
		if n, err := strconv.Atoi(at); err == nil && n >= 0 {
			args = append(args, "-map", "0:v:0", "-map", "0:a:"+strconv.Itoa(n))
		}
	}
	args = append(args, "pipe:1")
	s.streamFFmpeg(w, r, bin, args, src)
}

// handleTranscode 实时视频转码：拉取源，用 ffmpeg 把视频转 H.264、音轨转 AAC，
// 输出 fragmented MP4 流式回写。用于浏览器本身解不了视频编码（如 HEVC）且开启了
// 「允许视频转码」的场景，产物是任何浏览器都能播的 MP4，彻底解决页内播放。
// 开销显著高于重封装（重编码视频），故默认不开启（见 TranscodeEnabled / 设置页开关）。
func (s *Server) handleTranscode(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("u")
	if raw == "" {
		writeErr(w, http.StatusBadRequest, "缺少 u 参数")
		return
	}
	src, status, err := s.openPlaySource(r, raw)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	defer src.Close()

	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "服务端未安装 ffmpeg，无法转码（请在镜像中安装 ffmpeg）")
		return
	}
	if !s.transcodeOK {
		writeErr(w, http.StatusInternalServerError, "服务端 ffmpeg 缺少 libx264 编码器，无法转码（HEVC→H.264）。请换含 libx264 的 ffmpeg 构建，或关闭转码改用重封装/外部播放器")
		return
	}

	// 视频转 H.264（CRF 20 近无损画质），音轨转 AAC。产物任何浏览器可播。
	args := []string{"-loglevel", "error", "-i", "pipe:0",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20",
		"-c:a", "aac", "-b:a", "192k",
		"-movflags", "+frag_keyframe+empty_moov+delay_moov", "-f", "mp4"}
	// atrack：转码时把选定音轨映射进来（视频仍转 H.264）。
	if at := r.URL.Query().Get("atrack"); at != "" {
		if n, err := strconv.Atoi(at); err == nil && n >= 0 {
			args = append(args, "-map", "0:v:0", "-map", "0:a:"+strconv.Itoa(n))
		}
	}
	args = append(args, "pipe:1")
	s.streamFFmpeg(w, r, bin, args, src)
}

// streamFFmpeg 启动 ffmpeg 把 src 转封装/转码后流式写回响应。调用方负责关闭 src。
func (s *Server) streamFFmpeg(w http.ResponseWriter, r *http.Request, bin string, args []string, src io.Reader) {
	ctx := r.Context()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = src
	out, err := cmd.StdoutPipe()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "启动 ffmpeg 失败")
		return
	}
	var ffmpegErr bytes.Buffer
	cmd.Stderr = &ffmpegErr
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusInternalServerError, "启动 ffmpeg 失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	// io.Copy 在客户端断开（ctx 取消，ffmpeg 被杀）或 ffmpeg 退出时结束，忽略错误。
	if _, err := io.Copy(w, out); err != nil {
		_ = cmd.Wait()
		return
	}
	if err := cmd.Wait(); err != nil && ffmpegErr.Len() > 0 {
		log.Printf("ffmpeg: %s", ffmpegErr.String())
	}
}

// handleHLS 提供 HLS 按需切片的两个资源（分片用 ?key= 定位会话，与播放列表请求解耦）：
//   - /api/play/hls/index.m3u8?u=<源>&mode=remux|transcode[&aac=1][&atrack=N]&token=<鉴权>
//     触发（或复用）切片会话，等待索引生成后返回（分片 URL 已注入 key 与 token）。
//   - /api/play/hls/seg/<name>?key=<会话key>&token=<鉴权>
//     等待分片落盘后以静态文件服务（支持 Range，拖动精准）。
// key = sha256(源|模式|音轨)，同一文件重复播放复用同一 ffmpeg 会话（见 internal/hls）。
func (s *Server) handleHLS(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) < 4 {
		writeErr(w, http.StatusBadRequest, "HLS 路径格式错误")
		return
	}
	switch parts[3] {
	case hls.PlaylistName:
		raw := r.URL.Query().Get("u")
		mode := r.URL.Query().Get("mode")
		if raw == "" {
			writeErr(w, http.StatusBadRequest, "缺少 u 参数")
			return
		}
		aac := r.URL.Query().Get("aac") == "1"
		atrack := -1
		if v := r.URL.Query().Get("atrack"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				atrack = n
			}
		}
		// key 由 源+模式+音轨 稳定哈希，playlist 与 seg 请求据此定位同一会话。
		key := hls.KeyFor(raw, mode, atrack)
		if _, err := s.hlsMgr.Acquire(raw, mode, aac, atrack, s.hlsOpener(raw)); err != nil {
			writeErr(w, http.StatusBadGateway, "启动 HLS 生成失败: "+err.Error())
			return
		}
		path, err := s.hlsMgr.WaitPlaylist(key, hls.PlaylistWait)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "HLS 索引生成失败: "+err.Error())
			return
		}
		s.serveHLSPlaylist(w, r, path, key)
	case "seg":
		if len(parts) < 5 {
			writeErr(w, http.StatusBadRequest, "缺少分片名")
			return
		}
		name := parts[4]
		key := r.URL.Query().Get("key")
		if key == "" {
			writeErr(w, http.StatusBadRequest, "缺少 key 参数")
			return
		}
		path, err := s.hlsMgr.WaitSegment(key, name, hls.SegmentWait)
		if err != nil {
			writeErr(w, http.StatusNotFound, "HLS 分片不存在: "+err.Error())
			return
		}
		s.serveHLSSegment(w, r, path)
	default:
		writeErr(w, http.StatusNotFound, "未知 HLS 资源: "+parts[3])
	}
}

// hlsOpener 返回 SSRF 守卫后的取流闭包，供 HLS 会话在后台（与 HTTP 请求解耦的
// context.Background() 生命周期）持有源流。
func (s *Server) hlsOpener(raw string) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		src, _, err := s.openPlaySourceCtx(context.Background(), raw)
		return src, err
	}
}

// serveHLSPlaylist 读索引并改写分片 URL（注入 key 与 token），以 application/vnd.apple.mpegurl 返回。
func (s *Server) serveHLSPlaylist(w http.ResponseWriter, r *http.Request, path, key string) {
	content, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取 HLS 索引失败")
		return
	}
	tok := r.URL.Query().Get("token")
	content = hls.RewritePlaylist(content, tok, key)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(content); err != nil {
		log.Printf("[hls] 写索引失败: %v", err)
	}
}

// serveHLSSegment 以静态文件服务分片，支持 Range（拖动精准），分片为独立 GOP 故可随机起播。
func (s *Server) serveHLSSegment(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "分片不存在")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取分片状态失败")
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
}

// handleSubtitle 取外挂字幕字节并转成 WebVTT 返回给播放器。
// 字幕与媒体同存储源，经 OpenList 直链拉取后由 subtitle.Convert 统一转换（srt→vtt / ass→vtt）。
func (s *Server) handleSubtitle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fileID := q.Get("file")
	lang := q.Get("lang")
	if fileID == "" {
		writeErr(w, http.StatusBadRequest, "缺少 file 参数")
		return
	}
	f, err := s.Store.GetMediaFile(fileID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	if len(f.Subtitles) == 0 {
		writeErr(w, http.StatusNotFound, "该文件无外挂字幕")
		return
	}
	// 选字幕：精确匹配语言优先；未指定语言时优先 und（默认轨），否则第一条。
	var sub model.Subtitle
	matched := false
	for _, s2 := range f.Subtitles {
		if lang != "" && s2.Lang == lang {
			sub = s2
			matched = true
			break
		}
	}
	if !matched && lang == "" {
		for _, s2 := range f.Subtitles {
			if s2.Lang == "und" {
				sub = s2
				matched = true
				break
			}
		}
	}
	if !matched {
		sub = f.Subtitles[0]
	}

	st, err := s.Store.GetStorage(sub.StorageID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "存储源不存在")
		return
	}
	cl := s.clientFor(st)
	link, err := cl.GetLink(sub.Path, s.Cfg.ProxyRefresh)
	var rawURL string
	if err == nil && link.Data.RawURL != "" {
		rawURL = link.Data.RawURL
	}
	if rawURL == "" && st.SignKey != "" {
		rawURL, _ = cl.RawURL(sub.Path)
	}
	if rawURL == "" {
		writeErr(w, http.StatusBadGateway, "取字幕直链失败")
		return
	}
	target, err := parseOutboundURL(rawURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	resp, err := s.proxyClientFor(target).Do(req)
	if err != nil {
		if errors.Is(err, errBlockedTarget) {
			writeErr(w, http.StatusForbidden, errBlockedTarget.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, "拉取字幕失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeErr(w, http.StatusBadGateway, "字幕源返回 "+strconv.Itoa(resp.StatusCode))
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取字幕失败")
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := subtitle.Convert(sub.Ext, data, w); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
}

// probeFile 用 ffprobe 探测媒体：时长、音轨列表、编码。best-effort，
// ffprobe 缺失或超时/失败一律返回错误，由调用方标记 probe 跳过，避免每次播放都重试。
func (s *Server) probeFile(ctx context.Context, rawURL string) (dur int, audio []model.AudioTrack, vc, ac, container string, err error) {
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0, nil, "", "", "", fmt.Errorf("ffprobe 不可用")
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, bin, "-v", "error",
		"-show_entries", "stream=index,codec_type,codec_name:stream_tags=language,title:format=duration:format=format_name",
		"-of", "json", rawURL)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err = cmd.Run(); err != nil {
		return 0, nil, "", "", "", err
	}
	var probe struct {
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Tags      struct {
				Language string `json:"language"`
				Title    string `json:"title"`
			} `json:"tags"`
		} `json:"streams"`
		Format struct {
			Duration   string `json:"duration"`
			FormatName string `json:"format_name"`
		} `json:"format"`
	}
	if err = json.Unmarshal(out.Bytes(), &probe); err != nil {
		return 0, nil, "", "", "", err
	}
	if d, e := strconv.ParseFloat(probe.Format.Duration, 64); e == nil {
		dur = int(d)
	}
	// 容器从 ffprobe 的 format_name 归一（matroska→mkv，quicktime/mov/mp4→mp4 等）。
	// 这是「探测即真相」的关键：无扩展名的 strm 直链靠这一项认出真实容器，
	// 才能正确走重封装/转码，而不是因为文件名猜不出就被甩外部播放器。
	if c := normContainer(probe.Format.FormatName); c != "" {
		container = c
	}
	for _, st := range probe.Streams {
		switch st.CodecType {
		case "video":
			vc = normCodec(st.CodecName)
		case "audio":
			audio = append(audio, model.AudioTrack{
				Index: st.Index,
				Lang:  normLang(st.Tags.Language),
				Codec: st.CodecName,
				Title: st.Tags.Title,
			})
			if ac == "" {
				ac = normCodec(st.CodecName)
			}
		}
	}
	return dur, audio, vc, ac, container, nil
}

// sniffContainer 读媒体头部若干字节，靠魔数识别容器（移植自 Lunarr detectContainerFromMagic）。
// 仅作 ffprobe 之后的兜底：ffprobe 已能给容器时不会走到这里。best-effort——
// 取流失败、读取不足或无法识别都返回空串，交由后续分支（浏览器直链试播 / 外部播放器）兜底。
// 复用 openPlaySource 的 SSRF 守卫与本地目录白名单，不扩大既有取流的安全边界。
func (s *Server) sniffContainer(r *http.Request, raw string) string {
	src, _, err := s.openPlaySource(r, raw)
	if err != nil {
		return ""
	}
	defer src.Close()
	// 192 字节足以覆盖 mp4(8)/matroska(4)/avi(12)/ts(189) 的全部魔数判定。
	buf := make([]byte, 192)
	n, _ := io.ReadFull(src, buf)
	if n < 4 {
		return ""
	}
	return playback.SniffContainer(buf[:n])
}

// normContainer 把 ffprobe 的 format_name（常为逗号分隔列表，如 "matroska,webm"）
// 归一为 playback 包使用的容器名（mkv/mp4/webm）。未知格式返回空。
func normContainer(name string) string {
	for _, part := range strings.Split(name, ",") {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "matroska":
			return "mkv"
		case "mov", "mp4", "quicktime", "m4v":
			return "mp4"
		case "webm":
			return "webm"
		case "mpegts", "ts":
			return "ts"
		case "flv":
			return "flv"
		case "avi":
			return "avi"
		case "asf", "wmv":
			return "wmv"
		}
	}
	return ""
}

// normCodec 把 ffprobe 的编码名归一为 playback 包使用的命名（hevc→h265）。
func normCodec(c string) string {
	switch strings.ToLower(c) {
	case "hevc":
		return "h265"
	}
	return strings.ToLower(c)
}

// normLang 把 ffprobe 的语言标记（chi/eng/jpn...）归一为 ISO 639-1。
func normLang(l string) string {
	switch strings.ToLower(strings.TrimSpace(l)) {
	case "chi", "zho", "cn":
		return "zh"
	case "eng":
		return "en"
	case "jpn":
		return "ja"
	case "kor":
		return "ko"
	case "und", "":
		return "und"
	}
	return strings.ToLower(strings.TrimSpace(l))
}

func (s *Server) saveRecord(w http.ResponseWriter, r *http.Request) {
	u, _ := s.requireUser(r)
	var rec model.PlayRecord
	if err := readJSON(r, &rec); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体错误")
		return
	}
	rec.ID = "rec-" + rec.FileID
	rec.UserID = u.ID
	rec.UpdatedAt = model.NowMillis()
	_ = s.Store.SavePlayRecord(rec)
	writeJSON(w, rec)
}

// playItem 解析单个文件，返回五级降级决策 + 直链。
// containerExt 从 URL 或本地路径里取容器扩展名（mkv/mp4/m3u8…），忽略查询串。
// STRM 的容器在扫描时未必能拿到，播放前用来源 URL 补一次。
// 只从 URL「路径的最后一段」取扩展名——绝不可对整串取最后一个点，否则会把
// 主机里的点（如 127.0.0.1、example.com）误当扩展名分隔符，导致容器被解析成
// "1:5244/..." 这类垃圾值（内置 139cas 正是 127.0.0.1:5244，此 bug 会让所有
// 内置源 strm 的容器推断全错，进而被错误甩去外部播放器）。
func containerExt(u string) string {
	s := u
	// 先剥掉查询/片段
	if q := strings.IndexAny(s, "?#"); q >= 0 {
		s = s[:q]
	}
	// 再剥掉 scheme（http:// / file:// 等）
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// 只取路径最后一段（去掉 host 部分）
	seg := s
	if i := strings.LastIndex(s, "/"); i >= 0 {
		seg = s[i+1:]
	}
	// 仍残留 ':'（如 host:port）说明不是合法文件名段，无扩展名
	if strings.Contains(seg, ":") {
		return ""
	}
	if i := strings.LastIndex(seg, "."); i >= 0 {
		return strings.ToLower(seg[i+1:])
	}
	return ""
}

// rewriteBundledURL 把指向内置 139cas（127.0.0.1:5244，容器内、未对外暴露）的直链
// 改写为本服务同端口的 /openlist 反代路径。
//
// 2.0 同容器部署下，139cas 只监听 127.0.0.1:5244，浏览器/外部播放器跑在用户机器上，
// 根本连不到这个容器内部地址，于是原生 MP4/Strm（L0 直链）会黑屏、缺 ffmpeg 时
// 唤起外部播放器（L4）也会被甩到一个连不上的地址。NewMovie 在 8096 入口已挂载
// /openlist 反代把请求转发给 5244，因此只要把下发给浏览器的 URL 改写成同源的
// /openlist/...，浏览器就只跟 8096 打交道，由反代去取流（Range/流式都透传）。
//
// 仅当来源确为内置后端（s.Cfg.Bundled 且 host 等于 BundledURL 或回环地址）才改写；
// remux/transcode 是服务端内部取流（SSRF 守卫已放行 127.0.0.1 存储源），其 u 参数
// 里保留真实 5244 地址即可，不受本函数影响。外部 OpenList 的直链原样返回。
func (s *Server) rewriteBundledURL(rawURL string) string {
	if rawURL == "" || s.Cfg == nil || !s.Cfg.Bundled {
		return rawURL
	}
	base, err := url.Parse(s.Cfg.BundledURL)
	if err != nil || base.Host == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || !u.IsAbs() {
		return rawURL
	}
	host := strings.ToLower(u.Hostname())
	baseHost := strings.ToLower(base.Hostname())
	isBundledHost := host == baseHost ||
		host == "127.0.0.1" || host == "localhost" ||
		host == "[::1]" || host == "::1"
	if !isBundledHost {
		return rawURL
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	out := "/openlist" + p
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.Fragment
	}
	return out
}

func (s *Server) playItem(w http.ResponseWriter, r *http.Request, fileID string) {
	f, err := s.Store.GetMediaFile(fileID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	// 取源：原生文件走 GetLink；STRM 先用解析器归一化出真实来源（http / openlist / file）。
	// 这是 STRM 能在页内播放的关键——此前 playItem 对 strm 与原生文件一视同仁，
	// 而 http 直链型 strm 的 StorageID 为空，GetStorage("") 直接报错，自然只能甩外部播放器。
	var rawURL, directURL, triedPath string
	headers := map[string]string{}

	if f.Source == model.SrcStrm {
		// STRM 的本质（对标 Jellyfin/Emby/Plex/Kodi）：.strm 只是一行「指向媒体的文本」，
		// 播放时读出这行、拿到真实媒体地址，然后走与原生文件完全相同的
		// 「探测 → 五级降级」管线。所以这里只负责「解析出源」，不做任何播放决策。
		// 解析失败不再默默甩外部播放器，而是给出明确源后再由统一决策兜底。
		storages, _ := s.Store.ListStorages()
		rewrites, _ := s.Store.ListPathRewrites()
		res := strm.NewResolver(storages, rewrites).Resolve(f.StrmRaw)
		switch res.Scheme {
		case "http", "https":
			rawURL = res.RawURL
		case "openlist":
			stID := res.StorageID
			if stID == "" {
				stID = f.StorageID
			}
			st, e := s.Store.GetStorage(stID)
			if e != nil {
				writeErr(w, http.StatusBadRequest, "STRM 指向的存储源不存在："+stID)
				return
			}
			cl := s.clientFor(st)
			// resolveOpenListLink 会先用 res.Path 取链，失败则逐级剥前缀回退探测。
			// 这样即使 strm 本地路径没配 LocalRoot、前缀对不上存储内部路径，也能
			// 靠文件名/相对路径逐级命中，做到「无论什么 strm 都能页内播」。
			link, triedPath := s.resolveOpenListLink(cl, res.Path)
			if link != nil {
				rawURL = link.Data.RawURL
				directURL = link.Data.URL
				headers = link.Data.Headers
			}
			// 兜底（关键）：部分 STRM 的原始文本本身就是「可直接拉流」的 http(s) 直链，
			// 例如 CloudDrive2 的 .cas 经 OpenList 取链返回的就是 /d/...cas?sign= 这种
			// OpenList 中转链接——它自身 302 即跳转到真实网盘解密流，remux/转码端点能直接
			// 跟随并产出 MP4（已用真实 4GB 源验证）。当 resolveOpenListLink 对内部路径取链
			// 失败（路径编码对不上、模板占位符、或签名态异常）时，直接用原始 StrmRaw 兜底，
			// 避免「网盘直链为空」502，真正实现「无论什么 strm 都能页内播」，用户零配置。
			if rawURL == "" && directURL == "" && isStreamableURL(f.StrmRaw) {
				rawURL = f.StrmRaw
				triedPath = f.StrmRaw
			}
			if directURL == "" && st.SignKey != "" {
				directURL = cl.SignedDURL(triedPath) // 自行算签名，不必让用户关签名
			}
		case "file":
			rawURL = "file://" + res.Path
		default:
			writeErr(w, http.StatusBadRequest, "无法解析的 STRM 来源："+f.StrmRaw)
			return
		}
		// 容器兜底：解析后仍拿不到容器（多见于无扩展名的签名链 / OpenList /d/ 链接），
		// 尝试从直链/中转链/解析路径/原始文本的扩展名推断；推断不出也无妨——后面的
		// 「探测」会从媒体内容里识别容器，再不行就交给浏览器直接试播，绝不因为文件名
		// 猜不出就甩外部播放器（这正是此前 Strm 总被甩外部的根因）。
		if f.Container == "" {
			for _, cand := range []string{rawURL, directURL, triedPath, f.StrmRaw} {
				if c := containerExt(cand); c != "" {
					f.Container = c
					break
				}
			}
		}
		// 解析彻底失败（既无直链也无中转链）：明确报错，而不是让后面走到「拿不到源地址」的 502。
		if rawURL == "" && directURL == "" {
			writeErr(w, http.StatusBadGateway, "无法获取 STRM 播放源："+f.StrmRaw)
			return
		}
	} else {
		st, err := s.Store.GetStorage(f.StorageID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "存储源不存在")
			return
		}
		cl := s.clientFor(st)
		link, err := cl.GetLink(f.Path, s.Cfg.ProxyRefresh) // refresh=false 复用缓存
		if err == nil {
			rawURL = link.Data.RawURL
			directURL = link.Data.URL
			headers = link.Data.Headers
		}
		if directURL == "" && st.SignKey != "" {
			directURL = cl.SignedDURL(f.Path) // 自行算签名，不必让用户关签名
		}
	}

	// 懒探测：首次播放时 ffprobe 一次，补全容器/音轨列表/时长/编码并缓存进文件，
	// 供播放器做音轨切换与更准的降级决策。失败（ffprobe 缺失/超时/源不可达）标记 skipped 不再重试。
	// 探测源优先用 raw_url，没有再用 direct_url（OpenList /d/ 中转链同样可被 ffprobe 探测）。
	// 这一步是 strm 能正确页内播的关键：无扩展名的 strm 直链靠「探测」从媒体内容里认出
	// 容器/编码，而不是靠猜文件名——猜不出就只会被甩去外部播放器。
	probeSrc := rawURL
	if probeSrc == "" {
		probeSrc = directURL
	}
	if f.ProbeState == "pending" {
		if probeSrc != "" {
			if dur, atracks, pvc, pac, pcont, perr := s.probeFile(r.Context(), probeSrc); perr == nil {
				f.DurationSec = dur
				f.AudioTracks = atracks
				if pvc != "" {
					f.VideoCodec = pvc
				}
				if pac != "" {
					f.AudioCodec = pac
				}
				if pcont != "" {
					f.Container = pcont
				}
				f.ProbeState = "done"
				_ = s.Store.SaveMediaFile(f)
			} else {
				f.ProbeState = "skipped"
				_ = s.Store.SaveMediaFile(f)
			}
		} else {
			// 没有任何可用源可探测：标记 skipped，后面交给浏览器直接试播或外部播放器兜底。
			f.ProbeState = "skipped"
			_ = s.Store.SaveMediaFile(f)
		}
	}

	// 魔数嗅探兜底（移植自 Lunarr detectContainerFromMagic）：ffprobe 仍认不出容器时
	// （部分源 format_name 为空，或 ffprobe 不可用），读媒体头几字节靠魔数识别容器
	// （ftyp→mp4、EBML→mkv/webm、RIFF→avi、0x47→ts）。比猜扩展名更稳，避免无扩展名
	// strm 因容器推不出而被甩外部播放器。best-effort：任何失败都忽略，沿用现有兜底分支。
	if f.Container == "" && probeSrc != "" {
		if c := s.sniffContainer(r, probeSrc); c != "" {
			f.Container = c
		}
	}

	vc, ac := f.VideoCodec, f.AudioCodec
	if vc == "" {
		vc = "h264"
	}
	if ac == "" {
		ac = "aac"
	}
	// 视频转码开关：环境变量 VIDRIVE_TRANSCODE 为默认，前端「设置」里的
	// transcode_enabled 可覆盖（持久化）。开启后 HEVC 等浏览器解不了的编码会转成
	// H.264 页内播，彻底解决「MKV 视频不存在 / 依赖外部播放器」。
	transcode := s.Cfg.TranscodeEnabled
	if v, e := s.Store.GetSetting("transcode_enabled"); e == nil && (v == "1" || v == "true" || v == "on") {
		transcode = true
	}
	in := playback.Input{
		Container:        f.Container,
		VideoCodec:       vc,
		AudioCodec:       ac,
		RawURL:           rawURL,
		DirectURL:        directURL,
		SupportsRange:    true,
		TranscodeEnabled: transcode,
		FFmpegAvailable:  s.ffmpegOK,
		TranscodeAvailable: s.transcodeOK,
		// 客户端解码能力（前端 canPlayType 探测后上报，参考 Lunarr 能力协商）。
		// 默认 vp9/av1=true（沿用旧行为：旧前端/直接 API 不报时也按可解处理），
		// hevc=false（旧实现从不把 HEVC 当原生）。新前端会按真实能力覆盖——
		// 例如 Safari 报 hevc=1 即可直链 HEVC-in-MP4，Chrome 报 hevc=0 仍正常转码/外放。
		ClientHEVC: capFlag(r, "hevc", false),
		ClientAV1:  capFlag(r, "av1", true),
		ClientVP9:  capFlag(r, "vp9", true),
	}
	dec := playback.Select(in)

	// 2.0 修复：源若来自内置 139cas，其直链/中转链指向 127.0.0.1:5244（容器内、未对外暴露）。
	// 浏览器与外放播放器在用户机器上连不到该地址。把「直接下发给浏览器」的 URL 改写为本服务
	// 同端口的 /openlist 反代即可（remux/transcode 仍由服务端内部取流，不受影响）：
	//   - L0 直链：dec.URL 当前是 5244 直链，浏览器黑屏；改写为 /openlist/... 后由反代取流。
	//   - L4 外放：前端用 raw_url/direct_url 唤起外部播放器，同样改写为 /openlist/... 才可达。
	if dec.Level == playback.L0Direct {
		dec.URL = s.rewriteBundledURL(dec.URL)
	}
	rawURL = s.rewriteBundledURL(rawURL)
	directURL = s.rewriteBundledURL(directURL)

	// L2 重封装 / L3 视频转码：浏览器不认 MKV 等容器，或解不了视频编码（HEVC）。
	// 把源交给 HLS 切片（默认，见 internal/hls）或原有单 MP4 流端点实时处理。
	if dec.Level == playback.L2Remux || dec.Level == playback.L3Transcode {
		if src := playback.PickURL(in); src != "" {
			if s.hlsEnabled {
				// HLS 按需切片：把源切成独立分片（index.m3u8 + seg_*.ts），浏览器用 hls.js 拉取。
				// 分片为静态文件、支持 Range、GOP 对齐，拖动精准、起播快、通用性强
				// （对标 Plex/Jellyfin/Lunarr）。会话 key 由「源+模式+音轨」稳定哈希，
				// 重复播放/选音轨复用同一 ffmpeg（见 internal/hls），由端点按 query 计算。
				mode := "remux"
				if dec.Level == playback.L3Transcode {
					mode = "transcode"
				}
				u := "/api/play/hls/" + hls.PlaylistName +
					"?u=" + url.QueryEscape(src) + "&mode=" + mode
				// 音轨不兼容浏览器（DTS/TrueHD/Atmos）：让 HLS 端点视频拷贝、音轨转 AAC
				// （TS 虽能装 DTS，但浏览器解不了，仍需转 AAC 才能出声）。
				if dec.NeedsAudioTranscode {
					u += "&aac=1"
				}
				dec.URL = appendToken(u, getToken(r))
				// HLS 分片是静态文件，天然支持 Range/拖动。
				dec.SupportsRange = true
			} else {
				// 退回原有单 MP4 流：remux/transcode 端点实时处理成 fragmented MP4 流式播放。
				if dec.Level == playback.L3Transcode {
					dec.URL = playback.TranscodeURL(src)
				} else {
					u := playback.RemuxURL(src)
					if dec.NeedsAudioTranscode {
						u += "&aac=1"
					}
					dec.URL = u
				}
				// 重封装/转码流无法可靠响应 Range，页内从头播即可（fragmented mp4 仍可拖）。
				dec.SupportsRange = false
				dec.URL = appendToken(dec.URL, getToken(r))
			}
		} else {
			// 拿不到源地址：明确报错，而不是静默返回空 URL（否则前端 ArtPlayer 报「视频不存在」）。
			writeErr(w, http.StatusBadGateway,
				"无法获取播放源地址（网盘直链为空，可能需开启签名、或该存储不支持取链）")
			return
		}
	}

	// 续播位置：进度一直在存，却从来没人读过 —— 侧边栏那个「继续观看」点进去
	// 是从头开始播的，功能名存实亡。这里把上次的位置一并返回，前端 seek 过去。
	resume, total := 0, 0
	if u, ok := s.requireUser(r); ok {
		if rec, err := s.Store.GetPlayRecord(u.ID, fileID); err == nil {
			// 快看完了（剩不到 30 秒）就当作已看完，从头开始，避免一进去就是片尾。
			if rec.Duration == 0 || rec.Position < rec.Duration-30 {
				resume, total = rec.Position, rec.Duration
			}
		}
	}

	title, subtitle := "", ""
	if item, err := s.Store.GetMediaItem(f.ItemID); err == nil {
		title = item.Title
		if f.EpisodeNo > 0 {
			subtitle = "S" + strconv.Itoa(maxInt(f.SeasonNo, 1)) + "E" + strconv.Itoa(f.EpisodeNo)
		}
	}

	// 字幕列表：每条带语言、显示名与后端转换服务的 URL。
	// 字幕同理：ArtPlayer 用 fetch/track 加载，同样带不上 Authorization。
	subs := make([]map[string]string, 0, len(f.Subtitles))
	for _, s2 := range f.Subtitles {
		subs = append(subs, map[string]string{
			"lang":  s2.Lang,
			"title": s2.Title,
			"url":   appendToken("/api/play/subtitle?file="+fileID+"&lang="+s2.Lang, getToken(r)),
		})
	}

	// ffmpeg 缺失且本文件需要重封装/转码才能页内播 → 给前端一句人话提示，
	// 引导用户换含 ffmpeg 的镜像，而不是对着「外部播放器」发懵。
	warn := ""
	if !s.ffmpegOK && dec.Level == playback.L4External {
		bNative := nativeC(f.Container) && nativeV(vc) && nativeA(ac)
		if !bNative {
			warn = "服务端未安装 ffmpeg：MKV/HEVC 等需重封装或转码才能页内播放，已唤起外部播放器。" +
				"请部署含 ffmpeg 的镜像（tianjian518/newmovie:latest 已含 ffmpeg）。"
		}
	}

	writeJSON(w, map[string]interface{}{
		"level":           int(dec.Level),
		"label":           dec.Label,
		"reason":          dec.Reason,
		"url":             dec.URL,
		"raw_url":         rawURL,
		"direct_url":      directURL,
		"use_raw_url":     dec.UseRawURL,
		"supports_range":  dec.SupportsRange,
		"headers":         headers,
		"needs_transcode": dec.NeedsTranscode,
		"ffmpeg_ok":       s.ffmpegOK,
		"warn":            warn,
		"resume_position": resume,
		"resume_duration": total,
		"item_id":         f.ItemID,
		"title":           title,
		"subtitle":        subtitle,
		"subtitles":       subs,
		"audio_tracks":    f.AudioTracks,
	})
}

// nativeC/nativeV/nativeA 暴露给 playItem 做「浏览器原生可播」判断（与 selector 一致），
// 仅用于在响应里决定 ffmpeg 缺失时是否提示用户。
func nativeC(c string) bool {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "mp4", "webm", "mov":
		return true
	}
	return false
}
func nativeV(vc string) bool {
	switch strings.ToLower(strings.TrimSpace(vc)) {
	case "h264", "avc", "vp9", "av1":
		return true
	}
	return false
}
func nativeA(ac string) bool {
	switch strings.ToLower(strings.TrimSpace(ac)) {
	case "aac", "mp3", "opus":
		return true
	}
	return false
}

// isStreamableURL 判断一个 STRM 文本是否为「可直接拉流」的远程链接（http/https），
// 含 OpenList 的 /d/...?sign= 中转链接——它本身 302 即跳真实网盘流，可交给 remux/转码端点。
// 本地 file:// 或相对路径不在此列（需走本地读盘或挂载映射逻辑）。
func isStreamableURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// serveItemImage 经 OpenList 代理同目录本地图（poster.jpg / fanart.jpg）。
// 这些图的直链会过期，故不把直链交给前端，而是每次由服务端代理；并落本地缓存，
// 命中缓存直接返回，避免重复回源（服务端图片缓存层）。
func (s *Server) serveItemImage(w http.ResponseWriter, r *http.Request, id, kind string) {
	item, err := s.Store.GetMediaItem(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "条目不存在")
		return
	}
	var p, sid string
	if kind == "poster" {
		p, sid = item.PosterPath, item.PosterStorageID
	} else {
		p, sid = item.BackdropPath, item.BackdropStorageID
	}
	if p == "" {
		writeErr(w, http.StatusNotFound, "无本地图")
		return
	}
	if sid == "" {
		if lib, e := s.Store.GetLibrary(item.LibraryID); e == nil {
			sid = lib.StorageID
		}
	}
	st, err := s.Store.GetStorage(sid)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "存储源缺失")
		return
	}

	// 1) 命中缓存直接返回
	cachePath := filepath.Join(s.Cfg.CacheDir, "images", id+"_"+kind)
	if b, ct, ok := readImageCache(cachePath); ok {
		serveImageBytes(w, r, b, ct)
		return
	}

	// 2) 未命中：回源 OpenList 取图并落盘
	u, err := s.clientFor(st).RawURL(p)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "取直链失败: "+err.Error())
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	resp, err := imageClient.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "代理失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "读取失败: "+err.Error())
		return
	}
	writeImageCache(cachePath, b, ct)
	serveImageBytes(w, r, b, ct)
}

// handleImageProxy 海报/背景图代理：把 TMDB 图片 CDN 的图经本服务取回并缓存。
//
// 为什么需要它：2.0 浏览器只跟 8096 入口，而 TMDB 图片写死 image.tmdb.org，
// 在部分网络（墙内）直连被墙 → 海报整片空白。经本服务代理后，浏览器只请求
// /api/image?u=<编码后的图链>，由服务端去取图（服务端连通性通常更好，也可用
// TMDB_IMAGE_BASE 镜像），彻底解决「刮削有标题、海报全白」。
//
// 安全：仅放行 TMDB 图片 CDN 白名单主机（官方 image.tmdb.org + 用户配置的镜像），
// 不接收任意主机，避免被当成内网 SSRF 跳板。<img> 无法带 Authorization，故本端点公开。
func (s *Server) handleImageProxy(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("u")
	if raw == "" {
		writeErr(w, http.StatusBadRequest, "缺少 u 参数")
		return
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		writeErr(w, http.StatusBadRequest, "u 不是合法的 http(s) 图片地址")
		return
	}
	// SSRF 白名单：只允许 TMDB 图片 CDN 主机（官方 + 镜像），其余一律拒绝。
	allowed := false
	for _, h := range tmdb.ImageHosts() {
		if strings.EqualFold(u.Hostname(), h) {
			allowed = true
			break
		}
	}
	if !allowed {
		writeErr(w, http.StatusForbidden, "仅允许代理 TMDB 图片 CDN（image.tmdb.org 及其镜像）")
		return
	}

	// 缓存键：用完整图链做哈希，避免不同图互相串。
	sum := sha256.Sum256([]byte(u.String()))
	cachePath := filepath.Join(s.Cfg.CacheDir, "images", "tmdb_"+hex.EncodeToString(sum[:]))

	if b, ct, ok := readImageCache(cachePath); ok {
		serveImageBytes(w, r, b, ct)
		return
	}

	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", "NewMovie/2.0 (+image-proxy)")
	resp, err := imageClient.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "取图失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("图片源返回 %d", resp.StatusCode))
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "读取图片失败: "+err.Error())
		return
	}
	writeImageCache(cachePath, b, ct)
	serveImageBytes(w, r, b, ct)
}

// readImageCache 读取磁盘图片缓存（.bin + .ct 存 Content-Type）。
func readImageCache(p string) (b []byte, ct string, ok bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, "", false
	}
	if c, err := os.ReadFile(p + ".ct"); err == nil {
		ct = string(c)
	}
	return b, ct, true
}

// writeImageCache 原子写入图片缓存（临时文件 + rename，避免并发读到半截）。
func writeImageCache(p string, b []byte, ct string) {
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.WriteFile(tmp+".ct", []byte(ct), 0o644)
	_ = os.Rename(tmp, p)
	_ = os.Rename(tmp+".ct", p+".ct")
}

// serveImageBytes 返回图片字节，支持 Range 分段（浏览器/播放器常见）。
func serveImageBytes(w http.ResponseWriter, r *http.Request, b []byte, ct string) {
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	total := len(b)
	rng := r.Header.Get("Range")
	serveAll := func() {
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	}
	if rng == "" || !strings.HasPrefix(rng, "bytes=") {
		serveAll()
		return
	}
	start, end, res := parseRange(strings.TrimPrefix(rng, "bytes="), total)
	switch res {
	case rangeInvalid:
		serveAll() // 规格看不懂就当没带 Range，回整体 200
		return
	case rangeUnsatisfiable:
		w.Header().Set("Content-Range", "bytes */"+strconv.Itoa(total))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(end)+"/"+strconv.Itoa(total))
	w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(b[start : end+1])
}

// rangeResult parseRange 的三种结局。分开是因为「格式非法」按 RFC 7233 要求
// 忽略 Range 头回 200，而「合法但越界」必须回 416，两者不能混为一谈。
type rangeResult int

const (
	rangeInvalid       rangeResult = iota // 语法非法 → 忽略 Range
	rangeOK                               // 可满足
	rangeUnsatisfiable                    // 语法合法但越界 → 416
)

// parseRange 按 RFC 7233 把单段 bytes= 规格换算成闭区间 [start, end]。
//
//	"a-b"  第 a 到第 b 字节
//	"a-"   第 a 字节到结尾
//	"-n"   **最后** n 字节（suffix-byte-range-spec）
//
// 老实现把 "-n" 当成了 end=n，于是返回的是**开头** n 字节 —— 数据是错的。
// 播放器/浏览器探测文件尾（MP4 的 moov、MKV 的 Cues）时就会拿到垃圾数据。
func parseRange(spec string, total int) (start, end int, res rangeResult) {
	spec = strings.TrimSpace(spec)
	// 多段 Range（"0-99,200-299"）本实现不支持，退回整体返回。
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, rangeInvalid
	}
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return 0, 0, rangeInvalid
	}
	sPart := strings.TrimSpace(spec[:dash])
	ePart := strings.TrimSpace(spec[dash+1:])
	if sPart == "" && ePart == "" {
		return 0, 0, rangeInvalid
	}
	if total <= 0 {
		return 0, 0, rangeUnsatisfiable
	}

	if sPart == "" { // suffix：最后 n 字节
		n, err := strconv.Atoi(ePart)
		if err != nil || n < 0 {
			return 0, 0, rangeInvalid
		}
		if n == 0 {
			return 0, 0, rangeUnsatisfiable
		}
		if n > total {
			n = total
		}
		return total - n, total - 1, rangeOK
	}

	s, err := strconv.Atoi(sPart)
	if err != nil || s < 0 {
		return 0, 0, rangeInvalid
	}
	if s >= total {
		return 0, 0, rangeUnsatisfiable
	}
	if ePart == "" { // "a-"：到结尾
		return s, total - 1, rangeOK
	}
	e, err := strconv.Atoi(ePart)
	if err != nil || e < 0 {
		return 0, 0, rangeInvalid
	}
	if e >= total {
		e = total - 1
	}
	if s > e {
		return 0, 0, rangeUnsatisfiable
	}
	return s, e, rangeOK
}

// rescrapeItem 手动重刮削单个条目（例如初次扫描时还没配 TMDB Key，补配后重跑）。
// 仅管理员：刮削会修改全局条目的元数据（海报/简介/评分），普通用户不应能改。
func (s *Server) rescrapeItem(w http.ResponseWriter, r *http.Request, id string) {
	cur, _ := s.requireUser(r)
	if !cur.IsAdmin {
		writeErr(w, http.StatusForbidden, "仅管理员可重新刮削")
		return
	}
	item, err := s.Store.GetMediaItem(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "条目不存在")
		return
	}
	lib, err := s.Store.GetLibrary(item.LibraryID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "媒体库缺失")
		return
	}
	st, err := s.Store.GetStorage(lib.StorageID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "存储源缺失")
		return
	}
	cl := s.clientFor(st)
	searcher := s.newSearcher()
	// 保留已存本地图路径；NFO 路径重新推导代价高，这里以 TMDB 兜底为主。
	if err := scraper.Scrape(r.Context(), item, lib, s.Store, cl, searcher, "", item.PosterPath, item.BackdropPath, nil); err != nil {
		writeErr(w, http.StatusInternalServerError, "刮削失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

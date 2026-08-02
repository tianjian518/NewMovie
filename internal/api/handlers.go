// Package api 用标准库 net/http 暴露 Vidrive 的 REST 接口。
// 路由手写（零依赖）。前端通过 Bearer token 鉴权。
package api

import (
	"bytes"
	"context"
	"fmt"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
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
	"newmovie/internal/model"
	"newmovie/internal/openlist"
	"newmovie/internal/playback"
	"newmovie/internal/scanner"
	"newmovie/internal/scraper"
	"newmovie/internal/store"
	"newmovie/internal/subtitle"
	"newmovie/internal/tmdb"
)

// Version 是当前服务版本，健康检查与前端一并使用。
const Version = "1.1.12"

// Server 持有依赖。
type Server struct {
	Store store.Store
	Cfg   *config.Config

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
func New(st store.Store, cfg *config.Config) *Server { return &Server{Store: st, Cfg: cfg} }

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

func (s *Server) clientFor(storage model.Storage) *openlist.Client {
	return &openlist.Client{BaseURL: storage.BaseURL, Token: storage.Token, SignKey: storage.SignKey}
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
		writeJSON(w, map[string]interface{}{"ok": true, "version": Version, "name": "NewMovie"})
		return
	case p == "api/login" && r.Method == http.MethodPost:
		s.login(w, r)
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

func (s *Server) handleStorages(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		list, _ := s.Store.ListStorages()
		writeJSON(w, list)
	case len(parts) == 2 && r.Method == http.MethodPost:
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
		if err := s.Store.DeleteStorage(parts[2]); err != nil {
			writeErr(w, http.StatusNotFound, "存储源不存在")
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	case len(parts) == 3 && parts[2] == "test" && r.Method == http.MethodPost:
		s.testStorage(w, r)
	case len(parts) == 4 && parts[2] == "drives" && r.Method == http.MethodGet:
		st, err := s.Store.GetStorage(parts[3])
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
		BaseURL string `json:"base_url"`
		Token   string `json:"token"`
		SignKey string `json:"sign_key"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体错误")
		return
	}
	cl := &openlist.Client{BaseURL: body.BaseURL, Token: body.Token, SignKey: body.SignKey}
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
		writeJSON(w, list)
	case len(parts) == 2 && r.Method == http.MethodPost:
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
		if err := s.Store.DeleteLibrary(parts[2]); err != nil {
			writeErr(w, http.StatusNotFound, "媒体库不存在")
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	case len(parts) == 4 && parts[3] == "items" && r.Method == http.MethodGet:
		lib, err := s.Store.GetLibrary(parts[2])
		if err != nil {
			writeErr(w, http.StatusNotFound, "媒体库不存在")
			return
		}
		items, _ := s.Store.ListMediaItems(lib.ID)
		writeJSON(w, items)
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
		files, _ := s.Store.ListMediaFiles(item.ID)
		u, _ := s.requireUser(r)
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

// searchItems 全局搜索 + 筛选 + 排序：GET /api/search?q=&kind=&library=&sort=
//
// 库大了以后海报墙就是一面砖墙，没有搜索根本找不到东西。
// sort 支持 title / year / rating / recent，默认按标题。
func (s *Server) searchItems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	libFilter := strings.TrimSpace(r.URL.Query().Get("library"))
	sortBy := strings.TrimSpace(r.URL.Query().Get("sort"))

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
	if lim := r.URL.Query().Get("limit"); lim != "" {
		if n, err := strconv.Atoi(lim); err == nil && n > 0 && n < len(out) {
			out = out[:n]
		}
	}
	writeJSON(w, out)
}

// matchItem 手动把条目绑定到指定 TMDB ID 并立即重刮削：
// POST /api/items/:id/match  {"tmdb_id":27205,"kind":"movie"}
//
// 自动匹配再准也有认错的时候（同名翻拍、中文译名不一致、系列续作），
// 之前只能去网盘目录里手写 .vidrive.json 才能纠正 —— 对普通用户等于没救。
func (s *Server) matchItem(w http.ResponseWriter, r *http.Request, id string) {
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

func (s *Server) handleRewrites(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		list, _ := s.Store.ListPathRewrites()
		writeJSON(w, list)
	case len(parts) == 2 && r.Method == http.MethodPost:
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

// handleSettings 读写全局设置（如用户自填的 TMDB Key）。
// GET 返回全部；PUT/POST 接收 {key:value} 增量保存。
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, _ := s.Store.ListSettings()
		writeJSON(w, list)
	case http.MethodPut, http.MethodPost:
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
func (s *Server) testTMDB(w http.ResponseWriter, r *http.Request) {
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

// handlePlay 仅处理 /api/play/proxy（直链代理转发，L1）。
// 单文件播放决策走 /api/items/:id/play（见 playItem）。
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) >= 3 && parts[2] == "remux" && r.Method == http.MethodGet {
		s.handleRemux(w, r)
		return
	}
	if len(parts) >= 3 && parts[2] == "subtitle" && r.Method == http.MethodGet {
		s.handleSubtitle(w, r)
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
		for k, vs := range resp.Header {
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

// handleRemux 实时重封装：拉取源（受 SSRF 守卫约束），用 ffmpeg -c copy
// 转成 fragmented MP4 流式回写给浏览器。仅做容器转换、不重新编码，开销极低。
// 这样 MKV（h264/aac、hevc/aac 等）也能在页内 ArtPlayer 直接播，不必甩外部播放器。
func (s *Server) handleRemux(w http.ResponseWriter, r *http.Request) {
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
	client := s.proxyClientFor(target)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "构造请求失败")
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errBlockedTarget) {
			writeErr(w, http.StatusForbidden, errBlockedTarget.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, "拉取源失败: "+err.Error())
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		writeErr(w, http.StatusBadGateway, "源返回 "+strconv.Itoa(resp.StatusCode))
		return
	}
	defer resp.Body.Close()

	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "服务端未安装 ffmpeg，无法转封装（请在镜像中安装 ffmpeg）")
		return
	}

	ctx := r.Context()
	args := []string{"-loglevel", "error", "-i", "pipe:0"}
	// aac=1：视频保持拷贝（HEVC/H264 不变），仅把不兼容 MP4 的音轨（DTS/TrueHD/Atmos/FLAC）
	// 实时转成 AAC。4K 蓝光原盘（HEVC + DTS-HD/TrueHD/Atmos）借此页内可播，且几乎不耗服务端算力。
	if r.URL.Query().Get("aac") == "1" {
		args = append(args, "-c:v", "copy", "-c:a", "aac", "-b:a", "320k")
	} else {
		args = append(args, "-c", "copy")
	}
	args = append(args, "-f", "mp4", "-movflags", "+frag_keyframe+empty_moov")
	// atrack：仅抽取指定音轨（多音轨 MKV 切换语言用）。
	// 指定后不再 copy 全部流，而是显式映射视频 + 该音轨。
	if at := r.URL.Query().Get("atrack"); at != "" {
		if n, err := strconv.Atoi(at); err == nil && n >= 0 {
			args = append(args, "-map", "0:v:0", "-map", "0:a:"+strconv.Itoa(n))
		}
	}
	args = append(args, "pipe:1")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = resp.Body
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
		log.Printf("remux ffmpeg: %s", ffmpegErr.String())
	}
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
func (s *Server) probeFile(ctx context.Context, rawURL string) (dur int, audio []model.AudioTrack, vc, ac string, err error) {
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0, nil, "", "", fmt.Errorf("ffprobe 不可用")
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, bin, "-v", "error",
		"-show_entries", "stream=index,codec_type,codec_name:stream_tags=language,title:format=duration",
		"-of", "json", rawURL)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err = cmd.Run(); err != nil {
		return 0, nil, "", "", err
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
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err = json.Unmarshal(out.Bytes(), &probe); err != nil {
		return 0, nil, "", "", err
	}
	if d, e := strconv.ParseFloat(probe.Format.Duration, 64); e == nil {
		dur = int(d)
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
	return dur, audio, vc, ac, nil
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
func (s *Server) playItem(w http.ResponseWriter, r *http.Request, fileID string) {
	f, err := s.Store.GetMediaFile(fileID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	st, err := s.Store.GetStorage(f.StorageID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "存储源不存在")
		return
	}
	cl := s.clientFor(st)
	link, err := cl.GetLink(f.Path, s.Cfg.ProxyRefresh) // refresh=false 复用缓存
	var rawURL, directURL string
	headers := map[string]string{}
	if err == nil {
		rawURL = link.Data.RawURL
		directURL = link.Data.URL
		headers = link.Data.Headers
	}
	if directURL == "" && st.SignKey != "" {
		directURL = cl.SignedDURL(f.Path) // 自行算签名，不必让用户关签名
	}

	// 懒探测：首次播放时 ffprobe 一次，补全音轨列表/时长/编码并缓存进文件，
	// 供播放器做音轨切换与更准的降级决策。失败（ffprobe 缺失/超时）标记 skipped 不再重试。
	if f.ProbeState == "pending" && rawURL != "" {
		if dur, atracks, pvc, pac, perr := s.probeFile(r.Context(), rawURL); perr == nil {
			f.DurationSec = dur
			f.AudioTracks = atracks
			if pvc != "" {
				f.VideoCodec = pvc
			}
			if pac != "" {
				f.AudioCodec = pac
			}
			f.ProbeState = "done"
			_ = s.Store.SaveMediaFile(f)
		} else {
			f.ProbeState = "skipped"
			_ = s.Store.SaveMediaFile(f)
		}
	}

	vc, ac := f.VideoCodec, f.AudioCodec
	if vc == "" {
		vc = "h264"
	}
	if ac == "" {
		ac = "aac"
	}
	in := playback.Input{
		Container:        f.Container,
		VideoCodec:       vc,
		AudioCodec:       ac,
		RawURL:           rawURL,
		DirectURL:        directURL,
		SupportsRange:    true,
		TranscodeEnabled: false,
	}
	dec := playback.Select(in)

	// L2 重封装：浏览器不认 MKV 等容器，但编码本身可播。把源交给
	// /api/play/remux 实时 -c copy（或仅转音轨）成 MP4 流式播放，避免甩外部播放器。
	if dec.Level == playback.L2Remux {
		if src := playback.PickURL(in); src != "" {
			u := playback.RemuxURL(src)
			// 音轨不兼容 MP4（DTS/TrueHD/Atmos）：让 remux 端点视频拷贝、音轨转 AAC。
			if dec.NeedsAudioTranscode {
				u += "&aac=1"
			}
			dec.URL = u
			// 转封装流无法可靠响应 Range，页内从头播即可（fragmented mp4 仍可拖）。
			dec.SupportsRange = false
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
	subs := make([]map[string]string, 0, len(f.Subtitles))
	for _, s2 := range f.Subtitles {
		subs = append(subs, map[string]string{
			"lang":  s2.Lang,
			"title": s2.Title,
			"url":   "/api/play/subtitle?file=" + fileID + "&lang=" + s2.Lang,
		})
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
		"resume_position": resume,
		"resume_duration": total,
		"item_id":         f.ItemID,
		"title":           title,
		"subtitle":        subtitle,
		"subtitles":       subs,
		"audio_tracks":    f.AudioTracks,
	})
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
func (s *Server) rescrapeItem(w http.ResponseWriter, r *http.Request, id string) {
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

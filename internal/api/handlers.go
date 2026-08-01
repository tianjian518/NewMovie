// Package api 用标准库 net/http 暴露 Vidrive 的 REST 接口。
// 路由手写（零依赖）。前端通过 Bearer token 鉴权。
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/openlist"
	"newmovie/internal/playback"
	"newmovie/internal/scanner"
	"newmovie/internal/scraper"
	"newmovie/internal/store"
)

// Server 持有依赖。
type Server struct {
	Store store.Store
	Cfg   *config.Config
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

// Handler 返回 http.Handler。
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.route) }

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(p, "/")

	// 免鉴权
	switch {
	case p == "api/health":
		writeJSON(w, map[string]interface{}{"ok": true, "version": "1.1.0", "name": "NewMovie"})
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
		case "continue":
			s.handleContinue(w, r)
			return
		case "favorites":
			s.handleFavorites(w, r)
			return
		case "rewrites":
			s.handleRewrites(w, r, parts)
			return
		case "settings":
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
		st.ID = auth.GenID("st")
		st.CreatedAt = model.NowMillis()
		if st.RateLimit <= 0 {
			st.RateLimit = s.Cfg.DefaultRate
		}
		_ = s.Store.SaveStorage(st)
		writeJSON(w, st)
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
	default:
		writeErr(w, http.StatusNotFound, "unknown storages route")
	}
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
		writeErr(w, http.StatusBadGateway, "连接失败: "+err.Error())
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
		_ = s.Store.SaveLibrary(lib)
		writeJSON(w, lib)
	case len(parts) == 4 && parts[3] == "items" && r.Method == http.MethodGet:
		lib, err := s.Store.GetLibrary(parts[2])
		if err != nil {
			writeErr(w, http.StatusNotFound, "媒体库不存在")
			return
		}
		items, _ := s.Store.ListMediaItems(lib.ID)
		writeJSON(w, items)
	case len(parts) == 4 && parts[2] == "scan" && r.Method == http.MethodPost:
		s.startScan(w, r, parts[3])
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
	cl := s.clientFor(st)
	var searcher scraper.Searcher
	if key := s.tmdbKey(); key != "" {
		searcher = scraper.NewTMDBSearcher(key)
	}
	// 用脱离请求的 context：避免 HTTP 响应返回后请求 ctx 被取消而中断后台扫描。
	bg := context.Background()
	go func() {
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
		writeJSON(w, map[string]interface{}{"item": item, "files": files})
	case len(parts) == 4 && parts[3] == "play" && r.Method == http.MethodGet:
		s.playItem(w, r, parts[2])
	case len(parts) == 4 && parts[3] == "poster" && r.Method == http.MethodGet:
		s.serveItemImage(w, r, parts[2], "poster")
	case len(parts) == 4 && parts[3] == "backdrop" && r.Method == http.MethodGet:
		s.serveItemImage(w, r, parts[2], "backdrop")
	case len(parts) == 4 && parts[3] == "rescrape" && r.Method == http.MethodPost:
		s.rescrapeItem(w, r, parts[2])
	default:
		writeErr(w, http.StatusNotFound, "unknown items route")
	}
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

func (s *Server) handleContinue(w http.ResponseWriter, r *http.Request) {
	u, _ := s.requireUser(r)
	recs, _ := s.Store.ListContinue(u.ID)
	writeJSON(w, recs)
}

// --- 收藏 ---

func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	u, _ := s.requireUser(r)
	switch r.Method {
	case http.MethodGet:
		list, _ := s.Store.ListFavorites(u.ID)
		writeJSON(w, list)
	case http.MethodPost:
		var f model.Favorite
		if err := readJSON(r, &f); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体错误")
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

// --- 播放 ---

// handlePlay 仅处理 /api/play/proxy（直链代理转发，L1）。
// 单文件播放决策走 /api/items/:id/play（见 playItem）。
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) >= 3 && parts[2] == "proxy" && r.Method == http.MethodGet {
		u := r.URL.Query().Get("u")
		if u == "" {
			writeErr(w, http.StatusBadRequest, "缺少 u 参数")
			return
		}
		resp, err := http.Get(u)
		if err != nil {
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
		io.Copy(w, resp.Body)
		return
	}
	if len(parts) >= 3 && parts[2] == "record" && r.Method == http.MethodPost {
		s.saveRecord(w, r)
		return
	}
	writeErr(w, http.StatusNotFound, "unknown play route")
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
	})
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
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	resp, err := http.DefaultClient.Do(req)
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
	if rng == "" || !strings.HasPrefix(rng, "bytes=") {
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
		return
	}
	spec := strings.TrimPrefix(rng, "bytes=")
	var start, end int
	if !parseRange(spec, &start, &end) {
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
		return
	}
	if start < 0 {
		start = 0
	}
	if end <= 0 || end >= total {
		end = total - 1
	}
	if start > end {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(end)+"/"+strconv.Itoa(total))
	w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(b[start : end+1])
}

// parseRange 解析 HTTP Range 的 bytes= 规格（"start-end" / "start-" / "-end"）。
// 出错或格式非法返回 false。
func parseRange(spec string, start, end *int) bool {
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return false
	}
	sPart := strings.TrimSpace(spec[:dash])
	ePart := strings.TrimSpace(spec[dash+1:])
	if sPart != "" {
		v, err := strconv.Atoi(sPart)
		if err != nil {
			return false
		}
		*start = v
	}
	if ePart != "" {
		v, err := strconv.Atoi(ePart)
		if err != nil {
			return false
		}
		*end = v
	}
	return true
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
	var searcher scraper.Searcher
	if key := s.tmdbKey(); key != "" {
		searcher = scraper.NewTMDBSearcher(key)
	}
	// 保留已存本地图路径；NFO 路径重新推导代价高，这里以 TMDB 兜底为主。
	if err := scraper.Scrape(r.Context(), item, lib, s.Store, cl, searcher, "", item.PosterPath, item.BackdropPath, nil); err != nil {
		writeErr(w, http.StatusInternalServerError, "刮削失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

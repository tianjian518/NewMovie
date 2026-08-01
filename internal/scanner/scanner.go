// Package scanner 扫描媒体库：递归列目录、限速、增量 diff、断点续扫、刮削。
// 见 PLAN.md 第五节（风控）与第八节（Phase 1 扫描器）。
package scanner

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"newmovie/internal/model"
	"newmovie/internal/openlist"
	"newmovie/internal/parser"
	"newmovie/internal/scraper"
	"newmovie/internal/store"
	"newmovie/internal/strm"
)

// RateLimiter 简单令牌桶。
type RateLimiter struct {
	mu     sync.Mutex
	tokens float64
	rate   float64
	last   time.Time
}

func NewRateLimiter(rate float64) *RateLimiter {
	if rate <= 0 {
		rate = 2
	}
	return &RateLimiter{tokens: rate, rate: rate, last: time.Now()}
}

// Take 取一个令牌，必要时阻塞。
func (r *RateLimiter) Take() {
	r.mu.Lock()
	now := time.Now()
	r.tokens += now.Sub(r.last).Seconds() * r.rate
	if r.tokens > r.rate {
		r.tokens = r.rate
	}
	r.last = now
	if r.tokens >= 1 {
		r.tokens--
		r.mu.Unlock()
		return
	}
	wait := time.Duration((1 - r.tokens) / r.rate * float64(time.Second))
	r.mu.Unlock()
	time.Sleep(wait)
}

// Scan 对单个媒体库执行扫描（native/strm/mixed 均支持）。
// searcher 为 TMDB 刮削器（可为 nil 跳过 TMDB）；无 NFO 时 fallback 到它。
// storages 用于 strm 解析器（mixed/strm 模式）；rewrites 为路径重写规则。
func Scan(ctx context.Context, lib model.Library, st store.Store, client *openlist.Client,
	storages []model.Storage, rewrites []model.PathRewrite, rate float64,
	searcher scraper.Searcher, onProgress func(done, total int)) error {

	rl := NewRateLimiter(rate)
	job := model.ScanJob{
		ID:        "job-" + lib.ID,
		LibraryID: lib.ID,
		Status:    "running",
		StartedAt: model.NowMillis(),
	}
	_ = st.SaveScanJob(job)

	run := &runner{
		ctx: ctx, lib: lib, st: st, client: client,
		storages: storages, rewrites: rewrites, searcher: searcher, rl: rl,
		onProgress: onProgress,
	}

	var walk func(p string) error
	walk = func(p string) error {
		if run.ctx.Err() != nil {
			return run.ctx.Err()
		}
		run.rl.Take() // 限速：风控核心
		objs, err := run.client.List(p, false)
		if err != nil {
			return err
		}
		var names []string
		for _, o := range objs {
			names = append(names, o.Name)
		}
		for _, o := range objs {
			full := path.Join(p, o.Name)
			if o.IsDir {
				if err := walk(full); err != nil {
					return err
				}
				continue
			}
			if err := run.consume(p, names, o); err != nil {
				return err
			}
		}
		run.mu.Lock()
		run.cursor = p
		run.mu.Unlock()
		return nil
	}

	err := walk(lib.RootPath)
	run.mu.Lock()
	job.Done = run.done
	job.Total = run.total
	job.Cursor = run.cursor
	job.FinishedAt = model.NowMillis()
	if err != nil {
		job.Status = "failed"
	} else {
		job.Status = "done"
	}
	run.mu.Unlock()
	_ = st.UpdateScanJob(job)
	return err
}

// runner 扫描运行上下文，避免每层函数传一长串参数。
type runner struct {
	ctx      context.Context
	lib      model.Library
	st       store.Store
	client   *openlist.Client
	storages []model.Storage
	rewrites []model.PathRewrite
	searcher scraper.Searcher
	rl       *RateLimiter
	onProgress func(done, total int)
	force     bool // 强制重刮（忽略已有缓存）；rescrape 用

	mu     sync.Mutex
	done   int
	total  int
	cursor string
}

// consume 根据库模式决定如何入库一个文件，并触发刮削。
func (r *runner) consume(dir string, names []string, o openlist.FsObj) error {
	isStrm := strings.EqualFold(containerOf(o.Name), "strm")
	switch {
	case isStrm && (r.lib.Mode == model.ModeStrm || r.lib.Mode == model.ModeMixed):
		return r.consumeStrm(dir, names, o)
	case !isStrm && isVideo(o.Name) && (r.lib.Mode == model.ModeNative || r.lib.Mode == model.ModeMixed):
		return r.consumeNative(dir, names, o)
	}
	return nil // 模式不匹配，跳过
}

func (r *runner) bumpProgress() {
	r.mu.Lock()
	r.done++
	r.total++
	cur, tot := r.done, r.total
	r.mu.Unlock()
	if r.onProgress != nil {
		r.onProgress(cur, tot)
	}
}

func (r *runner) consumeNative(dir string, names []string, o openlist.FsObj) error {
	item, err := ingestNative(r.st, r.lib, r.storages, r.rewrites, path.Join(dir, o.Name), o)
	if err != nil {
		return err
	}
	r.bumpProgress()
	r.scrapeFor(item, dir, o.Name, names)
	return nil
}

func (r *runner) consumeStrm(dir string, names []string, o openlist.FsObj) error {
	full := path.Join(dir, o.Name)
	content, err := r.client.ReadText(full)
	if err != nil {
		return err
	}
	item, err := IngestStrm(r.st, r.lib, r.storages, r.rewrites, o.Name, content, dir)
	if err != nil {
		return err
	}
	r.bumpProgress()
	r.scrapeFor(item, dir, o.Name, names)
	return nil
}

// scrapeFor 计算同目录 NFO/本地图候选路径并刮削。
// 增量缓存：非强制且条目已有海报（说明之前刮过），直接跳过，避免重复打 TMDB / 读 NFO。
func (r *runner) scrapeFor(item model.MediaItem, dir, fileName string, names []string) {
	if !r.force && item.PosterURL != "" {
		return // 命中缓存，跳过
	}
	nfo, poster, backdrop := siblingPaths(dir, baseNoExt(fileName), names)
	r.rl.Take() // 刮削（读 NFO / 调 TMDB）额外一次请求，仍走限速
	_ = scraper.Scrape(r.ctx, item, r.lib, r.st, r.client, r.searcher, nfo, poster, backdrop)
}

// ingestNative 把原生（OpenList）视频文件入库为 MediaItem + MediaFile，返回规范化的 item。
func ingestNative(st store.Store, lib model.Library, storages []model.Storage, rewrites []model.PathRewrite,
	full string, o openlist.FsObj) (model.MediaItem, error) {

	pr := parser.Parse(o.Name)
	kind := model.KindMovie
	if pr.IsSeries {
		kind = model.KindSeries
	}
	item := model.MediaItem{
		ID:        "m-" + hash(full),
		LibraryID: lib.ID,
		Kind:      kind,
		Title:     pr.Title,
		Year:      pr.Year,
		CreatedAt: model.NowMillis(),
	}
	if err := st.UpsertMediaItemByTitle(item); err != nil {
		return model.MediaItem{}, err
	}
	// 取回规范化后的 item（去重合并后），供刮削使用
	saved, err := st.GetMediaItem(item.ID)
	if err != nil {
		saved = item
	}

	f := model.MediaFile{
		ID:          "f-" + hash(full),
		ItemID:      saved.ID,
		StorageID:   lib.StorageID,
		Source:      model.SrcNative,
		Path:        full,
		Size:        o.Size,
		Modified:    o.Modified,
		Container:   containerOf(o.Name),
		SeasonNo:    pr.Season,
		EpisodeNo:   pr.Episode,
		ProbeState:  "pending", // 懒探测（见 PLAN 4.4）
		CreatedAt:   model.NowMillis(),
	}
	if err := st.SaveMediaFile(f); err != nil {
		return saved, err
	}
	return saved, nil
}

// IngestStrm 处理一个 strm 文件（strm/mixed 模式）。
// raw 为 strm 文件内容（一行），strmPath 为 strm 文件所在目录（解析相对路径用）。
// 返回规范化的 item。
func IngestStrm(st store.Store, lib model.Library, storages []model.Storage, rewrites []model.PathRewrite,
	strmName, raw, strmpath string) (model.MediaItem, error) {

	res := strm.NewResolver(storages, rewrites).Resolve(raw)
	pr := parser.Parse(strmName)
	kind := model.KindMovie
	if pr.IsSeries {
		kind = model.KindSeries
	}
	item := model.MediaItem{
		ID:        "m-" + hash(strmName),
		LibraryID: lib.ID,
		Kind:      kind,
		Title:     pr.Title,
		Year:      pr.Year,
		CreatedAt: model.NowMillis(),
	}
	if err := st.UpsertMediaItemByTitle(item); err != nil {
		return model.MediaItem{}, err
	}
	saved, err := st.GetMediaItem(item.ID)
	if err != nil {
		saved = item
	}

	f := model.MediaFile{
		ID:          "f-" + hash(strmName),
		ItemID:      saved.ID,
		StorageID:   res.StorageID,
		Source:      model.SrcStrm,
		Path:        res.Path,
		StrmRaw:     raw,
		Container:   containerFromURL(raw),
		SeasonNo:    pr.Season,
		EpisodeNo:   pr.Episode,
		ProbeState:  "pending",
		CreatedAt:   model.NowMillis(),
	}
	if res.Scheme == "openlist" {
		f.Source = model.SrcStrm
	}
	if err := st.SaveMediaFile(f); err != nil {
		return saved, err
	}
	return saved, nil
}

// siblingPaths 依据视频/strm 文件名与同目录文件清单，推导 NFO / 本地海报 / 本地背景候选路径。
// 命中则返回 OpenList 内部路径，否则返回空串。
func siblingPaths(dir, baseNoExt string, names []string) (nfo, poster, backdrop string) {
	has := func(name string) bool {
		for _, s := range names {
			if strings.EqualFold(s, name) {
				return true
			}
		}
		return false
	}
	for _, n := range []string{baseNoExt + ".nfo", "movie.nfo", "tvshow.nfo"} {
		if has(n) {
			nfo = path.Join(dir, n)
			break
		}
	}
	for _, n := range []string{"poster.jpg", "folder.jpg", baseNoExt + ".jpg", "movie.jpg", "poster.png", baseNoExt + ".png"} {
		if has(n) {
			poster = path.Join(dir, n)
			break
		}
	}
	for _, n := range []string{"fanart.jpg", "backdrop.jpg", baseNoExt + "-fanart.jpg", "fanart.png"} {
		if has(n) {
			backdrop = path.Join(dir, n)
			break
		}
	}
	return
}

func isVideo(name string) bool {
	switch strings.ToLower(containerOf(name)) {
	case "mp4", "mkv", "webm", "mov", "avi", "ts", "m2ts", "flv":
		return true
	}
	return false
}

func containerOf(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

func baseNoExt(name string) string {
	if i := strings.LastIndex(name, "."); i > 0 {
		return name[:i]
	}
	return name
}

func containerFromURL(u string) string {
	i := strings.LastIndex(u, ".")
	if i < 0 {
		return ""
	}
	ext := u[i+1:]
	if q := strings.Index(ext, "?"); q >= 0 {
		ext = ext[:q]
	}
	return strings.ToLower(ext)
}

// hash 简易去重键（非加密）。
func hash(s string) string {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%016x", h)
}

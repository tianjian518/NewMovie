// Package scanner 扫描媒体库：递归列目录、限速、增量 diff、断点续扫、刮削。
// 见 PLAN.md 第五节（风控）与第八节（Phase 1 扫描器）。
package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
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

// MaxScanDepth 是目录递归的最大深度。
// 真实媒体库极少超过 10 层（库根/剧名/季/文件），设为 24 已非常宽松；
// 超过它几乎必然是目录成环或异常挂载，继续下钻只会把内存耗尽。
const MaxScanDepth = 24

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

	// 目录环 / 超深嵌套防护：
	// 网盘（尤其挂载了软链、循环 rclone/webdav 映射的源）可能返回自引用目录，
	// 无防护的递归会一直下钻 → 栈与内存爆掉 → 进程被 OOM Killer 杀死 →
	// 容器 restart 策略再拉起 → 无限重启。这里用「深度上限 + 已访问集合」双保险。
	visited := make(map[string]bool)

	var walk func(p string, depth int) error
	walk = func(p string, depth int) error {
		if run.ctx.Err() != nil {
			return run.ctx.Err()
		}
		if depth > MaxScanDepth {
			log.Printf("[scan] 目录深度超过上限 %d，跳过：%s", MaxScanDepth, p)
			return nil
		}
		// 规范化后判重，命中说明目录成环（或被重复挂载），直接跳过而非继续下钻。
		key := path.Clean(p)
		if visited[key] {
			log.Printf("[scan] 检测到目录环，跳过：%s", p)
			return nil
		}
		visited[key] = true

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
			// 跳过 . / .. 之类的自引用条目，否则同样会原地打转。
			if o.Name == "." || o.Name == ".." || o.Name == "" {
				continue
			}
			full := path.Join(p, o.Name)
			if o.IsDir {
				if err := walk(full, depth+1); err != nil {
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

	err := walk(lib.RootPath, 0)
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
	// .strm 与 .cas（139cas 的指针文件，OpenList rebrand）均按 strm 同类处理。
	ext := strings.ToLower(containerOf(o.Name))
	isStrm := ext == "strm" || ext == "cas"
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
	base := baseNoExt(fileName)
	nfo, poster, backdrop := siblingPaths(dir, base, names)

	// 剧集：tvshow.nfo 与系列级海报/背景通常在父目录（剧集根目录），需沿父目录向上查找。
	// 系列级 tvshow.nfo 优先级高于同目录的单集 nfo（episodedetails），因它才是系列身份（tmdb id/剧名）。
	if item.Kind == model.KindSeries {
		if t := r.findUp(dir, "tvshow.nfo", names); t != "" {
			nfo = t
		}
		if poster == "" {
			if p := r.findUp(dir, "poster.jpg", names); p != "" {
				poster = p
			}
		}
		if backdrop == "" {
			if b := r.findUp(dir, "fanart.jpg", names); b != "" {
				backdrop = b
			}
		}
	}

	// 手动锁定（同目录或父目录的 .vidrive.json）最高优先级。
	var manual *scraper.ManualMeta
	if v := r.findUp(dir, ".vidrive.json", names); v != "" {
		if s, ok := r.probe(v); ok {
			manual = parseVidriveJSON(s)
		}
	}

	r.rl.Take() // 刮削（读 NFO / 调 TMDB）额外一次请求，仍走限速
	_ = scraper.Scrape(r.ctx, item, r.lib, r.st, r.client, r.searcher, nfo, poster, backdrop, manual)
}

// probe 读取一个候选文本文件（NFO / .vidrive.json），返回内容与是否可读。
// 优先走 OpenList（native/mixed），失败再回退本地文件（ModeStrm 指向本地目录时）。
// 读不到（不存在/无权限）返回 ("", false)。
func (r *runner) probe(p string) (string, bool) {
	if s, err := r.client.ReadText(p); err == nil {
		return s, true
	}
	if b, err := os.ReadFile(p); err == nil {
		return string(b), true
	}
	return "", false
}

// findUp 在「同目录 + 向上若干层父目录」中查找名为 fileName 的文件，返回首个命中的完整路径。
// 用于 tvshow.nfo / poster.jpg / .vidrive.json 不在视频同目录、而在剧集根目录的场景。
// 同目录优先用 names 清单（零请求）；父目录逐层用 probe 探测存在性（命中即停，最多 6 层）。
func (r *runner) findUp(dir, fileName string, names []string) string {
	has := func(name string) bool {
		for _, s := range names {
			if strings.EqualFold(s, name) {
				return true
			}
		}
		return false
	}
	if has(fileName) {
		return path.Join(dir, fileName)
	}
	r.rl.Take() // 父目录探测同样走限速
	cur := dir
	for i := 0; i < 6; i++ {
		parent := path.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
		cand := path.Join(cur, fileName)
		if _, ok := r.probe(cand); ok {
			return cand
		}
	}
	return ""
}

// parseVidriveJSON 解析同目录 .vidrive.json，提取用户手动锁定的元数据。
// 例：{"tmdb_id":27205,"type":"movie","title":"盗梦空间","year":2010}
// type 取 "movie"/"tv" 用于强制条目类型；缺省不覆盖。
func parseVidriveJSON(s string) *scraper.ManualMeta {
	var m struct {
		TMDBID  int64  `json:"tmdb_id"`
		Type    string `json:"type"`
		Title   string `json:"title"`
		Year    int    `json:"year"`
		Season  int    `json:"season"`
		Episode int    `json:"episode"`
	}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	if m.TMDBID == 0 && m.Title == "" && m.Year == 0 {
		return nil
	}
	mm := &scraper.ManualMeta{TMDBID: m.TMDBID, Title: m.Title, Year: m.Year}
	if strings.EqualFold(m.Type, "tv") {
		mm.Kind = model.KindSeries
	} else if strings.EqualFold(m.Type, "movie") {
		mm.Kind = model.KindMovie
	}
	_ = m.Season
	_ = m.Episode
	return mm
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
		Size:        int64(o.Size),
		Modified:    int64(o.Modified),
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

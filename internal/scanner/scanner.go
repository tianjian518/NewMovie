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
	"strconv"
	"strings"
	"sync"
	"time"

	"newmovie/internal/model"
	"newmovie/internal/openlist"
	"newmovie/internal/parser"
	"newmovie/internal/scraper"
	"newmovie/internal/store"
	"newmovie/internal/strm"
	"newmovie/internal/subtitle"
)

// MaxScanDepth 是目录递归的最大深度。
// 真实媒体库极少超过 10 层（库根/剧名/季/文件），设为 24 已非常宽松；
// 超过它几乎必然是目录成环或异常挂载，继续下钻只会把内存耗尽。
const MaxScanDepth = 24

// MaxScanWarnings 单次扫描最多保留的警告条数。
// 一个挂载掉线的网盘能刷出上万条同样的错误，全存下来只会把 JSON 库撑爆，
// 而用户看前 50 条就足够定位问题了。
const MaxScanWarnings = 50

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
func Scan(ctx context.Context, lib model.Library, st store.Store, client openlist.FSClient,
	storages []model.Storage, rewrites []model.PathRewrite, rate float64,
	searcher scraper.Searcher, onProgress func(done, total int)) error {

	rl := NewRateLimiter(rate)
	// 用户手填的路径十有八九带着复制粘贴的脏东西（缺前导斜杠、尾斜杠、空格）。
	// 不在这里收口，OpenList 会对每一种变体都回 code=500，用户只看到「扫不出内容」。
	lib.RootPath = openlist.NormalizePath(lib.RootPath)

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
		dirCache:   make(map[string][]string),
	}

	// TMDB 健康检查：扫描开始时探测一次，不可用就跳过所有刮削。
	// 否则每集都会等 10s 超时 × 2 个备用域名 = 20s，138 集要等 46 分钟，
	// 用户看到的就是「扫描卡住了」。这里用 3s 超时快速探测，失败则置空 searcher。
	if run.searcher != nil {
		log.Printf("[scan] 开始 TMDB 健康检查（3s 超时）...")
		probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
		_, probeErr := run.searcher.Search(probeCtx, "movie", "test", 0)
		probeCancel()
		if probeErr != nil {
			log.Printf("[scan] TMDB 健康检查失败：%v，本次扫描跳过刮削", probeErr)
			run.warn("TMDB API 不可用（%v），本次扫描跳过刮削，仅入库文件。可稍后在设置中检查 API Key 或网络后重新扫描。", probeErr)
			run.searcher = nil
		} else {
			log.Printf("[scan] TMDB 健康检查通过，启用刮削")
		}
	}

	// 目录环 / 超深嵌套防护：
	// 网盘（尤其挂载了软链、循环 rclone/webdav 映射的源）可能返回自引用目录，
	// 无防护的递归会一直下钻 → 栈与内存爆掉 → 进程被 OOM Killer 杀死 →
	// 容器 restart 策略再拉起 → 无限重启。这里用「深度上限 + 已访问集合」双保险。
	visited := make(map[string]bool)

	// 实时进度回写：每 2 秒把当前进度（done/total/dirs/cursor）写入数据库，
	// 让前端轮询 /api/libraries/:id/scan 时能看到真实进度，而不是全程 0/0 直到结束。
	progressDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				run.mu.Lock()
				job.Done = run.done
				job.Total = run.total
				job.Dirs = run.dirs
				job.Cursor = run.cursor
				job.Skipped = run.skipped
				run.mu.Unlock()
				_ = st.UpdateScanJob(job)
			case <-progressDone:
				return
			}
		}
	}()

	// walk 只在「无法继续的致命错误」时返回 err（根目录读不到、上下文取消）。
	// 子目录级别的问题一律降级成 warning 继续走 —— 一个没权限的子目录
	// 不该让整个媒体库颗粒无收。
	var walk func(p string, depth int, isRoot bool) error
	walk = func(p string, depth int, isRoot bool) error {
		if run.ctx.Err() != nil {
			return run.ctx.Err()
		}
		if depth > MaxScanDepth {
			run.warn("目录深度超过上限 %d，跳过：%s", MaxScanDepth, p)
			return nil
		}
		// 规范化后判重，命中说明目录成环（或被重复挂载），直接跳过而非继续下钻。
		key := path.Clean(p)
		if visited[key] {
			run.warn("检测到目录环，跳过：%s", p)
			return nil
		}
		visited[key] = true

		run.rl.Take() // 限速：风控核心
		objs, err := run.client.List(p, false)
		if err != nil {
			if isRoot {
				return err // 根目录都读不到，继续没有意义
			}
			run.warn("目录读取失败已跳过：%s（%v）", p, err)
			return nil
		}
		run.mu.Lock()
		run.dirs++
		run.mu.Unlock()

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
				// 跳过 BluRay 原盘目录（BDMV/）：其下 STREAM/00001.m2ts 等是纯数字文件名，
				// 无法识别剧名和季集，且原盘播放需要特殊处理，暂时跳过避免产生垃圾条目。
				if strings.EqualFold(o.Name, "BDMV") {
					run.warn("跳过 BluRay 原盘目录：%s", full)
					continue
				}
				if err := walk(full, depth+1, false); err != nil {
					return err // 只可能是 ctx 取消，需要一路冒泡
				}
				continue
			}
			if err := run.consume(p, names, o); err != nil {
				// 单个文件入库/刮削失败（网盘抽风、strm 内容读不到）不该拖垮整库。
				run.warn("文件处理失败已跳过：%s（%v）", full, err)
			}
		}
		run.mu.Lock()
		run.cursor = p
		run.mu.Unlock()
		return nil
	}

	err := walk(lib.RootPath, 0, true)
	close(progressDone) // 停止实时进度回写 goroutine
	run.mu.Lock()
	job.Done = run.done
	job.Total = run.total
	job.Cursor = run.cursor
	job.Dirs = run.dirs
	job.Skipped = run.skipped
	job.Warnings = run.warnings
	job.FinishedAt = model.NowMillis()
	skipStrm, skipNative, done, dirs := run.skipStrm, run.skipNative, run.done, run.dirs
	run.mu.Unlock()

	if err != nil {
		job.Status = "failed"
		job.Error = FriendlyErr(lib.RootPath, err)
	} else {
		job.Status = "done"
		job.SkipHint = skipHint(lib, done, dirs, skipStrm, skipNative)
	}
	_ = st.UpdateScanJob(job)
	return err
}

// FriendlyErr 把 OpenList 的原始报错翻译成用户能照着操作的中文提示。
func FriendlyErr(root string, err error) string {
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "object not found"), strings.Contains(low, "not found"), strings.Contains(low, "404"):
		return fmt.Sprintf("网盘里找不到路径「%s」。请确认它是 OpenList 内部路径（以挂载点开头，如 /115/影视），而不是本地路径或带域名的网址。", root)
	case strings.Contains(low, "unauthorized"), strings.Contains(low, "401"), strings.Contains(low, "token"):
		return "OpenList 鉴权失败，请回到「设置」重新填写 Token 并测试连接。"
	case strings.Contains(low, "连接失败"), strings.Contains(low, "返回的不是 json"), strings.Contains(low, "返回了空响应"), strings.Contains(low, "返回 5"), strings.Contains(low, "connection refused"), strings.Contains(low, "no such host"), strings.Contains(low, "timeout"), strings.Contains(low, "deadline"):
		return fmt.Sprintf("连不上 OpenList（%v）。请检查地址是否可达、容器之间网络是否互通，或反向代理是否拦截了请求。", err)
	case strings.Contains(low, "context canceled"):
		return "扫描被中断。"
	}
	return "扫描失败：" + msg
}

// skipHint 在「扫描成功但一条都没入库」时，给出最可能的原因。
// 这是用户最容易卡住的地方：路径没错、连接也通，就是空空如也。
func skipHint(lib model.Library, done, dirs, skipStrm, skipNative int) string {
	if done > 0 {
		if skipStrm > 0 && lib.Mode == model.ModeNative {
			return fmt.Sprintf("另有 %d 个 .strm 文件因当前是「原生模式」被跳过，需要的话把媒体库改成「混合模式」。", skipStrm)
		}
		return ""
	}
	switch {
	case dirs == 0:
		return "根目录没读到任何内容。"
	case skipStrm > 0 && lib.Mode == model.ModeNative:
		return fmt.Sprintf("目录里找到 %d 个 .strm 文件，但当前媒体库是「原生模式」只收视频文件。请把模式改成「STRM 模式」或「混合模式」后重新扫描。", skipStrm)
	case skipNative > 0 && lib.Mode == model.ModeStrm:
		return fmt.Sprintf("目录里找到 %d 个视频文件，但当前媒体库是「STRM 模式」只收 .strm。请把模式改成「原生模式」或「混合模式」后重新扫描。", skipNative)
	default:
		return fmt.Sprintf("已遍历 %d 个目录，但没发现可识别的视频文件（支持 mp4/mkv/webm/mov/avi/ts/m2ts/flv 与 .strm）。请确认选的是存放影片的目录。", dirs)
	}
}

// runner 扫描运行上下文，避免每层函数传一长串参数。
type runner struct {
	ctx        context.Context
	lib        model.Library
	st         store.Store
	client     openlist.FSClient
	storages   []model.Storage
	rewrites   []model.PathRewrite
	searcher   scraper.Searcher
	rl         *RateLimiter
	onProgress func(done, total int)
	force      bool // 强制重刮（忽略已有缓存）；rescrape 用

	mu        sync.Mutex
	done      int
	total     int
	cursor    string
	dirCache  map[string][]string // 父目录文件清单缓存，避免重复探测

	dirs       int      // 已成功列举的目录数
	skipped    int      // 因模式不匹配跳过的文件总数
	skipStrm   int      // 其中：.strm 文件（native 模式下会被跳过）
	skipNative int      // 其中：普通视频文件（strm 模式下会被跳过）
	warnings   []string // 非致命问题，上限 MaxScanWarnings
}

// warn 记录一条非致命警告（同时打日志），超过上限后只计数不再追加。
func (r *runner) warn(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("[scan] %s", msg)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.warnings) < MaxScanWarnings {
		r.warnings = append(r.warnings, msg)
	} else if len(r.warnings) == MaxScanWarnings {
		r.warnings = append(r.warnings, fmt.Sprintf("（后续警告已省略，仅保留前 %d 条）", MaxScanWarnings))
	}
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
	// 模式不匹配则跳过，但要统计下来：
	// 「目录里明明有片子却扫出 0 条」几乎都是建库时模式选错，
	// 不记这笔账，用户永远只能看到一个空荡荡的海报墙。
	if isStrm || isVideo(o.Name) {
		r.mu.Lock()
		r.skipped++
		if isStrm {
			r.skipStrm++
		} else {
			r.skipNative++
		}
		r.mu.Unlock()
	}
	return nil
}

// dirChain 返回从文件所在目录到库根（不含库根之上）的各层目录名，由近及远。
// 供解析器在文件名只有集数时向上找剧名——网盘剧集普遍是「剧名/(季)/第N集.strm」结构。
// 限制在库根以内，避免把「电影」「国漫」这类分类目录当成剧名。
func (r *runner) dirChain(dir string) []string {
	root := path.Clean(r.lib.RootPath)
	cur := path.Clean(dir)
	var out []string
	for i := 0; i < MaxScanDepth; i++ {
		if cur == "/" || cur == "." || cur == "" {
			break
		}
		out = append(out, path.Base(cur))
		if cur == root {
			break // 库根本身可入选（如库根就叫「将夜 (2026)」），但不再往上
		}
		parent := path.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return out
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
	subs := detectSubtitles(dir, baseNoExt(o.Name), names, r.lib.StorageID)
	item, err := ingestNative(r.st, r.lib, r.storages, r.rewrites, path.Join(dir, o.Name), o, r.dirChain(dir), subs)
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
	subs := detectSubtitles(dir, baseNoExt(o.Name), names, r.lib.StorageID)
	item, err := IngestStrm(r.st, r.lib, r.storages, r.rewrites, o.Name, content, dir, r.dirChain(dir), subs)
	if err != nil {
		return err
	}
	r.bumpProgress()
	r.scrapeFor(item, dir, o.Name, names)
	return nil
}

// detectSubtitles 从同目录文件清单里挑出属于该媒体的外挂字幕。
// 规则：字幕基名等于媒体基名，或以「媒体基名.」开头（如 Movie.zh.srt / Movie.chi.ass）。
// 语言与显示名由文件名里的语言标记推断（subtitle.DetectLang）。
func detectSubtitles(dir, mediaBase string, names []string, storageID string) []model.Subtitle {
	var out []model.Subtitle
	for _, n := range names {
		ext := strings.ToLower(containerOf(n))
		if !subtitle.IsSubtitleExt(ext) {
			continue
		}
		base := n
		if i := strings.LastIndex(n, "."); i > 0 {
			base = n[:i]
		}
		if base != mediaBase && !strings.HasPrefix(base, mediaBase+".") {
			continue
		}
		lang, title := subtitle.DetectLang(n)
		out = append(out, model.Subtitle{
			ID:        "sub-" + hash(path.Join(dir, n)),
			StorageID: storageID,
			Path:      path.Join(dir, n),
			Lang:      lang,
			Title:     title,
			Ext:       ext,
			Source:    "sidecar",
		})
	}
	return out
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

	// 目录名中的 {tmdb-xxx} 格式（Emby/Jellyfin/Plex 通用的强制指定 TMDB ID 方式）。
	// 优先级低于 .vidrive.json，但高于自动搜索——用户既然在目录名里写了 TMDB ID，
	// 就不该再去猜，直接用它。遍历 dirChain 从近到远找第一个命中的。
	if manual == nil {
		for _, d := range r.dirChain(dir) {
			if id := extractTMDBIDFromName(d); id > 0 {
				manual = &scraper.ManualMeta{TMDBID: id}
				break
			}
		}
	}

	r.rl.Take() // 刮削（读 NFO / 调 TMDB）额外一次请求，仍走限速
	_ = scraper.Scrape(r.ctx, item, r.lib, r.st, r.client, r.searcher, nfo, poster, backdrop, manual)
}

// extractTMDBIDFromName 从目录/文件名中提取 {tmdb-12345} 格式的 TMDB ID。
// 支持 {tmdb-12345}、[tmdb-12345]、(tmdb-12345) 等常见变体，大小写不敏感。
// 未命中返回 0。
func extractTMDBIDFromName(name string) int64 {
	// 用正则太重量级，这里用简单的字符串查找：找 "tmdb-" 后面的数字。
	low := strings.ToLower(name)
	idx := strings.Index(low, "tmdb-")
	if idx < 0 {
		return 0
	}
	// 从 "tmdb-" 之后开始提取数字
	start := idx + 5
	end := start
	for end < len(name) && name[end] >= '0' && name[end] <= '9' {
		end++
	}
	if end == start {
		return 0
	}
	id, err := strconv.ParseInt(name[start:end], 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// listDirCached 获取目录的文件清单（带缓存），避免重复调用 OpenList API。
// 扫描过程中大量 findUp 会反复探测同一批父目录，缓存后每个目录只请求一次。
func (r *runner) listDirCached(dir string) []string {
	r.mu.Lock()
	if cached, ok := r.dirCache[dir]; ok {
		r.mu.Unlock()
		return cached
	}
	r.mu.Unlock()

	r.rl.Take()
	objs, err := r.client.List(dir, false)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.Name)
	}

	r.mu.Lock()
	r.dirCache[dir] = names
	r.mu.Unlock()
	return names
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
// 同目录优先用 names 清单（零请求）；父目录用 listDirCached 获取清单（带缓存，每目录只请求一次）。
func (r *runner) findUp(dir, fileName string, names []string) string {
	has := func(list []string) bool {
		for _, s := range list {
			if strings.EqualFold(s, fileName) {
				return true
			}
		}
		return false
	}
	if has(names) {
		return path.Join(dir, fileName)
	}
	cur := dir
	for i := 0; i < 6; i++ {
		parent := path.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
		parentNames := r.listDirCached(cur)
		if has(parentNames) {
			return path.Join(cur, fileName)
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
	full string, o openlist.FsObj, dirs []string, subs []model.Subtitle) (model.MediaItem, error) {

	pr := parser.ParseInDir(o.Name, dirs)
	kind := model.KindMovie
	if pr.IsSeries {
		kind = model.KindSeries
	}
	// 条目 ID 按 剧名+年份 生成：同一部剧的每一集必须归到同一个条目下，
	// 否则 20 集就会在海报墙上铺出 20 个方块。
	item := model.MediaItem{
		ID:        itemID(lib.ID, pr.Title, pr.Year),
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
	saved := resolveItem(st, item)

	f := model.MediaFile{
		ID:         "f-" + hash(full),
		ItemID:     saved.ID,
		StorageID:  lib.StorageID,
		Source:     model.SrcNative,
		Path:       full,
		Size:       int64(o.Size),
		Modified:   int64(o.Modified),
		Container:  containerOf(o.Name),
		SeasonNo:   pr.Season,
		EpisodeNo:  pr.Episode,
		Subtitles:  subs,
		ProbeState: "pending", // 懒探测（见 PLAN 4.4）
		CreatedAt:  model.NowMillis(),
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
	strmName, raw, strmpath string, dirs []string, subs []model.Subtitle) (model.MediaItem, error) {

	res := strm.NewResolver(storages, rewrites).Resolve(raw)
	pr := parser.ParseInDir(strmName, dirs)
	kind := model.KindMovie
	if pr.IsSeries {
		kind = model.KindSeries
	}
	// 同一部剧的多个 strm 必须合并成一个条目（ID 由 剧名+年份 决定），
	// 旧实现按文件名 hash 建 ID，导致「第1集」「第2集」各成一条。
	item := model.MediaItem{
		ID:        itemID(lib.ID, pr.Title, pr.Year),
		LibraryID: lib.ID,
		Kind:      kind,
		Title:     pr.Title,
		Year:      pr.Year,
		CreatedAt: model.NowMillis(),
	}
	if err := st.UpsertMediaItemByTitle(item); err != nil {
		return model.MediaItem{}, err
	}
	saved := resolveItem(st, item)

	// 文件 ID 必须按完整路径生成：不同剧里同名的「第1集.mp4.strm」若只按文件名 hash，
	// 会算出同一个 ID 而互相覆盖，后导入的剧会「吃掉」先导入那部剧的分集。
	f := model.MediaFile{
		ID:         "f-" + hash(path.Join(strmpath, strmName)),
		ItemID:     saved.ID,
		StorageID:  res.StorageID,
		Source:     model.SrcStrm,
		Path:       res.Path,
		StrmRaw:    raw,
		Container:  containerFromURL(raw),
		SeasonNo:   pr.Season,
		EpisodeNo:  pr.Episode,
		Subtitles:  subs,
		ProbeState: "pending",
		CreatedAt:  model.NowMillis(),
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
// itemID 由 库+剧名+年份 生成条目 ID，保证同一部剧的所有剧集落到同一条目。
func itemID(libID, title string, year int) string {
	return "m-" + hash(libID+"|"+strings.ToLower(strings.TrimSpace(title))+"|"+fmt.Sprint(year))
}

// resolveItem 取回 upsert 后的规范条目。
// UpsertMediaItemByTitle 以 标题+年份 去重并**保留已存在条目的 ID**，
// 旧版本库里的条目 ID 是按文件名 hash 生成的，与新规则不同；
// 直接按新 ID 取会落空并导致重复写入，故按 标题+年份 再兜一次底。
func resolveItem(st store.Store, want model.MediaItem) model.MediaItem {
	if saved, err := st.GetMediaItem(want.ID); err == nil {
		return saved
	}
	if list, err := st.ListMediaItems(want.LibraryID); err == nil {
		for _, x := range list {
			if x.Title == want.Title && x.Year == want.Year {
				return x
			}
		}
	}
	return want
}

func hash(s string) string {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%016x", h)
}

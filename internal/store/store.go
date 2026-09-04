// Package store 持久化层。
//
// 当前用单 JSON 文件实现（零依赖、沙箱内可跑），完全隐藏在 Store 接口后。
// 生产环境替换为 modernc.org/sqlite（纯 Go，无 CGO）只需新增一个实现，
// 接口与调用方完全不变。见 PLAN.md 第十三节说明。
package store

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"newmovie/internal/model"
)

// Store 是 Vidrive 所有持久化入口的抽象。
type Store interface {
	SaveStorage(s model.Storage) error
	ListStorages() ([]model.Storage, error)
	GetStorage(id string) (model.Storage, error)
	GetStorageByBaseURL(baseURL string) (model.Storage, error)
	DeleteStorage(id string) error

	SaveLibrary(l model.Library) error
	ListLibraries() ([]model.Library, error)
	GetLibrary(id string) (model.Library, error)
	DeleteLibrary(id string) error

	SaveMediaItem(m model.MediaItem) error
	ListMediaItems(libID string) ([]model.MediaItem, error)
	GetMediaItem(id string) (model.MediaItem, error)
	UpsertMediaItemByTitle(m model.MediaItem) error // 按标题+年份去重

	SaveMediaFile(f model.MediaFile) error
	ListMediaFiles(itemID string) ([]model.MediaFile, error)
	GetMediaFile(id string) (model.MediaFile, error)

	SavePathRewrite(r model.PathRewrite) error
	ListPathRewrites() ([]model.PathRewrite, error)

	SavePlayRecord(r model.PlayRecord) error
	GetPlayRecord(userID, fileID string) (model.PlayRecord, error)
	ListContinue(userID string) ([]model.PlayRecord, error)

	SaveFavorite(f model.Favorite) error
	ListFavorites(userID string) ([]model.Favorite, error)
	DeleteFavorite(userID, itemID string, kind model.FavoriteKind) error

	SaveScanJob(j model.ScanJob) error
	GetScanJob(id string) (model.ScanJob, error)
	GetLatestScanJob(libID string) (model.ScanJob, error)
	UpdateScanJob(j model.ScanJob) error

	SaveUser(u model.User) error
	GetUserByName(name string) (model.User, error)
	UpsertToken(userID, token string) error
	GetUserByToken(token string) (model.User, error)
	ListUsers() ([]model.User, error)
	DeleteUser(id string) error

	// 全局设置（如用户自填的 TMDB_API_KEY），键值对持久化。
	GetSetting(key string) (string, error)
	SaveSetting(key, value string) error
	ListSettings() (map[string]string, error)

	// Close 立即把挂起的修改落盘（进程退出前调用）。
	Close() error
}

type db struct {
	mu   sync.Mutex
	path string

	dirty  bool        // 有未落盘的修改
	timer  *time.Timer // 合并落盘定时器
	closed bool        // 已关闭：此后每次写入立即落盘

	Storages  []model.Storage  `json:"storages"`
	Libraries []model.Library  `json:"libraries"`
	Items     []model.MediaItem `json:"items"`
	Files     []model.MediaFile `json:"files"`
	Rewrites  []model.PathRewrite `json:"rewrites"`
	Records   []model.PlayRecord `json:"records"`
	Favorites []model.Favorite   `json:"favorites"`
	Jobs      []model.ScanJob    `json:"jobs"`
	Users     []model.User      `json:"users"`
	tokens    map[string]string // token -> userID
	Settings  map[string]string `json:"settings,omitempty"`

	// ---- 内存索引（不持久化）----
	// 切片才是数据本体（JSON 数组），索引只是加速结构，加载/删除后重建。
	//
	// 为什么必须有：入库路径上每次 SaveMediaFile / GetMediaItem 都要线性扫一遍
	// 全量切片。一个 5 万文件的库，扫描过程就是 5 万 × 5 万 = 25 亿次比较，
	// 在 NAS/树莓派这类弱 CPU 上足以让「扫描」看起来永远卡在 30%。
	itemIdx    map[string]int // item.ID -> 下标
	itemKeyIdx map[string]int // libID|title|year -> 下标（标题去重用）
	fileIdx    map[string]int // file.ID -> 下标
}

// NewJSONStore 打开（或创建）JSON 存储文件。
func NewJSONStore(path string) (Store, error) {
	d := &db{path: path, tokens: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, d) // 损坏则从头来，不致命
	}
	d.reindex()
	return d, nil
}

// itemKey 标题去重键。
func itemKey(libID, title string, year int) string {
	return libID + "\x00" + title + "\x00" + strconv.Itoa(year)
}

// reindex 重建全部内存索引（调用方持有 d.mu，或在构造期独占）。
func (d *db) reindex() {
	d.itemIdx = make(map[string]int, len(d.Items))
	d.itemKeyIdx = make(map[string]int, len(d.Items))
	d.fileIdx = make(map[string]int, len(d.Files))
	for i, x := range d.Items {
		d.itemIdx[x.ID] = i
		d.itemKeyIdx[itemKey(x.LibraryID, x.Title, x.Year)] = i
	}
	for i, x := range d.Files {
		d.fileIdx[x.ID] = i
	}
}

// putItemAt 就地替换条目并同步索引（标题可能被刮削改写，旧键要清掉）。
func (d *db) putItemAt(i int, m model.MediaItem) {
	old := d.Items[i]
	if oldKey := itemKey(old.LibraryID, old.Title, old.Year); oldKey != itemKey(m.LibraryID, m.Title, m.Year) {
		if j, ok := d.itemKeyIdx[oldKey]; ok && j == i {
			delete(d.itemKeyIdx, oldKey)
		}
	}
	if old.ID != m.ID {
		delete(d.itemIdx, old.ID)
	}
	d.Items[i] = m
	d.itemIdx[m.ID] = i
	d.itemKeyIdx[itemKey(m.LibraryID, m.Title, m.Year)] = i
}

// appendItem 追加条目并登记索引。
func (d *db) appendItem(m model.MediaItem) {
	d.Items = append(d.Items, m)
	i := len(d.Items) - 1
	d.itemIdx[m.ID] = i
	d.itemKeyIdx[itemKey(m.LibraryID, m.Title, m.Year)] = i
}

// flushDebounce 是合并落盘的等待窗口。
// 扫描时每入库一个条目就全量序列化整个库，是 O(n²) 的：1 万条目要写 1 万次、
// 每次都比上次更大，累计分配可达数 GB，在 ARM(NAS/树莓派) 这类弱 CPU/小内存设备上
// 会把进程拖垮甚至被 OOM Killer 杀掉 → 容器重启 → 重新扫描 → 再被杀，形成无限重启。
// 改为合并写：窗口内的多次修改只落盘一次。
const flushDebounce = 400 * time.Millisecond

// flush 标记脏数据并安排一次合并落盘（调用方持有 d.mu）。
func (d *db) flush() error {
	d.dirty = true
	if d.closed {
		return d.writeLocked()
	}
	if d.timer == nil {
		d.timer = time.AfterFunc(flushDebounce, d.flushAsync)
	} else {
		d.timer.Reset(flushDebounce)
	}
	return nil
}

// flushAsync 定时器回调：加锁后真正落盘。
func (d *db) flushAsync() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.writeLocked(); err != nil {
		log.Printf("[store] 落盘失败: %v", err)
	}
}

// writeLocked 原子写入（临时文件 + rename，调用方持有 d.mu）。
// 原子性很关键：进程若在写到一半时被杀，直接 WriteFile 会留下半截 JSON，
// 下次启动解析失败 → 数据全丢/启动异常。
func (d *db) writeLocked() error {
	if !d.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, d.path); err != nil {
		return err
	}
	d.dirty = false
	return nil
}

// Close 立即落盘并停止定时器，供进程优雅退出时调用，防止丢失最后一批写入。
func (d *db) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	return d.writeLocked()
}

func (d *db) SaveStorage(s model.Storage) error {
	d.mu.Lock(); defer d.mu.Unlock()
	for i, x := range d.Storages {
		if x.ID == s.ID { d.Storages[i] = s; return d.flush() }
	}
	d.Storages = append(d.Storages, s)
	return d.flush()
}

func (d *db) ListStorages() ([]model.Storage, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	return append([]model.Storage{}, d.Storages...), nil
}

func (d *db) GetStorage(id string) (model.Storage, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	for _, x := range d.Storages {
		if x.ID == id { return x, nil }
	}
	return model.Storage{}, os.ErrNotExist
}

func (d *db) GetStorageByBaseURL(baseURL string) (model.Storage, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	norm := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, x := range d.Storages {
		if strings.TrimRight(strings.TrimSpace(x.BaseURL), "/") == norm {
			return x, nil
		}
	}
	return model.Storage{}, os.ErrNotExist
}

func (d *db) DeleteStorage(id string) error {
	d.mu.Lock(); defer d.mu.Unlock()
	out := d.Storages[:0]
	found := false
	for _, x := range d.Storages {
		if x.ID == id {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		return os.ErrNotExist
	}
	d.Storages = out
	return d.flush()
}

func (d *db) SaveLibrary(l model.Library) error {
	d.mu.Lock(); defer d.mu.Unlock()
	for i, x := range d.Libraries {
		if x.ID == l.ID { d.Libraries[i] = l; return d.flush() }
	}
	d.Libraries = append(d.Libraries, l)
	return d.flush()
}

func (d *db) ListLibraries() ([]model.Library, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	return append([]model.Library{}, d.Libraries...), nil
}

func (d *db) GetLibrary(id string) (model.Library, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	for _, x := range d.Libraries {
		if x.ID == id { return x, nil }
	}
	return model.Library{}, os.ErrNotExist
}

func (d *db) DeleteLibrary(id string) error {
	d.mu.Lock(); defer d.mu.Unlock()
	out := d.Libraries[:0]
	found := false
	for _, x := range d.Libraries {
		if x.ID == id {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		return os.ErrNotExist
	}
	d.Libraries = out

	// 级联清理：老实现只删库记录，条目和文件全留在 JSON 里变成孤儿——
	// 既撑大存储文件、拖慢每一次线性扫描，删库重建后还可能被旧条目干扰。
	items := d.Items[:0]
	dropped := map[string]bool{}
	for _, x := range d.Items {
		if x.LibraryID == id {
			dropped[x.ID] = true
			continue
		}
		items = append(items, x)
	}
	d.Items = items
	files := d.Files[:0]
	for _, f := range d.Files {
		if dropped[f.ItemID] {
			continue
		}
		files = append(files, f)
	}
	d.Files = files
	d.reindex()
	return d.flush()
}

func (d *db) SaveMediaItem(m model.MediaItem) error {
	d.mu.Lock(); defer d.mu.Unlock()
	if i, ok := d.itemIdx[m.ID]; ok {
		d.putItemAt(i, m)
		return d.flush()
	}
	d.appendItem(m)
	return d.flush()
}

// UpsertMediaItemByTitle 按 ID 或 标题+年份 去重，剧集合集不重复建。
//
// 必须先按 ID 匹配：条目被刮削后标题会变成 TMDB 的官方名（如「将夜」→「将夜 第一季」），
// 此后再扫同一部剧，按标题就再也匹配不上，会 append 出一条同 ID 的重复记录——
// 表现为海报墙上同一部剧出现两个方块，其中一个永远没有海报。
func (d *db) UpsertMediaItemByTitle(m model.MediaItem) error {
	d.mu.Lock(); defer d.mu.Unlock()
	i, hit := -1, false
	if m.ID != "" {
		if j, ok := d.itemIdx[m.ID]; ok {
			i, hit = j, true
		}
	}
	if !hit {
		if j, ok := d.itemKeyIdx[itemKey(m.LibraryID, m.Title, m.Year)]; ok {
			i, hit = j, true
		}
	}
	if hit {
		x := d.Items[i]
		{
			// 保留更完整的元数据（本地图路径优先保留已存在的）
			if m.TMDBID != 0 { x.TMDBID = m.TMDBID }
			if m.Overview != "" { x.Overview = m.Overview }
			if m.PosterURL != "" { x.PosterURL = m.PosterURL }
			if m.BackdropURL != "" { x.BackdropURL = m.BackdropURL }
			if m.Rating > 0 { x.Rating = m.Rating }
			if m.PosterPath != "" && x.PosterPath == "" {
				x.PosterPath = m.PosterPath
				x.PosterStorageID = m.PosterStorageID
			}
			if m.BackdropPath != "" && x.BackdropPath == "" {
				x.BackdropPath = m.BackdropPath
				x.BackdropStorageID = m.BackdropStorageID
			}
		}
		d.putItemAt(i, x)
		return d.flush()
	}
	d.appendItem(m)
	return d.flush()
}

func (d *db) ListMediaItems(libID string) ([]model.MediaItem, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	out := []model.MediaItem{}
	for _, x := range d.Items {
		if x.LibraryID == libID { out = append(out, x) }
	}
	return out, nil
}

func (d *db) GetMediaItem(id string) (model.MediaItem, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	if i, ok := d.itemIdx[id]; ok {
		return d.Items[i], nil
	}
	return model.MediaItem{}, os.ErrNotExist
}

func (d *db) SaveMediaFile(f model.MediaFile) error {
	d.mu.Lock(); defer d.mu.Unlock()
	if i, ok := d.fileIdx[f.ID]; ok {
		d.Files[i] = f
		return d.flush()
	}
	d.Files = append(d.Files, f)
	d.fileIdx[f.ID] = len(d.Files) - 1
	return d.flush()
}

func (d *db) ListMediaFiles(itemID string) ([]model.MediaFile, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	out := []model.MediaFile{}
	for _, x := range d.Files {
		if x.ItemID == itemID { out = append(out, x) }
	}
	return out, nil
}

func (d *db) GetMediaFile(id string) (model.MediaFile, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	if i, ok := d.fileIdx[id]; ok {
		return d.Files[i], nil
	}
	return model.MediaFile{}, os.ErrNotExist
}

func (d *db) SavePathRewrite(r model.PathRewrite) error {
	d.mu.Lock(); defer d.mu.Unlock()
	for i, x := range d.Rewrites {
		if x.ID == r.ID { d.Rewrites[i] = r; return d.flush() }
	}
	d.Rewrites = append(d.Rewrites, r)
	return d.flush()
}

func (d *db) ListPathRewrites() ([]model.PathRewrite, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	out := append([]model.PathRewrite{}, d.Rewrites...)
	// 按优先级升序
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Priority < out[i].Priority {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (d *db) SavePlayRecord(r model.PlayRecord) error {
	d.mu.Lock(); defer d.mu.Unlock()
	r.UpdatedAt = model.NowMillis()
	for i, x := range d.Records {
		if x.UserID == r.UserID && x.FileID == r.FileID {
			d.Records[i] = r
			return d.flush()
		}
	}
	d.Records = append(d.Records, r)
	return d.flush()
}

func (d *db) GetPlayRecord(userID, fileID string) (model.PlayRecord, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	for _, x := range d.Records {
		if x.UserID == userID && x.FileID == fileID { return x, nil }
	}
	return model.PlayRecord{}, os.ErrNotExist
}

// ListContinue 返回有进度但未看完的播放记录（继续观看），按最近观看倒序。
// 不排序的话列表顺序等于写入顺序，最近看的那部反而排在最后，很反直觉。
func (d *db) ListContinue(userID string) ([]model.PlayRecord, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	out := []model.PlayRecord{}
	for _, x := range d.Records {
		if x.UserID == userID && x.Duration > 0 && x.Position > 0 && x.Position < x.Duration-30 {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

func (d *db) SaveFavorite(f model.Favorite) error {
	d.mu.Lock(); defer d.mu.Unlock()
	for _, x := range d.Favorites {
		if x.UserID == f.UserID && x.ItemID == f.ItemID && x.Kind == f.Kind {
			return nil // 已存在
		}
	}
	d.Favorites = append(d.Favorites, f)
	return d.flush()
}

func (d *db) ListFavorites(userID string) ([]model.Favorite, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	out := []model.Favorite{}
	for _, x := range d.Favorites {
		if x.UserID == userID { out = append(out, x) }
	}
	return out, nil
}

// DeleteFavorite 取消收藏。kind 为空表示删掉该条目下所有类型的收藏。
// 以前只有「加」没有「取消」，收藏了就再也去不掉，只能手改 JSON。
func (d *db) DeleteFavorite(userID, itemID string, kind model.FavoriteKind) error {
	d.mu.Lock(); defer d.mu.Unlock()
	out := d.Favorites[:0]
	found := false
	for _, x := range d.Favorites {
		if x.UserID == userID && x.ItemID == itemID && (kind == "" || x.Kind == kind) {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		return os.ErrNotExist
	}
	d.Favorites = out
	return d.flush()
}

func (d *db) SaveScanJob(j model.ScanJob) error {
	d.mu.Lock(); defer d.mu.Unlock()
	for i, x := range d.Jobs {
		if x.ID == j.ID { d.Jobs[i] = j; return d.flush() }
	}
	d.Jobs = append(d.Jobs, j)
	return d.flush()
}

func (d *db) GetScanJob(id string) (model.ScanJob, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	for _, x := range d.Jobs {
		if x.ID == id { return x, nil }
	}
	return model.ScanJob{}, os.ErrNotExist
}

// GetLatestScanJob 返回某媒体库最近一次（StartedAt 最大）的扫描任务。
func (d *db) GetLatestScanJob(libID string) (model.ScanJob, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	var best model.ScanJob
	found := false
	for _, x := range d.Jobs {
		if x.LibraryID != libID {
			continue
		}
		if !found || x.StartedAt > best.StartedAt {
			best = x
			found = true
		}
	}
	if !found {
		return model.ScanJob{}, os.ErrNotExist
	}
	return best, nil
}

func (d *db) UpdateScanJob(j model.ScanJob) error { return d.SaveScanJob(j) }

func (d *db) SaveUser(u model.User) error {
	d.mu.Lock(); defer d.mu.Unlock()
	for i, x := range d.Users {
		if x.ID == u.ID { d.Users[i] = u; return d.flush() }
	}
	d.Users = append(d.Users, u)
	return d.flush()
}

func (d *db) GetUserByName(name string) (model.User, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	for _, x := range d.Users {
		if x.Username == name { return x, nil }
	}
	return model.User{}, os.ErrNotExist
}

func (d *db) UpsertToken(userID, token string) error {
	d.mu.Lock(); defer d.mu.Unlock()
	for i := range d.Users {
		if d.Users[i].ID == userID { d.Users[i].Token = token }
	}
	d.tokens[token] = userID
	return d.flush()
}

func (d *db) GetUserByToken(token string) (model.User, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	uid, ok := d.tokens[token]
	if !ok { return model.User{}, os.ErrNotExist }
	for _, x := range d.Users {
		if x.ID == uid { return x, nil }
	}
	return model.User{}, os.ErrNotExist
}

// ListUsers 列出全部用户（用户名与权限信息；密码哈希保留但由调用方决定是否外泄）。
func (d *db) ListUsers() ([]model.User, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	out := make([]model.User, len(d.Users))
	copy(out, d.Users)
	return out, nil
}

// DeleteUser 删除用户及其 token。管理员本人不可删（由 API 层保证）。
func (d *db) DeleteUser(id string) error {
	d.mu.Lock(); defer d.mu.Unlock()
	for i, x := range d.Users {
		if x.ID == id {
			d.Users = append(d.Users[:i], d.Users[i+1:]...)
			delete(d.tokens, x.Token)
			return d.flush()
		}
	}
	return os.ErrNotExist
}

// ---- 全局设置 ----

func (d *db) GetSetting(key string) (string, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	if d.Settings == nil {
		return "", os.ErrNotExist
	}
	v, ok := d.Settings[key]
	if !ok {
		return "", os.ErrNotExist
	}
	return v, nil
}

func (d *db) SaveSetting(key, value string) error {
	d.mu.Lock(); defer d.mu.Unlock()
	if d.Settings == nil {
		d.Settings = map[string]string{}
	}
	d.Settings[key] = value
	return d.flush()
}

func (d *db) ListSettings() (map[string]string, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	out := map[string]string{}
	for k, v := range d.Settings {
		out[k] = v
	}
	return out, nil
}

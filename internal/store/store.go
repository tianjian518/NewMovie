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
	"sync"
	"time"

	"newmovie/internal/model"
)

// Store 是 Vidrive 所有持久化入口的抽象。
type Store interface {
	SaveStorage(s model.Storage) error
	ListStorages() ([]model.Storage, error)
	GetStorage(id string) (model.Storage, error)

	SaveLibrary(l model.Library) error
	ListLibraries() ([]model.Library, error)
	GetLibrary(id string) (model.Library, error)

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

	SaveScanJob(j model.ScanJob) error
	GetScanJob(id string) (model.ScanJob, error)
	UpdateScanJob(j model.ScanJob) error

	SaveUser(u model.User) error
	GetUserByName(name string) (model.User, error)
	UpsertToken(userID, token string) error
	GetUserByToken(token string) (model.User, error)

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
}

// NewJSONStore 打开（或创建）JSON 存储文件。
func NewJSONStore(path string) (Store, error) {
	d := &db{path: path, tokens: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, d) // 损坏则从头来，不致命
	}
	return d, nil
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

func (d *db) SaveMediaItem(m model.MediaItem) error {
	d.mu.Lock(); defer d.mu.Unlock()
	for i, x := range d.Items {
		if x.ID == m.ID { d.Items[i] = m; return d.flush() }
	}
	d.Items = append(d.Items, m)
	return d.flush()
}

// UpsertMediaItemByTitle 按 标题+年份 去重，剧集合集不重复建。
func (d *db) UpsertMediaItemByTitle(m model.MediaItem) error {
	d.mu.Lock(); defer d.mu.Unlock()
	for i, x := range d.Items {
		if x.Title == m.Title && x.Year == m.Year && x.LibraryID == m.LibraryID {
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
			d.Items[i] = x
			return d.flush()
		}
	}
	d.Items = append(d.Items, m)
	return d.flush()
}

func (d *db) ListMediaItems(libID string) ([]model.MediaItem, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	var out []model.MediaItem
	for _, x := range d.Items {
		if x.LibraryID == libID { out = append(out, x) }
	}
	return out, nil
}

func (d *db) GetMediaItem(id string) (model.MediaItem, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	for _, x := range d.Items {
		if x.ID == id { return x, nil }
	}
	return model.MediaItem{}, os.ErrNotExist
}

func (d *db) SaveMediaFile(f model.MediaFile) error {
	d.mu.Lock(); defer d.mu.Unlock()
	for i, x := range d.Files {
		if x.ID == f.ID { d.Files[i] = f; return d.flush() }
	}
	d.Files = append(d.Files, f)
	return d.flush()
}

func (d *db) ListMediaFiles(itemID string) ([]model.MediaFile, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	var out []model.MediaFile
	for _, x := range d.Files {
		if x.ItemID == itemID { out = append(out, x) }
	}
	return out, nil
}

func (d *db) GetMediaFile(id string) (model.MediaFile, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	for _, x := range d.Files {
		if x.ID == id { return x, nil }
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

// ListContinue 返回有进度但未看完的播放记录（继续观看）。
func (d *db) ListContinue(userID string) ([]model.PlayRecord, error) {
	d.mu.Lock(); defer d.mu.Unlock()
	var out []model.PlayRecord
	for _, x := range d.Records {
		if x.UserID == userID && x.Duration > 0 && x.Position > 0 && x.Position < x.Duration-30 {
			out = append(out, x)
		}
	}
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
	var out []model.Favorite
	for _, x := range d.Favorites {
		if x.UserID == userID { out = append(out, x) }
	}
	return out, nil
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

// Package store 持久化层。
//
// 当前用单 JSON 文件实现（零依赖、沙箱内可跑），完全隐藏在 Store 接口后。
// 生产环境替换为 modernc.org/sqlite（纯 Go，无 CGO）只需新增一个实现，
// 接口与调用方完全不变。见 PLAN.md 第十三节说明。
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

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
}

type db struct {
	mu sync.Mutex
	path string

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

func (d *db) flush() error {
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(d.path, b, 0o644)
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

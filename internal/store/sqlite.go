// Package store 的 SQLite 实现（生产默认）。
//
// 用 modernc.org/sqlite（纯 Go、无 CGO）驱动，替换早期的单 JSON 文件方案：
//   - 解决万级条目全量读写 JSON 的性能天花板（写入 O(n) 而非 O(n²)，查询走索引）
//   - 支持 LIKE 全文搜索下沉到 SQL，不再全表内存扫描
//   - 保留旧 JSON 数据自动迁移（首启发现 .json 且库为空时导入）
//
// 复杂结构（字幕/音轨列表、扫描警告）以 JSON 文本存入单列，读取时反序列化。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无 CGO

	"newmovie/internal/model"
)

// sqliteStore 是 Store 接口的 SQLite 实现。
type sqliteStore struct {
	db *sql.DB
}

// 表结构（初始化用）。
const schemaSQL = `
CREATE TABLE IF NOT EXISTS storages (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	type TEXT NOT NULL DEFAULT 'openlist',
	base_url TEXT NOT NULL DEFAULT '',
	token TEXT NOT NULL DEFAULT '',
	sign_key TEXT NOT NULL DEFAULT '',
	rate_limit REAL NOT NULL DEFAULT 2,
	local_root TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS libraries (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL DEFAULT 'native',
	storage_id TEXT NOT NULL DEFAULT '',
	root_path TEXT NOT NULL DEFAULT '',
	scan_rate REAL NOT NULL DEFAULT 2,
	created_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS media_items (
	id TEXT PRIMARY KEY,
	library_id TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT 'movie',
	tmdb_id INTEGER NOT NULL DEFAULT 0,
	title TEXT NOT NULL DEFAULT '',
	year INTEGER NOT NULL DEFAULT 0,
	overview TEXT NOT NULL DEFAULT '',
	poster_url TEXT NOT NULL DEFAULT '',
	backdrop_url TEXT NOT NULL DEFAULT '',
	rating REAL NOT NULL DEFAULT 0,
	poster_path TEXT NOT NULL DEFAULT '',
	poster_storage_id TEXT NOT NULL DEFAULT '',
	backdrop_path TEXT NOT NULL DEFAULT '',
	backdrop_storage_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_items_lib ON media_items(library_id);
CREATE INDEX IF NOT EXISTS idx_items_title ON media_items(title);
CREATE INDEX IF NOT EXISTS idx_items_lib_title_year ON media_items(library_id, title, year);
CREATE TABLE IF NOT EXISTS media_files (
	id TEXT PRIMARY KEY,
	item_id TEXT NOT NULL DEFAULT '',
	episode_id TEXT NOT NULL DEFAULT '',
	storage_id TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT 'native',
	path TEXT NOT NULL DEFAULT '',
	strm_raw TEXT NOT NULL DEFAULT '',
	size INTEGER NOT NULL DEFAULT 0,
	modified INTEGER NOT NULL DEFAULT 0,
	container TEXT NOT NULL DEFAULT '',
	video_codec TEXT NOT NULL DEFAULT '',
	audio_codec TEXT NOT NULL DEFAULT '',
	duration_sec INTEGER NOT NULL DEFAULT 0,
	season_no INTEGER NOT NULL DEFAULT 0,
	episode_no INTEGER NOT NULL DEFAULT 0,
	supports_range INTEGER NOT NULL DEFAULT 0,
	probe_state TEXT NOT NULL DEFAULT '',
	subtitles TEXT NOT NULL DEFAULT '',
	audio_tracks TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_files_item ON media_files(item_id);
CREATE TABLE IF NOT EXISTS path_rewrites (
	id TEXT PRIMARY KEY,
	priority INTEGER NOT NULL DEFAULT 0,
	pattern TEXT NOT NULL DEFAULT '',
	replacement TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS play_records (
	id TEXT NOT NULL DEFAULT '',
	user_id TEXT NOT NULL DEFAULT '',
	file_id TEXT NOT NULL DEFAULT '',
	position INTEGER NOT NULL DEFAULT 0,
	duration INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (user_id, file_id)
);
CREATE INDEX IF NOT EXISTS idx_records_user ON play_records(user_id, updated_at);
CREATE TABLE IF NOT EXISTS favorites (
	id TEXT NOT NULL DEFAULT '',
	user_id TEXT NOT NULL DEFAULT '',
	item_id TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT 'favorite',
	PRIMARY KEY (user_id, item_id, kind)
);
CREATE TABLE IF NOT EXISTS scan_jobs (
	id TEXT PRIMARY KEY,
	library_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'running',
	total INTEGER NOT NULL DEFAULT 0,
	done INTEGER NOT NULL DEFAULT 0,
	cursor TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	warnings TEXT NOT NULL DEFAULT '',
	skipped INTEGER NOT NULL DEFAULT 0,
	skip_hint TEXT NOT NULL DEFAULT '',
	dirs INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_jobs_lib ON scan_jobs(library_id, started_at);
CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	username TEXT NOT NULL DEFAULT '',
	password TEXT NOT NULL DEFAULT '',
	is_admin INTEGER NOT NULL DEFAULT 0,
	token TEXT NOT NULL DEFAULT '',
	child_mode INTEGER NOT NULL DEFAULT 0,
	allowed_libs TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_users_name ON users(username);
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL DEFAULT ''
);
`

// NewSQLiteStore 打开（或创建）SQLite 数据库并完成建表。
func NewSQLiteStore(path string) (Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite 单写者：单连接避免并发写锁（database is locked）。
	// 读多写少场景（媒体库浏览/播放为主），单连接足够且最稳。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, err
	}
	// Schema 演进：旧库的 users 表没有 child_mode / allowed_libs 两列，
	// SQLite 的 CREATE TABLE IF NOT EXISTS 不会给已有表补列，需显式 ALTER TABLE。
	// 列已存在时 ALTER 会报错，忽略即可（幂等）。
	_, _ = db.Exec("ALTER TABLE users ADD COLUMN child_mode INTEGER NOT NULL DEFAULT 0")
	_, _ = db.Exec("ALTER TABLE users ADD COLUMN allowed_libs TEXT NOT NULL DEFAULT ''")
	s := &sqliteStore{db: db}
	// 自动迁移：目标 SQLite 空，且同目录存在旧 JSON → 导入。
	if err := s.migrateIfEmpty(filepath.Dir(path)); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库连接。
func (s *sqliteStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// ---- 旧 JSON 迁移 ----

// legacyJSON 与旧 JSON 文件结构一致，仅用于首启迁移解析。
type legacyJSON struct {
	Storages  []model.Storage     `json:"storages"`
	Libraries []model.Library     `json:"libraries"`
	Items     []model.MediaItem   `json:"items"`
	Files     []model.MediaFile   `json:"files"`
	Rewrites  []model.PathRewrite `json:"rewrites"`
	Records   []model.PlayRecord  `json:"records"`
	Favorites []model.Favorite    `json:"favorites"`
	Jobs      []model.ScanJob     `json:"jobs"`
	Users     []model.User        `json:"users"`
	Settings  map[string]string   `json:"settings,omitempty"`
}

// migrateIfEmpty 若库中没有任何数据且存在旧 newmovie.json，则导入。
func (s *sqliteStore) migrateIfEmpty(dir string) error {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM media_items").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil // 已有数据，跳过迁移
	}
	jsonPath := filepath.Join(dir, "newmovie.json")
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil // 无旧文件，无需迁移
	}
	var old legacyJSON
	if err := json.Unmarshal(b, &old); err != nil {
		return nil // 旧文件损坏，忽略（不阻断启动）
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	rollback := func() { tx.Rollback() }
	for _, x := range old.Storages {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO storages
			(id,name,type,base_url,token,sign_key,rate_limit,local_root,created_at)
			VALUES(?,?,?,?,?,?,?,?,?)`,
			x.ID, x.Name, string(x.Type), x.BaseURL, x.Token, x.SignKey, x.RateLimit, x.LocalRoot, x.CreatedAt); err != nil {
			rollback()
			return err
		}
	}
	for _, x := range old.Libraries {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO libraries
			(id,name,mode,storage_id,root_path,scan_rate,created_at)
			VALUES(?,?,?,?,?,?,?)`,
			x.ID, x.Name, string(x.Mode), x.StorageID, x.RootPath, x.ScanRate, x.CreatedAt); err != nil {
			rollback()
			return err
		}
	}
	for _, x := range old.Items {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO media_items
			(id,library_id,kind,tmdb_id,title,year,overview,poster_url,backdrop_url,rating,
			 poster_path,poster_storage_id,backdrop_path,backdrop_storage_id,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			x.ID, x.LibraryID, string(x.Kind), x.TMDBID, x.Title, x.Year, x.Overview,
			x.PosterURL, x.BackdropURL, x.Rating, x.PosterPath, x.PosterStorageID,
			x.BackdropPath, x.BackdropStorageID, x.CreatedAt); err != nil {
			rollback()
			return err
		}
	}
	for _, x := range old.Files {
		if err := s.insertFileTx(tx, x); err != nil {
			rollback()
			return err
		}
	}
	for _, x := range old.Rewrites {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO path_rewrites (id,priority,pattern,replacement)
			VALUES(?,?,?,?)`, x.ID, x.Priority, x.Pattern, x.Replacement); err != nil {
			rollback()
			return err
		}
	}
	for _, x := range old.Records {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO play_records
			(id,user_id,file_id,position,duration,updated_at)
			VALUES(?,?,?,?,?,?)`, x.ID, x.UserID, x.FileID, x.Position, x.Duration, x.UpdatedAt); err != nil {
			rollback()
			return err
		}
	}
	for _, x := range old.Favorites {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO favorites (id,user_id,item_id,kind)
			VALUES(?,?,?,?)`, x.ID, x.UserID, x.ItemID, string(x.Kind)); err != nil {
			rollback()
			return err
		}
	}
	for _, x := range old.Jobs {
		if err := s.insertScanJobTx(tx, x); err != nil {
			rollback()
			return err
		}
	}
	for _, x := range old.Users {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO users (id,username,password,is_admin,token)
			VALUES(?,?,?,?,?)`, x.ID, x.Username, x.Password, boolInt(x.IsAdmin), x.Token); err != nil {
			rollback()
			return err
		}
	}
	for k, v := range old.Settings {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO settings (key,value) VALUES(?,?)`, k, v); err != nil {
			rollback()
			return err
		}
	}
	return tx.Commit()
}

// ---- 序列化辅助 ----

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func jsonStr(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// ---- 存储源 ----

func (s *sqliteStore) SaveStorage(x model.Storage) error {
	_, err := s.db.Exec(`INSERT INTO storages
		(id,name,type,base_url,token,sign_key,rate_limit,local_root,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, type=excluded.type, base_url=excluded.base_url,
			token=excluded.token, sign_key=excluded.sign_key, rate_limit=excluded.rate_limit,
			local_root=excluded.local_root, created_at=excluded.created_at`,
		x.ID, x.Name, string(x.Type), x.BaseURL, x.Token, x.SignKey, x.RateLimit, x.LocalRoot, x.CreatedAt)
	return err
}

func (s *sqliteStore) ListStorages() ([]model.Storage, error) {
	rows, err := s.db.Query(`SELECT id,name,type,base_url,token,sign_key,rate_limit,local_root,created_at FROM storages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Storage{}
	for rows.Next() {
		var x model.Storage
		var typ string
		if err := rows.Scan(&x.ID, &x.Name, &typ, &x.BaseURL, &x.Token, &x.SignKey, &x.RateLimit, &x.LocalRoot, &x.CreatedAt); err != nil {
			return nil, err
		}
		x.Type = model.StorageType(typ)
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *sqliteStore) GetStorage(id string) (model.Storage, error) {
	var x model.Storage
	var typ string
	err := s.db.QueryRow(`SELECT id,name,type,base_url,token,sign_key,rate_limit,local_root,created_at FROM storages WHERE id=?`, id).
		Scan(&x.ID, &x.Name, &typ, &x.BaseURL, &x.Token, &x.SignKey, &x.RateLimit, &x.LocalRoot, &x.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Storage{}, os.ErrNotExist
	}
	if err != nil {
		return model.Storage{}, err
	}
	x.Type = model.StorageType(typ)
	return x, nil
}

func (s *sqliteStore) GetStorageByBaseURL(baseURL string) (model.Storage, error) {
	norm := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	var x model.Storage
	var typ string
	err := s.db.QueryRow(`SELECT id,name,type,base_url,token,sign_key,rate_limit,local_root,created_at
		FROM storages WHERE rtrim(trim(base_url),'/')=?`, norm).
		Scan(&x.ID, &x.Name, &typ, &x.BaseURL, &x.Token, &x.SignKey, &x.RateLimit, &x.LocalRoot, &x.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Storage{}, os.ErrNotExist
	}
	if err != nil {
		return model.Storage{}, err
	}
	x.Type = model.StorageType(typ)
	return x, nil
}

func (s *sqliteStore) DeleteStorage(id string) error {
	res, err := s.db.Exec(`DELETE FROM storages WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return os.ErrNotExist
	}
	return nil
}

// ---- 媒体库 ----

func (s *sqliteStore) SaveLibrary(x model.Library) error {
	_, err := s.db.Exec(`INSERT INTO libraries (id,name,mode,storage_id,root_path,scan_rate,created_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, mode=excluded.mode, storage_id=excluded.storage_id,
			root_path=excluded.root_path, scan_rate=excluded.scan_rate, created_at=excluded.created_at`,
		x.ID, x.Name, string(x.Mode), x.StorageID, x.RootPath, x.ScanRate, x.CreatedAt)
	return err
}

func (s *sqliteStore) ListLibraries() ([]model.Library, error) {
	rows, err := s.db.Query(`SELECT id,name,mode,storage_id,root_path,scan_rate,created_at FROM libraries`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Library{}
	for rows.Next() {
		var x model.Library
		var mode string
		if err := rows.Scan(&x.ID, &x.Name, &mode, &x.StorageID, &x.RootPath, &x.ScanRate, &x.CreatedAt); err != nil {
			return nil, err
		}
		x.Mode = model.LibraryMode(mode)
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *sqliteStore) GetLibrary(id string) (model.Library, error) {
	var x model.Library
	var mode string
	err := s.db.QueryRow(`SELECT id,name,mode,storage_id,root_path,scan_rate,created_at FROM libraries WHERE id=?`, id).
		Scan(&x.ID, &x.Name, &mode, &x.StorageID, &x.RootPath, &x.ScanRate, &x.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Library{}, os.ErrNotExist
	}
	if err != nil {
		return model.Library{}, err
	}
	x.Mode = model.LibraryMode(mode)
	return x, nil
}

func (s *sqliteStore) DeleteLibrary(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 级联清理：删除该库的条目与其文件（与 JSON 实现行为一致）。
	if _, err := tx.Exec(`DELETE FROM media_files WHERE item_id IN (SELECT id FROM media_items WHERE library_id=?)`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM media_items WHERE library_id=?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM libraries WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return os.ErrNotExist
	}
	return tx.Commit()
}

// ---- 媒体条目 ----

func (s *sqliteStore) SaveMediaItem(x model.MediaItem) error {
	_, err := s.db.Exec(`INSERT INTO media_items
		(id,library_id,kind,tmdb_id,title,year,overview,poster_url,backdrop_url,rating,
		 poster_path,poster_storage_id,backdrop_path,backdrop_storage_id,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			library_id=excluded.library_id, kind=excluded.kind, tmdb_id=excluded.tmdb_id,
			title=excluded.title, year=excluded.year, overview=excluded.overview,
			poster_url=excluded.poster_url, backdrop_url=excluded.backdrop_url,
			rating=excluded.rating, poster_path=excluded.poster_path,
			poster_storage_id=excluded.poster_storage_id, backdrop_path=excluded.backdrop_path,
			backdrop_storage_id=excluded.backdrop_storage_id, created_at=excluded.created_at`,
		x.ID, x.LibraryID, string(x.Kind), x.TMDBID, x.Title, x.Year, x.Overview,
		x.PosterURL, x.BackdropURL, x.Rating, x.PosterPath, x.PosterStorageID,
		x.BackdropPath, x.BackdropStorageID, x.CreatedAt)
	return err
}

// UpsertMediaItemByTitle 按 ID 或 标题+年份 去重（语义与 JSON 版一致）。
// 命中时合并保留更完整的元数据（本地图路径优先保留已存在的）。
func (s *sqliteStore) UpsertMediaItemByTitle(m model.MediaItem) error {
	// 1) 按 ID 匹配
	if m.ID != "" {
		old, err := s.GetMediaItem(m.ID)
		if err == nil {
			return s.SaveMediaItem(mergeItem(old, m))
		}
	}
	// 2) 按 库+标题+年份 匹配
	var old model.MediaItem
	var kind string
	err := s.db.QueryRow(`SELECT id,library_id,kind,tmdb_id,title,year,overview,poster_url,
		backdrop_url,rating,poster_path,poster_storage_id,backdrop_path,backdrop_storage_id,created_at
		FROM media_items WHERE library_id=? AND title=? AND year=?`,
		m.LibraryID, m.Title, m.Year).
		Scan(&old.ID, &old.LibraryID, &kind, &old.TMDBID, &old.Title, &old.Year, &old.Overview,
			&old.PosterURL, &old.BackdropURL, &old.Rating, &old.PosterPath, &old.PosterStorageID,
			&old.BackdropPath, &old.BackdropStorageID, &old.CreatedAt)
	if err == nil {
		old.Kind = model.MediaKind(kind)
		// 命中：保留既有 ID，仅当新条目带了非空 ID 才迁移（旧库的文件名 hash ID → 剧名+年份 ID）。
		// 若 m.ID 为空则绝不动 old.ID——否则会把已有条目的主键清空，导致后续按 ID 查找全部落空。
		if m.ID != "" {
			old.ID = m.ID
		}
		return s.SaveMediaItem(mergeItem(old, m))
	}
	// 3) 插入新条目
	return s.SaveMediaItem(m)
}

// mergeItem 合并新数据 m 到既有条目 x（与 JSON 实现同规则）。
func mergeItem(x, m model.MediaItem) model.MediaItem {
	if m.TMDBID != 0 {
		x.TMDBID = m.TMDBID
	}
	if m.Overview != "" {
		x.Overview = m.Overview
	}
	if m.PosterURL != "" {
		x.PosterURL = m.PosterURL
	}
	if m.BackdropURL != "" {
		x.BackdropURL = m.BackdropURL
	}
	if m.Rating > 0 {
		x.Rating = m.Rating
	}
	if m.PosterPath != "" && x.PosterPath == "" {
		x.PosterPath = m.PosterPath
		x.PosterStorageID = m.PosterStorageID
	}
	if m.BackdropPath != "" && x.BackdropPath == "" {
		x.BackdropPath = m.BackdropPath
		x.BackdropStorageID = m.BackdropStorageID
	}
	if m.Kind != "" {
		x.Kind = m.Kind
	}
	return x
}

func (s *sqliteStore) ListMediaItems(libID string) ([]model.MediaItem, error) {
	rows, err := s.db.Query(`SELECT id,library_id,kind,tmdb_id,title,year,overview,poster_url,
		backdrop_url,rating,poster_path,poster_storage_id,backdrop_path,backdrop_storage_id,created_at
		FROM media_items WHERE library_id=? ORDER BY title`, libID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

func (s *sqliteStore) GetMediaItem(id string) (model.MediaItem, error) {
	var x model.MediaItem
	var kind string
	err := s.db.QueryRow(`SELECT id,library_id,kind,tmdb_id,title,year,overview,poster_url,
		backdrop_url,rating,poster_path,poster_storage_id,backdrop_path,backdrop_storage_id,created_at
		FROM media_items WHERE id=?`, id).
		Scan(&x.ID, &x.LibraryID, &kind, &x.TMDBID, &x.Title, &x.Year, &x.Overview,
			&x.PosterURL, &x.BackdropURL, &x.Rating, &x.PosterPath, &x.PosterStorageID,
			&x.BackdropPath, &x.BackdropStorageID, &x.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.MediaItem{}, os.ErrNotExist
	}
	if err != nil {
		return model.MediaItem{}, err
	}
	x.Kind = model.MediaKind(kind)
	return x, nil
}

// SearchMediaItems 是 SQLite 专有的高效搜索（Store 接口之外的扩展方法）。
// 由 handlers 通过类型断言使用：JSON store 无此方法时退回线性扫描。
func (s *sqliteStore) SearchMediaItems(q, kind, libID, sortBy string, offset, limit int) ([]model.MediaItem, error) {
	where, args := buildSearchWhere(q, kind, libID)
	order := "title"
	switch sortBy {
	case "year":
		order = "year DESC, title"
	case "rating":
		order = "rating DESC, title"
	case "recent":
		order = "created_at DESC, title"
	}
	sql := "SELECT id,library_id,kind,tmdb_id,title,year,overview,poster_url,backdrop_url,rating," +
		"poster_path,poster_storage_id,backdrop_path,backdrop_storage_id,created_at FROM media_items WHERE " +
		strings.Join(where, " AND ") + " ORDER BY " + order
	if limit > 0 {
		sql += " LIMIT ? OFFSET ?"
		args = append(args, limit, offset)
	}
	rows, err := s.db.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// CountMediaItems 返回符合搜索条件的条目总数（与 SearchMediaItems 共用过滤条件）。
// 用于前端分页：知道 total 才能显示「共 N 条」和精确页码，而不是靠「最后一页不满」来猜。
func (s *sqliteStore) CountMediaItems(q, kind, libID string) (int, error) {
	where, args := buildSearchWhere(q, kind, libID)
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM media_items WHERE "+strings.Join(where, " AND "), args...).Scan(&n)
	return n, err
}

// buildSearchWhere 构建搜索过滤条件（Search 与 Count 共用，保证两者口径一致）。
func buildSearchWhere(q, kind, libID string) ([]string, []interface{}) {
	where := []string{"1=1"}
	args := []interface{}{}
	if libID != "" {
		where = append(where, "library_id=?")
		args = append(args, libID)
	}
	if kind != "" {
		where = append(where, "kind=?")
		args = append(args, kind)
	}
	if q != "" {
		where = append(where, "(title LIKE ? OR overview LIKE ?)")
		like := "%" + strings.ToLower(q) + "%"
		args = append(args, like, like)
	}
	return where, args
}

func scanItems(rows *sql.Rows) ([]model.MediaItem, error) {
	out := []model.MediaItem{}
	for rows.Next() {
		var x model.MediaItem
		var kind string
		if err := rows.Scan(&x.ID, &x.LibraryID, &kind, &x.TMDBID, &x.Title, &x.Year, &x.Overview,
			&x.PosterURL, &x.BackdropURL, &x.Rating, &x.PosterPath, &x.PosterStorageID,
			&x.BackdropPath, &x.BackdropStorageID, &x.CreatedAt); err != nil {
			return nil, err
		}
		x.Kind = model.MediaKind(kind)
		out = append(out, x)
	}
	return out, rows.Err()
}

// ---- 媒体文件 ----

func (s *sqliteStore) SaveMediaFile(x model.MediaFile) error {
	return s.insertFileTx(s.db, x)
}

func (s *sqliteStore) insertFileTx(exec interface {
	Exec(string, ...interface{}) (sql.Result, error)
}, x model.MediaFile) error {
	_, err := exec.Exec(`INSERT INTO media_files
		(id,item_id,episode_id,storage_id,source,path,strm_raw,size,modified,container,
		 video_codec,audio_codec,duration_sec,season_no,episode_no,supports_range,probe_state,
		 subtitles,audio_tracks,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			item_id=excluded.item_id, episode_id=excluded.episode_id, storage_id=excluded.storage_id,
			source=excluded.source, path=excluded.path, strm_raw=excluded.strm_raw,
			size=excluded.size, modified=excluded.modified, container=excluded.container,
			video_codec=excluded.video_codec, audio_codec=excluded.audio_codec,
			duration_sec=excluded.duration_sec, season_no=excluded.season_no,
			episode_no=excluded.episode_no, supports_range=excluded.supports_range,
			probe_state=excluded.probe_state, subtitles=excluded.subtitles,
			audio_tracks=excluded.audio_tracks, created_at=excluded.created_at`,
		x.ID, x.ItemID, x.EpisodeID, x.StorageID, string(x.Source), x.Path, x.StrmRaw,
		x.Size, x.Modified, x.Container, x.VideoCodec, x.AudioCodec, x.DurationSec,
		x.SeasonNo, x.EpisodeNo, boolInt(x.SupportsRange), x.ProbeState,
		jsonStr(x.Subtitles), jsonStr(x.AudioTracks), x.CreatedAt)
	return err
}

func (s *sqliteStore) ListMediaFiles(itemID string) ([]model.MediaFile, error) {
	rows, err := s.db.Query(`SELECT id,item_id,episode_id,storage_id,source,path,strm_raw,size,modified,
		container,video_codec,audio_codec,duration_sec,season_no,episode_no,supports_range,probe_state,
		subtitles,audio_tracks,created_at FROM media_files WHERE item_id=?`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFiles(rows)
}

func (s *sqliteStore) GetMediaFile(id string) (model.MediaFile, error) {
	rows, err := s.db.Query(`SELECT id,item_id,episode_id,storage_id,source,path,strm_raw,size,modified,
		container,video_codec,audio_codec,duration_sec,season_no,episode_no,supports_range,probe_state,
		subtitles,audio_tracks,created_at FROM media_files WHERE id=?`, id)
	if err != nil {
		return model.MediaFile{}, err
	}
	defer rows.Close()
	fs, err := scanFiles(rows)
	if err != nil {
		return model.MediaFile{}, err
	}
	if len(fs) == 0 {
		return model.MediaFile{}, os.ErrNotExist
	}
	return fs[0], nil
}

func scanFiles(rows *sql.Rows) ([]model.MediaFile, error) {
	out := []model.MediaFile{}
	for rows.Next() {
		var x model.MediaFile
		var source string
		var supports int
		var subsJSON, tracksJSON string
		if err := rows.Scan(&x.ID, &x.ItemID, &x.EpisodeID, &x.StorageID, &source, &x.Path,
			&x.StrmRaw, &x.Size, &x.Modified, &x.Container, &x.VideoCodec, &x.AudioCodec,
			&x.DurationSec, &x.SeasonNo, &x.EpisodeNo, &supports, &x.ProbeState,
			&subsJSON, &tracksJSON, &x.CreatedAt); err != nil {
			return nil, err
		}
		x.Source = model.SourceType(source)
		x.SupportsRange = supports == 1
		if subsJSON != "" {
			_ = json.Unmarshal([]byte(subsJSON), &x.Subtitles)
		}
		if tracksJSON != "" {
			_ = json.Unmarshal([]byte(tracksJSON), &x.AudioTracks)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ---- strm 路径重写规则 ----

func (s *sqliteStore) SavePathRewrite(x model.PathRewrite) error {
	_, err := s.db.Exec(`INSERT INTO path_rewrites (id,priority,pattern,replacement)
		VALUES(?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET priority=excluded.priority, pattern=excluded.pattern, replacement=excluded.replacement`,
		x.ID, x.Priority, x.Pattern, x.Replacement)
	return err
}

func (s *sqliteStore) ListPathRewrites() ([]model.PathRewrite, error) {
	rows, err := s.db.Query(`SELECT id,priority,pattern,replacement FROM path_rewrites ORDER BY priority`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.PathRewrite{}
	for rows.Next() {
		var x model.PathRewrite
		if err := rows.Scan(&x.ID, &x.Priority, &x.Pattern, &x.Replacement); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ---- 播放记录 ----

func (s *sqliteStore) SavePlayRecord(x model.PlayRecord) error {
	x.UpdatedAt = model.NowMillis()
	_, err := s.db.Exec(`INSERT INTO play_records (id,user_id,file_id,position,duration,updated_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(user_id,file_id) DO UPDATE SET
			id=excluded.id, position=excluded.position, duration=excluded.duration, updated_at=excluded.updated_at`,
		x.ID, x.UserID, x.FileID, x.Position, x.Duration, x.UpdatedAt)
	return err
}

func (s *sqliteStore) GetPlayRecord(userID, fileID string) (model.PlayRecord, error) {
	var x model.PlayRecord
	err := s.db.QueryRow(`SELECT id,user_id,file_id,position,duration,updated_at
		FROM play_records WHERE user_id=? AND file_id=?`, userID, fileID).
		Scan(&x.ID, &x.UserID, &x.FileID, &x.Position, &x.Duration, &x.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.PlayRecord{}, os.ErrNotExist
	}
	if err != nil {
		return model.PlayRecord{}, err
	}
	return x, nil
}

func (s *sqliteStore) ListContinue(userID string) ([]model.PlayRecord, error) {
	rows, err := s.db.Query(`SELECT id,user_id,file_id,position,duration,updated_at
		FROM play_records WHERE user_id=? AND duration>0 AND position>0 AND position<duration-30
		ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.PlayRecord{}
	for rows.Next() {
		var x model.PlayRecord
		if err := rows.Scan(&x.ID, &x.UserID, &x.FileID, &x.Position, &x.Duration, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ---- 收藏 ----

func (s *sqliteStore) SaveFavorite(x model.Favorite) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO favorites (id,user_id,item_id,kind) VALUES(?,?,?,?)`,
		x.ID, x.UserID, x.ItemID, string(x.Kind))
	return err
}

func (s *sqliteStore) ListFavorites(userID string) ([]model.Favorite, error) {
	rows, err := s.db.Query(`SELECT id,user_id,item_id,kind FROM favorites WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Favorite{}
	for rows.Next() {
		var x model.Favorite
		var kind string
		if err := rows.Scan(&x.ID, &x.UserID, &x.ItemID, &kind); err != nil {
			return nil, err
		}
		x.Kind = model.FavoriteKind(kind)
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *sqliteStore) DeleteFavorite(userID, itemID string, kind model.FavoriteKind) error {
	args := []interface{}{userID, itemID}
	q := `DELETE FROM favorites WHERE user_id=? AND item_id=?`
	if kind != "" {
		q += ` AND kind=?`
		args = append(args, string(kind))
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return os.ErrNotExist
	}
	return nil
}

// ---- 扫描任务 ----

func (s *sqliteStore) SaveScanJob(x model.ScanJob) error {
	return s.insertScanJobTx(s.db, x)
}

func (s *sqliteStore) insertScanJobTx(exec interface {
	Exec(string, ...interface{}) (sql.Result, error)
}, x model.ScanJob) error {
	_, err := exec.Exec(`INSERT INTO scan_jobs
		(id,library_id,status,total,done,cursor,started_at,finished_at,error,warnings,skipped,skip_hint,dirs)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			library_id=excluded.library_id, status=excluded.status, total=excluded.total,
			done=excluded.done, cursor=excluded.cursor, started_at=excluded.started_at,
			finished_at=excluded.finished_at, error=excluded.error, warnings=excluded.warnings,
			skipped=excluded.skipped, skip_hint=excluded.skip_hint, dirs=excluded.dirs`,
		x.ID, x.LibraryID, x.Status, x.Total, x.Done, x.Cursor, x.StartedAt, x.FinishedAt,
		x.Error, jsonStr(x.Warnings), x.Skipped, x.SkipHint, x.Dirs)
	return err
}

func (s *sqliteStore) GetScanJob(id string) (model.ScanJob, error) {
	var x model.ScanJob
	var warnings string
	err := s.db.QueryRow(`SELECT id,library_id,status,total,done,cursor,started_at,finished_at,
		error,warnings,skipped,skip_hint,dirs FROM scan_jobs WHERE id=?`, id).
		Scan(&x.ID, &x.LibraryID, &x.Status, &x.Total, &x.Done, &x.Cursor, &x.StartedAt,
			&x.FinishedAt, &x.Error, &warnings, &x.Skipped, &x.SkipHint, &x.Dirs)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ScanJob{}, os.ErrNotExist
	}
	if err != nil {
		return model.ScanJob{}, err
	}
	if warnings != "" {
		_ = json.Unmarshal([]byte(warnings), &x.Warnings)
	}
	return x, nil
}

func (s *sqliteStore) GetLatestScanJob(libID string) (model.ScanJob, error) {
	var x model.ScanJob
	var warnings string
	err := s.db.QueryRow(`SELECT id,library_id,status,total,done,cursor,started_at,finished_at,
		error,warnings,skipped,skip_hint,dirs FROM scan_jobs WHERE library_id=?
		ORDER BY started_at DESC LIMIT 1`, libID).
		Scan(&x.ID, &x.LibraryID, &x.Status, &x.Total, &x.Done, &x.Cursor, &x.StartedAt,
			&x.FinishedAt, &x.Error, &warnings, &x.Skipped, &x.SkipHint, &x.Dirs)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ScanJob{}, os.ErrNotExist
	}
	if err != nil {
		return model.ScanJob{}, err
	}
	if warnings != "" {
		_ = json.Unmarshal([]byte(warnings), &x.Warnings)
	}
	return x, nil
}

func (s *sqliteStore) UpdateScanJob(x model.ScanJob) error { return s.SaveScanJob(x) }

// ---- 用户 ----

func (s *sqliteStore) SaveUser(x model.User) error {
	_, err := s.db.Exec(`INSERT INTO users (id,username,password,is_admin,token,child_mode,allowed_libs)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			username=excluded.username, password=excluded.password,
			is_admin=excluded.is_admin, token=excluded.token,
			child_mode=excluded.child_mode, allowed_libs=excluded.allowed_libs`,
		x.ID, x.Username, x.Password, boolInt(x.IsAdmin), x.Token,
		boolInt(x.ChildMode), jsonStr(x.AllowedLibs))
	return err
}

func (s *sqliteStore) GetUserByName(name string) (model.User, error) {
	var x model.User
	var isAdmin, childMode int
	var allowed string
	err := s.db.QueryRow(`SELECT id,username,password,is_admin,token,child_mode,allowed_libs FROM users WHERE username=?`, name).
		Scan(&x.ID, &x.Username, &x.Password, &isAdmin, &x.Token, &childMode, &allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return model.User{}, os.ErrNotExist
	}
	if err != nil {
		return model.User{}, err
	}
	x.IsAdmin = isAdmin == 1
	x.ChildMode = childMode == 1
	if allowed != "" {
		_ = json.Unmarshal([]byte(allowed), &x.AllowedLibs)
	}
	return x, nil
}

func (s *sqliteStore) UpsertToken(userID, token string) error {
	_, err := s.db.Exec(`UPDATE users SET token=? WHERE id=?`, token, userID)
	return err
}

func (s *sqliteStore) GetUserByToken(token string) (model.User, error) {
	var x model.User
	var isAdmin, childMode int
	var allowed string
	err := s.db.QueryRow(`SELECT id,username,password,is_admin,token,child_mode,allowed_libs FROM users WHERE token=? AND token<>''`, token).
		Scan(&x.ID, &x.Username, &x.Password, &isAdmin, &x.Token, &childMode, &allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return model.User{}, os.ErrNotExist
	}
	if err != nil {
		return model.User{}, err
	}
	x.IsAdmin = isAdmin == 1
	x.ChildMode = childMode == 1
	if allowed != "" {
		_ = json.Unmarshal([]byte(allowed), &x.AllowedLibs)
	}
	return x, nil
}

func (s *sqliteStore) ListUsers() ([]model.User, error) {
	rows, err := s.db.Query(`SELECT id,username,password,is_admin,token,child_mode,allowed_libs FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.User{}
	for rows.Next() {
		var x model.User
		var isAdmin, childMode int
		var allowed string
		if err := rows.Scan(&x.ID, &x.Username, &x.Password, &isAdmin, &x.Token, &childMode, &allowed); err != nil {
			return nil, err
		}
		x.IsAdmin = isAdmin == 1
		x.ChildMode = childMode == 1
		if allowed != "" {
			_ = json.Unmarshal([]byte(allowed), &x.AllowedLibs)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *sqliteStore) DeleteUser(id string) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return os.ErrNotExist
	}
	return nil
}

// ---- 全局设置 ----

func (s *sqliteStore) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", os.ErrNotExist
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

func (s *sqliteStore) SaveSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *sqliteStore) ListSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key,value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ensure *sqliteStore 实现 Store 接口（编译期检查）。
var _ Store = (*sqliteStore)(nil)

// 兼容引用 strconv（避免误删导入后编译失败的保护，实际 SearchMediaItems 用 limit int）。
var _ = strconv.Itoa

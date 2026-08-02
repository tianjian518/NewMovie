// Package model 定义 Vidrive 领域模型，对应 PLAN.md 第十节的数据模型。
// 全部用 plain struct + JSON tag，方便 SQLite / JSON 文件持久化互换。
package model

import "time"

// ---- 存储源 ----

type StorageType string

const (
	StorageOpenList StorageType = "openlist"
	StorageWebDAV   StorageType = "webdav"
	StorageLocal    StorageType = "local"
)

// Storage 一个存储源（OpenList 实例 / WebDAV / 本地）。
// Token、SignKey 在持久化时需加密，这里先明文（Store 层负责加密落盘）。
type Storage struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Type      StorageType `json:"type"`
	BaseURL   string      `json:"base_url"`   // 例: http://openlist:5244
	Token     string      `json:"token"`      // OpenList API token
	SignKey   string      `json:"sign_key"`   // OpenList 后台 "签名所有" 密钥
	RateLimit float64     `json:"rate_limit"` // req/s，默认 2
	// LocalRoot 该存储的本地挂载根目录（可选）。例：用 CloudDrive2 / rclone 把同一网盘
	// 挂到本地 /mnt/cloud，而 strm 文件里写的是 /mnt/cloud/媒体/A.mkv。填了 LocalRoot 后，
	// resolver 会自动把本地挂载前缀剥离，把 strm 路径映射成该存储的内部相对路径
	// （/媒体/A.mkv）去取链，本地路径型 .strm 即可直接页内播放——无需改 strm、无需
	// 路径重写、无需全局白名单。这是从源头解决"本地路径型 strm"的核心配置点。
	LocalRoot string `json:"local_root,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// ---- 媒体库 ----

type LibraryMode string

const (
	ModeNative LibraryMode = "native" // 直接对接 OpenList API
	ModeStrm   LibraryMode = "strm"   // 指向已有 strm 目录
	ModeMixed  LibraryMode = "mixed"  // 混合
)

type Library struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Mode      LibraryMode `json:"mode"`
	StorageID string      `json:"storage_id"` // 关联存储源
	RootPath  string      `json:"root_path"`  // OpenList 内部路径 或 strm 目录
	ScanRate  float64     `json:"scan_rate"`  // 覆盖存储源限速
	CreatedAt int64       `json:"created_at"`
}

// ---- 媒体条目 ----

type MediaKind string

const (
	KindMovie  MediaKind = "movie"
	KindSeries MediaKind = "series"
)

type MediaItem struct {
	ID          string    `json:"id"`
	LibraryID   string    `json:"library_id"`
	Kind        MediaKind `json:"kind"`
	TMDBID      int64     `json:"tmdb_id"`
	Title       string    `json:"title"`
	Year        int       `json:"year"`
	Overview    string    `json:"overview"`
	PosterURL   string    `json:"poster_url"`
	BackdropURL string    `json:"backdrop_url"`
	Rating      float64   `json:"rating"`
	// 同目录本地图（OpenList 内部路径），经 /api/items/:id/poster|backdrop 代理，
	// 优先级高于 NFO 远程图与 TMDB 自动图。为空表示无本地图。
	PosterPath        string `json:"poster_path,omitempty"`
	PosterStorageID   string `json:"poster_storage_id,omitempty"`
	BackdropPath      string `json:"backdrop_path,omitempty"`
	BackdropStorageID string `json:"backdrop_storage_id,omitempty"`
	CreatedAt         int64  `json:"created_at"`
}

type Season struct {
	ID        string `json:"id"`
	ItemID    string `json:"item_id"`
	SeasonNo  int    `json:"season_no"`
	PosterURL string `json:"poster_url"`
}

type Episode struct {
	ID        string `json:"id"`
	SeasonID  string `json:"season_id"`
	EpisodeNo int    `json:"episode_no"`
	Title     string `json:"title"`
}

// ---- 物理文件 ----

type SourceType string

const (
	SrcNative SourceType = "native"
	SrcStrm   SourceType = "strm"
)

type MediaFile struct {
	ID            string     `json:"id"`
	ItemID        string     `json:"item_id"`
	EpisodeID     string     `json:"episode_id,omitempty"`
	StorageID     string     `json:"storage_id"`
	Source        SourceType `json:"source"`             // native | strm
	Path          string     `json:"path"`               // OpenList 内部路径 或 本地绝对路径
	StrmRaw       string     `json:"strm_raw,omitempty"` // 原始 strm 文本行（仅 strm 源）
	Size          int64      `json:"size"`
	Modified      int64      `json:"modified"`
	Container     string     `json:"container"`   // mp4/mkv/webm
	VideoCodec    string     `json:"video_codec"` // h264/h265/av1...
	AudioCodec    string     `json:"audio_codec"` // aac/mp3/dts/truehd...
	DurationSec   int        `json:"duration_sec"`
	SeasonNo      int        `json:"season_no"`  // 剧集：季
	EpisodeNo     int        `json:"episode_no"` // 剧集：集
	SupportsRange bool       `json:"supports_range"`
	ProbeState    string     `json:"probe_state"` // pending/done/skipped
	// 外挂字幕（同目录 sidecar）与音轨列表，播放器用于切换。
	Subtitles   []Subtitle   `json:"subtitles,omitempty"`
	AudioTracks []AudioTrack `json:"audio_tracks,omitempty"`
	CreatedAt   int64        `json:"created_at"`
}

// Subtitle 一条外挂字幕（与媒体文件同目录的 sidecar）。
type Subtitle struct {
	ID        string `json:"id"`
	StorageID string `json:"storage_id"` // 与媒体同存储源
	Path      string `json:"path"`       // OpenList 内部路径
	Lang      string `json:"lang"`       // 语言代码：zh/en/und...
	Title     string `json:"title"`      // 显示名（如「简体中文」「English」）
	Ext       string `json:"ext"`        // srt/vtt/ass/ssa
	Source    string `json:"source"`     // sidecar
}

// AudioTrack 一条音轨（MKV/MP4 多音轨场景）。
type AudioTrack struct {
	Index int    `json:"index"` // 流序号（ffprobe 的 stream index）
	Lang  string `json:"lang"`  // 语言代码
	Codec string `json:"codec"` // aac/dts/truehd/opus...
	Title string `json:"title"` // 显示名
}

// ---- strm 路径重写规则 ----

type PathRewrite struct {
	ID          string `json:"id"`
	Priority    int    `json:"priority"`    // 越小越先匹配
	Pattern     string `json:"pattern"`     // 正则
	Replacement string `json:"replacement"` // 例: openlist://main/$1
}

// ---- 直链缓存 ----

type LinkCache struct {
	ID        string            `json:"id"`
	FileID    string            `json:"file_id"`
	RawURL    string            `json:"raw_url"`    // 网盘 CDN 真实直链
	DirectURL string            `json:"direct_url"` // /d/ 302 链接
	Headers   map[string]string `json:"headers"`
	ExpiresAt int64             `json:"expires_at"`
}

// ---- 用户 / 进度 / 收藏 ----

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Password string `json:"password"` // bcrypt/sha256 哈希
	IsAdmin  bool   `json:"is_admin"`
	Token    string `json:"-"`
}

type PlayRecord struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	FileID    string `json:"file_id"`
	Position  int    `json:"position"` // 秒
	Duration  int    `json:"duration"`
	UpdatedAt int64  `json:"updated_at"`
}

type FavoriteKind string

const (
	FavFavorite FavoriteKind = "favorite"
	FavWishlist FavoriteKind = "wishlist"
	FavWatched  FavoriteKind = "watched"
)

type Favorite struct {
	ID     string       `json:"id"`
	UserID string       `json:"user_id"`
	ItemID string       `json:"item_id"`
	Kind   FavoriteKind `json:"kind"`
}

// ---- 扫描任务 ----

type ScanJob struct {
	ID         string `json:"id"`
	LibraryID  string `json:"library_id"`
	Status     string `json:"status"` // running/done/failed
	Total      int    `json:"total"`
	Done       int    `json:"done"`
	Cursor     string `json:"cursor"` // 断点续扫位置
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`

	// Error 记录致命失败原因（根目录不存在、鉴权失败等）。
	// 没有它的时候，扫描失败在前端只表现为「扫不出内容」，用户完全无从排查。
	Error string `json:"error,omitempty"`
	// Warnings 记录非致命问题（某子目录无权限被跳过、目录成环等），最多 MaxScanWarnings 条。
	Warnings []string `json:"warnings,omitempty"`
	// Skipped 因库模式不匹配而跳过的文件数（如 native 库里全是 .strm）。
	Skipped int `json:"skipped"`
	// SkipHint 对 Skipped 的人话解释，直接展示给用户。
	SkipHint string `json:"skip_hint,omitempty"`
	// Dirs 已遍历目录数，用于区分「目录是空的」与「压根没走到」。
	Dirs int `json:"dirs"`
}

// NowMillis 便捷时间戳。
func NowMillis() int64 { return time.Now().UnixMilli() }

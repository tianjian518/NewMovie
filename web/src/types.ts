// 与后端 model 对齐的前端类型。

export interface Storage {
  id: string;
  name: string;
  type: string; // openlist | webdav | local
  base_url: string;
  token: string;
  sign_key: string;
  rate_limit: number;
  local_root?: string; // 本地挂载根（可选）。填了之后本地路径型 .strm 自动映射页内播放
}

export interface Library {
  id: string;
  name: string;
  mode: string; // native | strm | mixed
  storage_id: string;
  root_path: string;
  scan_rate: number;
  icon?: string;      // 自定义图标（emoji）
  color?: string;     // 自定义主题色（hex）
  sort_order?: number; // 排序顺序
}

export interface MediaItem {
  id: string;
  library_id: string;
  kind: string; // movie | series
  tmdb_id: number;
  title: string;
  year: number;
  overview: string;
  poster_url: string;
  backdrop_url: string;
  rating: number;
}

export interface MediaFile {
  id: string;
  item_id: string;
  episode_id: string;
  storage_id: string;
  source: string;
  path: string;
  strm_raw: string;
  size: number;
  container: string;
  video_codec: string;
  audio_codec: string;
  duration_sec: number;
  season_no: number;
  episode_no: number;
  supports_range: boolean;
  probe_state: string;
}

export interface PlayDecision {
  level: number;
  label: string;
  reason: string;
  url: string;
  raw_url: string;
  direct_url: string;
  use_raw_url: boolean;
  supports_range: boolean;
  headers: Record<string, string>;
  needs_transcode: boolean;
  // ffmpeg_ok：服务端是否安装了 ffmpeg。重封装/转码依赖它；缺失时 MKV/HEVC
  // 只能外部播放，后端会在 warn 里给出明确的换镜像提示。
  ffmpeg_ok: boolean;
  // warn：非致命但需提示用户的信息（如服务端未安装 ffmpeg）。
  warn: string;
  // 续播：上次看到的位置（秒），0 表示从头开始
  resume_position: number;
  resume_duration: number;
  item_id: string;
  title: string;
  subtitle: string;
  // 字幕与音轨清单，供播放器切换
  subtitles: Subtitle[];
  audio_tracks: AudioTrack[];
}

// 字幕（外挂，经后端转成 WebVTT）。
export interface Subtitle {
  lang: string;
  title: string;
  url: string;
}

// 音轨（MKV/MP4 多音轨）。
export interface AudioTrack {
  index: number;
  lang: string;
  codec: string;
  title: string;
}

// 详情接口返回：条目 + 文件 + 每个文件的观看进度 + 是否已收藏
export interface ItemDetail {
  item: MediaItem;
  files: MediaFile[];
  progress: Record<string, { position: number; duration: number }>;
  favored: boolean;
}

// 继续观看列表项（后端已聚合出条目信息）
export interface ContinueRow {
  id: string;
  file_id: string;
  position: number;
  duration: number;
  updated_at: number;
  season_no: number;
  episode_no: number;
  file_name: string;
  item?: MediaItem;
}

// 收藏列表项（后端已聚合出条目信息）
export interface FavoriteRow {
  id: string;
  item_id: string;
  kind: string;
  item: MediaItem;
}

export interface ScanJob {
  id: string;
  library_id: string;
  status: string;
  total: number;
  done: number;
  cursor?: string;
  error?: string;        // 致命失败原因（人话）
  warnings?: string[];   // 非致命问题，如某子目录被跳过
  skipped?: number;      // 因库模式不匹配跳过的文件数
  skip_hint?: string;    // 对 skipped 的解释与建议
  dirs?: number;         // 已遍历目录数
}

// BrowseResp 目录浏览接口返回，供建库时的目录树选择器使用。
export interface BrowseDir {
  name: string;
  path: string;
  modified: number;
}
export interface BrowseResp {
  path: string;
  parent: string;
  dirs: BrowseDir[];
  video_count: number;
  strm_count: number;
  suggest_mode: string;
}

export interface PathRewrite {
  id: string;
  priority: number;
  pattern: string;
  replacement: string;
}

// Health 是 /api/health 的响应。
export interface Health {
  ok: boolean;
  name: string;
  version: string;
  // ffmpeg 能力：重封装(L2)需要 ffmpeg，转码(L3)还需要 libx264。
  ffmpeg_ok: boolean;
  transcode_ok: boolean;
  transcode: boolean;
  // 2.0 内置网盘（139cas）：
  //   bundled        —— 是否运行在内置模式（容器镜像默认开）
  //   bundled_ready  —— 是否已自动接管成功（登录换 Token + 注册存储）
  //   bundled_prefix —— 网盘管理界面的挂载前缀，空串表示未开启反代
  bundled: boolean;
  bundled_ready: boolean;
  bundled_prefix: string;
}

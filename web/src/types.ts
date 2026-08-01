// 与后端 model 对齐的前端类型。

export interface Storage {
  id: string;
  name: string;
  type: string; // openlist | webdav | local
  base_url: string;
  token: string;
  sign_key: string;
  rate_limit: number;
}

export interface Library {
  id: string;
  name: string;
  mode: string; // native | strm | mixed
  storage_id: string;
  root_path: string;
  scan_rate: number;
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
  // 续播：上次看到的位置（秒），0 表示从头开始
  resume_position: number;
  resume_duration: number;
  item_id: string;
  title: string;
  subtitle: string;
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
}

export interface PathRewrite {
  id: string;
  priority: number;
  pattern: string;
  replacement: string;
}

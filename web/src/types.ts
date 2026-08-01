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

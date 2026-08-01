// 后端 API 客户端。token 存 localStorage，所有请求自动带 Authorization。
import type {
  Storage, Library, MediaItem, PlayDecision, ScanJob, PathRewrite,
  ItemDetail, ContinueRow, FavoriteRow, BrowseResp,
} from "./types";

const TOKEN_KEY = "vidrive_token";

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) || "";
}
export function setToken(t: string) {
  localStorage.setItem(TOKEN_KEY, t);
}
export function clearToken() {
  localStorage.removeItem(TOKEN_KEY);
}

async function req<T>(path: string, opts: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...((opts.headers as Record<string, string>) || {}),
  };
  const tok = getToken();
  if (tok) headers["Authorization"] = "Bearer " + tok;
  const resp = await fetch(path, { ...opts, headers });
  if (!resp.ok) {
    // 401 表示 token 失效，通知全局监听者（App 的 useEffect）退回登录页。
    // 否则用户会卡在主界面却所有请求报错，体验如同白板。
    if (resp.status === 401) {
      window.dispatchEvent(new Event("newmovie:unauthorized"));
    }
    const text = await resp.text();
    throw new Error(text || `HTTP ${resp.status}`);
  }
  return resp.json() as Promise<T>;
}

export const api = {
  health: () => req<any>("/api/health"),
  login: (username: string, password: string) =>
    req<{ token: string; is_admin: boolean }>("/api/login", {
      method: "POST",
      body: JSON.stringify({ username, password }),
    }),

  listStorages: () => req<Storage[]>("/api/storages"),
  createStorage: (s: Partial<Storage>) =>
    req<Storage>("/api/storages", { method: "POST", body: JSON.stringify(s) }),
  updateStorage: (id: string, s: Partial<Storage>) =>
    req<Storage>("/api/storages/" + id, { method: "PUT", body: JSON.stringify(s) }),
  deleteStorage: (id: string) =>
    req<{ ok: boolean }>("/api/storages/" + id, { method: "DELETE" }),
  testStorage: (s: { base_url: string; token: string; sign_key: string }) =>
    req<{ ok: boolean; drives: any[] }>("/api/storages/test", {
      method: "POST",
      body: JSON.stringify(s),
    }),
  listDrives: (id: string) => req<any[]>("/api/storages/" + id + "/drives"),
  // 浏览网盘目录（建库时的目录树选择器用），path 缺省为根。
  browse: (id: string, path = "/", refresh = false) =>
    req<BrowseResp>(
      "/api/storages/" + id + "/browse?path=" + encodeURIComponent(path) + (refresh ? "&refresh=1" : "")
    ),

  listLibraries: () => req<Library[]>("/api/libraries"),
  createLibrary: (l: Partial<Library>) =>
    req<Library>("/api/libraries", { method: "POST", body: JSON.stringify(l) }),
  deleteLibrary: (id: string) =>
    req<{ ok: boolean }>("/api/libraries/" + id, { method: "DELETE" }),
  libraryItems: (id: string) => req<MediaItem[]>("/api/libraries/" + id + "/items"),
  scan: (id: string) =>
    req<{ job_id: string; status: string }>("/api/libraries/" + id + "/scan", {
      method: "POST",
    }),

  scanJob: (jobId: string) => req<ScanJob>("/api/scan/" + jobId),
  latestScanJob: (libId: string) => req<ScanJob>("/api/libraries/" + libId + "/scan"),
  item: (id: string) => req<ItemDetail>("/api/items/" + id),
  rescrape: (id: string) =>
    req<{ ok: boolean }>("/api/items/" + id + "/rescrape", { method: "POST" }),
  // 手动把条目绑定到指定 TMDB ID（自动匹配认错时的补救手段）
  matchItem: (id: string, tmdb_id: number, kind: string) =>
    req<{ ok: boolean; item: MediaItem }>("/api/items/" + id + "/match", {
      method: "POST",
      body: JSON.stringify({ tmdb_id, kind }),
    }),
  play: (fileId: string) =>
    req<PlayDecision>("/api/items/" + fileId + "/play"),

  // 全局搜索 / 筛选 / 排序
  search: (p: { q?: string; kind?: string; library?: string; sort?: string; limit?: number }) => {
    const qs = new URLSearchParams();
    Object.entries(p).forEach(([k, v]) => { if (v !== undefined && v !== null && v !== "") qs.set(k, String(v)); });
    return req<MediaItem[]>("/api/search?" + qs.toString());
  },

  saveRecord: (fileId: string, position: number, duration: number) =>
    req<any>("/api/play/record", {
      method: "POST",
      body: JSON.stringify({ file_id: fileId, position, duration }),
    }),

  listContinue: () => req<ContinueRow[]>("/api/continue"),
  listFavorites: () => req<FavoriteRow[]>("/api/favorites"),
  addFavorite: (itemId: string, kind: string) =>
    req<any>("/api/favorites", {
      method: "POST",
      body: JSON.stringify({ item_id: itemId, kind }),
    }),
  removeFavorite: (itemId: string) =>
    req<{ ok: boolean }>("/api/favorites/" + itemId, { method: "DELETE" }),

  listRewrites: () => req<PathRewrite[]>("/api/rewrites"),
  createRewrite: (r: { pattern: string; replacement: string; priority: number }) =>
    req<PathRewrite>("/api/rewrites", { method: "POST", body: JSON.stringify(r) }),

  getSettings: () => req<Record<string, string>>("/api/settings"),
  saveSettings: (s: Record<string, string>) =>
    req<{ ok: boolean }>("/api/settings", { method: "PUT", body: JSON.stringify(s) }),
  testTmdb: (api_key: string, api_base: string) =>
    req<{ ok: boolean; endpoint: string; sample: string; poster: string }>("/api/settings/tmdb/test", {
      method: "POST",
      body: JSON.stringify({ api_key, api_base }),
    }),
};

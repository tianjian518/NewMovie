import { useCallback, useEffect, useState } from "react";
import { useParams, Link } from "react-router-dom";
import { api, isAdmin } from "../api";
import type { MediaItem, MediaFile } from "../types";
import PosterCard from "../components/PosterCard";

// 按季、集排序；无集号的（电影/未识别）沉底并按文件名排，
// 否则网盘返回什么顺序就显示什么顺序，13 集会乱序排列。
function sortEpisodes(files: MediaFile[]): MediaFile[] {
  return [...files].sort((a, b) => {
    if (a.episode_no > 0 && b.episode_no > 0) {
      return (a.season_no - b.season_no) || (a.episode_no - b.episode_no);
    }
    if (a.episode_no > 0) return -1;
    if (b.episode_no > 0) return 1;
    return a.path.localeCompare(b.path, "zh-CN");
  });
}

function fmtTime(sec: number): string {
  if (!sec || sec < 0) return "";
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = Math.floor(sec % 60);
  const pad = (n: number) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

// 手动匹配弹窗：自动刮削认错时（同名翻拍、译名不一致），
// 让用户直接填 TMDB ID 强制绑定。以前只能去网盘里手写 .vidrive.json。
function MatchDialog({ item, onClose, onDone }: {
  item: MediaItem; onClose: () => void; onDone: (m: MediaItem) => void;
}) {
  const [tmdbId, setTmdbId] = useState("");
  const [kind, setKind] = useState(item.kind || "movie");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  async function submit() {
    const id = Number(tmdbId.trim());
    if (!Number.isFinite(id) || id <= 0) { setErr("请填写有效的 TMDB ID（纯数字）"); return; }
    setBusy(true); setErr("");
    try {
      const r = await api.matchItem(item.id, id, kind);
      onDone(r.item);
      onClose();
    } catch (e: any) {
      setErr(e?.message || "匹配失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="fixed inset-0 bg-black/60 flex items-center justify-center z-50" onClick={onClose}>
      <div className="bg-panel rounded-xl p-6 w-96 space-y-3" onClick={(e) => e.stopPropagation()}>
        <h3 className="text-lg font-bold">手动匹配 TMDB</h3>
        <p className="text-xs text-gray-400 leading-relaxed">
          在 themoviedb.org 找到正确条目，地址栏 <code>/movie/27205</code> 里的数字就是 ID。
        </p>
        <div className="flex gap-2">
          <select value={kind} onChange={(e) => setKind(e.target.value)} className="bg-ink rounded p-2 text-sm">
            <option value="movie">电影</option>
            <option value="series">剧集</option>
          </select>
          <input
            autoFocus
            className="flex-1 bg-ink rounded p-2 text-sm"
            placeholder="TMDB ID，如 27205"
            value={tmdbId}
            onChange={(e) => setTmdbId(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && submit()}
          />
        </div>
        {err && <p className="text-red-400 text-sm">{err}</p>}
        <div className="flex justify-end gap-2 pt-1">
          <button onClick={onClose} className="px-3 py-1 text-sm text-gray-400 hover:text-gray-200">取消</button>
          <button disabled={busy} onClick={submit} className="bg-brand rounded px-3 py-1 text-sm disabled:opacity-60">
            {busy ? "匹配中…" : "确定"}
          </button>
        </div>
      </div>
    </div>
  );
}

// 详情页：海报/简介 + 文件（剧集）列表，点击进入播放。
export default function Detail() {
  const { id } = useParams();
  const [item, setItem] = useState<MediaItem | null>(null);
  const [files, setFiles] = useState<MediaFile[]>([]);
  const [progress, setProgress] = useState<Record<string, { position: number; duration: number }>>({});
  const [favored, setFavored] = useState(false);
  // 加载失败必须能被看见：老实现 .catch(()=>{}) 把错误吞了，
  // 页面永远停在「加载中…」，用户完全不知道发生了什么。
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(true);
  const [showMatch, setShowMatch] = useState(false);
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    if (!id) return;
    setLoading(true);
    setErr("");
    api.item(id)
      .then((r) => {
        setItem(r.item);
        setFiles(r.files || []);
        setProgress(r.progress || {});
        setFavored(!!r.favored);
      })
      .catch((e: any) => setErr(e?.message || "加载失败"))
      .finally(() => setLoading(false));
  }, [id]);

  useEffect(() => { load(); }, [load]);

  async function toggleFavorite() {
    if (!item || busy) return;
    setBusy(true);
    try {
      if (favored) { await api.removeFavorite(item.id); setFavored(false); }
      else { await api.addFavorite(item.id, "favorite"); setFavored(true); }
    } catch (e: any) {
      setErr(e?.message || "操作失败");
    } finally {
      setBusy(false);
    }
  }

  async function rescrape() {
    if (!item || busy) return;
    if (!confirm("重新从 TMDB/NFO 刮削该条目？")) return;
    setBusy(true);
    try {
      await api.rescrape(item.id);
      load();
    } catch (e: any) {
      setErr(e?.message || "刮削失败");
    } finally {
      setBusy(false);
    }
  }

  if (loading && !item) return <div className="text-gray-400">加载中…</div>;
  if (err && !item) {
    return (
      <div className="space-y-3">
        <p className="text-red-400">加载失败：{err}</p>
        <button onClick={load} className="bg-brand rounded px-3 py-1 text-sm">重试</button>
        <Link to="/" className="ml-2 text-sm text-blue-400">返回媒体库</Link>
      </div>
    );
  }
  if (!item) return null;

  const sorted = sortEpisodes(files);

  return (
    <div>
      {/* Hero：背景图 + 标题浮层（Emby 风格） */}
      <div
        className="relative h-64 rounded-xl mb-4 overflow-hidden bg-cover bg-center"
        style={{ backgroundImage: item.backdrop_url ? `url("${item.backdrop_url}")` : undefined, background: "#171b22" }}
      >
        <div className="absolute inset-0 bg-gradient-to-t from-black/90 via-black/30 to-transparent" />
        <div className="absolute bottom-0 left-0 p-6">
          <h1 className="text-3xl font-bold drop-shadow">
            {item.title}
            {item.year > 0 && <span className="text-gray-300 text-xl"> ({item.year})</span>}
          </h1>
          <div className="flex items-center gap-3 text-sm mt-2">
            {item.rating > 0 && <span className="text-yellow-400">★ {item.rating.toFixed(1)}</span>}
            <span className="text-gray-300">{item.kind === "series" ? "剧集" : "电影"}</span>
            {item.tmdb_id > 0 && <span className="text-gray-400">TMDB #{item.tmdb_id}</span>}
          </div>
          <div className="flex flex-wrap gap-2 mt-3">
            {sorted.length > 0 && (
              <Link to={"/play/" + sorted[0].id} className="bg-brand rounded px-4 py-1.5 text-sm font-semibold">
                {progress[sorted[0].id] ? "继续播放" : "▶ 播放"}
              </Link>
            )}
            <button
              disabled={busy}
              onClick={toggleFavorite}
              className={"rounded px-3 py-1.5 text-sm border disabled:opacity-60 " +
                (favored ? "bg-brand/20 border-brand text-brand" : "bg-black/40 border-white/20")}
            >
              {favored ? "已收藏 ✓" : "收藏"}
            </button>
          </div>
        </div>
      </div>
      <div className="flex gap-6">
        <div className="w-40 shrink-0 -mt-20 relative z-10"><PosterCard item={item} /></div>
        <div className="flex-1 pt-2">
          <p className="text-gray-300 mt-3 text-sm leading-relaxed">{item.overview || "暂无简介"}</p>
          {err && <p className="text-red-400 text-sm mt-2">{err}</p>}
          <div className="flex flex-wrap gap-2 mt-3">
            {isAdmin() && (
              <>
                <button disabled={busy} onClick={rescrape}
                  className="bg-card border border-white/10 rounded px-3 py-1 text-sm disabled:opacity-60">
                  重新刮削
                </button>
                <button disabled={busy} onClick={() => setShowMatch(true)}
                  className="bg-card border border-white/10 rounded px-3 py-1 text-sm disabled:opacity-60">
                  匹配错了？手动指定
                </button>
              </>
            )}
          </div>
        </div>
      </div>

      <h2 className="text-lg font-bold mt-6 mb-3">文件 / 剧集（{files.length}）</h2>
      {files.length === 0 && <p className="text-gray-500 text-sm">该条目下暂无文件。</p>}
      <div className="space-y-2">
        {sorted.map((f) => {
          const p = progress[f.id];
          const pct = p && p.duration > 0 ? Math.min(100, Math.round((p.position / p.duration) * 100)) : 0;
          return (
            <Link
              key={f.id}
              to={"/play/" + f.id}
              className="block bg-card rounded px-4 py-2 hover:ring-2 hover:ring-brand"
            >
              <div className="flex items-center justify-between gap-3">
                <span className="truncate">
                  {f.episode_no > 0 && (
                    <span className="text-brand mr-2">
                      {f.season_no > 1 ? `S${f.season_no}E${f.episode_no}` : `第 ${f.episode_no} 集`}
                    </span>
                  )}
                  {f.path.split("/").pop()}
                </span>
                <span className="text-xs text-gray-400 shrink-0">
                  {p ? `看到 ${fmtTime(p.position)}` : `${f.container || "?"} · ${f.source}`}
                </span>
              </div>
              {pct > 0 && (
                <div className="h-0.5 bg-white/10 rounded mt-1.5">
                  <div className="h-full bg-brand rounded" style={{ width: pct + "%" }} />
                </div>
              )}
            </Link>
          );
        })}
      </div>

      {showMatch && (
        <MatchDialog
          item={item}
          onClose={() => setShowMatch(false)}
          onDone={(m) => { setItem(m); load(); }}
        />
      )}
    </div>
  );
}

import { useCallback, useEffect, useRef, useState } from "react";
import { Routes, Route, Link, NavLink, useNavigate, useParams } from "react-router-dom";
import { api, getToken, setToken, clearToken, setAdmin, isAdmin } from "./api";
import type { MediaItem, ScanJob, ContinueRow, FavoriteRow, Library as LibraryType } from "./types";
import Library from "./pages/Library";
import Detail from "./pages/Detail";
import Player from "./pages/Player";
import Settings from "./pages/Settings";
import PosterCard from "./components/PosterCard";
import PosterRow from "./components/PosterRow";
import { stripJson } from "./components/DirPicker";

// Login 通过 onLogin 回调把 token 交回给 App，触发重新渲染。
// 只写 localStorage 是不够的 —— React 不会因为 localStorage 变化而重渲染，
// 那样点了登录会「毫无反应」（其实已登录成功，必须手动刷新才进得去）。
function Login({ onLogin }: { onLogin: (token: string) => void }) {
  const [u, setU] = useState("admin");
  const [p, setP] = useState("admin");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (busy) return;
    setErr("");
    setBusy(true);
    try {
      const r = await api.login(u, p);
      if (!r?.token) throw new Error("服务端未返回 token");
      setAdmin(r.is_admin); // 保存管理员角色，前端据此隐藏管理员功能 UI
      onLogin(r.token);
    } catch (e: any) {
      setErr(e?.message || "登录失败，请检查用户名和密码");
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="min-h-screen flex items-center justify-center">
      <form onSubmit={submit} className="bg-card p-8 rounded-xl w-80 space-y-4">
        <h1 className="text-2xl font-bold text-center">NewMovie</h1>
        <input className="w-full bg-ink rounded p-2" placeholder="用户名" value={u} onChange={(e) => setU(e.target.value)} />
        <input type="password" className="w-full bg-ink rounded p-2" placeholder="密码" value={p} onChange={(e) => setP(e.target.value)} />
        {err && <p className="text-red-400 text-sm">{err}</p>}
        <button disabled={busy} className="w-full bg-brand rounded p-2 font-semibold disabled:opacity-60">
          {busy ? "登录中…" : "登录"}
        </button>
      </form>
    </div>
  );
}

function fmtTime(sec: number): string {
  if (!sec || sec < 0) return "0:00";
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = Math.floor(sec % 60);
  const pad = (n: number) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

// 空状态/错误态统一样式，省得每个页面各写一套。
function Hint({ err, empty, onRetry }: { err?: string; empty?: string; onRetry?: () => void }) {
  if (err) {
    return (
      <div className="text-sm">
        <span className="text-red-400">加载失败：{err}</span>
        {onRetry && <button onClick={onRetry} className="ml-3 bg-brand rounded px-2 py-0.5">重试</button>}
      </div>
    );
  }
  return <p className="text-gray-500 text-sm">{empty}</p>;
}

function ContinuePage() {
  const [list, setList] = useState<ContinueRow[]>([]);
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(true);
  const load = () => {
    setLoading(true); setErr("");
    api.listContinue().then(setList).catch((e) => setErr(e?.message || "未知错误")).finally(() => setLoading(false));
  };
  useEffect(load, []);

  return (
    <div>
      <h2 className="text-xl font-bold mb-4">继续观看</h2>
      {loading ? <p className="text-gray-400 text-sm">加载中…</p> :
        list.length === 0 ? <Hint err={err} empty="还没有观看记录，去媒体库挑一部吧。" onRetry={load} /> : (
          <div className="grid grid-cols-2 md:grid-cols-4 lg:grid-cols-6 gap-4">
            {list.map((r) => {
              const pct = r.duration > 0 ? Math.min(100, Math.round((r.position / r.duration) * 100)) : 0;
              const label = r.episode_no > 0
                ? (r.season_no > 1 ? `S${r.season_no}E${r.episode_no}` : `第 ${r.episode_no} 集`)
                : "";
              return (
                <Link key={r.id} to={"/play/" + r.file_id} className="block group">
                  {r.item
                    ? <PosterCard item={r.item} />
                    : <div className="aspect-[2/3] bg-card rounded-lg flex items-center justify-center text-xs text-gray-500 p-2 text-center">{r.file_name}</div>}
                  <div className="h-1 bg-white/10 rounded mt-1.5">
                    <div className="h-full bg-brand rounded" style={{ width: pct + "%" }} />
                  </div>
                  <div className="text-xs text-gray-400 mt-1 truncate">
                    {label && <span className="text-brand mr-1">{label}</span>}
                    {fmtTime(r.position)} / {fmtTime(r.duration)}
                  </div>
                </Link>
              );
            })}
          </div>
        )}
    </div>
  );
}

// 续播卡片（带进度条），用于首页「继续观看」横滑行。
function ContinueCard({ r }: { r: ContinueRow }) {
  const pct = r.duration > 0 ? Math.min(100, Math.round((r.position / r.duration) * 100)) : 0;
  const label = r.episode_no > 0
    ? (r.season_no > 1 ? `S${r.season_no}E${r.episode_no}` : `第 ${r.episode_no} 集`)
    : "";
  return (
    <Link to={"/play/" + r.file_id} className="shrink-0 w-32 sm:w-36 snap-start block group">
      {r.item ? <PosterCard item={r.item} /> :
        <div className="aspect-[2/3] bg-card rounded-lg flex items-center justify-center text-xs text-gray-500 p-2 text-center">{r.file_name}</div>}
      <div className="h-1 bg-white/10 rounded mt-1.5">
        <div className="h-full bg-brand rounded" style={{ width: pct + "%" }} />
      </div>
      <div className="text-xs text-gray-400 mt-1 truncate">
        {label && <span className="text-brand mr-1">{label}</span>}
        {fmtTime(r.position)} / {fmtTime(r.duration)}
      </div>
    </Link>
  );
}

const modeLabel = (m: string) =>
  ({ native: "原生", strm: "STRM", mixed: "混合" } as Record<string, string>)[m] || m;

// 首页：参考网易爆米花 / 飞牛影视的「行式海报墙」布局。
//  - 继续观看、最近添加都是单行横滑，不再占满整屏；
//  - 每个媒体库直接在首页展示自己的「海报行」，点库名进详情，不必点进去才看得到海报墙。
function Dashboard() {
  const [cont, setCont] = useState<ContinueRow[]>([]);
  const [recent, setRecent] = useState<MediaItem[]>([]);
  const [random, setRandom] = useState<MediaItem[]>([]);
  const [libs, setLibs] = useState<LibraryType[]>([]);
  // 每个媒体库的最近添加条目，按库 ID 归集，用于首页直接铺海报行。
  const [libItems, setLibItems] = useState<Record<string, MediaItem[]>>({});
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    Promise.all([
      api.listContinue().then(setCont).catch(() => {}),
      api.search({ sort: "recent", limit: 18 }).then((r) => setRecent(r.items)).catch(() => {}),
      api.search({ sort: "random", limit: 12 }).then((r) => setRandom(r.items)).catch(() => {}),
      api.listLibraries().then(setLibs).catch(() => {}),
    ]).finally(() => setLoading(false));
  }, []);

  // 各媒体库最近添加：并发拉取，按库归集成字典；任一库失败不影响其它库。
  useEffect(() => {
    if (libs.length === 0) return;
    let alive = true;
    Promise.all(
      libs.map((l) =>
        api.search({ library: l.id, sort: "recent", limit: 12 })
          .then((r) => [l.id, r.items] as const)
          .catch(() => [l.id, [] as MediaItem[]] as const),
      ),
    ).then((pairs) => {
      if (!alive) return;
      const map: Record<string, MediaItem[]> = {};
      for (const [id, items] of pairs) map[id] = items;
      setLibItems(map);
    });
    return () => { alive = false; };
  }, [libs]);

  return (
    <div className="space-y-8">
      {cont.length > 0 && (
        <section>
          <h2 className="text-xl font-bold mb-3">继续观看</h2>
          <div className="flex gap-3 overflow-x-auto pb-2 snap-x">
            {cont.map((r) => <ContinueCard key={r.id} r={r} />)}
          </div>
        </section>
      )}

      <section>
        <h2 className="text-xl font-bold mb-3">最近添加</h2>
        {loading ? <p className="text-gray-400 text-sm">加载中…</p> :
          <PosterRow items={recent} empty="还没有内容，去「媒体库」扫描导入吧。" />}
      </section>

      {random.length > 0 && (
        <section>
          <div className="flex items-baseline justify-between mb-3">
            <h2 className="text-xl font-bold">随机推荐</h2>
            <button
              onClick={() => api.search({ sort: "random", limit: 12 }).then((r) => setRandom(r.items)).catch(() => {})}
              className="text-sm text-blue-400 hover:text-blue-300 shrink-0"
            >
              换一批 ↻
            </button>
          </div>
          <PosterRow items={random} empty="" />
        </section>
      )}

      {libs.map((l) => (
        <section key={l.id}>
          <div className="flex items-baseline justify-between mb-3">
            <Link to={"/library/" + l.id} className="text-lg font-bold hover:text-brand">
              {l.name}
              <span className="ml-2 text-xs font-normal text-gray-500 align-middle">{modeLabel(l.mode)}</span>
            </Link>
            <Link to={"/library/" + l.id} className="text-sm text-blue-400 shrink-0 ml-3">查看全部 →</Link>
          </div>
          <PosterRow
            items={libItems[l.id] || []}
            empty="这个媒体库还是空的，去扫描导入吧。"
          />
        </section>
      ))}

      <div className="pt-2">
        <Link to="/libraries" className="text-sm text-blue-400">管理媒体库 / 创建媒体库 →</Link>
      </div>
    </div>
  );
}

function FavoritesPage() {
  const [list, setList] = useState<FavoriteRow[]>([]);
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(true);
  const load = () => {
    setLoading(true); setErr("");
    api.listFavorites().then(setList).catch((e) => setErr(e?.message || "未知错误")).finally(() => setLoading(false));
  };
  useEffect(load, []);

  async function remove(itemId: string) {
    try {
      await api.removeFavorite(itemId);
      setList((l) => l.filter((x) => x.item_id !== itemId));
    } catch (e: any) {
      setErr(e?.message || "取消收藏失败");
    }
  }

  return (
    <div>
      <h2 className="text-xl font-bold mb-4">收藏</h2>
      {loading ? <p className="text-gray-400 text-sm">加载中…</p> :
        list.length === 0 ? <Hint err={err} empty="还没有收藏。在详情页点「收藏」加进来。" onRetry={load} /> : (
          <div className="grid grid-cols-3 md:grid-cols-5 lg:grid-cols-7 gap-4">
            {list.map((f) => (
              <div key={f.id} className="relative group">
                <Link to={"/item/" + f.item_id}><PosterCard item={f.item} /></Link>
                <button
                  title="取消收藏"
                  onClick={() => remove(f.item_id)}
                  className="absolute top-1 right-1 bg-black/70 hover:bg-red-600 rounded w-6 h-6 text-xs opacity-0 group-hover:opacity-100 transition"
                >×</button>
              </div>
            ))}
          </div>
        )}
    </div>
  );
}

const SORTS = [
  { v: "title", label: "标题" },
  { v: "year", label: "年份" },
  { v: "rating", label: "评分" },
  { v: "recent", label: "最近添加" },
];

function LibraryItems() {
  const { id = "" } = useParams();
  const [items, setItems] = useState<MediaItem[]>([]);
  const [job, setJob] = useState<ScanJob | null>(null);
  const [err, setErr] = useState("");
  const [scanning, setScanning] = useState(false);
  // 搜索 / 筛选 / 排序：库一大，纯海报墙就是一面砖墙，翻不动也找不到。
  const [q, setQ] = useState("");
  const [kind, setKind] = useState("");
  const [sortBy, setSortBy] = useState("title");
  // ---- 虚拟滚动：媒体库可能上万条目，一次全量渲染几百张海报会把 DOM 卡爆。 ----
  // 方案：按页（limit=PAGE）拉取 + 累积追加；滚动接近底部时加载下一页。
  // 底部放一个「哨兵」div，用 IntersectionObserver 观测它进入视口即触发 loadMore。
  const [page, setPage] = useState(0);
  const [hasMore, setHasMore] = useState(true);
  const [total, setTotal] = useState(0);
  const [loadingMore, setLoadingMore] = useState(false);
  const sentinelRef = useRef<HTMLDivElement>(null);
  const lockRef = useRef(false); // 防重复触发 loadMore（并发保护）
  const PAGE = 96;
  // 所有轮询定时器统一挂 ref，组件卸载时一定清掉。
  // 老实现在 scan() 里 setInterval 却没人管，用户点完扫描立刻切页，
  // 定时器会一直在后台请求，还对着已卸载组件 setState。
  const tickRef = useRef<any>(null);

  const clearTick = () => { if (tickRef.current) { clearInterval(tickRef.current); tickRef.current = null; } };

  // load 拉取一页；reset=true 表示重查（关键词/排序/库变化），重置分页从第 0 页开始。
  const load = useCallback((reset: boolean) => {
    if (!id) return;
    setErr("");
    const off = reset ? 0 : page * PAGE;
    api.search({ q, kind, library: id, sort: sortBy, limit: PAGE, offset: off })
      .then((r) => {
        setItems((prev) => (reset ? r.items : [...prev, ...r.items]));
        setTotal(r.total);
        // 用后端返回的 total 精确判断是否还有下一页，而不是靠「本页是否满」猜——
        // 最后一页正好满 PAGE 条时，旧逻辑会多打一次空请求。
        setHasMore(off + r.items.length < r.total);
        if (reset) setPage(1); else setPage((p) => p + 1);
      })
      .catch((e) => setErr(e?.message || "加载失败"));
  }, [id, q, kind, sortBy, page]);

  // 首次加载 & 搜索/筛选/排序变化时：重置并拉第 0 页
  useEffect(() => {
    setItems([]);
    setPage(0);
    setHasMore(true);
    const t = setTimeout(() => load(true), q ? 300 : 0);
    return () => clearTimeout(t);
  }, [load, q]); // load 已含 id/q/kind/sortBy，这里额外盯 q 做防抖

  // 底部哨兵：进入视口就加载下一页（提前 600px 预加载，滚动更跟手）
  useEffect(() => {
    const sentinel = sentinelRef.current;
    if (!sentinel || !hasMore) return;
    const io = new IntersectionObserver((entries) => {
      if (!entries[0].isIntersecting || lockRef.current || loadingMore) return;
      lockRef.current = true;
      setLoadingMore(true);
      load(false);
      requestAnimationFrame(() => {
        lockRef.current = false;
        setLoadingMore(false);
      });
    }, { rootMargin: "600px 0px" });
    io.observe(sentinel);
    return () => io.disconnect();
  }, [hasMore, loadingMore, load]);


  const poll = useCallback((jobId: string) => {
    clearTick();
    setScanning(true);
    tickRef.current = setInterval(async () => {
      try {
        const cur = await api.scanJob(jobId);
        setJob(cur);
        if (cur.status !== "running") { clearTick(); setScanning(false); load(true); }
      } catch {
        clearTick(); setScanning(false);
      }
    }, 1500);
  }, [load]);

  // 挂载时若后台已有进行中的扫描（如「创建并扫描导入」自动发起的），自动接管轮询。
  useEffect(() => {
    api.latestScanJob(id).then((j) => {
      setJob(j);
      if (j.status === "running") poll(j.id);
    }).catch(() => {});
    return clearTick; // 卸载时务必清理
  }, [id, poll]);

  async function scan() {
    if (scanning) return; // 双保险：按钮已禁用，这里再挡一次连点
    try {
      const r = await api.scan(id);
      poll(r.job_id);
    } catch (e: any) {
      // 后端对重复扫描回 409，属于预期内提示而非报错；
      // 预检失败回 400 且带人话原因，直接透出而不是丢一坨 JSON 给用户。
      const raw = e?.message || "";
      if (raw.includes("正在扫描")) {
        setErr("该媒体库正在扫描中，请稍候");
      } else {
        setErr(stripJson(raw) || "启动扫描失败");
        // 预检已把失败原因写进 ScanJob，拉一次好让下方诊断面板显示出来。
        api.latestScanJob(id).then(setJob).catch(() => {});
      }
    }
  }

  return (
    <div>
      <div className="flex flex-wrap items-center gap-2 mb-4">
        <h2 className="text-xl font-bold mr-auto">海报墙</h2>
        <input
          className="bg-card rounded px-3 py-1 text-sm w-48 border border-white/10"
          placeholder="搜索片名 / 简介"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <select value={kind} onChange={(e) => setKind(e.target.value)} className="bg-card rounded px-2 py-1 text-sm border border-white/10">
          <option value="">全部</option>
          <option value="movie">电影</option>
          <option value="series">剧集</option>
        </select>
        <select value={sortBy} onChange={(e) => setSortBy(e.target.value)} className="bg-card rounded px-2 py-1 text-sm border border-white/10">
          {SORTS.map((s) => <option key={s.v} value={s.v}>按{s.label}</option>)}
        </select>
        {isAdmin() && (
          <button
            onClick={scan}
            disabled={scanning}
            className="bg-brand rounded px-3 py-1 text-sm disabled:opacity-50"
          >
            {scanning ? "扫描中…" : "扫描导入"}
          </button>
        )}
      </div>
      {job && job.status === "running" && (
        <div className="text-sm text-gray-400 mb-3">
          扫描中 {job.done}/{job.total || "?"}
          {job.dirs ? <span className="text-gray-500"> · 已遍历 {job.dirs} 个目录</span> : null}
          {job.total > 0 && (
            <span className="inline-block align-middle ml-2 w-32 h-1 bg-white/10 rounded">
              <span className="block h-full bg-brand rounded" style={{ width: Math.min(100, (job.done / job.total) * 100) + "%" }} />
            </span>
          )}
        </div>
      )}
      <ScanDiagnostics job={job} />
      {err && <p className="text-red-400 text-sm mb-3">{err}</p>}
      {items.length === 0 ? (
        <p className="text-gray-500 text-sm">
          {q || kind ? "没有匹配的条目，换个关键词试试。" : "这个媒体库还是空的，点右上角「扫描导入」。"}
        </p>
      ) : (
        <>
          <div className="text-xs text-gray-500 mb-2">共 {total} 个条目{total > items.length ? `（已加载 ${items.length}）` : ""}</div>
          <div className="grid grid-cols-3 md:grid-cols-5 lg:grid-cols-7 gap-4">
            {items.map((it) => (
              <Link key={it.id} to={"/item/" + it.id}><PosterCard item={it} /></Link>
            ))}
          </div>
          {/* 虚拟滚动哨兵：进入视口加载下一页；到底后显示结束提示 */}
          {hasMore ? (
            <div ref={sentinelRef} className="py-6 text-center text-xs text-gray-500">
              {loadingMore ? "加载中…" : "继续下滑加载更多"}
            </div>
          ) : (
            items.length > PAGE && (
              <div className="py-6 text-center text-xs text-gray-500">
                已全部加载 · 共 {total} 个条目
              </div>
            )
          )}
        </>
      )}
    </div>
  );
}

// ScanDiagnostics 把扫描任务的失败原因/跳过提示/警告摊开给用户看。
// 以前扫描出问题只表现为「海报墙一片空白」，用户既不知道错在哪，也无从下手。
function ScanDiagnostics({ job }: { job: ScanJob | null }) {
  if (!job || job.status === "running") return null;
  const warns = job.warnings || [];
  if (!job.error && !job.skip_hint && warns.length === 0) return null;
  return (
    <div className="mb-3 space-y-2">
      {job.error && (
        <div className="rounded bg-red-500/10 border border-red-500/30 p-3 text-sm text-red-300">
          <b>扫描失败：</b>{job.error}
        </div>
      )}
      {job.skip_hint && (
        <div className="rounded bg-amber-500/10 border border-amber-500/30 p-3 text-sm text-amber-200">
          <b>提示：</b>{job.skip_hint}
        </div>
      )}
      {warns.length > 0 && (
        <details className="rounded bg-white/5 border border-white/10 p-3 text-sm text-gray-300">
          <summary className="cursor-pointer text-gray-400">
            有 {warns.length} 个目录/文件被跳过（点开查看）
          </summary>
          <ul className="mt-2 space-y-1 text-xs text-gray-400 max-h-48 overflow-y-auto">
            {warns.map((wmsg, i) => <li key={i} className="break-all">· {wmsg}</li>)}
          </ul>
        </details>
      )}
    </div>
  );
}

// ---- 响应式导航：桌面用左侧窄栏，手机用底部标签栏（仿飞牛影视 / 爆米花）----
// 以前是固定 192px 的 <aside>，手机上直接撑爆、毫无适配。
// 现在桌面保留较窄（w-44）的左侧栏，<md 时收起，改为固定在底部的标签栏。

type IconProps = { className?: string };
const icHome = ({ className }: IconProps) => (
  <svg className={className} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
    <path d="M3 10.5 12 3l9 7.5" /><path d="M5 9.5V21h14V9.5" />
  </svg>
);
const icLib = ({ className }: IconProps) => (
  <svg className={className} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
    <rect x="3" y="3" width="7.5" height="7.5" rx="1.2" /><rect x="13.5" y="3" width="7.5" height="7.5" rx="1.2" />
    <rect x="3" y="13.5" width="7.5" height="7.5" rx="1.2" /><rect x="13.5" y="13.5" width="7.5" height="7.5" rx="1.2" />
  </svg>
);
const icFav = ({ className }: IconProps) => (
  <svg className={className} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
    <path d="M12 20.5S4 15.7 4 9.8A4.3 4.3 0 0 1 12 7a4.3 4.3 0 0 1 8 2.8c0 5.9-8 10.7-8 10.7Z" />
  </svg>
);
const icSet = ({ className }: IconProps) => (
  <svg className={className} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
    <circle cx="12" cy="12" r="3" />
    <path d="M12 2.5v2.5M12 19v2.5M2.5 12H5M19 12h2.5M5 5l1.8 1.8M17.2 17.2 19 19M19 5l-1.8 1.8M6.8 17.2 5 19" />
  </svg>
);

// 云盘图标：2.0 内置网盘（139cas）管理入口。
const icCloud = ({ className = "" }: { className?: string }) => (
  <svg className={className} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
    <path d="M17.5 19a4.5 4.5 0 0 0 .5-8.97A6 6 0 0 0 6.2 9.4 4 4 0 0 0 6.5 19h11Z" />
  </svg>
);

const navItems = [
  { to: "/", label: "首页", Icon: icHome, end: true },
  { to: "/libraries", label: "媒体库", Icon: icLib, end: false },
  { to: "/favorites", label: "收藏", Icon: icFav, end: false },
  { to: "/settings", label: "设置", Icon: icSet, end: false },
];

// useBundled 读取健康检查里的内置网盘状态。
// 2.0 的容器内置了 139cas，此时侧边栏要多出一个「网盘挂载」入口；
// 1.x 式的裸跑部署完全看不到它，界面保持原样。
function useBundled() {
  const [info, setInfo] = useState<{ on: boolean; ready: boolean; prefix: string }>({
    on: false,
    ready: false,
    prefix: "",
  });
  useEffect(() => {
    let alive = true;
    let id: ReturnType<typeof setInterval> | undefined;
    const tick = () =>
      api
        .health()
        .then((h: any) => {
          if (!alive) return;
          const next = { on: !!h.bundled, ready: !!h.bundled_ready, prefix: h.bundled_prefix || "" };
          setInfo(next);
          // 接管成功（或压根没开内置）后就没必要继续轮询了。
          if (!next.on || next.ready) {
            if (id) clearInterval(id);
          }
        })
        .catch(() => {});
    tick();
    // 内置后端首次启动要几十秒，接管完成前每 5 秒探一次，让状态点及时转绿。
    id = setInterval(tick, 5000);
    return () => {
      alive = false;
      if (id) clearInterval(id);
    };
  }, []);
  return info;
}

// 桌面左侧窄栏（md 及以上显示）。
function Sidebar({ onLogout }: { onLogout: () => void }) {
  const bundled = useBundled();
  return (
    <aside className="hidden md:flex w-44 bg-panel p-4 space-y-1 shrink-0 flex-col border-r border-white/5">
      <div className="text-lg font-bold mb-5 px-3">NewMovie</div>
      {navItems.map(({ to, label, Icon, end }) => (
        <NavLink
          key={to}
          to={to}
          end={end}
          className={({ isActive }) =>
            "flex items-center gap-3 px-3 py-2 rounded-lg text-sm " +
            (isActive ? "bg-brand/15 text-brand font-medium" : "text-gray-300 hover:bg-card")
          }
        >
          <Icon className="w-5 h-5 shrink-0" />
          {label}
        </NavLink>
      ))}
      {bundled.on && bundled.prefix && (
        <a
          href={bundled.prefix}
          target="_blank"
          rel="noreferrer"
          title={bundled.ready ? "添加 / 管理你的网盘" : "内置网盘启动中…"}
          className="flex items-center gap-3 px-3 py-2 rounded-lg text-sm text-gray-300 hover:bg-card"
        >
          {icCloud({ className: "w-5 h-5 shrink-0" })}
          <span className="flex-1">网盘挂载</span>
          <span
            className={
              "w-1.5 h-1.5 rounded-full shrink-0 " +
              (bundled.ready ? "bg-emerald-400" : "bg-amber-400 animate-pulse")
            }
          />
        </a>
      )}
      <button
        onClick={onLogout}
        className="mt-auto text-left px-3 py-2 rounded-lg text-sm text-gray-500 hover:bg-card hover:text-gray-300"
      >
        退出登录
      </button>
    </aside>
  );
}

// 手机底部标签栏（<md 显示），仿飞牛影视：图标 + 文字，等宽分布。
function BottomNav() {
  const bundled = useBundled();
  return (
    <nav className="md:hidden fixed bottom-0 inset-x-0 z-30 bg-panel/95 backdrop-blur border-t border-white/10 pb-[env(safe-area-inset-bottom)]">
      <div className="flex">
        {navItems.map(({ to, label, Icon, end }) => (
          <NavLink
            key={to}
            to={to}
            end={end}
            className={({ isActive }) =>
              "flex-1 flex flex-col items-center gap-0.5 py-2 text-[11px] " +
              (isActive ? "text-brand" : "text-gray-400")
            }
          >
            <Icon className="w-6 h-6" />
            {label}
          </NavLink>
        ))}
        {bundled.on && bundled.prefix && (
          <a
            href={bundled.prefix}
            target="_blank"
            rel="noreferrer"
            className="flex-1 flex flex-col items-center gap-0.5 py-2 text-[11px] text-gray-400 relative"
          >
            {icCloud({ className: "w-6 h-6" })}
            网盘
            {!bundled.ready && (
              <span className="absolute top-1.5 right-[30%] w-1.5 h-1.5 rounded-full bg-amber-400 animate-pulse" />
            )}
          </a>
        )}
      </div>
    </nav>
  );
}

export default function App() {
  // token 必须是可更新的 state：登录成功后调用 setTok 才会重新渲染进主界面。
  const [token, setTok] = useState(getToken());
  const nav = useNavigate();

  // token 失效（后端返回 401）时自动退回登录页，避免整站白板。
  useEffect(() => {
    const onUnauthorized = () => {
      clearToken();
      setTok("");
    };
    window.addEventListener("newmovie:unauthorized", onUnauthorized);
    return () => window.removeEventListener("newmovie:unauthorized", onUnauthorized);
  }, []);

  if (!token) {
    return (
      <Login
        onLogin={(t) => {
          setToken(t);
          setTok(t);
          nav("/", { replace: true });
        }}
      />
    );
  }

  function logout() {
    clearToken();
    setTok("");
    nav("/", { replace: true });
  }

  return (
    <div className="min-h-screen flex">
      <Sidebar onLogout={logout} />
      {/* 手机端底部标签栏占位：main 加底部内边距，避免内容被遮挡 */}
      <main className="flex-1 p-4 sm:p-6 pb-20 md:pb-6 overflow-auto">
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/continue" element={<ContinuePage />} />
          <Route path="/favorites" element={<FavoritesPage />} />
          <Route path="/libraries" element={<Library />} />
          <Route path="/library/:id" element={<LibraryItems />} />
          <Route path="/item/:id" element={<Detail />} />
          <Route path="/play/:fileId" element={<Player />} />
          <Route path="/settings" element={<Settings />} />
        </Routes>
      </main>
      <BottomNav />
    </div>
  );
}

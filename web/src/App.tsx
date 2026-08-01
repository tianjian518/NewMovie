import { useEffect, useState } from "react";
import { Routes, Route, Link, useNavigate } from "react-router-dom";
import { api, getToken, setToken, clearToken } from "./api";
import type { MediaItem, ScanJob } from "./types";
import Library from "./pages/Library";
import Detail from "./pages/Detail";
import Player from "./pages/Player";
import Settings from "./pages/Settings";
import PosterCard from "./components/PosterCard";

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

function ContinuePage() {
  const [list, setList] = useState<any[]>([]);
  useEffect(() => { api.listContinue().then(setList).catch(() => {}); }, []);
  return (
    <div>
      <h2 className="text-xl font-bold mb-4">继续观看</h2>
      <div className="grid grid-cols-2 md:grid-cols-4 lg:grid-cols-6 gap-4">
        {list.map((r) => (
          <Link key={r.id} to={"/play/" + r.file_id} className="bg-card rounded p-3">
            <div>进度 {r.duration ? Math.round((r.position / r.duration) * 100) : 0}%</div>
            <div className="text-sm text-gray-400">{r.position}s / {r.duration}s</div>
          </Link>
        ))}
      </div>
    </div>
  );
}

function FavoritesPage() {
  const [list, setList] = useState<any[]>([]);
  useEffect(() => { api.listFavorites().then(setList).catch(() => {}); }, []);
  return (
    <div>
      <h2 className="text-xl font-bold mb-4">收藏</h2>
      <pre className="text-xs text-gray-400">{JSON.stringify(list, null, 2)}</pre>
    </div>
  );
}

function LibraryItems() {
  const id = window.location.pathname.split("/").pop() || "";
  const [items, setItems] = useState<MediaItem[]>([]);
  const [job, setJob] = useState<ScanJob | null>(null);

  const load = () => api.libraryItems(id).then(setItems).catch(() => {});
  useEffect(() => { load(); }, [id]);

  // 挂载时若后台已有进行中的扫描（如「创建并扫描导入」自动发起的），自动轮询进度。
  useEffect(() => {
    let tick: any;
    api.latestScanJob(id).then((j) => {
      setJob(j);
      if (j.status === "running") {
        tick = setInterval(async () => {
          const cur = await api.scanJob(j.id);
          setJob(cur);
          if (cur.status !== "running") { clearInterval(tick); load(); }
        }, 1500);
      }
    }).catch(() => {});
    return () => { if (tick) clearInterval(tick); };
  }, [id]);

  async function scan() {
    const r = await api.scan(id);
    const tick = setInterval(async () => {
      const j = await api.scanJob(r.job_id);
      setJob(j);
      if (j.status !== "running") { clearInterval(tick); load(); }
    }, 1500);
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <h2 className="text-xl font-bold">海报墙</h2>
        <button onClick={scan} className="bg-brand rounded px-3 py-1 text-sm">扫描导入</button>
      </div>
      {job && job.status === "running" && (
        <div className="text-sm text-gray-400 mb-3">扫描中 {job.done}/{job.total}</div>
      )}
      <div className="grid grid-cols-3 md:grid-cols-5 lg:grid-cols-7 gap-4">
        {items.map((it) => (
          <Link key={it.id} to={"/item/" + it.id}><PosterCard item={it} /></Link>
        ))}
      </div>
    </div>
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
      <aside className="w-48 bg-panel p-4 space-y-2 shrink-0 flex flex-col">
        <div className="text-xl font-bold mb-6">NewMovie</div>
        <Link className="block px-3 py-2 rounded hover:bg-card" to="/">媒体库</Link>
        <Link className="block px-3 py-2 rounded hover:bg-card" to="/continue">继续观看</Link>
        <Link className="block px-3 py-2 rounded hover:bg-card" to="/favorites">收藏</Link>
        <Link className="block px-3 py-2 rounded hover:bg-card" to="/settings">设置</Link>
        <button onClick={logout} className="mt-auto text-left px-3 py-2 rounded text-gray-400 hover:bg-card hover:text-gray-200">
          退出登录
        </button>
      </aside>
      <main className="flex-1 p-6 overflow-auto">
        <Routes>
          <Route path="/" element={<Library />} />
          <Route path="/continue" element={<ContinuePage />} />
          <Route path="/favorites" element={<FavoritesPage />} />
          <Route path="/library/:id" element={<LibraryItems />} />
          <Route path="/item/:id" element={<Detail />} />
          <Route path="/play/:fileId" element={<Player />} />
          <Route path="/settings" element={<Settings />} />
        </Routes>
      </main>
    </div>
  );
}

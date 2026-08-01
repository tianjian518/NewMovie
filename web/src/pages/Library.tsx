import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api } from "../api";
import type { Library, Storage } from "../types";

// 媒体库列表页：显示所有库，点击进入海报墙；支持新建与删除。
export default function Library() {
  const nav = useNavigate();
  const [libs, setLibs] = useState<Library[]>([]);
  const [storages, setStorages] = useState<Storage[]>([]);
  const [showForm, setShowForm] = useState(false);
  const [form, setForm] = useState({ name: "", mode: "native", storage_id: "", root_path: "" });
  const [msg, setMsg] = useState("");

  const loadLibs = () => api.listLibraries().then(setLibs).catch(() => {});
  useEffect(() => { loadLibs(); }, []);
  useEffect(() => { api.listStorages().then(setStorages).catch(() => {}); }, []);

  async function create() {
    if (!form.name || !form.storage_id || !form.root_path) {
      setMsg("请填写名称、选择存储源、填网盘内部路径");
      return;
    }
    const lib = await api.createLibrary({
      name: form.name,
      mode: form.mode as any,
      storage_id: form.storage_id,
      root_path: form.root_path,
    });
    // 建好立即扫描导入，不用再手动找按钮。
    try {
      await api.scan(lib.id);
    } catch { /* 扫描失败不影响建库，用户后可手动重试 */ }
    setShowForm(false);
    setForm({ name: "", mode: "native", storage_id: "", root_path: "" });
    setMsg("");
    loadLibs();
    nav("/library/" + lib.id); // 直接进入海报墙看扫描进度
  }

  async function del(id: string) {
    await api.deleteLibrary(id);
    loadLibs();
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <h2 className="text-xl font-bold">媒体库</h2>
        <button onClick={() => setShowForm((v) => !v)} className="bg-brand rounded px-3 py-1 text-sm">
          {showForm ? "收起" : "创建媒体库"}
        </button>
      </div>

      {showForm && (
        <div className="bg-card rounded-lg p-4 space-y-3 max-w-xl mb-4">
          <input className="w-full bg-ink rounded p-2" placeholder="媒体库名称（如 我的电影）" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
          <select className="w-full bg-ink rounded p-2" value={form.mode} onChange={(e) => setForm({ ...form, mode: e.target.value })}>
            <option value="native">原生模式（直接读 OpenList 网盘）</option>
            <option value="strm">STRM 模式（指向已有 strm 目录）</option>
            <option value="mixed">混合模式</option>
          </select>
          <select className="w-full bg-ink rounded p-2" value={form.storage_id} onChange={(e) => setForm({ ...form, storage_id: e.target.value })}>
            <option value="">选择存储源（先去「设置」绑定 OpenList）</option>
            {storages.map((s) => <option key={s.id} value={s.id}>{s.name} · {s.base_url}</option>)}
          </select>
          <input className="w-full bg-ink rounded p-2" placeholder="网盘内部路径（如 /115_open/Video，不要带域名）" value={form.root_path} onChange={(e) => setForm({ ...form, root_path: e.target.value })} />
          <button onClick={create} className="bg-brand rounded px-3 py-1 text-sm">创建并扫描导入</button>
          {msg && <p className="text-sm text-red-400">{msg}</p>}
        </div>
      )}

      <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
        {libs.map((l) => (
          <div key={l.id} className="bg-card rounded-lg p-5 hover:ring-2 hover:ring-brand">
            <Link to={"/library/" + l.id} className="text-lg font-semibold block">{l.name}</Link>
            <div className="text-xs text-gray-400 mt-2">{modeLabel(l.mode)} · {l.root_path}</div>
            <button onClick={() => del(l.id)} className="text-red-400 hover:text-red-300 text-xs mt-3">删除</button>
          </div>
        ))}
        {libs.length === 0 && !showForm && (
          <p className="text-gray-400 text-sm">还没有媒体库。点右上角「创建媒体库」，选好 OpenList 存储源并填网盘路径即可导入。</p>
        )}
      </div>
    </div>
  );
}

function modeLabel(m: string) {
  return { native: "原生模式", strm: "STRM 模式", mixed: "混合模式" }[m] || m;
}

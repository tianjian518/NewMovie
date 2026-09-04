import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api, isAdmin } from "../api";
import DirPicker, { stripJson } from "../components/DirPicker";
import type { Library, Storage } from "../types";

// 媒体库列表页：显示所有库，点击进入海报墙；支持新建与删除。
export default function Library() {
  const nav = useNavigate();
  const [libs, setLibs] = useState<Library[]>([]);
  const [storages, setStorages] = useState<Storage[]>([]);
  const [showForm, setShowForm] = useState(false);
  const [form, setForm] = useState({ name: "", mode: "native", storage_id: "", root_path: "" });
  const [msg, setMsg] = useState("");
  const [picking, setPicking] = useState(false);
  const [hint, setHint] = useState("");
  const [busy, setBusy] = useState(false);

  const loadLibs = () => api.listLibraries().then(setLibs).catch(() => {});
  useEffect(() => { loadLibs(); }, []);

  // 全局横幅：服务端缺少 ffmpeg 时，MKV/HEVC 无法页内播放，提前告知用户换镜像。
  const [ffmpegOk, setFfmpegOk] = useState<boolean | null>(null);
  useEffect(() => {
    api.health().then((h) => setFfmpegOk(!!h.ffmpeg_ok)).catch(() => {});
  }, []);
  useEffect(() => {
    api.listStorages().then((s) => {
      setStorages(s);
      // 只有一个存储源时直接选中，少一步无意义的点击。
      if (s.length === 1) setForm((f) => (f.storage_id ? f : { ...f, storage_id: s[0].id }));
    }).catch(() => {});
  }, []);

  async function create() {
    if (!form.name || !form.storage_id || !form.root_path) {
      setMsg("请填写名称、选择存储源，并选好网盘目录");
      return;
    }
    setBusy(true);
    setMsg("");
    try {
      const lib = await api.createLibrary({
        name: form.name,
        mode: form.mode as any,
        storage_id: form.storage_id,
        root_path: form.root_path,
      });
      // 建好立即扫描导入。扫描失败必须让用户看见——以前这里静默 catch 掉，
      // 用户只会得到一个永远空着的海报墙，完全不知道哪里出了问题。
      try {
        await api.scan(lib.id);
      } catch (e: any) {
        setMsg("媒体库已创建，但扫描没能启动：" + stripJson(e?.message));
        setBusy(false);
        loadLibs();
        return;
      }
      setShowForm(false);
      setForm({ name: "", mode: "native", storage_id: "", root_path: "" });
      setHint("");
      loadLibs();
      nav("/library/" + lib.id); // 直接进入海报墙看扫描进度
    } catch (e: any) {
      setMsg(stripJson(e?.message) || "创建失败");
    } finally {
      setBusy(false);
    }
  }

  async function del(id: string) {
    await api.deleteLibrary(id);
    loadLibs();
  }

  const storage = storages.find((s) => s.id === form.storage_id);

  return (
    <div>
      {ffmpegOk === false && (
        <div className="mb-4 bg-amber-500/10 border border-amber-500/40 rounded-lg p-3 text-sm text-amber-200">
          ⚠️ 服务端未安装 <b>ffmpeg</b>：MKV / HEVC / 4K 等资源无法页内播放，只能唤起外部播放器。
          请重新部署含 ffmpeg 的镜像（<code className="bg-black/30 px-1 rounded">tianjian518/newmovie:latest</code> 已含 ffmpeg）。
        </div>
      )}
      <div className="flex items-center justify-between mb-4">
        <h2 className="text-xl font-bold">媒体库</h2>
        {isAdmin() && (
          <button onClick={() => setShowForm((v) => !v)} className="bg-brand rounded px-3 py-1 text-sm">
            {showForm ? "收起" : "创建媒体库"}
          </button>
        )}
      </div>

      {showForm && (
        <div className="bg-card rounded-lg p-4 space-y-3 max-w-xl mb-4">
          <input
            className="w-full bg-ink rounded p-2"
            placeholder="媒体库名称（如 我的电影）"
            value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })}
          />
          <select
            className="w-full bg-ink rounded p-2"
            value={form.storage_id}
            onChange={(e) => setForm({ ...form, storage_id: e.target.value, root_path: "" })}
          >
            <option value="">选择存储源（先去「设置」绑定 OpenList）</option>
            {storages.map((s) => <option key={s.id} value={s.id}>{s.name} · {s.base_url}</option>)}
          </select>

          {/* 目录选择：点按钮浏览网盘，不再让用户凭空手打路径 */}
          <div className="space-y-1">
            <div className="flex gap-2">
              <input
                className="flex-1 bg-ink rounded p-2 text-sm"
                placeholder="网盘内部路径（点右侧「浏览」选择）"
                value={form.root_path}
                onChange={(e) => setForm({ ...form, root_path: e.target.value })}
              />
              <button
                onClick={() => {
                  if (!form.storage_id) { setMsg("请先选择存储源"); return; }
                  setMsg("");
                  setPicking(true);
                }}
                className="bg-white/10 hover:bg-white/20 rounded px-3 text-sm whitespace-nowrap"
              >
                浏览…
              </button>
            </div>
            {hint && <p className="text-xs text-green-400">{hint}</p>}
          </div>

          <select
            className="w-full bg-ink rounded p-2"
            value={form.mode}
            onChange={(e) => setForm({ ...form, mode: e.target.value })}
          >
            <option value="native">原生模式（直接读 OpenList 网盘里的视频文件）</option>
            <option value="strm">STRM 模式（目录里存的是 .strm 指针文件）</option>
            <option value="mixed">混合模式（视频与 .strm 都收）</option>
          </select>
          <p className="text-xs text-gray-500">
            模式选错是「扫不出内容」最常见的原因。用上面的「浏览」选目录时，系统会看一眼里面有什么并自动帮你选好。
          </p>

          <button onClick={create} disabled={busy} className="bg-brand rounded px-3 py-1 text-sm disabled:opacity-50">
            {busy ? "创建中…" : "创建并扫描导入"}
          </button>
          {msg && <p className="text-sm text-red-400">{msg}</p>}
        </div>
      )}

      {picking && storage && (
        <DirPicker
          storageId={storage.id}
          initialPath={form.root_path || "/"}
          onClose={() => setPicking(false)}
          onPick={(p, suggest, video, strm) => {
            // 顺手用探测到的内容纠正库模式与库名，减少用户出错的机会。
            const patch: any = { ...form, root_path: p };
            if (suggest) patch.mode = suggest;
            if (!form.name) patch.name = p.split("/").filter(Boolean).pop() || "新媒体库";
            setForm(patch);
            setPicking(false);
            setHint(
              video + strm > 0
                ? `已选择：${p}（本层 ${video} 个视频${strm ? ` / ${strm} 个 strm` : ""}${suggest ? `，已自动切换为${modeLabel(suggest)}` : ""}）`
                : `已选择：${p}（本层没有视频，将递归扫描其子目录）`
            );
          }}
        />
      )}

      <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
        {libs.map((l) => (
          <div key={l.id} className="bg-card rounded-lg p-4 md:p-5 hover:ring-2 hover:ring-brand">
            <Link to={"/library/" + l.id} className="text-lg font-semibold block">{l.name}</Link>
            <div className="text-xs text-gray-400 mt-2 break-all">{modeLabel(l.mode)} · {l.root_path}</div>
            {isAdmin() && (
              <button onClick={() => del(l.id)} className="text-red-400 hover:text-red-300 text-xs mt-3">删除</button>
            )}
          </div>
        ))}
        {libs.length === 0 && !showForm && (
          <p className="text-gray-400 text-sm">还没有媒体库。点右上角「创建媒体库」，选好 OpenList 存储源后用「浏览」挑一个目录即可导入。</p>
        )}
      </div>
    </div>
  );
}

function modeLabel(m: string) {
  return ({ native: "原生模式", strm: "STRM 模式", mixed: "混合模式" } as Record<string, string>)[m] || m;
}

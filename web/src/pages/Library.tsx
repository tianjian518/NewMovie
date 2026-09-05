import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api, isAdmin } from "../api";
import DirPicker, { stripJson } from "../components/DirPicker";
import type { Library, Storage } from "../types";

// 预设图标（emoji），用户可从中选择
const ICON_OPTIONS = ["🎬", "📺", "🎮", "🎵", "📚", "🎨", "🎭", "🌟", "🔥", "💎", "🚀", "🎯"];
// 预设主题色（用于卡片渐变背景）
const COLOR_OPTIONS = [
  "#6366f1", // 靛蓝
  "#8b5cf6", // 紫色
  "#ec4899", // 粉红
  "#ef4444", // 红色
  "#f97316", // 橙色
  "#eab308", // 黄色
  "#22c55e", // 绿色
  "#14b8a6", // 青色
  "#06b6d4", // 天蓝
  "#3b82f6", // 蓝色
  "#64748b", // 石板灰
  "#a855f7", // 紫罗兰
];

// 媒体库列表页：显示所有库，点击进入海报墙；支持新建、编辑、删除。
export default function Library() {
  const nav = useNavigate();
  const [libs, setLibs] = useState<Library[]>([]);
  const [itemCounts, setItemCounts] = useState<Record<string, number>>({});
  const [storages, setStorages] = useState<Storage[]>([]);
  const [showForm, setShowForm] = useState(false);
  const [form, setForm] = useState({ name: "", mode: "native", storage_id: "", root_path: "" });
  const [msg, setMsg] = useState("");
  const [picking, setPicking] = useState(false);
  const [hint, setHint] = useState("");
  const [busy, setBusy] = useState(false);
  // 编辑状态
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editForm, setEditForm] = useState({ name: "", icon: "", color: "" });

  const loadLibs = () => api.listLibraries().then(setLibs).catch(() => {});
  useEffect(() => { loadLibs(); }, []);

  // 加载每个媒体库的条目数量
  useEffect(() => {
    const counts: Record<string, number> = {};
    Promise.all(
      libs.map((l) =>
        api.libraryItems(l.id).then((items: any[]) => { counts[l.id] = items.length; }).catch(() => {})
      )
    ).then(() => setItemCounts(counts));
  }, [libs]);

  // 全局横幅：服务端缺少 ffmpeg 时，MKV/HEVC 无法页内播放，提前告知用户换镜像。
  const [ffmpegOk, setFfmpegOk] = useState<boolean | null>(null);
  useEffect(() => {
    api.health().then((h) => setFfmpegOk(!!h.ffmpeg_ok)).catch(() => {});
  }, []);
  useEffect(() => {
    api.listStorages().then((s) => {
      setStorages(s);
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
      nav("/library/" + lib.id);
    } catch (e: any) {
      setMsg(stripJson(e?.message) || "创建失败");
    } finally {
      setBusy(false);
    }
  }

  async function del(id: string) {
    if (!confirm("确定删除这个媒体库吗？（不会删除网盘里的文件）")) return;
    await api.deleteLibrary(id);
    loadLibs();
  }

  function startEdit(lib: Library) {
    setEditingId(lib.id);
    setEditForm({ name: lib.name, icon: lib.icon || "🎬", color: lib.color || "#6366f1" });
  }

  async function saveEdit() {
    if (!editingId) return;
    try {
      await api.updateLibrary(editingId, {
        name: editForm.name,
        icon: editForm.icon,
        color: editForm.color,
      });
      setEditingId(null);
      loadLibs();
    } catch (e: any) {
      alert("保存失败：" + stripJson(e?.message));
    }
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
      <div className="flex items-center justify-between mb-5">
        <h2 className="text-xl font-bold">媒体库</h2>
        {isAdmin() && (
          <button onClick={() => setShowForm((v) => !v)} className="bg-brand rounded px-3 py-1.5 text-sm font-medium">
            {showForm ? "收起" : "+ 创建媒体库"}
          </button>
        )}
      </div>

      {showForm && (
        <div className="bg-card rounded-xl p-4 space-y-3 max-w-xl mb-5 border border-white/5">
          <input
            className="w-full bg-ink rounded-lg p-2.5"
            placeholder="媒体库名称（如 我的电影）"
            value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })}
          />
          <select
            className="w-full bg-ink rounded-lg p-2.5"
            value={form.storage_id}
            onChange={(e) => setForm({ ...form, storage_id: e.target.value, root_path: "" })}
          >
            <option value="">选择存储源（先去「设置」绑定 OpenList）</option>
            {storages.map((s) => <option key={s.id} value={s.id}>{s.name} · {s.base_url}</option>)}
          </select>
          <div className="space-y-1">
            <div className="flex gap-2">
              <input
                className="flex-1 bg-ink rounded-lg p-2.5 text-sm"
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
                className="bg-white/10 hover:bg-white/20 rounded-lg px-3 text-sm whitespace-nowrap"
              >
                浏览…
              </button>
            </div>
            {hint && <p className="text-xs text-green-400">{hint}</p>}
          </div>
          <select
            className="w-full bg-ink rounded-lg p-2.5"
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
          <button onClick={create} disabled={busy} className="bg-brand rounded-lg px-4 py-2 text-sm font-medium disabled:opacity-50">
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

      {/* 媒体库卡片网格：整齐对齐，飞牛影视风格 */}
      <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-4">
        {libs.map((l) => {
          const icon = l.icon || "🎬";
          const color = l.color || "#6366f1";
          const count = itemCounts[l.id];
          const isEditing = editingId === l.id;

          if (isEditing) {
            return (
              <div key={l.id} className="bg-card rounded-xl p-4 border border-brand/40 space-y-3">
                <input
                  className="w-full bg-ink rounded-lg p-2 text-sm"
                  value={editForm.name}
                  onChange={(e) => setEditForm({ ...editForm, name: e.target.value })}
                  placeholder="媒体库名称"
                />
                <div>
                  <p className="text-xs text-gray-400 mb-1.5">图标</p>
                  <div className="flex flex-wrap gap-1.5">
                    {ICON_OPTIONS.map((ic) => (
                      <button
                        key={ic}
                        onClick={() => setEditForm({ ...editForm, icon: ic })}
                        className={"w-8 h-8 rounded-lg text-lg flex items-center justify-center transition-all " +
                          (editForm.icon === ic ? "bg-brand/30 ring-2 ring-brand" : "bg-ink hover:bg-white/10")}
                      >
                        {ic}
                      </button>
                    ))}
                  </div>
                </div>
                <div>
                  <p className="text-xs text-gray-400 mb-1.5">主题色</p>
                  <div className="flex flex-wrap gap-1.5">
                    {COLOR_OPTIONS.map((c) => (
                      <button
                        key={c}
                        onClick={() => setEditForm({ ...editForm, color: c })}
                        className={"w-6 h-6 rounded-full transition-all " +
                          (editForm.color === c ? "ring-2 ring-white ring-offset-2 ring-offset-card" : "")}
                        style={{ backgroundColor: c }}
                      />
                    ))}
                  </div>
                </div>
                <div className="flex gap-2 pt-1">
                  <button onClick={saveEdit} className="flex-1 bg-brand rounded-lg py-1.5 text-sm font-medium">保存</button>
                  <button onClick={() => setEditingId(null)} className="flex-1 bg-white/10 rounded-lg py-1.5 text-sm">取消</button>
                </div>
              </div>
            );
          }

          return (
            <div
              key={l.id}
              className="group relative rounded-xl overflow-hidden transition-all duration-200 hover:scale-[1.02] hover:shadow-xl cursor-pointer border border-white/5"
              style={{ background: `linear-gradient(135deg, ${color}22 0%, ${color}08 100%)` }}
            >
              <Link to={"/library/" + l.id} className="block p-4">
                {/* 图标区域 */}
                <div
                  className="w-12 h-12 rounded-xl flex items-center justify-center text-2xl mb-3 shadow-lg"
                  style={{ backgroundColor: color + "33", boxShadow: `0 4px 12px ${color}40` }}
                >
                  {icon}
                </div>
                {/* 名称 */}
                <h3 className="font-semibold text-base truncate">{l.name}</h3>
                {/* 元信息 */}
                <div className="flex items-center gap-2 mt-1.5 text-xs text-gray-400">
                  <span>{modeLabel(l.mode)}</span>
                  {count !== undefined && (
                    <>
                      <span className="text-gray-600">·</span>
                      <span>{count} 部</span>
                    </>
                  )}
                </div>
              </Link>
              {/* 悬停操作按钮 */}
              {isAdmin() && (
                <div className="absolute top-2 right-2 opacity-0 group-hover:opacity-100 transition-opacity flex gap-1">
                  <button
                    onClick={(e) => { e.preventDefault(); e.stopPropagation(); startEdit(l); }}
                    className="w-7 h-7 rounded-lg bg-black/50 backdrop-blur flex items-center justify-center text-xs hover:bg-black/70"
                    title="编辑"
                  >
                    ✏️
                  </button>
                  <button
                    onClick={(e) => { e.preventDefault(); e.stopPropagation(); del(l.id); }}
                    className="w-7 h-7 rounded-lg bg-black/50 backdrop-blur flex items-center justify-center text-xs hover:bg-red-500/70"
                    title="删除"
                  >
                    🗑️
                  </button>
                </div>
              )}
            </div>
          );
        })}
        {libs.length === 0 && !showForm && (
          <p className="text-gray-400 text-sm col-span-full">还没有媒体库。点右上角「创建媒体库」，选好 OpenList 存储源后用「浏览」挑一个目录即可导入。</p>
        )}
      </div>
    </div>
  );
}

function modeLabel(m: string) {
  return ({ native: "原生模式", strm: "STRM 模式", mixed: "混合模式" } as Record<string, string>)[m] || m;
}

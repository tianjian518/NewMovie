import { useEffect, useState } from "react";
import { api } from "../api";
import type { BrowseResp } from "../types";

// DirPicker 网盘目录选择器。
//
// 建库时让用户手打「网盘内部路径」是个糟糕的设计：路径全靠脑补，
// 少个斜杠、多个空格就整库扫不出东西，还查不出原因。
// 这里改成逐级点选——左边面包屑定位，中间列子目录，底部实时告诉你
// 「这个文件夹里有几个视频/几个 strm」，选完直接回填。
export default function DirPicker({
  storageId,
  initialPath = "/",
  onPick,
  onClose,
}: {
  storageId: string;
  initialPath?: string;
  onPick: (path: string, suggestMode: string, videoCount: number, strmCount: number) => void;
  onClose: () => void;
}) {
  const [path, setPath] = useState(initialPath || "/");
  const [data, setData] = useState<BrowseResp | null>(null);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");

  async function load(p: string, refresh = false) {
    setLoading(true);
    setErr("");
    try {
      const d = await api.browse(storageId, p, refresh);
      setData(d);
      setPath(d.path);
    } catch (e: any) {
      // 后端已经把 OpenList 的原始报错翻成了人话，直接透出即可。
      setErr(stripJson(e?.message) || "读取目录失败");
      setData(null);
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    if (storageId) load(initialPath || "/");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [storageId]);

  const crumbs = buildCrumbs(path);
  const hasMedia = !!data && data.video_count + data.strm_count > 0;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-4" onClick={onClose}>
      <div
        className="bg-card w-full max-w-2xl rounded-xl shadow-2xl flex flex-col max-h-[80vh]"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between border-b border-white/10 px-4 py-3">
          <h3 className="font-semibold">选择网盘目录</h3>
          <div className="flex items-center gap-3">
            <button
              onClick={() => load(path, true)}
              className="text-xs text-gray-400 hover:text-white"
              title="强制刷新 OpenList 缓存（会真实回源，请勿频繁点）"
            >
              刷新
            </button>
            <button onClick={onClose} className="text-gray-400 hover:text-white">✕</button>
          </div>
        </div>

        {/* 面包屑：随时能跳回上层，不用一路点返回 */}
        <div className="flex flex-wrap items-center gap-1 px-4 py-2 text-sm border-b border-white/5">
          {crumbs.map((c, i) => (
            <span key={c.path} className="flex items-center gap-1">
              {i > 0 && <span className="text-gray-600">/</span>}
              <button
                onClick={() => load(c.path)}
                className={
                  c.path === path
                    ? "text-brand font-medium"
                    : "text-gray-300 hover:text-white hover:underline"
                }
              >
                {c.name}
              </button>
            </span>
          ))}
        </div>

        <div className="flex-1 overflow-y-auto px-2 py-2 min-h-[16rem]">
          {loading && <p className="p-4 text-sm text-gray-400">读取中…</p>}
          {err && (
            <div className="m-3 rounded bg-red-500/10 border border-red-500/30 p-3 text-sm text-red-300">
              {err}
            </div>
          )}
          {!loading && !err && data && (
            <>
              {data.parent !== "" && (
                <button
                  onClick={() => load(data.parent)}
                  className="w-full text-left px-3 py-2 rounded hover:bg-white/5 text-gray-400 text-sm"
                >
                  ↰ 返回上级
                </button>
              )}
              {data.dirs.map((d) => (
                <button
                  key={d.path}
                  onDoubleClick={() => load(d.path)}
                  onClick={() => load(d.path)}
                  className="w-full text-left px-3 py-2 rounded hover:bg-white/5 flex items-center gap-2"
                >
                  <span className="text-yellow-500/80">📁</span>
                  <span className="truncate">{d.name}</span>
                </button>
              ))}
              {data.dirs.length === 0 && (
                <p className="p-4 text-sm text-gray-500">
                  这一层没有子目录{hasMedia ? "，可以直接选它" : ""}。
                </p>
              )}
            </>
          )}
        </div>

        {/* 底部：当前目录里到底有没有片子，一眼看清再决定选不选 */}
        <div className="border-t border-white/10 px-4 py-3 space-y-2">
          <div className="text-xs text-gray-400 break-all">
            当前：<span className="text-gray-200">{path}</span>
          </div>
          {data && (
            <div className="text-xs">
              {hasMedia ? (
                <span className="text-green-400">
                  本层含 {data.video_count} 个视频
                  {data.strm_count > 0 ? ` · ${data.strm_count} 个 strm` : ""}
                  {data.suggest_mode ? ` · 建议用${modeLabel(data.suggest_mode)}` : ""}
                </span>
              ) : (
                <span className="text-gray-500">
                  本层没有视频文件（子目录里可能有，选它也能递归扫描）
                </span>
              )}
            </div>
          )}
          <div className="flex justify-end gap-2">
            <button onClick={onClose} className="px-3 py-1.5 rounded text-sm text-gray-300 hover:bg-white/5">
              取消
            </button>
            <button
              onClick={() => data && onPick(path, data.suggest_mode, data.video_count, data.strm_count)}
              disabled={!data}
              className="bg-brand rounded px-4 py-1.5 text-sm disabled:opacity-40"
            >
              选择此目录
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

// buildCrumbs 把 /a/b/c 拆成可点击的面包屑。
function buildCrumbs(p: string) {
  const out = [{ name: "根目录", path: "/" }];
  const segs = p.split("/").filter(Boolean);
  let cur = "";
  for (const s of segs) {
    cur += "/" + s;
    out.push({ name: s, path: cur });
  }
  return out;
}

function modeLabel(m: string) {
  return ({ native: "原生模式", strm: "STRM 模式", mixed: "混合模式" } as Record<string, string>)[m] || m;
}

// stripJson 后端错误体是 {"error":"..."} 形式，直接显示原始 JSON 太丑。
export function stripJson(msg?: string): string {
  if (!msg) return "";
  try {
    const o = JSON.parse(msg);
    return o.error || o.message || msg;
  } catch {
    return msg;
  }
}

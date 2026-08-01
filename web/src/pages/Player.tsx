import { useEffect, useRef, useState } from "react";
import { useParams, Link } from "react-router-dom";
import ArtPlayer from "artplayer";
import { api } from "../api";
import type { PlayDecision } from "../types";

function fmtTime(sec: number): string {
  if (!sec || sec < 0) return "0:00";
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = Math.floor(sec % 60);
  const pad = (n: number) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

// 播放页：请求五级降级决策，按策略渲染。
//  L0/L1 → ArtPlayer 直接播
//  L2/L3 → 提示需 Remux/转码（Phase 3 含 ffmpeg 镜像）
//  L4    → 唤起外部播放器（直链传递）
// 进度全程回传后端（继续观看）。
export default function Player() {
  const { fileId } = useParams();
  const [dec, setDec] = useState<PlayDecision | null>(null);
  const [err, setErr] = useState("");
  const [resumed, setResumed] = useState(0);
  const ref = useRef<HTMLDivElement>(null);
  const artRef = useRef<any>(null);

  useEffect(() => {
    if (!fileId) return;
    api.play(fileId).then(setDec).catch((e) => setErr(e.message));
  }, [fileId]);

  useEffect(() => {
    if (!dec || !ref.current) return;
    // 仅 L0/L1 用内置播放器
    if ((dec.level === 0 || dec.level === 1 || dec.level === 2) && dec.url) {
      const art = new ArtPlayer({
        container: ref.current,
        url: dec.url,
        type: dec.url.endsWith(".m3u8") ? "m3u8" : "auto",
        autoplay: true,
        playbackRate: true,
        subtitle: { encoding: "utf-8" },
      });
      artRef.current = art;

      // 续播：进度一直在往后端存，却从来没读回来过 ——
      // 侧边栏「继续观看」点进去居然是从头播的，这次真的接上。
      const resume = dec.resume_position || 0;
      art.on("ready", () => {
        if (resume > 3) {
          try { art.currentTime = resume; } catch { /* 源不支持 seek 就算了 */ }
          setResumed(resume);
        }
      });

      const flush = () => {
        const v = art.video;
        if (v && v.currentTime > 0) {
          api.saveRecord(fileId!, Math.floor(v.currentTime), Math.floor(v.duration || 0)).catch(() => {});
        }
      };
      const save = setInterval(flush, 10000);
      // 关标签页/刷新时补存一次，否则最后不到 10 秒的进度会丢。
      window.addEventListener("beforeunload", flush);

      return () => {
        clearInterval(save);
        window.removeEventListener("beforeunload", flush);
        flush(); // 离开页面前再落一次盘
        art.destroy();
      };
    }
  }, [dec, fileId]);

  if (err) {
    return (
      <div className="space-y-3">
        <p className="text-red-400">播放源解析失败：{err}</p>
        <Link to="/" className="text-sm text-blue-400">返回媒体库</Link>
      </div>
    );
  }
  if (!dec) return <div className="text-gray-400">解析播放源…</div>;

  const ext = dec.raw_url || dec.direct_url;

  return (
    <div>
      {(dec.title || dec.subtitle) && (
        <h2 className="text-lg font-bold mb-2">
          {dec.item_id ? <Link to={"/item/" + dec.item_id} className="hover:text-brand">{dec.title}</Link> : dec.title}
          {dec.subtitle && <span className="text-gray-400 text-sm ml-2">{dec.subtitle}</span>}
        </h2>
      )}
      <div className="mb-3 flex flex-wrap items-center gap-2 text-sm">
        <span className="bg-brand rounded px-2 py-0.5">{dec.label}</span>
        <span className="text-gray-400">{dec.reason}</span>
        {resumed > 0 && (
          <span className="text-gray-400 border border-white/10 rounded px-2 py-0.5">
            已从 {fmtTime(resumed)} 继续播放
          </span>
        )}
      </div>

      {(dec.level === 0 || dec.level === 1 || dec.level === 2) && dec.url ? (
        <div ref={ref} className="w-full aspect-video bg-black rounded-xl" />
      ) : (
        <div className="aspect-video bg-card rounded-xl flex flex-col items-center justify-center gap-3 text-center p-6">
          <p className="text-gray-300">
            {dec.level === 4
              ? "已为你选择外部播放器（原画画质/音质）。"
              : "当前源需要服务端 Remux / 转码（Phase 3 的 :full 镜像提供），或唤起外部播放器。"}
          </p>
          {ext && (
            <a href={ext} target="_blank" rel="noreferrer" className="bg-brand rounded px-4 py-2">
              唤起外部播放器
            </a>
          )}
        </div>
      )}

      {ext && (dec.level === 0 || dec.level === 1 || dec.level === 2) && (
        <div className="mt-3">
          <a href={ext} target="_blank" rel="noreferrer" className="text-sm text-blue-400">或在新窗口用外部播放器打开</a>
        </div>
      )}
    </div>
  );
}

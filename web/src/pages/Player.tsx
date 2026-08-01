import { useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import ArtPlayer from "artplayer";
import { api } from "../api";
import type { PlayDecision } from "../types";

// 播放页：请求五级降级决策，按策略渲染。
//  L0/L1 → ArtPlayer 直接播
//  L2/L3 → 提示需 Remux/转码（Phase 3 含 ffmpeg 镜像）
//  L4    → 唤起外部播放器（直链传递）
// 进度全程回传后端（继续观看）。
export default function Player() {
  const { fileId } = useParams();
  const [dec, setDec] = useState<PlayDecision | null>(null);
  const [err, setErr] = useState("");
  const ref = useRef<HTMLDivElement>(null);
  const artRef = useRef<any>(null);

  useEffect(() => {
    if (!fileId) return;
    api.play(fileId).then(setDec).catch((e) => setErr(e.message));
  }, [fileId]);

  useEffect(() => {
    if (!dec || !ref.current) return;
    // 仅 L0/L1 用内置播放器
    if ((dec.level === 0 || dec.level === 1) && dec.url) {
      const art = new ArtPlayer({
        container: ref.current,
        url: dec.url,
        type: dec.url.endsWith(".m3u8") ? "m3u8" : "auto",
        autoplay: true,
        playbackRate: true,
        subtitle: { encoding: "utf-8" },
      });
      artRef.current = art;
      const save = setInterval(() => {
        if (art.video && art.video.currentTime > 0) {
          api.saveRecord(fileId!, Math.floor(art.video.currentTime), Math.floor(art.video.duration || 0)).catch(() => {});
        }
      }, 10000);
      return () => { clearInterval(save); art.destroy(); };
    }
  }, [dec, fileId]);

  if (err) return <div className="text-red-400">{err}</div>;
  if (!dec) return <div className="text-gray-400">解析播放源…</div>;

  const ext = dec.raw_url || dec.direct_url;

  return (
    <div>
      <div className="mb-3 flex items-center gap-2 text-sm">
        <span className="bg-brand rounded px-2 py-0.5">{dec.label}</span>
        <span className="text-gray-400">{dec.reason}</span>
      </div>

      {(dec.level === 0 || dec.level === 1) && dec.url ? (
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

      {ext && (dec.level === 0 || dec.level === 1) && (
        <div className="mt-3">
          <a href={ext} target="_blank" rel="noreferrer" className="text-sm text-blue-400">或在新窗口用外部播放器打开</a>
        </div>
      )}
    </div>
  );
}

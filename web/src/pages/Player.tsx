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
//  L2    → 服务端实时转封装（MKV 等）后页内播，支持选音轨
//  L3/L4 → 唤起外部播放器（直链传递）
// 字幕经后端转成 WebVTT 后可切换；多音轨 MKV 经 remux 选轨。
// 进度全程回传后端（继续观看）。
export default function Player() {
  const { fileId } = useParams();
  const [dec, setDec] = useState<PlayDecision | null>(null);
  const [err, setErr] = useState("");
  const [resumed, setResumed] = useState(0);
  const [subLang, setSubLang] = useState("off");
  const [audIdx, setAudIdx] = useState(-1);
  const [playErr, setPlayErr] = useState("");
  const ref = useRef<HTMLDivElement>(null);
  const artRef = useRef<any>(null);

  useEffect(() => {
    if (!fileId) return;
    api.play(fileId).then(setDec).catch((e) => setErr(e.message));
  }, [fileId]);

  // 把切换逻辑挂到实例上，供下方下拉调用。
  const applySub = (lang: string) => {
    const art = artRef.current;
    if (!art) return;
    setSubLang(lang);
    try {
      if (lang === "off") {
        art.subtitle.hide();
      } else {
        const s = (dec?.subtitles || []).find((x) => x.lang === lang);
        if (s) {
          art.subtitle.switch(s.url, s.title);
          art.subtitle.show();
        }
      }
    } catch {
      /* 某些格式无字幕轨时忽略 */
    }
  };
  const applyAud = (idx: number) => {
    const art = artRef.current;
    if (!art || !dec || dec.level !== 2) return;
    setAudIdx(idx);
    if (idx < 0) return;
    const cur = art.currentTime || 0;
    const sep = dec.url.includes("?") ? "&" : "?";
    const u = dec.url + sep + "atrack=" + idx;
    try {
      art.switchUrl(u);
      const onCp = () => {
        try { art.currentTime = cur; } catch {}
        art.video.removeEventListener("canplay", onCp);
      };
      art.video.addEventListener("canplay", onCp);
    } catch {
      /* 切换失败静默 */
    }
  };

  useEffect(() => {
    if (!dec || !ref.current) return;
    setPlayErr("");
    // L0 直链 / L1 代理 / L2 重封装 / L3 转码 都走页内 ArtPlayer。
    if ((dec.level === 0 || dec.level === 1 || dec.level === 2 || dec.level === 3) && dec.url) {
      const art = new ArtPlayer({
        container: ref.current,
        url: dec.url,
        type: dec.url.endsWith(".m3u8") ? "m3u8" : "auto",
        autoplay: true,
        playbackRate: true,
        subtitle: { encoding: "utf-8" },
      });
      artRef.current = art;

      // 视频加载/解码失败（如浏览器无 HEVC 解码器）：给出明确引导，而不是一片黑。
      const onErr = () => {
        setPlayErr("视频加载失败：可能是浏览器不支持该编码（如 HEVC/H.265）。可在「设置」开启「允许视频转码(HEVC→H.264)」后重试，或点下方用外部播放器打开。");
      };
      art.on("error", onErr);
      if (art.video) art.video.addEventListener("error", onErr);

      // 续播：进度一直在往后端存，这次接上，从断点接着放。
      const resume = dec.resume_position || 0;
      art.on("ready", () => {
        if (resume > 3) {
          try { art.currentTime = resume; } catch {}
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
      window.addEventListener("beforeunload", flush);

      return () => {
        clearInterval(save);
        window.removeEventListener("beforeunload", flush);
        flush();
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
  const subs = dec.subtitles || [];
  const auds = dec.audio_tracks || [];
  const canSwitchAud = dec.level === 2 && auds.length > 1;

  return (
    <div>
      {(dec.title || dec.subtitle) && (
        <h2 className="text-lg font-bold mb-2">
          {dec.item_id ? <Link to={"/item/" + dec.item_id} className="hover:text-brand">{dec.title}</Link> : dec.title}
          {dec.subtitle && <span className="text-gray-400 text-sm ml-2">{dec.subtitle}</span>}
        </h2>
      )}

      {/* 字幕 / 音轨切换 */}
      {(subs.length > 0 || canSwitchAud) && (
        <div className="mb-3 flex flex-wrap items-center gap-4 text-sm">
          {subs.length > 0 && (
            <label className="flex items-center gap-2">
              <span className="text-gray-400">字幕</span>
              <select
                className="bg-card rounded px-2 py-1"
                value={subLang}
                onChange={(e) => applySub(e.target.value)}
              >
                <option value="off">关闭</option>
                {subs.map((s) => (
                  <option key={s.lang + s.title} value={s.lang}>{s.title}</option>
                ))}
              </select>
            </label>
          )}
          {canSwitchAud && (
            <label className="flex items-center gap-2">
              <span className="text-gray-400">音轨</span>
              <select
                className="bg-card rounded px-2 py-1"
                value={audIdx}
                onChange={(e) => applyAud(Number(e.target.value))}
              >
                <option value={-1}>默认</option>
                {auds.map((a, i) => (
                  <option key={a.index} value={i}>
                    {a.title || a.lang || "音轨" + (i + 1)}（{a.codec}）
                  </option>
                ))}
              </select>
            </label>
          )}
          {resumed > 0 && (
            <span className="text-gray-400 border border-white/10 rounded px-2 py-0.5">
              已从 {fmtTime(resumed)} 继续播放
            </span>
          )}
        </div>
      )}

      {(dec.level === 0 || dec.level === 1 || dec.level === 2 || dec.level === 3) && dec.url ? (
        <div ref={ref} className="w-full aspect-video bg-black rounded-xl" />
      ) : (
        <div className="aspect-video bg-card rounded-xl flex flex-col items-center justify-center gap-3 text-center p-6">
          <p className="text-gray-300">
            {dec.level === 4
              ? "已为你选择外部播放器（原画画质/音质）。"
              : "当前源需要服务端 Remux / 转码才能页内播放，或唤起外部播放器。"}
          </p>
          {ext && (
            <a href={ext} target="_blank" rel="noreferrer" className="bg-brand rounded px-4 py-2">
              唤起外部播放器
            </a>
          )}
        </div>
      )}

      {dec.warn && (
        <div className="mt-3 bg-amber-500/10 border border-amber-500/40 rounded-lg p-4 text-sm text-amber-200">
          <p className="font-medium mb-1">⚠️ 服务端缺少 ffmpeg</p>
          <p>{dec.warn}</p>
          <p className="mt-2 text-amber-200/80">
            重新部署含 ffmpeg 的镜像（<code className="bg-black/30 px-1 rounded">tianjian518/newmovie:latest</code> 已含）后，MKV / HEVC 即可页内播放，无需外部播放器。
          </p>
        </div>
      )}

      {ext && (dec.level === 0 || dec.level === 1 || dec.level === 2 || dec.level === 3) && (
        <div className="mt-3">
          <a href={ext} target="_blank" rel="noreferrer" className="text-sm text-blue-400">或在新窗口用外部播放器打开</a>
        </div>
      )}
      {playErr && (
        <div className="mt-3 bg-red-500/10 border border-red-500/30 rounded-lg p-4 text-sm text-red-300">
          {playErr}
        </div>
      )}
    </div>
  );
}

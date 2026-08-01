import { useEffect, useState } from "react";
import { useParams, Link } from "react-router-dom";
import { api } from "../api";
import type { MediaItem, MediaFile } from "../types";
import PosterCard from "../components/PosterCard";

// 详情页：海报/简介 + 文件（剧集）列表，点击进入播放。
export default function Detail() {
  const { id } = useParams();
  const [item, setItem] = useState<MediaItem | null>(null);
  const [files, setFiles] = useState<MediaFile[]>([]);

  useEffect(() => {
    if (!id) return;
    api.item(id).then((r) => { setItem(r.item); setFiles(r.files); }).catch(() => {});
  }, [id]);

  if (!item) return <div className="text-gray-400">加载中…</div>;

  return (
    <div>
      <div
        className="h-48 rounded-xl mb-4 bg-cover bg-center"
        style={{ backgroundImage: item.backdrop_url ? `url(${item.backdrop_url})` : undefined, background: "#171b22" }}
      />
      <div className="flex gap-6">
        <div className="w-40 shrink-0"><PosterCard item={item} /></div>
        <div className="flex-1">
          <h1 className="text-2xl font-bold">{item.title}{item.year > 0 && <span className="text-gray-400 text-lg"> ({item.year})</span>}</h1>
          {item.rating > 0 && <div className="text-yellow-400">★ {item.rating}</div>}
          <p className="text-gray-300 mt-3 text-sm leading-relaxed">{item.overview}</p>
          <div className="flex gap-2 mt-3">
            <button
              className="bg-brand rounded px-3 py-1 text-sm"
              onClick={async () => { await api.addFavorite(item.id, "favorite"); alert("已收藏"); }}
            >收藏</button>
            <button
              className="bg-card border border-white/10 rounded px-3 py-1 text-sm"
              onClick={async () => {
                if (!confirm("重新从 TMDB/NFO 刮削该条目？")) return;
                await api.rescrape(item.id);
                alert("已触发重新刮削");
                api.item(item.id).then((r) => setItem(r.item));
              }}
            >重新刮削</button>
          </div>
        </div>
      </div>

      <h2 className="text-lg font-bold mt-6 mb-3">文件 / 剧集（{files.length}）</h2>
      <div className="space-y-2">
        {files.map((f) => (
          <Link
            key={f.id}
            to={"/play/" + f.id}
            className="flex items-center justify-between bg-card rounded px-4 py-2 hover:ring-2 hover:ring-brand"
          >
            <span className="truncate">{f.path.split("/").pop()}</span>
            <span className="text-xs text-gray-400">{f.container || "?"} · {f.source}</span>
          </Link>
        ))}
      </div>
    </div>
  );
}

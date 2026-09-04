import { useEffect, useState } from "react";
import type { MediaItem } from "../types";

// 海报墙卡片。海报图加载失败时回退到标题首字占位（深色紧凑，Emby 风），
// 避免出现「破图」占位（测试报告 Bug-04）。
export default function PosterCard({ item }: { item: MediaItem }) {
  const [imgFailed, setImgFailed] = useState(false);
  // 切换影片（poster_url 变化）时重置失败状态，避免复用组件实例时残留旧状态。
  useEffect(() => setImgFailed(false), [item.poster_url]);

  const showImg = Boolean(item.poster_url) && !imgFailed;

  return (
    <div className="bg-card rounded-lg overflow-hidden transition-all duration-200 ease-out hover:scale-[1.03] hover:shadow-lg hover:shadow-brand/20 hover:ring-2 hover:ring-brand/60 group">
      <div className="aspect-[2/3] bg-ink relative overflow-hidden">
        {showImg ? (
          <img
            src={item.poster_url}
            alt={item.title}
            className="w-full h-full object-cover transition-transform duration-300 group-hover:scale-105"
            loading="lazy"
            onError={() => setImgFailed(true)}
          />
        ) : (
          <div className="w-full h-full flex items-center justify-center text-3xl font-bold text-gray-600">
            {item.title.slice(0, 1)}
          </div>
        )}
        {/* 评分角标 */}
        {item.rating > 0 && (
          <span className="absolute top-1 left-1 text-[10px] bg-black/70 text-yellow-400 px-1.5 py-0.5 rounded font-medium">
            ★ {item.rating.toFixed(1)}
          </span>
        )}
        {item.year > 0 && (
          <span className="absolute bottom-1 right-1 text-[10px] bg-black/70 px-1 rounded">{item.year}</span>
        )}
        {/* 悬停渐变遮罩，让标题更清晰 */}
        <div className="absolute inset-0 bg-gradient-to-t from-black/60 via-transparent to-transparent opacity-0 group-hover:opacity-100 transition-opacity duration-200" />
      </div>
      <div className="p-2 text-sm truncate" title={item.title}>{item.title}</div>
    </div>
  );
}

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
    <div className="bg-card rounded-lg overflow-hidden hover:ring-2 hover:ring-brand transition">
      <div className="aspect-[2/3] bg-ink relative">
        {showImg ? (
          <img
            src={item.poster_url}
            alt={item.title}
            className="w-full h-full object-cover"
            loading="lazy"
            onError={() => setImgFailed(true)}
          />
        ) : (
          <div className="w-full h-full flex items-center justify-center text-3xl font-bold text-gray-600">
            {item.title.slice(0, 1)}
          </div>
        )}
        {item.year > 0 && (
          <span className="absolute bottom-1 right-1 text-[10px] bg-black/70 px-1 rounded">{item.year}</span>
        )}
      </div>
      <div className="p-2 text-sm truncate" title={item.title}>{item.title}</div>
    </div>
  );
}

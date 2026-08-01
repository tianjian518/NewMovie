import type { MediaItem } from "../types";

// 海报墙卡片。无海报图时用标题首字占位（深色紧凑，Emby 风）。
export default function PosterCard({ item }: { item: MediaItem }) {
  return (
    <div className="bg-card rounded-lg overflow-hidden hover:ring-2 hover:ring-brand transition">
      <div className="aspect-[2/3] bg-ink relative">
        {item.poster_url ? (
          <img src={item.poster_url} alt={item.title} className="w-full h-full object-cover" loading="lazy" />
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

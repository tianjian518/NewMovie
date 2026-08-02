import { Link } from "react-router-dom";
import type { MediaItem } from "../types";
import PosterCard from "./PosterCard";

// 横向滚动的海报行（网易爆米花 / 飞牛影视风格）：单行、可左右滑，不占满整屏。
// 用固定宽度 + shrink-0 保证每张海报不被压缩；snap 让滑动有吸附感。
export default function PosterRow({ items, empty }: { items: MediaItem[]; empty?: string }) {
  if (items.length === 0) {
    return <p className="text-gray-500 text-sm">{empty || "暂无内容"}</p>;
  }
  return (
    <div className="flex gap-3 overflow-x-auto pb-2 snap-x">
      {items.map((it) => (
        <Link key={it.id} to={"/item/" + it.id} className="shrink-0 w-32 sm:w-36 snap-start">
          <PosterCard item={it} />
        </Link>
      ))}
    </div>
  );
}

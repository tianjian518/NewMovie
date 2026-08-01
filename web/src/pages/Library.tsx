import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import type { Library } from "../types";

// 媒体库列表页：显示所有库，点击进入海报墙。
export default function Library() {
  const [libs, setLibs] = useState<Library[]>([]);
  useEffect(() => { api.listLibraries().then(setLibs).catch(() => {}); }, []);
  return (
    <div>
      <h2 className="text-xl font-bold mb-4">媒体库</h2>
      <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
        {libs.map((l) => (
          <Link key={l.id} to={"/library/" + l.id} className="bg-card rounded-lg p-5 hover:ring-2 hover:ring-brand">
            <div className="text-lg font-semibold">{l.name}</div>
            <div className="text-xs text-gray-400 mt-2">{modeLabel(l.mode)} · {l.root_path}</div>
          </Link>
        ))}
        {libs.length === 0 && (
          <p className="text-gray-400 text-sm">还没有媒体库，去「设置」绑定 OpenList 并创建。</p>
        )}
      </div>
    </div>
  );
}

function modeLabel(m: string) {
  return { native: "原生模式", strm: "STRM 模式", mixed: "混合模式" }[m] || m;
}

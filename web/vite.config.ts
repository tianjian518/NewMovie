import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// 构建产物输出到 cmd/server/dist，供 Go 二进制 embed。
export default defineConfig({
  plugins: [react()],
  base: "/",
  build: {
    outDir: "../cmd/server/dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // 开发期把 /api 代理到本地 Go 后端（默认 :8096）
      "/api": "http://127.0.0.1:8096",
    },
  },
});

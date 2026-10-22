import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist",
    chunkSizeWarningLimit: 800,
  },
  server: {
    host: "127.0.0.1", // 显式绑定 IPv4，避免 localhost 解析为 ::1 导致 127.0.0.1 连不上
    port: 5174,
    strictPort: true, // 端口被占时直接失败，避免悄悄换端口导致 pimg.ps1 等待超时
    proxy: {
      // 必须用对象形式并显式 changeOrigin:false——字符串简写会被 vite 强制
      // changeOrigin:true，把 Host 改写成 8790，而后端 CSRF 校验要求
      // Origin(5174) 与 Host 一致，导致所有 Cookie 会话写请求被 403 拒绝
      "/api": { target: "http://127.0.0.1:8790", changeOrigin: false },
      "/media": { target: "http://127.0.0.1:8790", changeOrigin: false },
    },
  },
});

import path from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(import.meta.dirname, "./src"),
    },
  },
  server: {
    proxy: {
      // 开发模式把 API 代理到 Go 后端:go run ./cmd/betterocr -serve 127.0.0.1:8787
      "/api": "http://127.0.0.1:8787",
    },
  },
})

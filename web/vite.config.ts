import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Vite 配置：
// - dev：proxy /api → 网关 :8080（前端 5173 跨域问题就地解决）
// - build：产物输出到 ../web_dist，由 Go 的 go:embed 内嵌为单二进制
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      // 开发期把 /api 请求转发给本地网关，浏览器无跨域
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
      // 调试台直接打网关统一入口（/v1/chat/completions）
      '/v1': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: '../web_dist',
    emptyOutDir: true,
  },
})

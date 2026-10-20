import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

// 构建产物直接输出到 Go 的 embed 目录，go build 时自动打进二进制
export default defineConfig({
  plugins: [vue(), tailwindcss()],
  base: '/',
  build: {
    outDir: '../internal/web/static',
    emptyOutDir: true,
    assetsDir: 'assets',
  },
  server: {
    // 本地开发：npm run dev，接口转发到正在跑的 Go 服务
    proxy: {
      '/api': 'http://127.0.0.1:5200',
      '/play': 'http://127.0.0.1:5200',
    },
  },
})

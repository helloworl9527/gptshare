import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
	base: '/admin/',
  plugins: [vue()],
	build: {
		outDir: '../internal/unifiedui/static',
		emptyOutDir: true,
	},
  server: {
    host: '127.0.0.1',
    proxy: {
      '/api': { target: process.env.VITE_API_TARGET || 'https://127.0.0.1:8080', changeOrigin: false, secure: false },
		'/health': { target: process.env.VITE_API_TARGET || 'https://127.0.0.1:8080', changeOrigin: false, secure: false },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.js'],
    include: ['src/**/*.test.js'],
    css: true,
  },
})

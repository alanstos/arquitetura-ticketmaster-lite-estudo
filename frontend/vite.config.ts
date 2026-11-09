import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// O frontend chama sempre /api/...; o Vite repassa para o backend Go.
// No docker compose o alvo é http://backend:8080 (via API_PROXY_TARGET).
export default defineConfig({
  plugins: [react()],
  server: {
    host: '0.0.0.0',
    port: 5173,
    watch: { usePolling: true },
    proxy: {
      '/api': process.env.API_PROXY_TARGET ?? 'http://localhost:8080',
    },
  },
})

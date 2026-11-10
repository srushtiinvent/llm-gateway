import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// In dev, /v1 and /admin are forwarded to the Go gateway, so there are no CORS problems.
// Change the port here if your gateway does not run on :8080.
const GATEWAY = process.env.GATEWAY_URL || 'http://localhost:8080';

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/v1': GATEWAY,
      '/admin': GATEWAY,
      '/healthz': GATEWAY,
    },
  },
});

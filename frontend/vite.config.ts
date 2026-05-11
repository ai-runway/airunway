import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'

// Inject a strict Content-Security-Policy meta tag into index.html for
// production builds only. In `vite dev`, React refresh injects inline scripts
// and HMR uses ws:// connections, both of which a strict CSP would block.
// Keeping the CSP build-only lets dev work normally while production gets the
// strict policy. connect-src is intentionally permissive (http:/https:) so
// deployments that point VITE_API_URL at a different origin still work; tighten
// further per-deployment via reverse-proxy headers if needed.
const PROD_CSP = [
  "default-src 'self'",
  "script-src 'self'",
  "style-src 'self' 'unsafe-inline' https://api.fontshare.com https://fonts.googleapis.com",
  "font-src 'self' https://api.fontshare.com https://cdn.fontshare.com https://fonts.gstatic.com",
  "img-src 'self' data: https:",
  "connect-src 'self' http: https:",
  "frame-src 'none'",
  "object-src 'none'",
  "base-uri 'self'",
].join('; ')

function cspPlugin(): Plugin {
  return {
    name: 'inject-csp-meta',
    apply: 'build',
    transformIndexHtml() {
      return [
        {
          tag: 'meta',
          attrs: {
            'http-equiv': 'Content-Security-Policy',
            content: PROD_CSP,
          },
          injectTo: 'head-prepend',
        },
      ]
    },
  }
}

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [react(), cspPlugin()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:3001',
        changeOrigin: true,
        // Increase timeout for long-running operations like Helm installs (10 minutes)
        timeout: 600000,
        configure: (proxy) => {
          // Also set proxyTimeout for the underlying http-proxy
          proxy.on('proxyReq', (_proxyReq, _req, res) => {
            // Set socket timeout on the response
            res.setTimeout(600000);
          });
        },
      },
    },
  },
})

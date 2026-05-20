import { defineConfig, loadEnv, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'

function getApiOrigin(apiUrl: string | undefined): string | null {
  const trimmed = apiUrl?.trim()
  if (!trimmed) return null

  // Relative API paths resolve against the frontend origin and are covered by 'self'.
  if (trimmed.startsWith('/')) return null

  try {
    const url = new URL(trimmed)
    if (url.protocol !== 'http:' && url.protocol !== 'https:') {
      console.warn(`Ignoring unsupported VITE_API_URL protocol for CSP: ${url.protocol}`)
      return null
    }
    return url.origin
  } catch {
    console.warn('Ignoring invalid VITE_API_URL for CSP connect-src')
    return null
  }
}

function buildProdCsp(apiUrl: string | undefined): string {
  const connectSrc = ["'self'"]
  const apiOrigin = getApiOrigin(apiUrl)
  if (apiOrigin) {
    connectSrc.push(apiOrigin)
  }

  return [
    "default-src 'self'",
    "script-src 'self'",
    "style-src 'self' 'unsafe-inline' https://api.fontshare.com https://fonts.googleapis.com",
    "font-src 'self' https://api.fontshare.com https://cdn.fontshare.com https://fonts.gstatic.com",
    "img-src 'self' data: https:",
    `connect-src ${connectSrc.join(' ')}`,
    "frame-src 'none'",
    "object-src 'none'",
    "base-uri 'self'",
  ].join('; ')
}

// Inject a strict Content-Security-Policy meta tag into index.html for
// production builds only. In `vite dev`, React refresh injects inline scripts
// and HMR uses ws:// connections, both of which a strict CSP would block.
// Keeping the CSP build-only lets dev work normally while production gets the
// strict policy. When VITE_API_URL points at a different origin, that exact
// origin is added to connect-src; otherwise API calls stay same-origin.
function cspPlugin(apiUrl: string | undefined): Plugin {
  const prodCsp = buildProdCsp(apiUrl)

  return {
    name: 'inject-csp-meta',
    apply: 'build',
    transformIndexHtml() {
      return [
        {
          tag: 'meta',
          attrs: {
            'http-equiv': 'Content-Security-Policy',
            content: prodCsp,
          },
          injectTo: 'head-prepend',
        },
      ]
    },
  }
}

// https://vitejs.dev/config/
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, __dirname, '')

  return {
    plugins: [react(), cspPlugin(env.VITE_API_URL)],
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
  }
})

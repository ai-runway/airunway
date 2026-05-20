import React from 'react'
import ReactDOM from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import App from './App'
import './index.css'

// Playwright e2e tests set this flag via page.addInitScript() to disable
// React Query retries and stale time, so error/404 states appear immediately.
const isE2E = typeof window !== 'undefined' && (window as any).__E2E_TEST__ === true

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: isE2E ? 0 : 5000,
      retry: isE2E ? false : 3,
    },
  },
})

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  </React.StrictMode>,
)

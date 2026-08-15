import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App.tsx'
import { registerServiceWorker } from './lib/sw'
import './styles/app.css'

const rootEl = document.getElementById('root')
if (!rootEl) throw new Error('#root 容器缺失，无法挂载 Reader 应用')

createRoot(rootEl).render(
  <StrictMode>
    <App />
  </StrictMode>,
)

// Service Worker only owns the public application shell. Private cache hydration
// is started by App after the authoritative identity gate succeeds.
if (import.meta.env.PROD) {
  registerServiceWorker(() => {
    window.dispatchEvent(new Event('webtag:sw-update-ready'))
  })
}

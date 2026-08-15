/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { VitePWA } from 'vite-plugin-pwa'
import { normalizeReaderBasePath, readerCacheIDForBase } from './src/lib/sw-ownership'

// Reader 前端构建配置。
// - dev proxy：把 /api 转发到本地后端，开发期规避 CORS（生产由用户配置后端 CORS_ORIGINS 放行 reader 地址）。
//   后端地址由 VITE_DEV_API_TARGET 覆盖，默认 http://localhost:8000。
// - test：jsdom 环境 + globals + setup（jest-dom matchers）。
const DEV_API_TARGET = process.env.VITE_DEV_API_TARGET ?? 'http://localhost:8000'
const READER_BASE = normalizeReaderBasePath(process.env.VITE_BASE ?? '/reader/')

export default defineConfig({
  // Go 后端把构建产物挂在 /reader/ 下（internal/app/reader.go），资源引用必须
  // 带这个前缀，否则 index.html 里的 /assets/*.js 会打到后端根路径上 404。
  // 独立部署（自己用 nginx serve）时同样把站点挂到 /reader/ 即可；要挂到根路径
  // 就用 VITE_BASE=/ 覆盖。
  base: READER_BASE,
  plugins: [
    react(),
    // PF10：Service Worker **只预缓存应用外壳**，不碰任何 API。
    //
    // API 的 stale-while-revalidate 已经由 lib/cache 的 store + IndexedDB
    // 完整实现（PF3/PF4）。让 SW 再缓存一遍会产生两套语义不同、失效时机不同
    // 的缓存，调试成本远超收益。这里的唯一职责是「离线/弱网下应用外壳仍能
    // 加载」。
    VitePWA({
      registerType: 'prompt',
      // 关掉自动注入的 registerSW.js：我们在 main.tsx 里手动注册，以便把
      // 「新版本就绪」接到界面上的提示按钮而不是静默刷新。两边都注册的话
      // 同一个 SW 会被注册两次，更新事件也会重复触发。
      injectRegister: false,
      // 只收外壳资源。navigateFallback 让 SPA 深链在离线时也能进来，
      // 但 /api/ 必须排除——否则一个失败的 API 请求会被回以 index.html。
      workbox: {
        // Cache Storage is origin-wide. Include the deployment base so two
        // Readers on one origin have disjoint shell-cache ownership.
        cacheId: readerCacheIDForBase(READER_BASE),
        globPatterns: ['**/*.{js,css,html,woff2}'],
        navigateFallback: 'index.html',
        navigateFallbackDenylist: [/^\/api\//, /^\/reader\/api\//],
        // 不给 API 配任何 runtimeCaching：见上方说明。
        runtimeCaching: [],
        cleanupOutdatedCaches: true,
      },
      manifest: false,
      devOptions: { enabled: false },
    }),
  ],
  build: {
    rollupOptions: {
      output: {
        // 把第三方依赖按用途切成独立 chunk：markdown 渲染栈（react-markdown +
        // remark/micromark/mdast/hast/unist 一大坨，体积主力）单独成块，与 React
        // 运行时分开。收益：① 这些库变动频率极低，独立 chunk 让它们跨部署长期命中
        // 浏览器缓存（主应用改动不再使其失效）；② 与主 chunk 并行下载。
        // PF9 起 markdown chunk 已由 LazyMarkdownView 用 React.lazy 移出首屏：
        // index.html 的初始加载里不再有它（加载期间用 PlainTextView 降级渲染
        // 同一段文本，用户看到的是内容而不是骨架屏）。
        manualChunks(id) {
          if (!id.includes('node_modules')) return
          if (
            /[\\/]node_modules[\\/](react-markdown|remark-gfm|remark|mdast|micromark|hast|unist|vfile|property-information|space-separated-tokens|comma-separated-tokens|decode-named-character-reference|character-entities|trim-lines|markdown-table|ccount|devlop|hastscript|web-namespaces|zwitch|longest-streak|mdurl|html-void-elements|bail|trough|unified|is-plain-obj|estree-util|stringify-entities)[\\/]/.test(
              id,
            )
          ) {
            return 'markdown'
          }
          if (/[\\/]node_modules[\\/](react|react-dom|scheduler)[\\/]/.test(id)) {
            return 'react-vendor'
          }
        },
      },
    },
  },
  server: {
    proxy: {
      '/api': {
        target: DEV_API_TARGET,
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: './src/test/setup.ts',
    // Browser specs run under Playwright and use its own test lifecycle. Keep
    // Vitest discovery inside src/ so e2e/*.spec.ts is never loaded by both runners.
    include: ['src/**/*.{test,spec}.{ts,tsx}'],
    css: false,
    // PF9 把 markdown 渲染改成懒加载边界之后，多条用例要等一次动态 import
    // 解析。空载时几十毫秒就够，但 CI 上与构建并发时会顶到 waitFor 的默认
    // 1000ms 上限 —— 表现为随机变红的一批 MainView 用例。放宽超时不掩盖任何
    // 真实失败：真坏了仍然会红，只是不再因为机器忙而误报。
    testTimeout: 15000,
    hookTimeout: 15000,
    // vi.spyOn 装的 spy 不归 vi.unstubAllGlobals 管，用例自己不还原就会漏到
    // 同文件后续用例里。审查用探针实测到过：sw.test.ts 里对
    // document.readyState 的 spy 一直挂着，后面几条用例全在 'loading' 下跑。
    // 今天不失败纯属它们没读这个值——把还原交给框架，不靠每个作者记得写。
    restoreMocks: true,
  },
})

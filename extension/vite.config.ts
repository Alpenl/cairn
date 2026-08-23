/// <reference types="vitest" />

import { basename, dirname, resolve } from 'node:path'
import { defineConfig, type UserConfig, type PluginOption } from 'vite'
import Vue from '@vitejs/plugin-vue'
import VueI18nPlugin from '@intlify/unplugin-vue-i18n/vite'
import Icons from 'unplugin-icons/vite'
import postcssPresetEnv from 'postcss-preset-env'
import { isDev, port, r, BROWSER_DIR } from './scripts/utils'
import packageJson from './package.json'
import {
  VIEW_ENTRIES,
  WEB_VIEWS,
  type WebView,
} from './scripts/build-target'

export const sharedConfig: UserConfig = {
  root: r('src'),
  cacheDir: r('node_modules/.vite'),
  resolve: {
    alias: {
      '@/': `${r('src')}/`,
    },
  },
  define: {
    __DEV__: isDev,
    __NAME__: JSON.stringify(packageJson.name),
  },
  css: {
    postcss: {
      plugins: [
        postcssPresetEnv({ stage: 3, features: { 'nesting-rules': true } }),
      ],
    },
  },
  plugins: [
    Vue(),
    VueI18nPlugin({
      include: resolve(__dirname, './src/locales/**'),
    }),
    // 本地组件目录已随上游新标签页一起删除；采集页面用到的 Naive UI
    // 组件均在 SFC 中显式 import。
    Icons(), // https://github.com/antfu/unplugin-icons

    {
      name: 'extension-build-entry',
      transformIndexHtml: {
        order: 'pre',
        handler(html, context) {
          const view = basename(dirname(context.filename)) as WebView
          const entry = VIEW_ENTRIES[view]
          if (!entry) return html

          return html.replace('./main.ts', `../${entry}`)
        },
      },
    },

    /**
     * Firefox 构建时需要注入 favicon link，否则标签页上没有图标
     * Chrome 构建时不注入，避免与 manifest.json 中的 icons 冲突导致Edge不展示图标的问题
     */
    {
      name: 'firefox-favicon-inject',
      transformIndexHtml(html) {
        if (process.env.BROWSER === 'firefox') {
          return html.replace(
            '</head>',
            '  <link rel="icon" href="/assets/img/icon/icon.svg" />\n</head>',
          )
        }
        return html
      },
    },
  ],
  optimizeDeps: {
    include: ['vue', 'naive-ui', 'vue-i18n'],
  },
}

export default defineConfig(({ command }) => ({
  ...sharedConfig,
  plugins: sharedConfig.plugins,
  base: command === 'serve' ? `http://localhost:${port}/` : '/dist/',
  server: {
    port,
    hmr: {
      host: 'localhost',
    },
  },
  build: {
    target: 'esnext',
    watch: isDev ? {} : undefined,
    outDir: r(`${BROWSER_DIR}/dist`),
    emptyOutDir: false, // 保留未由 Vite 处理的静态资源
    sourcemap: isDev ? 'inline' : false,
    minify: process.env.NO_MINIFY ? false : 'esbuild',
    chunkSizeWarningLimit: 1000,
    // https://developer.chrome.com/docs/webstore/program_policies/#:~:text=Code%20Readability%20Requirements
    // terserOptions: {
    //   mangle: false,
    // },
    rollupOptions: {
      input: {
        ...Object.fromEntries(
          WEB_VIEWS.map((view) => [view, r(`src/${view}/index.html`)]),
        ),
      },
      output: {
        entryFileNames(chunk) {
          const view = chunk.name as WebView
          const viewEntry = VIEW_ENTRIES[view]
          if (!viewEntry) return 'assets/[name]-[hash].js'
          // 页面 chunk 必须真的包含它声明的入口模块。历史上 popup/options 的
          // HTML 曾在改入口后仍指向旧 main.ts，产物照常构建、装上才发现是空壳。
          const expectedEntry = r(`src/${viewEntry}`)
          const hasExpectedEntry = chunk.moduleIds.some(
            (id) => id.split('?')[0] === expectedEntry,
          )
          if (!hasExpectedEntry) {
            throw new Error(
              `${view} chunk does not contain its capture entry: ${viewEntry}`,
            )
          }
          return 'assets/[name].capture-[hash].js'
        },
        manualChunks(id) {
          // 浏览器扩展从本地磁盘加载，无网络请求开销，chunk 不宜过多
          if (id.includes('node_modules')) {
            if (id.includes('naive-ui')) {
              return 'vendor-naive-ui'
            }
            // 其余第三方依赖统一归入一个 vendor chunk
            return 'vendor-libs'
          }

          // 业务代码统一归入 main chunk，避免碎片化
          if (['~icon'].some((pkg) => id.includes(pkg))) {
            return 'main'
          }
        },
      },
    },
  },
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: ['../test/setup.ts'],
    coverage: {
      reportsDirectory: '../coverage',
    },
  },
}))

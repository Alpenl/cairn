import { defineConfig } from 'vite'
import { sharedConfig } from './vite.config'
import { isDev, r, BROWSER_DIR } from './scripts/utils'
import packageJson from './package.json'
import { buildProfile } from './scripts/build-profile'

// bundling the content script using Vite
export default defineConfig({
  ...sharedConfig,
  define: {
    __DEV__: isDev,
    __NAME__: JSON.stringify(packageJson.name),
    // https://github.com/vitejs/vite/issues/9320
    // https://github.com/vitejs/vite/issues/9186
    'process.env.NODE_ENV': JSON.stringify(
      isDev ? 'development' : 'production',
    ),
  },
  build: {
    watch: isDev ? {} : undefined,
    outDir: r(`${BROWSER_DIR}/dist/background`),
    cssCodeSplit: false,
    emptyOutDir: false,
    sourcemap: isDev ? 'inline' : false,
    minify: process.env.NO_MINIFY ? false : 'esbuild',
    lib: {
      entry: r(buildProfile.backgroundEntry),
      formats: ['es'],
    },
    rollupOptions: {
      output: {
        entryFileNames: 'index.mjs',
      },
    },
  },
})

import { afterEach, describe, expect, it } from 'vitest'
import { copyFile, mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, resolve } from 'node:path'
import { verifyBuildOutput } from '../scripts/verify-build-output'
import { EXTENSION_ROOT, LEGAL_FILES } from '../scripts/release-artifacts'

const roots: string[] = []

async function write(root: string, path: string, contents = '') {
  const target = resolve(root, path)
  await mkdir(dirname(target), { recursive: true })
  await writeFile(target, contents)
}

async function createCaptureOutput() {
  const root = await mkdtemp(resolve(tmpdir(), 'webtag-capture-output-'))
  roots.push(root)
  const manifest = {
    default_locale: 'zh_CN',
    action: {
      default_icon: '/assets/img/icon/icon.png',
      default_popup: '/dist/popup/index.html',
    },
    icons: { 16: '/assets/img/icon/icon-16x16.png' },
    options_ui: { page: '/dist/options/index.html' },
    background: { service_worker: '/dist/background/index.mjs' },
    content_scripts: [
      {
        matches: ['<all_urls>'],
        js: ['/dist/contentScripts/rss.global.js'],
      },
    ],
  }

  await write(root, 'manifest.json', JSON.stringify(manifest))
  await write(root, '_locales/zh_CN/messages.json', '{}')
  await write(root, 'assets/img/icon/icon.png')
  await write(root, 'assets/img/icon/icon-16x16.png')
  await write(
    root,
    'dist/popup/index.html',
    '<title>WebTag</title><script type="module" src="/dist/assets/popup.capture-test.js"></script>',
  )
  await write(
    root,
    'dist/options/index.html',
    '<title>WebTag Settings</title><script type="module" src="/dist/assets/options.capture-test.js"></script>',
  )
  await write(root, 'dist/background/index.mjs', 'const target="extension"')
  await write(root, 'dist/contentScripts/rss.global.js', 'const rss=true')
  await write(root, 'dist/assets/popup.capture-test.js', 'const capture=true')
  await write(root, 'dist/assets/options.capture-test.js', 'const capture=true')
  for (const name of LEGAL_FILES) {
    await copyFile(resolve(EXTENSION_ROOT, name), resolve(root, name))
  }
  return root
}

afterEach(async () => {
  await Promise.all(
    roots.splice(0).map((root) => rm(root, { recursive: true })),
  )
})

describe('verifyBuildOutput', () => {
  it('接受只包含 capture 入口、图标资产且 manifest 引用完整的产物', async () => {
    const root = await createCaptureOutput()

    const report = await verifyBuildOutput(root)

    expect(report.target).toBe('extension')
    expect(report.largestOwnChunk).not.toBeNull()
  })

  it('拒绝 capture 产物中的 legacy content script', async () => {
    const root = await createCaptureOutput()
    await write(root, 'dist/contentScripts/index.global.js', 'legacy')

    await expect(verifyBuildOutput(root)).rejects.toThrow(
      'Capture output includes legacy entries',
    )
  })

  it('拒绝 manifest 指向不存在的本地文件', async () => {
    const root = await createCaptureOutput()
    await rm(resolve(root, 'dist/popup/index.html'))

    await expect(verifyBuildOutput(root)).rejects.toThrow(
      'Manifest references missing files: dist/popup/index.html',
    )
  })

  it('拒绝 HTML 仍指向 legacy main entry 的 capture 产物', async () => {
    const root = await createCaptureOutput()
    await write(
      root,
      'dist/popup/index.html',
      '<script type="module" src="/dist/assets/popup.js"></script>',
    )
    await write(root, 'dist/assets/popup.js', 'const legacy=true')

    await expect(verifyBuildOutput(root)).rejects.toThrow(
      'Capture popup HTML points to a non-capture entry',
    )
  })

  it('拒绝缺少根 LICENSE 的构建产物', async () => {
    const root = await createCaptureOutput()
    await rm(resolve(root, 'LICENSE'))

    await expect(verifyBuildOutput(root)).rejects.toThrow(
      'Build output is missing root legal file: LICENSE',
    )
  })

  it('拒绝篡改根 NOTICE 的构建产物', async () => {
    const root = await createCaptureOutput()
    await write(root, 'NOTICE', 'tampered')

    await expect(verifyBuildOutput(root)).rejects.toThrow(
      'Build output legal file does not match authoritative extension/NOTICE',
    )
  })

  it('拒绝用 workspace 根 MIT LICENSE 替代 Extension GPL LICENSE', async () => {
    const root = await createCaptureOutput()
    await copyFile(
      resolve(EXTENSION_ROOT, '../LICENSE'),
      resolve(root, 'LICENSE'),
    )

    await expect(verifyBuildOutput(root)).rejects.toThrow(
      'Build output legal file does not match authoritative extension/LICENSE',
    )
  })
})

/// <reference types="vite/client" />

import { createRoot } from 'react-dom/client'

import { MarkdownView } from '../src/components/MarkdownView'
import { sanitizeFeedHTML } from '../src/lib/feed-html'
import { resetApplicationCache } from '../src/lib/sw'
import '../src/styles/app.css'

declare global {
  interface Window {
    readerSecurityHarness: {
      resetApplicationCache(): Promise<void>
    }
  }
}

const root = document.getElementById('root')
if (!root) throw new Error('reader security harness root is missing')

const params = new URLSearchParams(window.location.search)
const probe = params.get('probe') ?? '/__test__/image-probe'
const cspLeak = params.get('cspLeak')
const port = window.location.port
const mediaURLs = [
  `${probe}?kind=relative`,
  `http://127.0.0.1:${port}${probe}?kind=loopback`,
  `http://127.1:${port}${probe}?kind=short-loopback`,
  `http://2130706433:${port}${probe}?kind=integer-loopback`,
  'http://10.0.0.1/__test__/image-probe?kind=private',
  'http://169.254.169.254/__test__/image-probe?kind=link-local',
  'http://192.0.2.1/__test__/image-probe?kind=reserved',
  'https://tracker.invalid/__test__/image-probe?kind=public',
  'data:image/png;base64,AA==',
]
const feedHTML = sanitizeFeedHTML(
  [
    '<p>Feed body</p>',
    `<img alt="Feed loopback" src="${mediaURLs[1]}">`,
    `<img alt="Feed srcset" src="${mediaURLs[0]}" srcset="${mediaURLs[4]} 1x, ${mediaURLs[7]} 2x">`,
    `<picture><source srcset="${mediaURLs[5]} 2x"><img alt="Feed picture" src="${mediaURLs[6]}"></picture>`,
    `<video poster="${mediaURLs[7]}"><source src="${mediaURLs[4]}"></video>`,
    `<svg><image href="${mediaURLs[7]}"></image></svg>`,
    `<p style="background-image:url('${mediaURLs[7]}')">CSS body</p>`,
  ].join(''),
  window.location.href,
)

window.readerSecurityHarness = { resetApplicationCache }

createRoot(root).render(
  <main>
    <MarkdownView
      blockKey="content-document"
      text={['Saved body', ...mediaURLs.map((url, index) => `![Markdown ${index + 1}](${url})`)].join('\n\n')}
      anns={[]}
      onClickHL={() => undefined}
    />
    <section dangerouslySetInnerHTML={{ __html: feedHTML }} />
    {cspLeak && <img alt="CSP leak probe" src={cspLeak} />}
  </main>,
)

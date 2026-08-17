import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const css = readFileSync(resolve(process.cwd(), 'src/styles/app.css'), 'utf8')

function firstRule(selector: string): string {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const match = css.match(new RegExp(`${escaped}\\s*\\{([^}]*)\\}`))
  if (!match) throw new Error(`missing CSS rule for ${selector}`)
  return match[1]
}

function mediaBlocks(condition: string): string {
  const marker = `@media (${condition})`
  const blocks: string[] = []
  let cursor = 0
  while (cursor < css.length) {
    const start = css.indexOf(marker, cursor)
    if (start < 0) break
    const open = css.indexOf('{', start + marker.length)
    if (open < 0) break
    let depth = 1
    let end = open + 1
    while (end < css.length && depth > 0) {
      if (css[end] === '{') depth += 1
      if (css[end] === '}') depth -= 1
      end += 1
    }
    blocks.push(css.slice(open + 1, end - 1))
    cursor = end
  }
  return blocks.join('\n')
}

describe('Reader responsive content width', () => {
  it('uses one reading canvas without a second prose width cap', () => {
    const root = firstRule(':root')
    const scroll = firstRule('.reader-scroll')
    const linkReader = firstRule('.reader-inner')
    const feedReader = firstRule('.rss-reader-inner')
    const prose = firstRule('.reader-prose')

    expect(root).toMatch(/--reader-content-width:\s*800px\s*;/)
    expect(root).not.toMatch(/--reader-prose-width\s*:/)
    expect(root).toMatch(/--reader-gutter:\s*clamp\(17px,\s*3vw,\s*48px\)\s*;/)
    expect(scroll).toMatch(/padding-inline:\s*var\(--reader-gutter\)\s*;/)

    expect(linkReader).toMatch(/max-width:\s*var\(--reader-content-width\)\s*;/)
    expect(linkReader).toMatch(/padding:\s*44px\s+0\s+120px\s*;/)
    expect(feedReader).toMatch(/max-width:\s*var\(--reader-content-width\)\s*;/)
    expect(feedReader).toMatch(/padding:\s*42px\s+0\s+100px\s*;/)
    expect(prose).toMatch(/width:\s*100%\s*;/)
  })

  it('lets rendered prose and wide media use the full canvas', () => {
    const proseBlocks = firstRule(
      '.reader-flow > :where(p, h1, h2, h3, h4, h5, h6, ul, ol, blockquote, dl, address)',
    )
    const containerBlocks = firstRule(
      '.reader-flow > :where(div, section, article, header, footer)',
    )
    const wideBlocks = firstRule('.reader-flow > :where(pre, table, figure, video, iframe)')
    const localScroll = firstRule('.reader-flow :where(pre, table)')
    const images = firstRule('.reader-flow img')
    const expandedImages = firstRule('.reader-flow img.reader-image-expand')
    const centeredRenderedProse = firstRule(
      '.reader-flow:is(.md, .rss-feed-content) > :where(p, h1, h2, h3, h4, h5, h6, ul, ol, blockquote, dl, address)',
    )

    expect(proseBlocks).toMatch(/width:\s*100%\s*;/)
    expect(containerBlocks).toMatch(/width:\s*100%\s*;/)
    expect(centeredRenderedProse).toMatch(/margin-inline:\s*0\s*;/)
    expect(wideBlocks).toMatch(/max-width:\s*100%\s*;/)
    expect(localScroll).toMatch(/max-width:\s*100%\s*;/)
    expect(localScroll).toMatch(/overflow-x:\s*auto\s*;/)
    expect(images).toMatch(/width:\s*auto\s*;/)
    expect(images).toMatch(/max-width:\s*100%\s*;/)
    expect(images).toMatch(/height:\s*auto\s*;/)
    expect(expandedImages).toMatch(/width:\s*100%\s*;/)
  })

  it('lets supporting panes shrink before compact mode', () => {
    const root = firstRule(':root')

    expect(root).toMatch(/--sidebar-width:\s*clamp\(196px,\s*10\.5vw,\s*212px\)\s*;/)
    expect(root).toMatch(/--reader-content-half-width:\s*400px\s*;/)
    expect(root).toMatch(/--reading-gap-min:\s*26px\s*;/)
    expect(root).toMatch(/--reader-rail-width:\s*288px\s*;/)
    expect(root).toMatch(/--list-pane-width:\s*clamp\(288px,\s*calc\(50vw\s*-/)
    expect(root).toMatch(/--chrome-width:\s*calc\(var\(--sidebar-effective-width\)\s*\+\s*var\(--list-pane-width\)\)\s*;/)
    expect(root).toMatch(/--feed-list-pane-width:\s*clamp\(340px,\s*25\.5vw,\s*380px\)\s*;/)
    expect(firstRule('.sidebar')).toMatch(/width:\s*var\(--sidebar-width\)\s*;/)
    expect(firstRule('.list-pane')).toMatch(/width:\s*var\(--list-pane-width\)\s*;/)
    expect(firstRule('.rss-list-pane')).toMatch(/width:\s*var\(--feed-list-pane-width\)\s*;/)
  })

  it('uses a responsive reading rail without removing the RSS ReaderToc', () => {
    const pane = firstRule('.reader-pane')
    const cardTitle = firstRule('.card h3')

    expect(pane).toMatch(/container:\s*readerpane\s*\/\s*inline-size\s*;/)
    expect(cardTitle).toMatch(/display:\s*-webkit-box\s*;/)
    expect(cardTitle).toMatch(/-webkit-line-clamp:\s*2\s*;/)
    expect(cardTitle).toMatch(/-webkit-box-orient:\s*vertical\s*;/)
    expect(cardTitle).toMatch(/overflow:\s*hidden\s*;/)

    expect(css).toMatch(
      /@container\s+readerpane\s*\(min-width:\s*1240px\)[\s\S]*?\.reader-rail\s*\{[^}]*display:\s*block\s*;/,
    )
    expect(css).toMatch(
      /@container\s+readerpane\s*\(max-width:\s*1559px\)[\s\S]*?\.body\.focus-mode[^}]*\.reader-rail\s*\{\s*display:\s*none\s*;/,
    )
    expect(css).toMatch(
      /@container\s+readerpane\s*\(min-width:\s*1560px\)[\s\S]*?\.body\.focus-mode[^}]*\.reader-rail\s*\{\s*display:\s*block\s*;/,
    )
    expect(css).toMatch(
      /@media\s*\(min-width:\s*1820px\)[\s\S]*?50vw\s*-\s*var\(--chrome-width\)\s*-\s*var\(--reader-content-half-width\)/,
    )

    expect(css).not.toMatch(/\.st-badge/)
    expect(css).not.toMatch(/\.card-signals/)
    expect(css).not.toMatch(/\.sig(?:\W|$)/)
    expect(css).not.toMatch(/\.card-err/)
    expect(css).not.toMatch(/\.mini-tag\.lowconf/)
    expect(css).not.toMatch(/\.inline-state\.warn/)
    expect(css).not.toMatch(/\.is-sub/)
    expect(css).toMatch(/\.reader-toc\s*\{/) // RSS workspace compatibility
  })

  it('keeps the wide article and reading rail in the first grid row', () => {
    const article = firstRule('.reader-pane:not(.rss-detail-pane) .reader-inner')

    expect(article).toMatch(/grid-column:\s*2\s*;/)
    expect(article).toMatch(/grid-row:\s*1\s*;/)
    expect(css).toMatch(
      /@container\s+readerpane\s*\(min-width:\s*1240px\)[\s\S]*?\.reader-rail\s*\{[^}]*grid-column:\s*4\s*;[^}]*grid-row:\s*1\s*;/,
    )
  })

  it('gives the RSS reader the shared reading controls and focus canvas', () => {
    expect(css).toMatch(
      /\.rss-detail-pane \.rss-feed-summary,[\s\S]*?\.rss-detail-pane \.rss-article-footer\s*\{[\s\S]*?--reading-font-size[\s\S]*?--reading-line-height/,
    )
    expect(css).toMatch(
      /\.rss-workspace:has\(\.feed-focus-mode\) > \.rss-sidebar,[\s\S]*?\.rss-workspace:has\(\.feed-focus-mode\) > \.rss-list-pane\s*\{\s*display:\s*none\s*;/,
    )
    expect(css).toMatch(
      /\.rss-detail-pane\.feed-focus-mode \.rss-reader-inner\s*\{[\s\S]*?max-width:\s*880px\s*;/,
    )
  })

  it('uses distinct compact-desktop and narrow single-pane breakpoints', () => {
    expect(css).toMatch(
      /@media\s*\(max-width:\s*1439px\)\s*\{[\s\S]*?\.mobile-nav-toggle\s*\{[^}]*display:\s*inline-flex\s*;/,
    )
    expect(css).toMatch(
      /@media\s*\(max-width:\s*1024px\)\s*\{[\s\S]*?\.mobile-back\s*\{[^}]*display:\s*inline-flex\s*;/,
    )
    expect(css).toMatch(
      /@media\s*\(max-width:\s*1024px\)\s*\{[\s\S]*?\.rss-workspace:not\(\.rss-mobile-detail\) \.rss-detail-pane\s*\{/,
    )
    expect(css).toMatch(
      /@media\s*\(max-width:\s*1024px\)\s*\{[\s\S]*?\.body\.mobile-detail-active > \.reader-pane\s*\{/,
    )
    expect(css).not.toMatch(/@media\s*\(max-width:\s*1280px\)/)
  })

  it('keeps the full titlebar through tablet widths and compacts only on phones', () => {
    expect(firstRule('.titlebar')).toMatch(/height:\s*52px\s*;/)
    expect(firstRule('.search-pill .ph')).toMatch(/white-space:\s*nowrap\s*;/)
    expect(firstRule('.search-pill .ph')).toMatch(/text-overflow:\s*ellipsis\s*;/)
    expect(mediaBlocks('max-width: 1439px')).not.toMatch(/\.titlebar\s*\{/)

    const phone = mediaBlocks('max-width: 639px')
    expect(phone).toMatch(/\.titlebar\s*\{[^}]*padding:\s*0 8px\s*;/)
    expect(phone).toMatch(/\.search-pill\s*\{[^}]*width:\s*32px\s*;/)
    expect(phone).not.toMatch(/height:\s*48px\s*;/)
  })

  it('keeps original-content controls on one row at 390px', () => {
    const phone = mediaBlocks('max-width: 520px')
    expect(phone).toMatch(/\.orig-content-actions\s*\{[^}]*flex-wrap:\s*nowrap\s*;/)
    expect(phone).not.toMatch(/\.orig-content-head\s*\{[^}]*flex-direction:\s*column\s*;/)

    expect(mediaBlocks('max-width: 359px')).toMatch(
      /\.orig-content-head\s*\{[^}]*flex-direction:\s*column\s*;/,
    )
  })

  it('reclaims auxiliary panes before a tool can squeeze or cover desktop reading', () => {
    expect(firstRule('.body.mobile-tool-open > .sidebar')).toMatch(/display:\s*none\s*;/)
    expect(css).toMatch(
      /@media\s*\(max-width:\s*1599px\)\s*\{[\s\S]*?\.body\.mobile-tool-open > \.list-pane\s*\{[^}]*display:\s*none\s*;/,
    )
    expect(css).toMatch(
      /@media\s*\(max-width:\s*1024px\)\s*\{[\s\S]*?\.body\.mobile-tool-open > \.reader-pane\s*\{[^}]*visibility:\s*hidden\s*;/,
    )
    expect(css).toMatch(
      /@media\s*\(max-width:\s*1024px\)\s*\{[\s\S]*?\.chat,\s*\.note-panel\s*\{[^}]*position:\s*absolute\s*;/,
    )
  })

  it('正文目录只借左侧留白，阅读区不够宽就不出现', () => {
    const scroll = firstRule('.reader-scroll')
    const toc = firstRule('.reader-toc')

    // 门槛按阅读区自身宽度算：800px 正文 + 两侧留白放得下 188px 目录才显示，
    // 专注模式正文更宽（880px），门槛相应抬高。
    expect(scroll).toMatch(/container-type:\s*inline-size\s*;/)
    expect(toc).toMatch(/display:\s*none\s*;/)
    expect(toc).toMatch(/position:\s*sticky\s*;/)
    expect(toc).toMatch(/height:\s*0\s*;/)
    expect(toc).toMatch(/width:\s*188px\s*;/)
    expect(css).toMatch(
      /@container\s*\(min-width:\s*1152px\)\s*\{\s*\.reader-toc\s*\{[^}]*display:\s*block\s*;/,
    )
    expect(css).toMatch(
      /@container\s*\(max-width:\s*1231px\)\s*\{\s*\.reader-toc\.focused\s*\{[^}]*display:\s*none\s*;/,
    )
  })

  it('keeps subscription management actions visible while only the source list scrolls', () => {
    const sidebar = firstRule('.sidebar.rss-sidebar')
    const folderList = firstRule('.rss-folder-list')
    const footer = firstRule('.rss-sidebar-footer')
    const batchMove = firstRule('.rss-batch-move .rss-menu-pop')

    expect(sidebar).toMatch(/overflow-y:\s*hidden\s*;/)
    expect(folderList).toMatch(/min-height:\s*0\s*;/)
    expect(folderList).toMatch(/overflow-y:\s*auto\s*;/)
    expect(footer).toMatch(/flex-shrink:\s*0\s*;/)
    expect(batchMove).toMatch(/right:\s*-62px\s*;/)
    expect(batchMove).toMatch(/bottom:\s*32px\s*;/)
  })
})

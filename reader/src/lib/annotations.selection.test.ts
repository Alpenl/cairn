import { beforeAll, describe, expect, it } from 'vitest'
import { getSelectionInfo } from './annotations'

// jsdom 的 Range 不实现 getBoundingClientRect（浏览器中可用）→ 测试中桩一个零矩形，
// 仅验证字符偏移测量逻辑，不验证视觉定位。
beforeAll(() => {
  if (!Range.prototype.getBoundingClientRect) {
    Range.prototype.getBoundingClientRect = () =>
      ({ left: 0, top: 0, width: 0, height: 0, right: 0, bottom: 0, x: 0, y: 0 }) as DOMRect
  }
})

function selectRange(node: Node, start: number, end: number): void {
  const r = document.createRange()
  r.setStart(node, start)
  r.setEnd(node, end)
  const sel = window.getSelection()!
  sel.removeAllRanges()
  sel.addRange(r)
}

describe('getSelectionInfo：选区 → 块内字符偏移', () => {
  it('单文本节点测量 start/end/text/blockKey', () => {
    document.body.innerHTML = '<div data-hl-block="summary">Hello world here</div>'
    const tn = document.querySelector('[data-hl-block]')!.firstChild!
    selectRange(tn, 6, 11) // "world"
    const info = getSelectionInfo(document.body)
    expect(info).not.toBeNull()
    expect(info!.blockKey).toBe('summary')
    expect(info!.start).toBe(6)
    expect(info!.end).toBe(11)
    expect(info!.text).toBe('world')
    expect(info!.quote).toEqual({
      exact: 'world',
      prefix: 'Hello ',
      suffix: ' here',
    })
  })

  it('选区起点在嵌套 <strong> 后，偏移按块内累计纯文本计算', () => {
    document.body.innerHTML = '<p data-hl-block="dr-1"><strong>AB</strong>cdef</p>'
    const block = document.querySelector('[data-hl-block]')!
    const cdef = block.childNodes[1] // "cdef" 文本节点
    selectRange(cdef, 1, 3) // "de"，块内偏移 = 2(AB) + 1 = 3
    const info = getSelectionInfo(document.body)
    expect(info!.start).toBe(3)
    expect(info!.end).toBe(5)
    expect(info!.text).toBe('de')
  })

  it('选区不足 2 字符 → null', () => {
    document.body.innerHTML = '<div data-hl-block="summary">abcd</div>'
    const tn = document.querySelector('[data-hl-block]')!.firstChild!
    selectRange(tn, 0, 1)
    expect(getSelectionInfo(document.body)).toBeNull()
  })

  it('选区块不在 scope 内 → null', () => {
    document.body.innerHTML =
      '<div id="scope"></div><div data-hl-block="summary">outside text</div>'
    const tn = document.querySelector('[data-hl-block]')!.firstChild!
    selectRange(tn, 0, 4)
    const scope = document.getElementById('scope') as HTMLElement
    expect(getSelectionInfo(scope)).toBeNull()
  })
})

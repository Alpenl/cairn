/**
 * 阅读时长公式的守卫。
 *
 * 这些用例存在的理由很具体：此前公式只在 DetailPane 渲染出来的「约 N 分钟」里被
 * 间接断言，而 `Math.ceil` 会把改动压平——两个除数各漂 25%、两个兜底参数对调、
 * 去掉 `Math.max(1, …)` 的下限，四种变异**一条测试都不红**。所以样本必须刻意
 * 挑成「非对称、且落在取整边界内侧」的。
 */
import { describe, expect, it } from 'vitest'

import { readingMinutes } from './reading-time'

describe('readingMinutes', () => {
  // 样本刻意取大。`Math.ceil` 会把小样本压平——800 汉字在 400 字/分下是 2 分钟，
  // 在 500 字/分下（1.6 → 2）**也是** 2 分钟，除数漂了却测不出来。取到 4000 字
  // 这一档，10 / 8 / 12 就分得开了。
  //
  // 两条 fallback 样本还刻意不对称（cjk=4000 → 10 而 words=4400 → 20），这样
  // 两个形参一旦对调就会同时红——相邻同类型位参对调是这个签名最容易挨的一刀。
  it.each([
    // [说明, text, fallbackCjk, fallbackWords, 期望分钟]
    ['空输入给下限 1 分钟，不给 0', '', 0, 0, 1],
    ['纯中文 4000 字 = 10 分钟（400 字/分）', '字'.repeat(4000), 0, 0, 10],
    ['纯西文 4400 词 = 20 分钟（220 词/分）', 'word '.repeat(4400), 0, 0, 20],
    ['中英混排两项相加后取整', '字'.repeat(4000) + ' ' + 'word '.repeat(4400), 0, 0, 30],
    ['text 为空时退回后端计数：4000 汉字 → 10', '', 4000, 0, 10],
    ['text 为空时退回后端计数：4400 西文词 → 20', '', 0, 4400, 20],
    ['text 非空则忽略后端计数', '字'.repeat(400), 99999, 99999, 1],
    ['1 个字也落在 ceil 的第一格里 → 1 分钟', '字', 0, 0, 1],
    ['余数向上取整：500 字 = 1.25 → 2 分钟（floor 会给 1）', '字'.repeat(500), 0, 0, 2],
  ])('%s', (_name, text, cjk, words, expected) => {
    expect(readingMinutes(text, cjk, words)).toBe(expected)
  })

  // 后端两项计数在 DTO 里是 omitempty，为 0 时字段缺失 → undefined，会落到默认形参。
  it('兜底参数缺省时按 0 处理，不产出 NaN', () => {
    expect(readingMinutes('')).toBe(1)
  })
})

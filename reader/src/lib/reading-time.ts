/**
 * 阅读时长估算。
 *
 * 单独成模块（而不是留在 DetailPane 里），一是因为导出函数会让组件文件丢掉
 * fast refresh，二是为了让公式本身能被直接断言——穿过组件渲染去测的话，
 * `Math.ceil` 会把「除数漂了」「两个兜底参数对调了」这类改动压成同一个数字，
 * 测试全绿而公式已经错了。
 */

/** 中文按每分钟 400 字，西文按每分钟 220 词。 */
const CJK_PER_MINUTE = 400
const WORDS_PER_MINUTE = 220

/**
 * 阅读时长（分钟）。**公式只有这一份**——折叠态与展开态、有正文与只有摘要，
 * 全都走它，否则同一篇文章会在展开前后给出两个数字。
 *
 * text 为空时退回后端数好的两项计数（保存原文时算好写进列，见 PF6），两者口径相同。
 */
export function readingMinutes(text: string, fallbackCjk = 0, fallbackWords = 0): number {
  const cjk = text ? (text.match(/[\u3400-\u9fff]/g) || []).length : fallbackCjk
  const words = text ? (text.match(/[A-Za-z0-9]+(?:['’-][A-Za-z0-9]+)*/g) || []).length : fallbackWords
  // 下限 1 分钟：内容极短时「约 0 分钟」读起来像出错了。
  return Math.max(1, Math.ceil(cjk / CJK_PER_MINUTE + words / WORDS_PER_MINUTE))
}

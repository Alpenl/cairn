/**
 * 缓存写入路径上的「数据到底变没变」判定。
 *
 * 它决定了后台校验回来之后要不要惊动 React。stale-while-revalidate 的收益
 * 一半在「先渲染旧的」，另一半就在这里：绝大多数校验拿回来的是和缓存里
 * 一模一样的数据，此时若照样 setState，整棵树白重渲染一次——而列表每 30 秒
 * 校验一次、订阅每 60 秒一次、翻译轮询 1.2 秒一次。
 *
 * 策略：容器逐键比，数组比长度 + **逐项逐字段**比，嵌套对象最多再下探一层。
 *
 * 早先的版本只比 id / updated_at 这类「身份字段」，图的是便宜。那是错的——
 * 后端并不保证每次内容变化都会动 updated_at（译文任务 pending → done 就是
 * 现成的反例），于是真实更新被判成"没变"，界面永远停在旧状态。缓存宁可
 * 多通知一次，也不能吞掉一次真实更新。
 */

import { isRecord } from '../records'

/**
 * 逐字段浅比较两个条目。
 *
 * 曾经这里只比 id / updated_at 这类「身份字段」，理由是便宜。那是错的：
 * 后端并不保证每次内容变化都会动 updated_at（译文任务从 pending 变成 done
 * 就是现成的反例），于是真实更新被判成"没变"、界面永远停在旧状态——正是
 * 缓存最不能犯的那种错。
 *
 * 逐字段比的代价其实很小：列表项是扁平结构（十几个标量），30 条也就几百次
 * 比较，远低于一次 React 重渲染。长文本字段（译文正文）的字符串比较在 V8 里
 * 先比长度再 memcmp，同样远快于重渲染 + 重新 parse。
 */
function sameItem(left: unknown, right: unknown): boolean {
  if (left === right) return true
  if (!isRecord(left) || !isRecord(right)) return false

  const leftKeys = Object.keys(left)
  if (leftKeys.length !== Object.keys(right).length) return false

  for (const key of leftKeys) {
    if (!Object.prototype.hasOwnProperty.call(right, key)) return false
    const a = left[key]
    const b = right[key]
    if (a === b) continue
    if (Array.isArray(a) && Array.isArray(b)) {
      // 标签这类标量数组：逐项严格相等。
      if (a.length !== b.length) return false
      if (a.some((value, index) => value !== b[index])) return false
      continue
    }
    // 嵌套对象再往下只比一层，避免在正文这类字段上滑向无界递归。
    if (isRecord(a) && isRecord(b)) {
      if (!sameItem(a, b)) return false
      continue
    }
    return false
  }
  return true
}

function sameArray(left: unknown[], right: unknown[]): boolean {
  if (left.length !== right.length) return false
  for (let index = 0; index < left.length; index += 1) {
    if (!sameItem(left[index], right[index])) return false
  }
  return true
}

/**
 * 判断两份资源快照是否「对界面而言相同」。
 *
 * 相同 → 保留旧对象、不通知订阅者；不同 → 换新对象并触发重渲染。
 */
export function sameResource(left: unknown, right: unknown): boolean {
  if (left === right) return true
  if (left === null || right === null || left === undefined || right === undefined) {
    return false
  }

  if (Array.isArray(left) || Array.isArray(right)) {
    if (!Array.isArray(left) || !Array.isArray(right)) return false
    return sameArray(left, right)
  }

  if (isRecord(left) && isRecord(right)) {
    const leftKeys = Object.keys(left)
    const rightKeys = Object.keys(right)
    if (leftKeys.length !== rightKeys.length) return false
    for (const key of leftKeys) {
      if (!Object.prototype.hasOwnProperty.call(right, key)) return false
      const a = left[key]
      const b = right[key]
      if (a === b) continue
      if (Array.isArray(a) || Array.isArray(b)) {
        if (!Array.isArray(a) || !Array.isArray(b) || !sameArray(a, b)) return false
        continue
      }
      // 容器内的标量必须严格相等（total / page / limit / counts 这类）。
      // 嵌套对象只比一层：再深就滑向"深度递归"那一侧的代价。
      if (isRecord(a) && isRecord(b)) {
        if (!sameResource(a, b)) return false
        continue
      }
      return false
    }
    return true
  }

  return false
}

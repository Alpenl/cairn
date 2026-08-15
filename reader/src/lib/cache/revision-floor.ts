/**
 * 每条链接的 content_revision 下界，持久化到 localStorage。
 *
 * ## 为什么必须落盘
 *
 * 写正文的响应带回自增后的代次（`LinkContentResponse.content_revision`），
 * MainView 当场把它记进下界，`active` 汇合点据此抬升——列表投影再陈旧也拖不回去。
 * 但列表缓存本身是**持久化到 IndexedDB** 的（见 `cache/persist.ts`），而下界如果
 * 只活在 useRef 里，刷新一次就没了：
 *
 *   保存原文（服务端 7 → 8）
 *     → 列表缓存条目仍是第 7 代，且已落盘
 *     → 用户在 `useLinks` 那 30 秒静默刷新之前刷新页面
 *     → hydrate 出第 7 代，下界随内存一起蒸发
 *     → 划线 envelope（第 8 代）被判失配，正文划线从界面消失
 *     → 下一次任意写入以幸存者重建 envelope，localStorage 里那份也没了
 *
 * 终点是**不可恢复的用户数据丢失**，与它要修的那个 bug 是同一种。所以下界必须
 * 和划线 envelope 一样落盘——两者是同一件事的两半：一个记「正文到第几代了」，
 * 一个记「划线锚在第几代上」，只持久化其中一半，另一半就会在刷新后指向虚空。
 *
 * ## 为什么不是去调 invalidateLibrary
 *
 * 直觉的修法是「保存后顺手 `invalidateLibrary()`，让列表缓存立刻失效」。
 *
 * 这条路当初走不通：`TRANSLATIONS_CACHE_PREFIX` 曾是 `'GET /api/links/'`，落在
 * `LINKS_CACHE_PREFIX`（`'GET /api/links'`，无尾随分隔符）之下，于是
 * `invalidateLibrary()` 会连坐打掉全库译文，而失效之后没有任何东西会重取。那条
 * 连坐现已修掉（译文搬进独立命名空间，`useCachedResource` 也有了失效自愈）。
 *
 * **但下界仍然必需**，别以为连坐修好了就可以退回去：`invalidateLibrary()` 消不掉
 * 「列表请求在写操作**之前**发出、之后才落地」这个竞态——那份响应本来就带着旧
 * 代次，跟缓存有没有被失效无关。它能做的只是把陈旧窗口从 30 秒压到一个往返。
 * 保存原文要不要顺带 invalidateLibrary 是另一个独立的取舍（多一次全量重取），
 * 与这里要解决的丢划线无关。
 *
 * ## 单调性
 *
 * 代次由服务端单调自增（写正文 +1、阅读↔网站转换 +1、历史迁移置空 +1，全仓无
 * 递减路径，schema 里还有 `CHECK (content_revision > 0)`），且写进下界的值只来自
 * **已提交**的写响应（CAS 未命中走 409，不产出响应）。所以下界恒 ≤ 服务端真值，
 * 只升不降是安全的。
 */

import { ownedStorageKey, readOwnedStorage, writeOwnedStorage } from '../storage-ownership'

/** 当前 identity 的物理键，供消费方订阅 `storage` 事件。 */
export function revisionFloorStorageKey(): string | null {
  return ownedStorageKey('revisionFloor')
}

/**
 * 保留条目数上限。下界的作用期只是「本机写入 → 列表追上」这一小段，超出上限时
 * 丢最早写入的那批（JSON 对象对非数字键保持插入序，UUID 正是非数字键）。
 * 丢错了也只是退回没有下界的行为，不会让代次变高。
 */
const FLOOR_LIMIT = 200

/** 读取全量下界；缺失 / 损坏 / localStorage 不可用时回退空表（fail-safe）。 */
export function loadRevisionFloors(): Map<string, number> {
  try {
    const raw = readOwnedStorage('revisionFloor')
    if (!raw) return new Map()
    const parsed: unknown = JSON.parse(raw)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return new Map()
    const out = new Map<string, number>()
    for (const [id, value] of Object.entries(parsed as Record<string, unknown>)) {
      // 逐条过滤：损坏的一项不该拖垮整张表，更不该把 NaN / 负数 / 字符串
      // 塞进下界——那会让 active 的代次被抬到一个不存在的值上。
      if (typeof value === 'number' && Number.isInteger(value) && value > 0) {
        out.set(id, value)
      }
    }
    // 读侧也截断：今天写侧总会截断，所以盘上不会超限；但 FLOOR_LIMIT 一旦调小，
    // 存量用户读回的就是超限表。让两侧口径一致，省得淘汰逻辑只在写侧存在。
    trimToLimit(out)
    return out
  } catch {
    return new Map()
  }
}

/**
 * 就地抬升某条链接的下界并落盘。新值不高于已有值时**不写盘**——那是最热的一条
 * 路（同一篇反复保存），早退让它零成本。返回是否真的抬升了。
 *
 * ⚠ 副作用不止「抬升一条」：真的抬升时会把**盘上整张表**按 max 并进 `floors`，
 * 也可能为了守住上限从 `floors` 里删掉若干最早的条目。调用方持有的那张 Map 会被
 * 就地改写。
 *
 * 合并**不跳过 `linkId` 自己**：另一个标签页若已把同一条抬得比本次 `revision`
 * 更高，返回后 `floors.get(linkId)` 会**高于传进来的值**。这是想要的行为（下界
 * 仍 ≤ 服务端真值，取的本来就是 max），但别把本函数读成「floors[linkId] 恰好
 * 等于 revision」。
 */
export function noteRevisionFloor(
  floors: Map<string, number>,
  linkId: string,
  revision: number,
): boolean {
  if (!Number.isInteger(revision) || revision <= 0) return false
  const held = floors.get(linkId)
  if (held !== undefined && revision <= held) return false
  floors.set(linkId, revision)

  // 写盘前先把盘上现状按 max 合并进来。
  //
  // 不合并就是**整表覆写**，而每个标签页手里都是自己 mount 那一刻的快照：A 页
  // 保存了 L1，B 页（更早挂载、手里没有 L1）随后保存 L2，一覆写就把 L1 的下界
  // 抹了。A 页再刷新，就退回本模块开头描述的那条丢划线的路——同一种数据丢失，
  // 入口从「刷新」换成「第二个标签页」。
  //
  // 下界是单调量，max 合并天然幂等、无冲突，不需要锁或版本号。
  const onDisk = loadRevisionFloors()
  for (const [id, value] of onDisk) {
    const mine = floors.get(id)
    if (mine === undefined || value > mine) floors.set(id, value)
  }
  trimToLimit(floors, linkId)

  writeOwnedStorage('revisionFloor', JSON.stringify(Object.fromEntries(floors)))
  return true
}

/**
 * 把 `incoming` 按 max 并进 `base`，返回**新表**（调用方用它换引用触发重算）。
 *
 * 跨标签页跟进必须走这条，不能直接拿盘上那份整表替换：本页可能持有盘上没有的
 * 更高值——落盘失败（隐私模式 / 配额满，`noteRevisionFloor` 静默吞掉异常）、或者
 * 本页那条被另一页写满 FLOOR_LIMIT 挤了出去。替换会让下界**下降**，而下降正是
 * 本模块开头那段安全性论证明确排除的情形，后果是不可恢复的划线丢失。
 */
export function mergeRevisionFloors(
  base: Map<string, number>,
  incoming: Map<string, number>,
): Map<string, number> {
  // 从 incoming 起手、再把 base 并上去，顺序是**刻意**的：淘汰从队首开始，这样
  // base（本页手里正在用的那些）排在队尾、最后才被淘汰。反过来写的话，另一页
  // 只要写满 FLOOR_LIMIT，本页那条就会在合并的同一瞬间被挤掉——下界凭空消失，
  // 正是这个函数要防的事。
  const merged = new Map(incoming)
  for (const [id, value] of base) {
    const held = merged.get(id)
    // `Map.set` 命中已有键**不移位**，所以必须先 delete 再 set，才能真的把 base
    // 那批挪到队尾。只靠 set 的话，两边都有的键会保留 incoming 给它的位置——落在
    // 队首就会被下面的 trim 砍掉，本页手里的下界照样凭空消失。
    if (held !== undefined) merged.delete(id)
    merged.set(id, held !== undefined && held > value ? held : value)
  }
  // 两页各持满 FLOOR_LIMIT 且互不相交时，合并后 2×LIMIT 会被砍掉一半——按上面
  // 的顺序，被砍的是 incoming（另一页）那批。可以接受：下界只是「本机写入 → 列表
  // 追上」这一小段的短命量，盘上那份没被改写，下一次 noteRevisionFloor 会重新
  // 从盘合并回来。
  trimToLimit(merged)
  return merged
}

/**
 * 就地把表截到 FLOOR_LIMIT 条，从队首开始丢（Map 保持插入序，且 `set` 命中已有
 * 键不会移位，所以这是 FIFO 不是 LRU）。
 *
 * 准确说是按「**在本表里首次出现**」排序，不是按全局首次写入：合并时从盘上捞
 * 回来的条目会追加到队尾，因而排在本标签页较早写的条目之后。这是想要的方向
 * ——另一个标签页刚写的下界比本页的陈年条目更值得留。
 *
 * `keep` 指定一条永不被淘汰的键：抬升某条下界的同时又把它淘汰掉，等于这次调用
 * 没发生，却仍然返回成功——最坏的一种「看起来成功了」。
 */
function trimToLimit(floors: Map<string, number>, keep?: string): void {
  let excess = floors.size - FLOOR_LIMIT
  if (excess <= 0) return
  for (const key of floors.keys()) {
    if (excess <= 0) break
    if (key === keep) continue
    floors.delete(key)
    excess -= 1
  }
}

import { afterEach, describe, expect, it, vi } from 'vitest'

import { loadRevisionFloors, mergeRevisionFloors, noteRevisionFloor } from './revision-floor'
import { ownedStorageKey } from '../storage-ownership'
import { readerIdentity } from '../identity'

afterEach(() => {
  localStorage.clear()
  vi.restoreAllMocks()
})

function stored(): Record<string, unknown> {
  const raw = localStorage.getItem(ownedStorageKey('revisionFloor') ?? '')
  return raw ? (JSON.parse(raw) as Record<string, unknown>) : {}
}

describe('content_revision 下界', () => {
  it('same-origin A/B 下界互不可见，切回 A 后仍可恢复', () => {
    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    noteRevisionFloor(new Map(), 'A-link', 8)

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    expect(loadRevisionFloors().has('A-link')).toBe(false)
    noteRevisionFloor(new Map(), 'B-link', 4)

    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    expect([...loadRevisionFloors().entries()]).toEqual([['A-link', 8]])
  })

  it('抬升并落盘；刷新后读得回来', () => {
    const floors = new Map<string, number>()
    expect(noteRevisionFloor(floors, 'L1', 8)).toBe(true)
    expect(stored()).toEqual({ L1: 8 })
    // 「刷新」= 丢掉内存里那份，重新 load。这正是它落盘的全部理由。
    expect(loadRevisionFloors().get('L1')).toBe(8)
  })

  it('只升不降：更小或相等的代次既不改内存也不写盘', () => {
    const floors = new Map<string, number>()
    noteRevisionFloor(floors, 'L1', 8)
    const setItem = vi.spyOn(Storage.prototype, 'setItem')

    expect(noteRevisionFloor(floors, 'L1', 7)).toBe(false)
    expect(noteRevisionFloor(floors, 'L1', 8)).toBe(false)

    expect(floors.get('L1')).toBe(8)
    // 同一篇反复保存是最热的一条路，早退让它零成本。
    expect(setItem).not.toHaveBeenCalled()
  })

  it('拒绝非法代次——抬到一个不存在的代次比不抬更糟', () => {
    const floors = new Map<string, number>()
    for (const bad of [0, -1, 1.5, Number.NaN]) {
      expect(noteRevisionFloor(floors, 'L1', bad)).toBe(false)
    }
    expect(floors.size).toBe(0)
  })

  it('损坏的条目被逐条丢弃，不拖垮整张表', () => {
    localStorage.setItem(
      ownedStorageKey('revisionFloor') ?? '',
      JSON.stringify({ good: 3, zero: 0, negative: -2, fractional: 1.5, text: '4', nothing: null }),
    )
    const floors = loadRevisionFloors()
    expect([...floors.entries()]).toEqual([['good', 3]])
  })

  it('整张表损坏时回退空表，不抛', () => {
    localStorage.setItem(ownedStorageKey('revisionFloor') ?? '', '{ not json')
    expect(loadRevisionFloors().size).toBe(0)
    localStorage.setItem(ownedStorageKey('revisionFloor') ?? '', '[1,2,3]')
    expect(loadRevisionFloors().size).toBe(0)
  })

  it('超出上限时淘汰最早写入的，但绝不淘汰本次刚写的那条', () => {
    const floors = new Map<string, number>()
    for (let i = 0; i < 200; i += 1) noteRevisionFloor(floors, `L${i}`, 1)
    expect(floors.size).toBe(200)

    noteRevisionFloor(floors, 'newest', 5)

    expect(floors.size).toBe(200)
    // 淘汰本次刚写的那条 = 这次调用等于没发生，是最坏的一种"看起来成功了"。
    expect(floors.get('newest')).toBe(5)
    expect(floors.has('L0')).toBe(false)
    expect(floors.has('L199')).toBe(true)
  })

  // 上一条里 `newest` 是新键、必然排在队尾，而淘汰从队首开始——保护分支根本
  // 不可达，删掉它那条用例照样绿。真正让它生效的场景是：进函数时表已超限，
  // 且被抬的那条正好排在队首。
  it('被抬升的那条即使排在队首也不会被自己这次调用淘汰掉', () => {
    const floors = new Map<string, number>()
    for (let i = 0; i < 205; i += 1) floors.set(`L${i}`, 1)

    expect(noteRevisionFloor(floors, 'L0', 9)).toBe(true)

    // 淘汰掉它 = 返回了 true，但这次调用等于没发生。
    expect(floors.get('L0')).toBe(9)
    expect(floors.size).toBe(200)
  })

  // M-1：每个标签页手里都是自己 mount 那一刻的快照。整表覆写会让后写的一方
  // 抹掉先写方的下界——同一种丢划线，入口从「刷新」换成「第二个标签页」。
  it('写盘时按 max 合并盘上现状，不覆盖另一个标签页写的下界', () => {
    const tabA = new Map<string, number>()
    noteRevisionFloor(tabA, 'L1', 8)

    // B 页更早挂载，手里是空表，完全不知道 L1。
    const tabB = new Map<string, number>()
    noteRevisionFloor(tabB, 'L2', 3)

    const disk = loadRevisionFloors()
    expect(disk.get('L1')).toBe(8)
    expect(disk.get('L2')).toBe(3)
  })

  it('合并取 max：盘上更高时不被内存里的旧值压低', () => {
    const tabA = new Map<string, number>()
    noteRevisionFloor(tabA, 'L1', 12)

    const tabB = new Map<string, number>([['L1', 5]])
    noteRevisionFloor(tabB, 'L2', 1)

    expect(loadRevisionFloors().get('L1')).toBe(12)
    expect(tabB.get('L1')).toBe(12)
  })

  // max 的**另一半方向**。上一条只钉住「盘上更高 → 采纳盘上」，而把合并写成
  // 无条件 `floors.set(id, value)`（盘上永远赢）同样能满足它——内存更高的那个
  // 方向是裸奔的，而它可达且有害：写盘失败之后内存就高于盘上。
  it('合并取 max：内存更高时不被盘上的旧值压回去', () => {
    const floors = new Map<string, number>()
    noteRevisionFloor(floors, 'L1', 5)

    // 配额满 / 隐私模式：内存抬到 12，盘上还停在 5。
    const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('QuotaExceededError')
    })
    noteRevisionFloor(floors, 'L1', 12)
    setItem.mockRestore()

    // 下一次任意写入会触发合并——盘上那个 5 不能把内存里的 12 压回去。
    noteRevisionFloor(floors, 'L2', 1)

    expect(floors.get('L1')).toBe(12)
    // 顺带把盘上那份也治好了。
    expect(loadRevisionFloors().get('L1')).toBe(12)
  })

  it('读侧同样截断——FLOOR_LIMIT 调小后存量用户不会读回超限表', () => {
    const oversized: Record<string, number> = {}
    for (let i = 0; i < 250; i += 1) oversized[`L${i}`] = 1
    localStorage.setItem(ownedStorageKey('revisionFloor') ?? '', JSON.stringify(oversized))

    const floors = loadRevisionFloors()

    expect(floors.size).toBe(200)
    // 从队首丢，留下的是最近写入的那批。
    expect(floors.has('L0')).toBe(false)
    expect(floors.has('L249')).toBe(true)
  })

  // 跨标签页跟进必须走 max 合并。直接拿盘上那份整表替换会让下界**下降**，而
  // 下降正是本模块安全性论证明确排除的情形——后果是不可恢复的划线丢失。
  describe('mergeRevisionFloors：跨页跟进不得让下界下降', () => {
    it('本页持有盘上没有的更高值时保住它（落盘失败留下的状态）', () => {
      const floors = new Map<string, number>()
      noteRevisionFloor(floors, 'L1', 5)
      const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
        throw new Error('QuotaExceededError')
      })
      noteRevisionFloor(floors, 'L1', 9) // 内存 9，盘上还是 5
      setItem.mockRestore()

      const merged = mergeRevisionFloors(floors, loadRevisionFloors())

      expect(merged.get('L1')).toBe(9)
    })

    it('本页那条被另一页挤出上限时也不会凭空消失', () => {
      const mine = new Map<string, number>([['L1', 9]])
      // 另一页写满了 FLOOR_LIMIT，盘上没有 L1。
      const theirs = new Map<string, number>()
      for (let i = 0; i < 200; i += 1) theirs.set(`X${i}`, 1)

      const merged = mergeRevisionFloors(mine, theirs)

      expect(merged.get('L1')).toBe(9)
      expect(merged.size).toBeLessThanOrEqual(200)
    })

    // 上一条里 L1 只在 base 里有，`set` 自然追加到队尾。两边**都有**那一条才是
    // 洞：`Map.set` 命中已有键不移位，L1 会留在 incoming 给它的队首位置，然后被
    // trim 砍掉——本页的下界照样消失。
    it('两边都有的键也要被挪到队尾，不能留在 incoming 的队首被砍掉', () => {
      const incoming = new Map<string, number>([['L1', 1]])
      for (let i = 0; i < 200; i += 1) incoming.set(`X${i}`, 1)
      const base = new Map<string, number>([['L1', 9]])

      const merged = mergeRevisionFloors(base, incoming)

      expect(merged.get('L1')).toBe(9)
    })

    it('盘上更高时采纳盘上（合并是 max，不是「本页永远赢」）', () => {
      const merged = mergeRevisionFloors(new Map([['L1', 3]]), new Map([['L1', 11]]))
      expect(merged.get('L1')).toBe(11)
    })
  })

  it('localStorage 写失败时内存那份仍然生效（配额满 / 隐私模式）', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('QuotaExceededError')
    })
    const floors = new Map<string, number>()
    expect(noteRevisionFloor(floors, 'L1', 8)).toBe(true)
    expect(floors.get('L1')).toBe(8)
  })
})

import { useCallback, useState } from 'react'
import { readOwnedStorage, writeOwnedStorage } from './storage-ownership'

/** 钉选数据形状（标签 / 域名）。 */
export interface Pins {
  tags: string[]
  domains: string[]
}

/** 钉选 kind。 */
export type PinKind = keyof Pins

function loadPins(): Pins {
  try {
    const raw = JSON.parse(readOwnedStorage('pins') || '{}') as Partial<Pins>
    // 逐项校验：损坏 / 旧版本数据可能把 tags|domains 写成非数组，spread 覆盖默认值后
    // 会让 usePins 的 .includes 抛 TypeError；这里在持久化数据边界 fail closed。
    return {
      tags: Array.isArray(raw?.tags) ? raw.tags : [],
      domains: Array.isArray(raw?.domains) ? raw.domains : [],
    }
  } catch {
    return { tags: [], domains: [] }
  }
}

/** 钉选标签 / 域名，localStorage 持久化（键 webtag:pins:v1）。 */
export function usePins(): [Pins, (kind: PinKind, name: string) => void] {
  const [pins, setPins] = useState<Pins>(loadPins)
  const toggle = useCallback((kind: PinKind, name: string) => {
    setPins((p) => {
      const has = p[kind].includes(name)
      const next: Pins = {
        ...p,
        [kind]: has ? p[kind].filter((x) => x !== name) : [...p[kind], name],
      }
      writeOwnedStorage('pins', JSON.stringify(next))
      return next
    })
  }, [])
  return [pins, toggle]
}

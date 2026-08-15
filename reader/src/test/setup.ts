// Vitest 全局 setup：注入 @testing-library/jest-dom 的自定义匹配器，
// 并在每个测试后清理 DOM 与 localStorage，保证用例隔离。
import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach } from 'vitest'
import { cleanup } from '@testing-library/react'

import { configure } from '@testing-library/react'

import { resourceStore } from '../lib/cache/store'
import { readerIdentity } from '../lib/identity'
import { readingFocusStore, readingPreferenceStore } from '../lib/reading-surface'

// waitFor 的默认 1000ms 在 CI 与构建并发时不够：PF9 之后多条用例要等一次
// 动态 import 解析。这里放宽的是**等待上限**，不是断言口径——真失败仍然会红。
configure({ asyncUtilTimeout: 5000 })

beforeEach(() => {
  window.history.replaceState({}, '', '/')
  const lease = readerIdentity.install({
    serverClientDataNamespace: 'reader-test-server-namespace',
    physicalNamespace: 'reader-test-physical-namespace',
  })
  resourceStore.activateIdentity(lease)
})

afterEach(() => {
  cleanup()
  window.getSelection()?.removeAllRanges()
  localStorage.clear()
  // 请求缓存是模块级单例，跨用例共享。不清的话上一个用例喂进去的数据会成为
  // 下一个用例的"已有缓存"——而 SWR 的语义正是"有缓存就先渲染缓存"，于是
  // 一个断言空列表的用例会拿到上一个用例的链接，且看起来像被测代码的 bug。
  readerIdentity.clear()
  resourceStore.deactivateIdentity()
  readingFocusStore.set(false)
  readingPreferenceStore.reset()
})

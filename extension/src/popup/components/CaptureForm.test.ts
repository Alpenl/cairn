/**
 * CaptureForm.test.ts — 采集表单组件单元测试。
 *
 * 覆盖本次 UX 加固的 popup 行为（非像素渲染）：
 *   - 快照防误读：fetchLatest 回贴的陈旧终态快照（done/failed/still-processing
 *     且超过 10 分钟）归 idle 不回贴；新鲜终态、进行中态正常回贴。
 *   - 常驻设置入口：顶部齿轮按钮任何状态可达 → chrome.runtime.openOptionsPage。
 *   - still-processing 中性态：展示中性提示文案、不带 error 修饰类。
 *   - 结果页面标注：状态区渲染快照自带的 title/url。
 *   - 采集专用打包：隐藏关系面板和完成态「打开知识库」后续动作。
 *
 * useCaptureBridge 被 mock：start / fetchLatest / onUpdate 由测试控制返回，
 * 避免依赖真实 webext-bridge。chrome.* 由 test/setup.ts 提供 mock。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { ref } from 'vue'
import { i18nPlugin } from '../../../test/i18n-stub'
import type { CaptureSnapshot } from '@/background/capture-protocol'

// ── useCaptureBridge mock ───────────────────────────────────
// 每个用例通过 setBridgeBehavior 定制 fetchLatest / start 的返回值。

let fetchLatestResult: CaptureSnapshot = {
  stage: 'idle',
  url: '',
  title: '',
  updatedAt: 0,
}
const startMock = vi.fn<
  (note: string, tabId?: number) => Promise<CaptureSnapshot>
>(() =>
  Promise.resolve<CaptureSnapshot>({
    stage: 'submitted',
    url: 'https://example.com',
    title: 'Example',
    updatedAt: Date.now(),
  }),
)

vi.mock('@/popup/composables/useCaptureBridge', () => ({
  useCaptureBridge: () => ({
    start: startMock,
    fetchLatest: () => Promise.resolve(fetchLatestResult),
    onUpdate: () => () => {},
  }),
}))

// ── useLibraryRelation mock：可控关系状态 + 记录 query 调用 ──
// 逐用例改写 relationState 来模拟「已在库中 / 同站计数」等场景。
const relationState = {
  alreadyExists: ref(false),
  existingCreatedAt: ref<string | null>(null),
  sameSiteCount: ref<number | null>(null),
}
const queryRelationMock = vi.fn(() => Promise.resolve())

vi.mock('@/popup/composables/useLibraryRelation', () => ({
  useLibraryRelation: () => ({
    alreadyExists: relationState.alreadyExists,
    existingCreatedAt: relationState.existingCreatedAt,
    sameSiteCount: relationState.sameSiteCount,
    query: queryRelationMock,
  }),
}))

import CaptureForm from './CaptureForm.vue'

/** 用 i18n 桩挂载 CaptureForm。 */
function mountForm(props: { siteEnabled?: boolean } = {}) {
  return mount(CaptureForm, {
    props,
    global: {
      plugins: [i18nPlugin],
      // NButton / NInput 是 naive-ui 全局组件，测试里用 stub 占位（只验逻辑）。
      stubs: {
        NButton: {
          template:
            '<button v-bind="$attrs" @click="$emit(\'click\')"><slot /></button>',
        },
        NInput: { template: '<textarea />' },
      },
    },
  })
}

beforeEach(() => {
  fetchLatestResult = { stage: 'idle', url: '', title: '', updatedAt: 0 }
  startMock.mockClear()
  // 关系面板状态逐用例重置为「无关系」（个别用例改写为已存 / 同站计数）。
  relationState.alreadyExists.value = false
  relationState.existingCreatedAt.value = null
  relationState.sameSiteCount.value = null
  queryRelationMock.mockClear()
  // chrome.tabs.query 在 CaptureForm 中以 Promise 形式调用，覆盖默认回调 mock。
  ;(
    chrome.tabs.query as unknown as ReturnType<typeof vi.fn>
  ).mockImplementation(() =>
    Promise.resolve([
      { id: 1, url: 'https://current.example.com', title: 'Current Page' },
    ]),
  )
  ;(
    chrome.runtime.openOptionsPage as unknown as ReturnType<typeof vi.fn>
  ).mockReset()
  ;(chrome.tabs.create as unknown as ReturnType<typeof vi.fn>).mockReset()
  ;(chrome.tabs.create as unknown as ReturnType<typeof vi.fn>).mockReturnValue(
    Promise.resolve({}),
  )
})

describe('CaptureForm — 快照防误读', () => {
  it('陈旧的 done 终态快照（超过 10 分钟）不回贴，保持 idle', async () => {
    fetchLatestResult = {
      stage: 'done',
      url: 'https://old.example.com',
      title: '旧页面',
      // 11 分钟前的终态：视为陈旧。
      updatedAt: Date.now() - 11 * 60 * 1000,
    }
    const wrapper = mountForm()
    await flushPromises()
    // idle：状态区整块不渲染。
    expect(wrapper.find('.capture-form__status').exists()).toBe(false)
  })

  it('新鲜的 done 终态快照（10 分钟内）正常回贴', async () => {
    fetchLatestResult = {
      stage: 'done',
      url: 'https://fresh.example.com',
      title: '新页面',
      updatedAt: Date.now() - 60 * 1000,
    }
    const wrapper = mountForm()
    await flushPromises()
    expect(wrapper.find('.capture-form__status').exists()).toBe(true)
    expect(wrapper.find('.capture-form__status--done').exists()).toBe(true)
  })

  it('陈旧的 failed 终态快照不回贴', async () => {
    fetchLatestResult = {
      stage: 'failed',
      url: 'https://old.example.com',
      title: '旧失败页',
      errorKind: 'job-failed',
      updatedAt: Date.now() - 20 * 60 * 1000,
    }
    const wrapper = mountForm()
    await flushPromises()
    expect(wrapper.find('.capture-form__status').exists()).toBe(false)
  })

  it('进行中态（parsing）即使时间久也回贴（不视为陈旧终态）', async () => {
    fetchLatestResult = {
      stage: 'parsing',
      url: 'https://running.example.com',
      title: '解析中页',
      updatedAt: Date.now() - 30 * 60 * 1000,
    }
    const wrapper = mountForm()
    await flushPromises()
    expect(wrapper.find('.capture-form__status').exists()).toBe(true)
  })

  it('状态区渲染快照自带的页面标题（明确是哪个页面的结果）', async () => {
    fetchLatestResult = {
      stage: 'done',
      url: 'https://result.example.com',
      title: '采集结果页标题',
      updatedAt: Date.now() - 60 * 1000,
    }
    const wrapper = mountForm()
    await flushPromises()
    const page = wrapper.find('.capture-form__status-page')
    expect(page.exists()).toBe(true)
    expect(page.text()).toContain('采集结果页标题')
  })
})

describe('CaptureForm — still-processing 中性态', () => {
  it('still-processing 展示中性提示且不带 failed 修饰类', async () => {
    fetchLatestResult = {
      stage: 'still-processing',
      url: 'https://slow.example.com',
      title: '慢解析页',
      updatedAt: Date.now() - 60 * 1000,
    }
    const wrapper = mountForm()
    await flushPromises()
    expect(wrapper.find('.capture-form__status').exists()).toBe(true)
    // 不是失败：无 failed 修饰类（中性色而非红色）。
    expect(wrapper.find('.capture-form__status--failed').exists()).toBe(false)
    // 中性提示文案的 i18n key 出现在状态区。
    expect(wrapper.find('.capture-form__status').text()).toContain(
      'capture.statusStillProcessingHint',
    )
  })
})

describe('CaptureForm — 常驻设置入口', () => {
  it('顶部齿轮按钮在 idle 态也可见并触发 openOptionsPage', async () => {
    const wrapper = mountForm()
    await flushPromises()
    const settingsBtn = wrapper.find('.capture-form__settings-btn')
    expect(settingsBtn.exists()).toBe(true)
    await settingsBtn.trigger('click')
    expect(chrome.runtime.openOptionsPage).toHaveBeenCalledTimes(1)
  })
})

describe('CaptureForm — site capability', () => {
  it('hides site while capability is unknown or unavailable', async () => {
    const wrapper = mountForm()
    await flushPromises()

    expect(wrapper.text()).not.toContain('capture.kindSite')
  })

  it('shows site only while the strict site capability is active', async () => {
    const wrapper = mountForm({ siteEnabled: true })
    await flushPromises()
    expect(wrapper.text()).toContain('capture.kindSite')

    await wrapper.setProps({ siteEnabled: false })

    expect(wrapper.text()).not.toContain('capture.kindSite')
  })

  it('sends an explicitly selected site intent through the popup bridge', async () => {
    const wrapper = mountForm({ siteEnabled: true })
    await flushPromises()

    await wrapper
      .get<HTMLInputElement>('.n-radio-input[value="site"]')
      .setValue(true)
    await wrapper.get('.capture-form__submit').trigger('click')
    await flushPromises()

    expect(startMock).toHaveBeenCalledWith('', 1, 'site')
  })

  it('cannot send a stale site intent after capability revocation', async () => {
    const wrapper = mountForm({ siteEnabled: true })
    await flushPromises()

    await wrapper
      .get<HTMLInputElement>('.n-radio-input[value="site"]')
      .setValue(true)
    await wrapper.setProps({ siteEnabled: false })
    await wrapper.get('.capture-form__submit').trigger('click')
    await flushPromises()

    expect(wrapper.text()).not.toContain('capture.kindSite')
    expect(startMock).toHaveBeenCalledWith('', 1, 'auto')
    expect(startMock).not.toHaveBeenCalledWith('', 1, 'site')
  })
})

describe('CaptureForm — capture-only 隐藏知识库后续动作', () => {
  it('done 态也不出现「打开知识库」按钮', async () => {
    fetchLatestResult = {
      stage: 'done',
      url: 'https://done.example.com',
      title: '已完成页',
      updatedAt: Date.now() - 60 * 1000,
    }
    const wrapper = mountForm()
    await flushPromises()
    expect(wrapper.find('.capture-form__status-action').exists()).toBe(false)
    expect(chrome.tabs.create).not.toHaveBeenCalled()
  })

  it('非 done 态同样不出现「打开知识库」按钮', async () => {
    fetchLatestResult = {
      stage: 'parsing',
      url: 'https://x.example.com',
      title: 'x',
      updatedAt: Date.now(),
    }
    const wrapper = mountForm()
    await flushPromises()
    expect(wrapper.find('.capture-form__status-action').exists()).toBe(false)
  })
})

describe('CaptureForm — capture-only 隐藏关系面板', () => {
  it('挂载后不再做库关系查询', async () => {
    mountForm()
    await flushPromises()
    expect(queryRelationMock).not.toHaveBeenCalled()
  })

  it('即使 relation mock 标记为已存在，也不渲染关系面板', async () => {
    relationState.alreadyExists.value = true
    relationState.existingCreatedAt.value = '2026-01-01T00:00:00Z'
    relationState.sameSiteCount.value = 5
    const wrapper = mountForm()
    await flushPromises()
    expect(wrapper.find('.library-relation').exists()).toBe(false)
  })

  it('主按钮始终保持「采集到 WebTag」文案', async () => {
    relationState.alreadyExists.value = true
    const wrapper = mountForm()
    await flushPromises()
    expect(wrapper.find('.capture-form__submit').text()).toContain(
      'capture.submitButton',
    )
  })
})

describe('CaptureForm — bridge 异常收口', () => {
  it('start message reject 时回到 failed 并重新启用按钮', async () => {
    startMock.mockRejectedValueOnce(new Error('bridge unavailable'))
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const wrapper = mountForm()
    await flushPromises()

    await wrapper.find('.capture-form__submit').trigger('click')
    await flushPromises()

    expect(wrapper.find('.capture-form__status--failed').exists()).toBe(true)
    expect(wrapper.find('.capture-form__status').text()).toContain(
      'capture.error.capture-unexpected',
    )
    expect(wrapper.find('.capture-form__submit').attributes('disabled')).toBe(
      undefined,
    )
    expect(warn).toHaveBeenCalled()
    warn.mockRestore()
  })
})

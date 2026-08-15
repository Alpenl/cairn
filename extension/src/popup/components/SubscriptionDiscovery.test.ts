import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { i18nPlugin } from '../../../test/i18n-stub'

const mocks = vi.hoisted(() => ({
  getDiscovery: vi.fn(),
  getSettings: vi.fn(),
  buildClient: vi.fn(),
  findSubscription: vi.fn(),
  createSubscription: vi.fn(),
}))

vi.mock('@/popup/composables/useRssDiscovery', () => ({
  getRssDiscovery: (...args: unknown[]) => mocks.getDiscovery(...args),
}))

vi.mock('@/composables/useSettings', () => ({
  getWebTagSettings: () => mocks.getSettings(),
}))

vi.mock('@/api/webtag-client', () => ({
  buildWebTagClientFromSettings: (...args: unknown[]) =>
    mocks.buildClient(...args),
}))

import SubscriptionDiscovery from './SubscriptionDiscovery.vue'

const tabsQuery = chrome.tabs.query as unknown as ReturnType<
  typeof vi.fn<(...args: unknown[]) => Promise<chrome.tabs.Tab[]>>
>
const tabsCreate = chrome.tabs.create as unknown as ReturnType<
  typeof vi.fn<(...args: unknown[]) => Promise<chrome.tabs.Tab>>
>

const settings = {
  backendUrl: 'https://api.example.com',
  readerUrl: 'https://reader.example.com/reader',
  accessToken: 'token',
  linkOpenBehavior: 'new' as const,
}

function mountDiscovery() {
  return mount(SubscriptionDiscovery, {
    global: {
      plugins: [i18nPlugin],
      stubs: {
        Icon: { template: '<i />' },
        NButton: {
          inheritAttrs: false,
          template:
            '<button class="n-button" :disabled="$attrs.disabled" @click="$emit(\'click\')"><slot name="icon" /><slot /></button>',
        },
      },
    },
  })
}

beforeEach(() => {
  mocks.getDiscovery.mockReset()
  mocks.getSettings.mockReset()
  mocks.buildClient.mockReset()
  mocks.findSubscription.mockReset()
  mocks.createSubscription.mockReset()
  tabsQuery.mockReset()
  tabsCreate.mockReset()
  vi.mocked(chrome.runtime.openOptionsPage).mockReset()

  tabsQuery.mockResolvedValue([
    { id: 8, url: 'https://example.com', title: 'Example' } as chrome.tabs.Tab,
  ])
  mocks.getDiscovery.mockResolvedValue({
    ok: true,
    state: {
      pageUrl: 'https://example.com',
      feeds: [
        {
          title: 'Posts',
          url: 'https://example.com/feed.xml',
          format: 'rss',
        },
        {
          title: 'Atom',
          url: 'https://example.com/atom.xml',
          format: 'atom',
        },
      ],
    },
  })
  mocks.getSettings.mockResolvedValue(settings)
  mocks.buildClient.mockReturnValue({
    findSubscriptionByUrl: mocks.findSubscription,
    createSubscription: mocks.createSubscription,
  })
})

describe('SubscriptionDiscovery', () => {
  it('lists every discovered feed, checks status and never subscribes silently', async () => {
    mocks.findSubscription
      .mockResolvedValueOnce({ ok: true, data: null })
      .mockResolvedValueOnce({
        ok: true,
        data: {
          id: 'sub-2',
          url: 'https://example.com/atom.xml',
          title: 'Atom',
        },
      })
    mocks.createSubscription.mockResolvedValue({
      ok: true,
      data: {
        id: 'sub-1',
        url: 'https://example.com/feed.xml',
        title: 'Posts',
      },
    })

    const wrapper = mountDiscovery()
    await flushPromises()
    expect(wrapper.findAll('.subscription-discovery__feed')).toHaveLength(2)
    expect(wrapper.text()).toContain('subscriptions.subscribe')
    expect(wrapper.text()).toContain('subscriptions.subscribed')
    expect(mocks.createSubscription).not.toHaveBeenCalled()

    await wrapper.findAll('.n-button')[0].trigger('click')
    await flushPromises()
    expect(mocks.createSubscription).toHaveBeenCalledWith(
      'https://example.com/feed.xml',
    )
    expect(
      wrapper.findAll('.subscription-discovery__feed')[0].text(),
    ).toContain('subscriptions.subscribed')
  })

  it('shows the existing-style settings action when the backend is not configured', async () => {
    mocks.buildClient.mockReturnValue(null)
    const wrapper = mountDiscovery()
    await flushPromises()
    expect(wrapper.text()).toContain('subscriptions.notConfigured')
    await wrapper.find('.subscription-discovery__config-note').trigger('click')
    expect(chrome.runtime.openOptionsPage).toHaveBeenCalledTimes(1)
    expect(mocks.createSubscription).not.toHaveBeenCalled()
  })

  it('shows a restricted state without checking the backend', async () => {
    mocks.getDiscovery.mockResolvedValue({ ok: false, error: 'restricted' })
    const wrapper = mountDiscovery()
    await flushPromises()
    expect(wrapper.text()).toContain('subscriptions.restricted')
    expect(mocks.getSettings).not.toHaveBeenCalled()
    expect(mocks.findSubscription).not.toHaveBeenCalled()
  })

  it('opens the explicitly configured Reader subscriptions view', async () => {
    mocks.findSubscription.mockResolvedValue({ ok: true, data: null })
    tabsCreate.mockResolvedValue({} as chrome.tabs.Tab)
    const wrapper = mountDiscovery()
    await flushPromises()
    await wrapper.find('.subscription-discovery__reader').trigger('click')
    expect(chrome.tabs.create).toHaveBeenCalledWith({
      url: 'https://reader.example.com/reader?view=subscriptions',
    })
  })
})

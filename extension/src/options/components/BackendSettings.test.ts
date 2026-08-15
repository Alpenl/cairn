import { flushPromises, mount } from '@vue/test-utils'
import { NButton, NInput, NRadioGroup } from 'naive-ui'
import { computed, nextTick, ref } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  CAPTURE_DESTINATION_STORAGE_KEY,
  type CaptureDestination,
} from '@/api/capture-preferences'
import type { ExtensionCapabilityPolicy } from '@/api/capabilities'
import type {
  UseWebTagSettingsReturn,
  WebTagSettings,
} from '@/composables/useSettings'
import { i18nPlugin } from '../../../test/i18n-stub'

const composableMocks = vi.hoisted(() => ({
  useWebTagSettings: vi.fn(),
  useExtensionCapabilities: vi.fn(),
  buildWebTagClientFromSettings: vi.fn(),
}))

vi.mock('@/composables/useSettings', () => ({
  useWebTagSettings: composableMocks.useWebTagSettings,
}))

vi.mock('@/composables/useCapabilities', () => ({
  useExtensionCapabilities: composableMocks.useExtensionCapabilities,
}))

vi.mock('@/api/webtag-client', () => ({
  buildWebTagClientFromSettings: composableMocks.buildWebTagClientFromSettings,
}))

import BackendSettings from './BackendSettings.vue'

const DEFAULT_SETTINGS: WebTagSettings = {
  backendUrl: 'https://api.example.test',
  readerUrl: 'https://reader.example.test',
  accessToken: 'test-token',
  connectionOwnerFingerprint: 'a'.repeat(64),
  connectionRevision: 1,
  linkOpenBehavior: 'current',
}

let policy = ref<ExtensionCapabilityPolicy>({ inbox: false, site: false })
let probing = ref(true)
let settingsState: UseWebTagSettingsReturn

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => {
    resolve = done
  })
  return { promise, resolve }
}

function mountSettings() {
  return mount(BackendSettings, {
    global: {
      plugins: [i18nPlugin],
    },
  })
}

function destinationGroupValue(
  wrapper: ReturnType<typeof mountSettings>,
): string {
  return wrapper.get<HTMLInputElement>('.n-radio-input:checked').element.value
}

function hasDestination(
  wrapper: ReturnType<typeof mountSettings>,
  destination: CaptureDestination,
): boolean {
  return wrapper.find(`.n-radio-input[value="${destination}"]`).exists()
}

async function seedDestination(destination: CaptureDestination): Promise<void> {
  await chrome.storage.local.set({
    [CAPTURE_DESTINATION_STORAGE_KEY]: destination,
  })
}

beforeEach(async () => {
  await chrome.storage.local.clear()
  vi.clearAllMocks()
  policy = ref({ inbox: false, site: false })
  probing = ref(true)

  const settings = ref<WebTagSettings>({ ...DEFAULT_SETTINGS })
  settingsState = {
    settings,
    ready: Promise.resolve(),
    update: vi.fn(async () => ({ ok: true as const })),
    confirmConnection: vi.fn(async (connection) => {
      settings.value = {
        ...settings.value,
        ...connection,
        connectionRevision: settings.value.connectionRevision + 1,
      }
      return { ok: true as const }
    }),
    reload: vi.fn(async () => undefined),
    dispose: vi.fn(),
  }
  composableMocks.useWebTagSettings.mockReturnValue(settingsState)
  composableMocks.useExtensionCapabilities.mockReturnValue({
    policy: computed(() => policy.value),
    probing,
  })
  composableMocks.buildWebTagClientFromSettings.mockReturnValue({
    testConnection: vi.fn(async () => ({ ok: true as const, data: true })),
    getSessionIdentity: vi.fn(async () => ({
      ok: true as const,
      data: {
        client_data_namespace: 'namespace-b',
        representation_contract: 'v3',
      },
    })),
  })
})

describe('BackendSettings confirmed connection draft', () => {
  it('typing or pasting URL and token performs no request and no commit', async () => {
    const wrapper = mountSettings()
    await flushPromises()
    const inputs = wrapper.findAllComponents(NInput)

    inputs[0].vm.$emit('update:value', 'https://api-b.example.test')
    inputs[2].vm.$emit('update:value', 'token-b-prefix')
    inputs[2].vm.$emit('update:value', 'token-b')
    await flushPromises()

    expect(composableMocks.buildWebTagClientFromSettings).not.toHaveBeenCalled()
    expect(settingsState.confirmConnection).not.toHaveBeenCalled()
    expect(settingsState.settings.value).toEqual(DEFAULT_SETTINGS)
    wrapper.unmount()
  })

  it('tests and commits one immutable URL/token snapshot while later edits stay draft-only', async () => {
    const pending = deferred<{ ok: true; data: true }>()
    const testConnection = vi.fn(() => pending.promise)
    composableMocks.buildWebTagClientFromSettings.mockReturnValue({
      testConnection,
      getSessionIdentity: vi.fn(async () => ({
        ok: true as const,
        data: {
          client_data_namespace: 'namespace-b',
          representation_contract: 'v3',
        },
      })),
    })
    const wrapper = mountSettings()
    await flushPromises()
    const inputs = wrapper.findAllComponents(NInput)
    inputs[0].vm.$emit('update:value', ' https://api-b.example.test/path ')
    inputs[1].vm.$emit('update:value', ' https://reader-b.example.test ')
    inputs[2].vm.$emit('update:value', ' token-b ')
    await nextTick()
    await wrapper.getComponent(NButton).trigger('click')

    expect(composableMocks.buildWebTagClientFromSettings).toHaveBeenCalledWith({
      backendUrl: 'https://api-b.example.test/path',
      readerUrl: 'https://reader-b.example.test',
      accessToken: 'token-b',
    })
    inputs[0].vm.$emit('update:value', 'https://api-c.example.test')
    inputs[2].vm.$emit('update:value', 'token-c')
    pending.resolve({ ok: true, data: true })
    await flushPromises()

    await vi.waitFor(() => {
      expect(settingsState.confirmConnection).toHaveBeenCalledTimes(1)
    })
    expect(settingsState.confirmConnection).toHaveBeenCalledWith(
      expect.objectContaining({
        backendUrl: 'https://api-b.example.test/path',
        readerUrl: 'https://reader-b.example.test',
        accessToken: 'token-b',
        connectionOwnerFingerprint: expect.stringMatching(/^[a-f0-9]{64}$/),
      }),
    )
    expect(settingsState.settings.value.backendUrl).toBe(
      'https://api-b.example.test/path',
    )
    expect(settingsState.settings.value.accessToken).toBe('token-b')
    expect(inputs[0].props('value')).toBe('https://api-c.example.test')
    expect(inputs[2].props('value')).toBe('token-c')
    wrapper.unmount()
  })

  it('ignores a second connection-test click while the first candidate is pending', async () => {
    const pending = deferred<{ ok: true; data: true }>()
    const testConnection = vi.fn(() => pending.promise)
    composableMocks.buildWebTagClientFromSettings.mockReturnValue({
      testConnection,
      getSessionIdentity: vi.fn(async () => ({
        ok: true as const,
        data: {
          client_data_namespace: 'namespace-b',
          representation_contract: 'v3',
        },
      })),
    })
    const wrapper = mountSettings()
    await flushPromises()
    const inputs = wrapper.findAllComponents(NInput)
    inputs[0].vm.$emit('update:value', 'https://api-b.example.test')
    inputs[2].vm.$emit('update:value', 'token-b')
    await nextTick()

    const button = wrapper.getComponent(NButton)
    await button.trigger('click')
    inputs[0].vm.$emit('update:value', 'https://api-c.example.test')
    inputs[2].vm.$emit('update:value', 'token-c')
    await button.trigger('click')

    expect(composableMocks.buildWebTagClientFromSettings).toHaveBeenCalledTimes(
      1,
    )
    expect(testConnection).toHaveBeenCalledTimes(1)
    expect(settingsState.confirmConnection).not.toHaveBeenCalled()

    pending.resolve({ ok: true, data: true })
    await flushPromises()

    await vi.waitFor(() => {
      expect(settingsState.confirmConnection).toHaveBeenCalledTimes(1)
    })
    expect(settingsState.confirmConnection).toHaveBeenCalledWith(
      expect.objectContaining({
        backendUrl: 'https://api-b.example.test',
        accessToken: 'token-b',
      }),
    )
    wrapper.unmount()
  })

  it('does not commit a candidate when the explicit connection test fails', async () => {
    composableMocks.buildWebTagClientFromSettings.mockReturnValue({
      testConnection: vi.fn(async () => ({
        ok: false as const,
        error: { kind: 'unauthorized' as const, message: 'denied' },
      })),
      getSessionIdentity: vi.fn(),
    })
    const wrapper = mountSettings()
    await flushPromises()
    const inputs = wrapper.findAllComponents(NInput)
    inputs[0].vm.$emit('update:value', 'https://api-b.example.test')
    inputs[2].vm.$emit('update:value', 'token-b')
    await nextTick()
    await wrapper.getComponent(NButton).trigger('click')
    await flushPromises()

    expect(settingsState.confirmConnection).not.toHaveBeenCalled()
    expect(settingsState.settings.value).toEqual(DEFAULT_SETTINGS)
    wrapper.unmount()
  })

  it('reopening settings restores confirmed values and discards uncommitted draft', async () => {
    const first = mountSettings()
    await flushPromises()
    first
      .findAllComponents(NInput)[0]
      .vm.$emit('update:value', 'https://draft-only.example.test')
    await flushPromises()
    first.unmount()

    const reopened = mountSettings()
    await flushPromises()
    expect(reopened.findAllComponents(NInput)[0].props('value')).toBe(
      DEFAULT_SETTINGS.backendUrl,
    )
    expect(composableMocks.buildWebTagClientFromSettings).not.toHaveBeenCalled()
    reopened.unmount()
  })
})

describe('BackendSettings capture destination capabilities', () => {
  it('hides Inbox while the capability probe is pending', async () => {
    probing.value = true

    const wrapper = mountSettings()
    await flushPromises()

    expect(hasDestination(wrapper, 'inbox')).toBe(false)
    expect(hasDestination(wrapper, 'library')).toBe(true)
    wrapper.unmount()
  })

  it('keeps Inbox hidden after a failed or disabled capability probe', async () => {
    probing.value = false
    policy.value = { inbox: false, site: false }

    const wrapper = mountSettings()
    await flushPromises()

    expect(hasDestination(wrapper, 'inbox')).toBe(false)
    expect(destinationGroupValue(wrapper)).toBe('library')
    wrapper.unmount()
  })

  it('shows Inbox only after the capability is granted', async () => {
    probing.value = false
    policy.value = { inbox: true, site: false }

    const wrapper = mountSettings()
    await flushPromises()

    expect(hasDestination(wrapper, 'inbox')).toBe(true)
    wrapper.unmount()
  })

  it('renders a stored Inbox preference as Library without overwriting it', async () => {
    await seedDestination('inbox')
    const setSpy = vi.spyOn(chrome.storage.local, 'set')
    setSpy.mockClear()
    probing.value = false
    policy.value = { inbox: false, site: false }

    const wrapper = mountSettings()
    await flushPromises()

    expect(destinationGroupValue(wrapper)).toBe('library')
    expect(setSpy).not.toHaveBeenCalled()
    expect(
      await chrome.storage.local.get(CAPTURE_DESTINATION_STORAGE_KEY),
    ).toEqual({ [CAPTURE_DESTINATION_STORAGE_KEY]: 'inbox' })

    wrapper.getComponent(NRadioGroup).vm.$emit('update:value', 'inbox')
    await flushPromises()
    expect(setSpy).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('removes Inbox immediately when a live capability lease is revoked', async () => {
    await seedDestination('inbox')
    const setSpy = vi.spyOn(chrome.storage.local, 'set')
    setSpy.mockClear()
    probing.value = false
    policy.value = { inbox: true, site: false }

    const wrapper = mountSettings()
    await flushPromises()
    expect(hasDestination(wrapper, 'inbox')).toBe(true)
    expect(destinationGroupValue(wrapper)).toBe('inbox')

    policy.value = { inbox: false, site: false }
    await nextTick()

    expect(hasDestination(wrapper, 'inbox')).toBe(false)
    expect(destinationGroupValue(wrapper)).toBe('library')
    expect(setSpy).not.toHaveBeenCalled()
    expect(
      await chrome.storage.local.get(CAPTURE_DESTINATION_STORAGE_KEY),
    ).toEqual({ [CAPTURE_DESTINATION_STORAGE_KEY]: 'inbox' })
    wrapper.unmount()
  })
})

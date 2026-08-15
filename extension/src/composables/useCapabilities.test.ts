import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, ref, type Ref } from 'vue'
import { describe, expect, it, vi } from 'vitest'
import type { ApiResult, WebTagClient } from '@/api/webtag-client'
import type { CapabilitiesResponse } from '@/api/types'
import { DEFAULT_WEBTAG_SETTINGS, type WebTagSettings } from './useSettings'
import { useExtensionCapabilities } from './useCapabilities'

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => {
    resolve = done
  })
  return { promise, resolve }
}

function capabilities(inbox: boolean, site: boolean): CapabilitiesResponse {
  return {
    library_kinds: site,
    site_library: site,
    site_management: site,
    reader: { inbox },
  } as unknown as CapabilitiesResponse
}

function mountProbe(
  settings: Ref<WebTagSettings>,
  buildClient: (
    value: WebTagSettings,
  ) => Pick<WebTagClient, 'getCapabilities'> | null,
) {
  let state!: ReturnType<typeof useExtensionCapabilities>
  const wrapper = mount(
    defineComponent({
      setup() {
        state = useExtensionCapabilities(
          settings,
          Promise.resolve(),
          buildClient,
        )
        return () => null
      },
    }),
  )
  return { wrapper, state }
}

describe('useExtensionCapabilities', () => {
  it('keeps every feature disabled until the probe returns', async () => {
    const pending = deferred<ApiResult<CapabilitiesResponse | null>>()
    const settings = ref({
      ...DEFAULT_WEBTAG_SETTINGS,
      backendUrl: 'https://api-a.example.test',
    })
    const { wrapper, state } = mountProbe(settings, () => ({
      getCapabilities: vi.fn(() => pending.promise),
    }))

    await flushPromises()
    expect(state.probing.value).toBe(true)
    expect(state.policy.value).toEqual({ inbox: false, site: false })

    pending.resolve({ ok: true, data: capabilities(true, true) })
    await flushPromises()

    expect(state.probing.value).toBe(false)
    expect(state.policy.value).toEqual({ inbox: true, site: true })
    wrapper.unmount()
  })

  it('fails closed when the probe rejects or returns an API error', async () => {
    const settings = ref({
      ...DEFAULT_WEBTAG_SETTINGS,
      backendUrl: 'https://api.example.test',
    })
    const getCapabilities = vi
      .fn<() => Promise<ApiResult<CapabilitiesResponse | null>>>()
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValueOnce({
        ok: false,
        error: { kind: 'unauthorized', message: 'denied' },
      })
    const { wrapper, state } = mountProbe(settings, () => ({
      getCapabilities,
    }))

    await flushPromises()
    expect(state.probing.value).toBe(false)
    expect(state.policy.value).toEqual({ inbox: false, site: false })

    await state.refresh()
    expect(state.policy.value).toEqual({ inbox: false, site: false })
    wrapper.unmount()
  })

  it('does not probe while backend URL or token is only an unconfirmed draft', async () => {
    const settings = ref({
      ...DEFAULT_WEBTAG_SETTINGS,
      backendUrl: 'https://api-a.example.test',
      accessToken: 'token-a',
    })
    const buildClient = vi.fn(() => ({
      getCapabilities: vi.fn(async () => ({
        ok: true as const,
        data: capabilities(true, true),
      })),
    }))
    const { wrapper } = mountProbe(settings, buildClient)
    await flushPromises()
    expect(buildClient).toHaveBeenCalledTimes(1)

    settings.value = {
      ...settings.value,
      backendUrl: 'https://api-b.example.test',
    }
    await flushPromises()
    settings.value = { ...settings.value, accessToken: 'token-b-prefix' }
    await flushPromises()

    expect(buildClient).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('does not commit a late response from an old settings identity', async () => {
    const oldProbe = deferred<ApiResult<CapabilitiesResponse | null>>()
    const settings = ref({
      ...DEFAULT_WEBTAG_SETTINGS,
      backendUrl: 'https://api-a.example.test',
    })
    const clients = new Map<string, Pick<WebTagClient, 'getCapabilities'>>([
      [
        'https://api-a.example.test',
        { getCapabilities: vi.fn(() => oldProbe.promise) },
      ],
      [
        'https://api-b.example.test',
        {
          getCapabilities: vi.fn(async () => ({
            ok: true as const,
            data: capabilities(true, false),
          })),
        },
      ],
    ])
    const { wrapper, state } = mountProbe(
      settings,
      (value) => clients.get(value.backendUrl) ?? null,
    )
    await flushPromises()
    expect(state.probing.value).toBe(true)

    settings.value = {
      ...settings.value,
      backendUrl: 'https://api-b.example.test',
      connectionRevision: settings.value.connectionRevision + 1,
    }
    await flushPromises()
    expect(state.policy.value).toEqual({ inbox: true, site: false })

    oldProbe.resolve({ ok: true, data: capabilities(true, true) })
    await flushPromises()

    expect(state.policy.value).toEqual({ inbox: true, site: false })
    wrapper.unmount()
  })

  it('revokes and refreshes the lease when the installation token changes', async () => {
    const tokenProbe = deferred<ApiResult<CapabilitiesResponse | null>>()
    const settings = ref({
      ...DEFAULT_WEBTAG_SETTINGS,
      backendUrl: 'https://api.example.test',
    })
    const buildClient = vi
      .fn<
        (value: WebTagSettings) => Pick<WebTagClient, 'getCapabilities'> | null
      >()
      .mockReturnValueOnce({
        getCapabilities: vi.fn(async () => ({
          ok: true as const,
          data: capabilities(true, true),
        })),
      })
      .mockReturnValueOnce({
        getCapabilities: vi.fn(() => tokenProbe.promise),
      })
    const { wrapper, state } = mountProbe(settings, buildClient)
    await flushPromises()

    const oldLease = state.capabilityLease.value
    expect(state.policy.value).toEqual({ inbox: true, site: true })

    settings.value = {
      ...settings.value,
      accessToken: 'token-a',
      connectionRevision: settings.value.connectionRevision + 1,
    }
    await flushPromises()

    expect(oldLease.isCurrent()).toBe(false)
    expect(state.probing.value).toBe(true)
    expect(state.policy.value).toEqual({ inbox: false, site: false })

    tokenProbe.resolve({ ok: true, data: capabilities(false, true) })
    await flushPromises()
    expect(state.policy.value).toEqual({ inbox: false, site: true })

    const callsBeforeUnmount = buildClient.mock.calls.length
    wrapper.unmount()
    settings.value = { ...settings.value, accessToken: 'token-b' }
    await flushPromises()
    expect(buildClient).toHaveBeenCalledTimes(callsBeforeUnmount)
  })

  it('does not commit a late response from an old installation token', async () => {
    const oldTokenProbe = deferred<ApiResult<CapabilitiesResponse | null>>()
    const settings = ref({
      ...DEFAULT_WEBTAG_SETTINGS,
      backendUrl: 'https://api.example.test',
    })
    const buildClient = vi
      .fn<
        (value: WebTagSettings) => Pick<WebTagClient, 'getCapabilities'> | null
      >()
      .mockReturnValueOnce({
        getCapabilities: vi.fn(async () => ({
          ok: true as const,
          data: capabilities(false, false),
        })),
      })
      .mockReturnValueOnce({
        getCapabilities: vi.fn(() => oldTokenProbe.promise),
      })
      .mockReturnValueOnce({
        getCapabilities: vi.fn(async () => ({
          ok: true as const,
          data: capabilities(true, false),
        })),
      })
    const { wrapper, state } = mountProbe(settings, buildClient)
    await flushPromises()

    settings.value = {
      ...settings.value,
      accessToken: 'old-token',
      connectionRevision: settings.value.connectionRevision + 1,
    }
    await flushPromises()
    expect(buildClient).toHaveBeenCalledTimes(2)
    expect(state.probing.value).toBe(true)

    settings.value = {
      ...settings.value,
      accessToken: 'new-token',
      connectionRevision: settings.value.connectionRevision + 1,
    }
    await flushPromises()
    expect(state.policy.value).toEqual({ inbox: true, site: false })

    oldTokenProbe.resolve({ ok: true, data: capabilities(true, true) })
    await flushPromises()
    expect(state.policy.value).toEqual({ inbox: true, site: false })
    expect(buildClient).toHaveBeenCalledTimes(3)
    wrapper.unmount()
  })
})

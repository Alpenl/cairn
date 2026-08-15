import {
  computed,
  onUnmounted,
  readonly,
  ref,
  shallowRef,
  watch,
  type Ref,
} from 'vue'
import {
  DISABLED_EXTENSION_CAPABILITY_POLICY,
  deriveExtensionCapabilityPolicy,
  ExtensionCapabilityLease,
} from '@/api/capabilities'
import {
  buildWebTagClientFromSettings,
  type WebTagClient,
} from '@/api/webtag-client'
import type { WebTagSettings } from './useSettings'

type CapabilityClient = Pick<WebTagClient, 'getCapabilities'>
type CapabilityClientFactory = (
  settings: WebTagSettings,
) => CapabilityClient | null

export function useExtensionCapabilities(
  settings: Ref<WebTagSettings>,
  settingsReady: Promise<void>,
  buildClient: CapabilityClientFactory = buildWebTagClientFromSettings,
) {
  const lease = shallowRef(
    new ExtensionCapabilityLease(DISABLED_EXTENSION_CAPABILITY_POLICY),
  )
  const probing = ref(true)
  let requestGeneration = 0
  let disposed = false

  const installDisabledLease = (): ExtensionCapabilityLease => {
    lease.value.revoke()
    const next = new ExtensionCapabilityLease(
      DISABLED_EXTENSION_CAPABILITY_POLICY,
    )
    lease.value = next
    return next
  }

  const refresh = async (): Promise<void> => {
    const request = ++requestGeneration
    const operationLease = installDisabledLease()
    probing.value = true

    await settingsReady.catch(() => undefined)
    if (
      disposed ||
      request !== requestGeneration ||
      !operationLease.isCurrent()
    )
      return

    const activationRevision = settings.value.connectionRevision
    const client = buildClient({ ...settings.value })
    if (!client) {
      probing.value = false
      return
    }

    let capabilities
    try {
      capabilities = await client.getCapabilities()
    } catch {
      capabilities = null
    }
    if (
      disposed ||
      request !== requestGeneration ||
      !operationLease.isCurrent() ||
      settings.value.connectionRevision !== activationRevision
    ) {
      return
    }

    operationLease.revoke()
    lease.value = new ExtensionCapabilityLease(
      capabilities?.ok
        ? deriveExtensionCapabilityPolicy(capabilities.data)
        : DISABLED_EXTENSION_CAPABILITY_POLICY,
    )
    probing.value = false
  }

  const stopWatching = watch(
    () => settings.value.connectionRevision,
    () => {
      void refresh()
    },
    { immediate: true },
  )

  const dispose = (): void => {
    if (disposed) return
    disposed = true
    requestGeneration += 1
    lease.value.revoke()
    stopWatching()
  }
  onUnmounted(dispose)

  return {
    capabilityLease: readonly(lease),
    policy: computed(() => lease.value.policy),
    probing: readonly(probing),
    refresh,
    dispose,
  }
}

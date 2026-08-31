<script setup lang="ts">
import { computed, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NInput, NRadioButton, NRadioGroup } from 'naive-ui'
import { showToast } from '@/common/toast'
import {
  useWebTagSettings,
  type ConfirmedConnection,
} from '@/composables/useSettings'
import {
  DEFAULT_CAPTURE_DESTINATION,
  getCaptureDestination,
  normalizeCaptureDestination,
  setCaptureDestination,
  type CaptureDestination,
} from '@/api/capture-preferences'
import { buildWebTagClientFromSettings } from '@/api/webtag-client'
import type { ApiErrorKind } from '@/api/types'
import { useExtensionCapabilities } from '@/composables/useCapabilities'
import {
  createCaptureOwnerFingerprint,
  DISABLED_EXTENSION_CAPABILITY_POLICY,
} from '@/api/capabilities'

const { t } = useI18n()
const { settings, ready, confirmConnection, dispose } = useWebTagSettings()
const { policy: confirmedCapabilityPolicy } = useExtensionCapabilities(
  settings,
  ready,
)
onUnmounted(dispose)

const loaded = ref(false)
type ConnectionDraft = Pick<
  ConfirmedConnection,
  'backendUrl' | 'readerUrl' | 'accessToken'
>
const connectionDraft = ref<ConnectionDraft>({
  backendUrl: '',
  readerUrl: '',
  accessToken: '',
})
void ready.then(() => {
  connectionDraft.value = {
    backendUrl: settings.value.backendUrl,
    readerUrl: settings.value.readerUrl,
    accessToken: settings.value.accessToken,
  }
  loaded.value = true
})

const normalizeDraft = (draft: ConnectionDraft): ConnectionDraft => ({
  backendUrl: draft.backendUrl.trim(),
  readerUrl: draft.readerUrl.trim(),
  accessToken: draft.accessToken.trim(),
})

const draftMatchesConfirmed = computed(() => {
  const draft = normalizeDraft(connectionDraft.value)
  return (
    draft.backendUrl === settings.value.backendUrl.trim() &&
    draft.readerUrl === settings.value.readerUrl.trim() &&
    draft.accessToken === settings.value.accessToken.trim()
  )
})

const capabilityPolicy = computed(() =>
  draftMatchesConfirmed.value
    ? confirmedCapabilityPolicy.value
    : DISABLED_EXTENSION_CAPABILITY_POLICY,
)

const captureDestination = ref<CaptureDestination>(DEFAULT_CAPTURE_DESTINATION)
const destinationLoaded = ref(false)
const destinationSaving = ref(false)
const effectiveCaptureDestination = computed<CaptureDestination>(() =>
  captureDestination.value === 'inbox' && !capabilityPolicy.value.inbox
    ? 'library'
    : captureDestination.value,
)
void getCaptureDestination()
  .then((value) => {
    captureDestination.value = value
  })
  .finally(() => {
    destinationLoaded.value = true
  })

const handleDestinationChange = async (value: string): Promise<void> => {
  if (!destinationLoaded.value || destinationSaving.value) return
  const next = normalizeCaptureDestination(value)
  if (next === 'inbox' && !capabilityPolicy.value.inbox) return
  const previous = captureDestination.value
  captureDestination.value = next
  destinationSaving.value = true
  try {
    await setCaptureDestination(next)
  } catch {
    captureDestination.value = previous
    showToast.error(t('webtagSettings.saveFailed'))
  } finally {
    destinationSaving.value = false
  }
}

type ConnState = 'idle' | 'testing' | 'success' | 'error'
const connState = ref<ConnState>('idle')
const connErrorKind = ref<ApiErrorKind | null>(null)
const connectionTestRunning = ref(false)

const ERROR_MESSAGE_KEY: Record<ApiErrorKind, string> = {
  'not-modified': 'webtagSettings.testResultOther',
  'identity-mismatch': 'webtagSettings.testResultOther',
  unauthorized: 'webtagSettings.testResultUnauthorized',
  'network-unreachable': 'webtagSettings.testResultUnreachable',
  timeout: 'webtagSettings.testResultTimeout',
  'rate-limited': 'webtagSettings.testResultRateLimited',
  other: 'webtagSettings.testResultOther',
}

const connResultText = computed(() => {
  if (connState.value === 'testing') return t('webtagSettings.testTesting')
  if (connState.value === 'success')
    return t('webtagSettings.testResultSuccess')
  if (connState.value === 'error' && connErrorKind.value) {
    return t(ERROR_MESSAGE_KEY[connErrorKind.value])
  }
  return ''
})

const canTest = computed(
  () =>
    loaded.value &&
    !connectionTestRunning.value &&
    connectionDraft.value.backendUrl.trim().length > 0,
)

const resetConnectionResult = () => {
  connState.value = 'idle'
  connErrorKind.value = null
}

const handleTestConnection = async () => {
  if (!canTest.value || connectionTestRunning.value) return

  connectionTestRunning.value = true
  connState.value = 'testing'
  connErrorKind.value = null
  const candidate = Object.freeze(normalizeDraft(connectionDraft.value))
  try {
    const client = buildWebTagClientFromSettings(candidate)
    if (!client) {
      connState.value = 'error'
      connErrorKind.value = 'other'
      showToast.error(t(ERROR_MESSAGE_KEY.other))
      return
    }

    const result = await client.testConnection()
    if (!result.ok) {
      connState.value = 'error'
      connErrorKind.value = result.error.kind
      showToast.error(t(ERROR_MESSAGE_KEY[result.error.kind]))
      return
    }

    const identity = await client.getSessionIdentity()
    if (!identity.ok) {
      connState.value = 'error'
      connErrorKind.value = identity.error.kind
      showToast.error(t(ERROR_MESSAGE_KEY[identity.error.kind]))
      return
    }

    const connectionOwnerFingerprint = await createCaptureOwnerFingerprint(
      candidate.backendUrl,
      identity.data,
    )
    const commitResult = await confirmConnection({
      ...candidate,
      connectionOwnerFingerprint,
    })
    if (!commitResult.ok) {
      connState.value = 'error'
      connErrorKind.value = 'other'
      showToast.error(t(ERROR_MESSAGE_KEY.other))
      return
    }

    if (
      JSON.stringify(normalizeDraft(connectionDraft.value)) ===
      JSON.stringify(candidate)
    ) {
      connState.value = 'success'
      showToast.success(t('webtagSettings.testResultSuccess'))
    } else {
      resetConnectionResult()
    }
  } catch (error) {
    console.warn('[WebTag] connection test failed unexpectedly', error)
    connState.value = 'error'
    connErrorKind.value = 'other'
    showToast.error(t(ERROR_MESSAGE_KEY.other))
  } finally {
    connectionTestRunning.value = false
  }
}
</script>

<template>
  <section class="backend-settings">
    <header class="backend-settings__header">
      <h1>{{ t('webtagSettings.paneTitle') }}</h1>
      <p>{{ t('webtagSettings.connectionSection') }}</p>
    </header>

    <div class="backend-settings__fields">
      <label class="backend-settings__field">
        <span>{{ t('webtagSettings.backendUrlLabel') }}</span>
        <NInput
          :value="connectionDraft.backendUrl"
          :disabled="!loaded"
          :placeholder="t('webtagSettings.backendUrlPlaceholder')"
          clearable
          @update:value="
            (value: string) => {
              connectionDraft.backendUrl = value
              resetConnectionResult()
            }
          "
        />
        <small>{{ t('webtagSettings.backendUrlTip') }}</small>
      </label>

      <label class="backend-settings__field">
        <span>{{ t('webtagSettings.readerUrlLabel') }}</span>
        <NInput
          :value="connectionDraft.readerUrl"
          :disabled="!loaded"
          :placeholder="t('webtagSettings.readerUrlPlaceholder')"
          clearable
          @update:value="
            (value: string) => {
              connectionDraft.readerUrl = value
              resetConnectionResult()
            }
          "
        />
        <small>{{ t('webtagSettings.readerUrlTip') }}</small>
      </label>

      <label class="backend-settings__field">
        <span>{{ t('webtagSettings.tokenLabel') }}</span>
        <NInput
          :value="connectionDraft.accessToken"
          :disabled="!loaded"
          type="password"
          show-password-on="click"
          :placeholder="t('webtagSettings.tokenPlaceholder')"
          clearable
          @update:value="
            (value: string) => {
              connectionDraft.accessToken = value
              resetConnectionResult()
            }
          "
        />
        <small>{{ t('webtagSettings.tokenTip') }}</small>
      </label>

      <label class="backend-settings__field">
        <span>{{ t('webtagSettings.captureDestinationLabel') }}</span>
        <NRadioGroup
          :value="effectiveCaptureDestination"
          :disabled="!loaded || !destinationLoaded || destinationSaving"
          name="capture-destination"
          @update:value="handleDestinationChange"
        >
          <NRadioButton
            v-if="capabilityPolicy.inbox"
            value="inbox"
          >
            {{ t('webtagSettings.captureDestinationInbox') }}
          </NRadioButton>
          <NRadioButton value="library">
            {{ t('webtagSettings.captureDestinationLibrary') }}
          </NRadioButton>
        </NRadioGroup>
        <small>{{ t('webtagSettings.captureDestinationTip') }}</small>
        <small
          v-if="destinationSaving"
          class="backend-settings__saving"
          role="status"
        >
          {{ t('webtagSettings.captureDestinationSaving') }}
        </small>
      </label>
    </div>

    <div class="backend-settings__actions">
      <NButton
        :loading="connState === 'testing'"
        :disabled="!canTest"
        type="primary"
        @click="handleTestConnection"
      >
        {{ t('webtagSettings.testConnectionButton') }}
      </NButton>
      <span
        v-if="connResultText"
        class="backend-settings__result"
        :class="{
          'backend-settings__result--ok': connState === 'success',
          'backend-settings__result--error': connState === 'error',
        }"
        role="status"
      >
        {{ connResultText }}
      </span>
    </div>
  </section>
</template>

<style scoped>
.backend-settings {
  min-width: 0;
}

.backend-settings__header {
  padding-bottom: 20px;
  border-bottom: 1px solid var(--gray-alpha-10);
}

.backend-settings__header h1 {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: 650;
  color: var(--n-text-color-1);
}

.backend-settings__header p {
  margin: 6px 0 0;
  font-size: var(--text-sm);
  color: var(--n-text-color-3);
}

.backend-settings__fields {
  display: grid;
  gap: 22px;
  padding: 24px 0;
}

.backend-settings__field {
  display: grid;
  grid-template-columns: minmax(120px, 150px) minmax(0, 1fr);
  align-items: start;
  gap: 8px 18px;
}

.backend-settings__field > span {
  padding-top: 6px;
  font-size: var(--text-base);
  color: var(--n-text-color-2);
}

.backend-settings__field > small {
  grid-column: 2;
  font-size: var(--text-xs);
  line-height: 1.5;
  color: var(--n-text-color-3);
  opacity: var(--opacity-muted);
}

.backend-settings__actions {
  display: flex;
  align-items: center;
  gap: 12px;
  padding-top: 20px;
  border-top: 1px solid var(--gray-alpha-10);
}

.backend-settings__result {
  min-width: 0;
  font-size: var(--text-sm);
  color: var(--n-text-color-3);
}

.backend-settings__result--ok {
  color: var(--color-success);
}

.backend-settings__result--error {
  color: var(--color-error);
}

@media (max-width: 640px) {
  .backend-settings__field {
    grid-template-columns: minmax(0, 1fr);
  }

  .backend-settings__field > span {
    padding-top: 0;
  }

  .backend-settings__field > small {
    grid-column: 1;
  }

  .backend-settings__actions {
    align-items: flex-start;
    flex-direction: column;
  }
}
</style>

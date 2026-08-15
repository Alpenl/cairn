<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Icon } from '@iconify/vue'
import {
  buildWebTagClientFromSettings,
  type WebTagClient,
} from '@/api/webtag-client'
import type { ApiErrorKind } from '@/api/types'
import {
  getWebTagSettings,
  type WebTagSettings,
} from '@/composables/useSettings'
import { getRssDiscovery } from '@/popup/composables/useRssDiscovery'
import { resolveSubscriptionsReaderUrl } from '@/popup/reader-url'
import type { DiscoveredFeed } from '@/rss/protocol'

type DiscoveryState = 'loading' | 'ready' | 'restricted' | 'unavailable'
type FeedState =
  | 'checking'
  | 'available'
  | 'subscribing'
  | 'subscribed'
  | 'not-configured'
  | 'error'

interface FeedView extends DiscoveredFeed {
  state: FeedState
  errorKind?: ApiErrorKind
}

const { t } = useI18n()
const discoveryState = ref<DiscoveryState>('loading')
const feeds = ref<FeedView[]>([])
const readerUrl = ref<string | null>(null)
let client: WebTagClient | null = null

const hasFeeds = computed(() => feeds.value.length > 0)
const isNotConfigured = computed(() =>
  feeds.value.some((feed) => feed.state === 'not-configured'),
)

const errorText = (feed: FeedView): string => {
  if (!feed.errorKind) return ''
  return t(`subscriptions.error.${feed.errorKind}`)
}

const checkFeed = async (feed: FeedView): Promise<void> => {
  if (!client) {
    feed.state = 'not-configured'
    return
  }
  feed.state = 'checking'
  feed.errorKind = undefined
  const result = await client.findSubscriptionByUrl(feed.url)
  if (result.ok) {
    feed.state = result.data ? 'subscribed' : 'available'
    return
  }
  feed.state = 'error'
  feed.errorKind = result.error.kind
}

const subscribe = async (feed: FeedView): Promise<void> => {
  if (feed.state === 'not-configured') {
    void chrome.runtime.openOptionsPage()
    return
  }
  if (!client || feed.state === 'subscribing' || feed.state === 'subscribed') {
    return
  }
  if (feed.state === 'error' || feed.state === 'checking') {
    await checkFeed(feed)
    return
  }

  feed.state = 'subscribing'
  feed.errorKind = undefined
  const result = await client.createSubscription(feed.url)
  if (result.ok) {
    feed.state = 'subscribed'
    return
  }

  // A status check can race another popup or Reader tab. Re-check a conflict
  // before showing an error so an already-created subscription is recognized.
  if (result.error.status === 409) {
    const status = await client.findSubscriptionByUrl(feed.url)
    if (status.ok && status.data) {
      feed.state = 'subscribed'
      return
    }
  }
  feed.state = 'error'
  feed.errorKind = result.error.kind
}

const buttonText = (feed: FeedView): string => {
  const keyByState: Record<FeedState, string> = {
    checking: 'subscriptions.checking',
    available: 'subscriptions.subscribe',
    subscribing: 'subscriptions.subscribing',
    subscribed: 'subscriptions.subscribed',
    'not-configured': 'subscriptions.openSettings',
    error: 'subscriptions.retry',
  }
  return t(keyByState[feed.state])
}

const buttonIcon = (feed: FeedView): string => {
  if (feed.state === 'subscribed') return 'ion:checkmark-circle-outline'
  if (feed.state === 'error') return 'ion:refresh-outline'
  if (feed.state === 'not-configured') return 'ion:settings-outline'
  return 'ion:add-circle-outline'
}

const openReader = (): void => {
  if (readerUrl.value) void chrome.tabs.create({ url: readerUrl.value })
}

const openSettings = (): void => {
  void chrome.runtime.openOptionsPage()
}

const load = async (): Promise<void> => {
  let tab: chrome.tabs.Tab | undefined
  try {
    ;[tab] = await chrome.tabs.query({ active: true, currentWindow: true })
  } catch {
    discoveryState.value = 'unavailable'
    return
  }
  if (tab?.id === undefined || !tab.url) {
    discoveryState.value = 'restricted'
    return
  }

  const discovery = await getRssDiscovery(tab.id, tab.url)
  if (!discovery.ok) {
    discoveryState.value = discovery.error
    return
  }
  feeds.value = discovery.state.feeds.map((feed) => ({
    ...feed,
    state: 'checking',
  }))
  discoveryState.value = 'ready'
  if (!feeds.value.length) return

  try {
    const settings: WebTagSettings = await getWebTagSettings()
    readerUrl.value = resolveSubscriptionsReaderUrl(settings)
    client = buildWebTagClientFromSettings(settings)
  } catch {
    client = null
  }
  await Promise.all(feeds.value.map(checkFeed))
}

onMounted(load)
</script>

<template>
  <section
    class="subscription-discovery"
    aria-live="polite"
  >
    <div class="subscription-discovery__header">
      <div class="subscription-discovery__heading">
        <Icon icon="ion:logo-rss" />
        <h2>{{ t('subscriptions.title') }}</h2>
      </div>
      <button
        v-if="hasFeeds && readerUrl"
        type="button"
        class="subscription-discovery__reader"
        :title="t('subscriptions.openReader')"
        :aria-label="t('subscriptions.openReader')"
        @click="openReader"
      >
        <Icon icon="ion:open-outline" />
      </button>
    </div>

    <p
      v-if="discoveryState === 'loading'"
      class="subscription-discovery__message"
    >
      {{ t('subscriptions.discovering') }}
    </p>
    <p
      v-else-if="discoveryState === 'restricted'"
      class="subscription-discovery__message"
    >
      {{ t('subscriptions.restricted') }}
    </p>
    <p
      v-else-if="discoveryState === 'unavailable'"
      class="subscription-discovery__message subscription-discovery__message--error"
    >
      {{ t('subscriptions.unavailable') }}
    </p>
    <p
      v-else-if="!hasFeeds"
      class="subscription-discovery__message"
    >
      {{ t('subscriptions.noneFound') }}
    </p>

    <div
      v-if="hasFeeds"
      class="subscription-discovery__list"
    >
      <article
        v-for="feed in feeds"
        :key="feed.url"
        class="subscription-discovery__feed"
      >
        <div class="subscription-discovery__feed-info">
          <strong :title="feed.title">{{ feed.title || feed.url }}</strong>
          <span :title="feed.url">{{ feed.url }}</span>
          <small>{{ feed.format.toUpperCase() }}</small>
        </div>
        <NButton
          size="small"
          :type="feed.state === 'available' ? 'primary' : 'default'"
          :loading="feed.state === 'checking' || feed.state === 'subscribing'"
          :disabled="feed.state === 'subscribed'"
          @click="subscribe(feed)"
        >
          <template #icon>
            <Icon :icon="buttonIcon(feed)" />
          </template>
          {{ buttonText(feed) }}
        </NButton>
        <p
          v-if="errorText(feed)"
          class="subscription-discovery__feed-error"
        >
          {{ errorText(feed) }}
        </p>
      </article>
    </div>

    <button
      v-if="isNotConfigured"
      type="button"
      class="subscription-discovery__config-note"
      @click="openSettings"
    >
      {{ t('subscriptions.notConfigured') }}
    </button>
  </section>
</template>

<style scoped>
.subscription-discovery {
  display: grid;
  gap: 8px;
  min-width: 0;
  padding-top: 2px;
}

.subscription-discovery__header,
.subscription-discovery__heading {
  display: flex;
  align-items: center;
}

.subscription-discovery__header {
  justify-content: space-between;
  gap: 8px;
}

.subscription-discovery__heading {
  min-width: 0;
  gap: 6px;
  color: #b45309;
}

.subscription-discovery__heading h2 {
  margin: 0;
  color: var(--n-text-color-1);
  font-size: var(--text-sm, 13px);
  font-weight: 600;
}

.subscription-discovery__reader {
  display: grid;
  flex: 0 0 28px;
  width: 28px;
  height: 28px;
  padding: 0;
  border: 0;
  border-radius: 6px;
  place-items: center;
  color: var(--n-text-color-3);
  background: transparent;
  cursor: pointer;
}

.subscription-discovery__reader:hover,
.subscription-discovery__reader:focus-visible {
  color: var(--n-text-color-1);
  background: var(--gray-alpha-100, rgba(128, 128, 128, 0.08));
}

.subscription-discovery__message {
  margin: 0;
  font-size: var(--text-xs, 12px);
  line-height: 1.45;
  color: var(--n-text-color-3);
}

.subscription-discovery__message--error,
.subscription-discovery__feed-error {
  color: var(--color-error, #c92a2a);
}

.subscription-discovery__list {
  display: grid;
  gap: 8px;
}

.subscription-discovery__feed {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  align-items: center;
  gap: 8px;
  padding: 9px 10px;
  border: 1px solid var(--gray-alpha-10, rgba(128, 128, 128, 0.1));
  border-radius: 6px;
}

.subscription-discovery__feed-info {
  display: grid;
  min-width: 0;
  gap: 2px;
}

.subscription-discovery__feed-info strong,
.subscription-discovery__feed-info span {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.subscription-discovery__feed-info strong {
  font-size: var(--text-sm, 13px);
  color: var(--n-text-color-1);
}

.subscription-discovery__feed-info span,
.subscription-discovery__feed-info small {
  font-size: var(--text-xs, 12px);
  color: var(--n-text-color-3);
}

.subscription-discovery__feed-error {
  grid-column: 1 / -1;
  margin: 0;
  font-size: var(--text-xs, 12px);
  line-height: 1.4;
}

.subscription-discovery__config-note {
  padding: 7px 9px;
  border: 0;
  border-radius: 6px;
  text-align: left;
  font-size: var(--text-xs, 12px);
  color: var(--n-text-color-2);
  background: var(--gray-alpha-100, rgba(128, 128, 128, 0.08));
  cursor: pointer;
}
</style>

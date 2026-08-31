<script setup lang="ts">
/**
 * CaptureForm — 当前页采集表单（扩展图标 popup 的主界面）。
 *
 * 实现扩展图标 popup 的主采集流程：
 *   - 顶部常驻设置入口（齿轮图标，任何状态可达 → 打开设置页）
 *   - 当前页标题 + URL（只读展示）
 *   - 可选多行备注输入框
 *   - 「采集到 WebTag」主按钮
 *   - 采集状态区：展示最近一次采集任务状态
 *     （已提交 / 解析中 / 完成 / 仍在解析中 / 失败，失败带原因），
 *     并标注该结果对应的页面标题/URL，让用户明确「这是哪个页面的结果」。
 *
 * 数据流：
 *   - 当前页标题/URL：popup 打开时 chrome.tabs.query 取活动标签
 *   - 采集触发与状态：经 useCaptureBridge 与 background captureHandler 通信
 *   - popup 打开时先 fetchLatest 拉取 background 的最近快照（覆盖「popup 关闭后
 *     重开仍显示上次进度」场景），并订阅 onUpdate 接收后续推送
 *
 * 快照防误读（本次 UX 加固）：
 *   - fetchLatest 回贴的快照若是**陈旧的终态**（done/failed/still-processing 且
 *     生成时间超过 SNAPSHOT_STALE_MS），归 idle 不回贴——避免用户隔很久重开
 *     popup 仍看到上次某个无关页面的「采集完成」，误以为是当前页的结果。
 *   - 状态区渲染快照自带的 url/title，明确「这是哪个页面的结果」。
 *
 * 所有面向用户文案走 vue-i18n（capture.* 命名空间）。失败原因分两类：
 * 客户端错误按 errorKind 取 capture.error.* 本地化文案；后端任务失败则
 * 原样渲染后端下发的 errorMessage（动态文案，非 i18n key）。
 */
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Icon } from '@iconify/vue'
import { NButton, NInput, NRadioButton, NRadioGroup } from 'naive-ui'
import { useCaptureBridge } from '@/popup/composables/useCaptureBridge'
import { isForbiddenUrl } from '@/env'
import type {
  CaptureSnapshot,
  CaptureStage,
  RequestedLibraryKind,
} from '@/background/capture-protocol'

const props = withDefaults(
  defineProps<{
    siteEnabled?: boolean
  }>(),
  { siteEnabled: false },
)

const { t } = useI18n()
const bridge = useCaptureBridge()
const SETTINGS_ICON = 'ion:settings-outline'

/**
 * 终态快照的过期阈值（毫秒）。fetchLatest 回贴时，超过此时长的终态
 * （done/failed/still-processing）快照被视为陈旧、归 idle 不回贴：
 * 避免用户隔很久重开 popup 仍看到上次无关页面的结果，误以为是当前页。
 * 取 10 分钟：足够覆盖「刚采完顺手重开 popup 想确认」的正常间隔，
 * 又能让长期搁置后重开的 popup 干净回到 idle。
 */
const SNAPSHOT_STALE_MS = 10 * 60 * 1000

// ── 当前页信息 ──────────────────────────────────────────────

/** 当前活动标签的标题与 URL（只读展示）。 */
const pageTitle = ref('')
const pageUrl = ref('')
/**
 * popup 打开时解析到的活动标签 id。
 * 触发采集时连同消息一起传给 background，保证采集的是「用户看到的那个标签」，
 * 而非 background 自行 re-query 出的可能已切换的活动标签。
 */
const pageTabId = ref<number | undefined>(undefined)

/** 用户填写的可选备注。 */
const note = ref('')
/** 每次打开默认自动，不继承上一次的显式采集去向。 */
const requestedKind = ref<RequestedLibraryKind>('auto')
watch(
  () => props.siteEnabled,
  (enabled) => {
    if (!enabled && requestedKind.value === 'site') requestedKind.value = 'auto'
  },
)

/**
 * 当前活动页是否为受限页面（chrome:// / 应用商店 / 扩展页等）。
 * 受限页面无法注入采集脚本，据此禁用采集按钮并给出明确提示，
 * 而非让用户点击后落到一个泛化的失败结果。
 */
const isRestrictedPage = computed<boolean>(
  () => pageUrl.value.length > 0 && isForbiddenUrl(pageUrl.value),
)

// ── 采集状态 ────────────────────────────────────────────────

/** 最近一次采集快照。idle 表示尚无采集记录。 */
const snapshot = ref<CaptureSnapshot>({
  stage: 'idle',
  url: '',
  title: '',
  updatedAt: 0,
})

/** 「采集中」期间（capturing/submitted/parsing）禁用按钮，避免重复提交。 */
const isBusy = computed<boolean>(() => {
  const s = snapshot.value.stage
  return s === 'capturing' || s === 'submitted' || s === 'parsing'
})

/** 采集按钮是否禁用：采集中、或当前页为受限页面。 */
const isSubmitDisabled = computed<boolean>(
  () => isBusy.value || isRestrictedPage.value,
)

/** 当前阶段对应的状态文案。idle 时为空。 */
const stageText = computed<string>(() => {
  const STAGE_TEXT_KEY: Record<Exclude<CaptureStage, 'idle'>, string> = {
    capturing: 'capture.statusCapturing',
    submitted: 'capture.statusSubmitted',
    parsing: 'capture.statusParsing',
    done: 'capture.statusDone',
    'still-processing': 'capture.statusStillProcessing',
    failed: 'capture.statusFailed',
  }
  const stage = snapshot.value.stage
  if (stage === 'idle') return ''
  return t(STAGE_TEXT_KEY[stage])
})

/** 状态区是否可见（idle 时整块隐藏）。 */
const showStatus = computed<boolean>(() => snapshot.value.stage !== 'idle')

/** 是否为「仍在解析中」中性终态（非失败）—— 据此展示中性提示文案。 */
const isStillProcessing = computed<boolean>(
  () => snapshot.value.stage === 'still-processing',
)

/**
 * 状态区展示的结果页面信息（快照自带的 title/url）。
 * 让用户明确「这是哪个页面的采集结果」——尤其在 popup 重开、活动标签已切换时，
 * 状态区对应的页面可能与顶部当前页信息不同。title 为空时回退 url。
 */
const resultPageLabel = computed<string>(() => {
  const s = snapshot.value
  const title = s.title?.trim()
  if (title) return title
  return s.url?.trim() || ''
})

/**
 * 失败时展示的原因文案。
 *
 * background 对失败的两类来源分别处理：
 *   - 客户端侧错误（注入失败 / 未配置 / 鉴权 / 网络 / 超时等）：只下发
 *     errorKind，不带 errorMessage —— 这里按 errorKind 取 capture.error.*
 *     的本地化文案，保证 en-US 用户看到英文。
 *   - 后端任务失败：errorMessage 是后端返回的真实失败原因（动态文案），
 *     原样渲染（Vue 文本插值自动转义）。
 * 两者皆缺时回退到 capture.statusFailed。
 */
const failureReason = computed<string>(() => {
  if (snapshot.value.stage !== 'failed') return ''
  // 后端真实失败原因优先原样展示。
  const backendMessage = snapshot.value.errorMessage
  if (backendMessage) return backendMessage
  // 客户端侧错误：按 errorKind 取本地化文案。
  const kind = snapshot.value.errorKind
  if (kind) return t(`capture.error.${kind}`)
  return t('capture.statusFailed')
})

/** 失败原因是否为「后端未配置」——据此提示用户去设置页。 */
const isUnconfigured = computed<boolean>(
  () => snapshot.value.errorKind === 'not-configured',
)

/**
 * 提交按钮文案：忙碌中显示「采集中…」；空闲时固定显示「采集到 WebTag」。
 */
const submitButtonText = computed<string>(() => {
  if (isBusy.value) return t('capture.submitting')
  return t('capture.submitButton')
})

// ── 交互 ────────────────────────────────────────────────────

/** 点击「采集到 WebTag」：触发采集。 */
const handleSubmit = async (): Promise<void> => {
  // 采集中或受限页面：直接忽略（按钮本应已禁用，这里再兜一层）。
  if (isSubmitDisabled.value) return
  // 乐观置为 capturing，给用户即时反馈（background 也会推送同一阶段）。
  const requested =
    requestedKind.value === 'site' && !props.siteEnabled
      ? 'auto'
      : requestedKind.value
  snapshot.value = {
    stage: 'capturing',
    url: pageUrl.value,
    title: pageTitle.value,
    updatedAt: Date.now(),
    requestedKind: requested,
  }
  try {
    // 传 popup 已解析的 tabId，保证采集到「用户看到的那个标签」。
    const result = await bridge.start(note.value, pageTabId.value, requested)
    // 用 background 返回的初始快照覆盖（含真实抓取到的 url/title 或失败信息）。
    applySnapshot(result)
  } catch (error) {
    // The optimistic state must not leave the popup permanently disabled when
    // the message bridge rejects before background can publish a snapshot.
    console.warn('[capture] start message failed unexpectedly', error)
    snapshot.value = {
      stage: 'failed',
      url: pageUrl.value,
      title: pageTitle.value,
      errorKind: 'capture-unexpected',
      requestedKind: requested,
      updatedAt: Date.now(),
    }
  }
}

/** 打开扩展设置页（options 页）。 */
const handleOpenSettings = (): void => {
  chrome.runtime.openOptionsPage()
}

/**
 * 应用一份新快照：仅当它比当前快照更新时才覆盖，避免乱序推送回退状态。
 */
const applySnapshot = (next: CaptureSnapshot): void => {
  if (next.updatedAt >= snapshot.value.updatedAt) {
    snapshot.value = next
  }
}

/**
 * 判断一份快照是否为「陈旧终态」：终态（done/failed/still-processing）且
 * 生成时间已超过 SNAPSHOT_STALE_MS。陈旧终态在 popup 重开时不应回贴，
 * 避免误导用户以为是当前页的结果。中间态（capturing/submitted/parsing）
 * 不视为陈旧——它们代表「还在进行」，重开 popup 应继续展示进度。
 */
const isStaleTerminalSnapshot = (s: CaptureSnapshot): boolean => {
  const isTerminal =
    s.stage === 'done' || s.stage === 'failed' || s.stage === 'still-processing'
  if (!isTerminal) return false
  return Date.now() - s.updatedAt > SNAPSHOT_STALE_MS
}

// ── 生命周期 ────────────────────────────────────────────────

onMounted(async () => {
  // 读取当前活动标签信息用于只读展示，并记录 tabId 供采集时传给 background。
  try {
    const [activeTab] = await chrome.tabs.query({
      active: true,
      currentWindow: true,
    })
    pageTitle.value = activeTab?.title || ''
    pageUrl.value = activeTab?.url || ''
    pageTabId.value = activeTab?.id
  } catch {
    // 查询失败（极少见）：保持空串，不阻塞采集。
  }

  // 订阅 background 的后续状态推送。
  bridge.onUpdate(applySnapshot)

  // 拉取 background 持有的最近一次采集快照（popup 重开时恢复进度展示）。
  try {
    const latest = await bridge.fetchLatest()
    // 陈旧终态快照不回贴：避免误导用户以为是当前页的结果（归 idle）。
    if (!isStaleTerminalSnapshot(latest)) {
      applySnapshot(latest)
    }
  } catch {
    // background 未响应（如 SW 刚被回收）：保持 idle，用户可重新采集。
  }
})
</script>

<template>
  <div class="capture-form">
    <!-- 顶部栏：标题 + 常驻设置入口（任何状态可达）。 -->
    <div class="capture-form__header">
      <h1 class="capture-form__title">
        {{ t('capture.title') }}
      </h1>
      <button
        type="button"
        class="capture-form__settings-btn"
        :title="t('capture.openSettingsAria')"
        :aria-label="t('capture.openSettingsAria')"
        @click="handleOpenSettings"
      >
        <Icon
          class="capture-form__settings-icon"
          :icon="SETTINGS_ICON"
        />
      </button>
    </div>

    <!-- 当前页信息（只读） -->
    <div class="capture-form__page">
      <div
        class="capture-form__page-title"
        :title="pageTitle"
      >
        {{ pageTitle || t('capture.pageTitleFallback') }}
      </div>
      <div
        class="capture-form__page-url"
        :title="pageUrl"
      >
        {{ pageUrl }}
      </div>
    </div>

    <slot name="after-page" />

    <NRadioGroup
      v-model:value="requestedKind"
      class="capture-form__kind"
      name="requested-library-kind"
      :disabled="isBusy"
      :aria-label="t('capture.kindLabel')"
    >
      <NRadioButton value="auto">{{ t('capture.kindAuto') }}</NRadioButton>
      <NRadioButton value="reading">
        {{ t('capture.kindReading') }}
      </NRadioButton>
      <NRadioButton
        v-if="props.siteEnabled"
        value="site"
      >
        {{ t('capture.kindSite') }}
      </NRadioButton>
    </NRadioGroup>

    <!-- 可选备注 -->
    <NInput
      v-model:value="note"
      class="capture-form__note"
      type="textarea"
      :rows="3"
      :placeholder="t('capture.notePlaceholder')"
      :disabled="isBusy"
    />

    <!-- 采集按钮 -->
    <NButton
      class="capture-form__submit"
      type="primary"
      block
      :loading="isBusy"
      :disabled="isSubmitDisabled"
      @click="handleSubmit"
    >
      {{ submitButtonText }}
    </NButton>

    <!-- 受限页面提示：chrome:// / 应用商店等无法采集 -->
    <div
      v-if="isRestrictedPage"
      class="capture-form__restricted"
    >
      {{ t('capture.restrictedPage') }}
    </div>

    <!-- 采集状态区 -->
    <div
      v-if="showStatus"
      class="capture-form__status"
      :class="{
        'capture-form__status--done': snapshot.stage === 'done',
        'capture-form__status--failed': snapshot.stage === 'failed',
      }"
    >
      <div class="capture-form__status-line">
        {{ stageText }}
      </div>
      <!-- 结果对应的页面（快照自带 title/url），明确「这是哪个页面的结果」。 -->
      <div
        v-if="resultPageLabel"
        class="capture-form__status-page"
        :title="resultPageLabel"
      >
        {{ t('capture.resultPageLabel') }}：{{ resultPageLabel }}
      </div>
      <!-- 仍在解析中：中性提示，引导去知识库桌面查看（非红色 error）。 -->
      <div
        v-if="isStillProcessing"
        class="capture-form__status-reason"
      >
        {{ t('capture.statusStillProcessingHint') }}
      </div>
      <!-- 失败原因 -->
      <div
        v-if="failureReason"
        class="capture-form__status-reason"
      >
        {{ failureReason }}
      </div>
      <!-- 未配置：引导去设置页 -->
      <NButton
        v-if="isUnconfigured"
        class="capture-form__status-action"
        size="tiny"
        @click="handleOpenSettings"
      >
        {{ t('capture.openSettings') }}
      </NButton>
    </div>
  </div>
</template>

<style scoped>
.capture-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-3, 12px);
  width: 320px;
  padding: 16px;
  box-sizing: border-box;

  .capture-form__header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2, 8px);
    min-width: 0;
  }

  .capture-form__title {
    flex: 1;
    min-width: 0;
    margin: 0;
    font-size: var(--text-lg, 16px);
    font-weight: 600;
    color: var(--n-text-color-1);
  }

  .capture-form__settings-btn {
    flex-shrink: 0;
    display: flex;
    align-items: center;
    justify-content: center;
    width: 28px;
    height: 28px;
    padding: 0;
    border: none;
    border-radius: 6px;
    background: transparent;
    color: var(--n-text-color-3);
    cursor: pointer;
    transition:
      background-color var(--transition-fast, 0.15s),
      color var(--transition-fast, 0.15s);
  }

  .capture-form__settings-btn:hover {
    color: var(--n-text-color-1);
    background-color: var(--gray-alpha-100, rgba(128, 128, 128, 0.08));
  }

  .capture-form__settings-btn:focus-visible {
    outline: 2px solid var(--gray-alpha-35, rgba(128, 128, 128, 0.35));
    outline-offset: -2px;
  }

  .capture-form__settings-icon {
    font-size: 18px;
  }

  .capture-form__page {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
    padding: 10px 12px;
    border-radius: 8px;
    background-color: var(--gray-alpha-100, rgba(128, 128, 128, 0.08));
  }

  .capture-form__kind {
    display: flex;
    width: 100%;
  }

  .capture-form__page-title {
    font-size: var(--text-sm, 13px);
    font-weight: 500;
    color: var(--n-text-color-1);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .capture-form__page-url {
    font-size: var(--text-xs, 12px);
    color: var(--n-text-color-3);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .capture-form__restricted {
    padding: 10px 12px;
    border-radius: 8px;
    font-size: var(--text-xs, 12px);
    line-height: 1.5;
    color: var(--n-text-color-3);
    background-color: var(--gray-alpha-100, rgba(128, 128, 128, 0.08));
  }

  .capture-form__status {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 10px 12px;
    border-radius: 8px;
    background-color: var(--gray-alpha-100, rgba(128, 128, 128, 0.08));
  }

  .capture-form__status-line {
    font-size: var(--text-sm, 13px);
    font-weight: 500;
    color: var(--n-text-color-2);
  }

  .capture-form__status-page {
    font-size: var(--text-xs, 12px);
    color: var(--n-text-color-3);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .capture-form__status-reason {
    font-size: var(--text-xs, 12px);
    line-height: 1.5;
    color: var(--n-text-color-3);
  }

  .capture-form__status-action {
    align-self: flex-start;
  }
}

.capture-form__status--done .capture-form__status-line {
  color: var(--color-success, #22c55e);
}

.capture-form__status--failed .capture-form__status-line {
  color: var(--color-error, #ef4444);
}
</style>

/**
 * @module background/capture-store
 * 采集状态持久化层 —— 把「进行中采集任务」与「最近一次采集快照」加密封装后
 * 写入 chrome.storage.session，让 MV3 Service Worker 被回收后仍能恢复。
 *
 * 为什么需要它（MV3 SW 回收问题）：
 *   Chrome MV3 的 Service Worker 空闲约 30s 后会被回收。采集轮询
 *   （runCapturePolling）与状态（latestSnapshot / captureInFlight）原先只活在
 *   controller 闭包 + 一个 setTimeout 循环里——SW 一旦被回收，循环凭空消失，
 *   popup 永远卡在「解析中」，重新唤醒的 SW 又只能返回 idle。
 *   把状态落盘后：① 重唤醒的 SW 能读回真实进度；② 看门狗 alarm 可据此续跑轮询。
 *
 * 为什么选 chrome.storage.session 而非 local：
 *   in-flight 采集状态是「短暂的、随浏览器会话存在」的临时数据——浏览器关闭后
 *   未完成的采集本就该作废。storage.session 内存级、随会话清空，语义最贴合，
 *   也避免污染持久化的 local 存储。
 *
 * 设计：所有 chrome.storage.session IO 和 AES-GCM 封装收口在
 * createSessionCaptureStore 返回的适配器里。captureHandler 只依赖抽象
 * CaptureStore 接口，单测传入内存 stub，不需要给 chrome 全局打补丁。
 *
 * 消费者：src/background/captureHandler.ts（采集编排）、
 *         src/background/capture-messaging.ts（生产侧组装、看门狗）。
 */

import type { CaptureSnapshot, RequestedLibraryKind } from './capture-protocol'
import { IDLE_CAPTURE_SNAPSHOT } from './capture-protocol'
import type { CaptureRequestDestination } from '@/api/capture-preferences'
import type { IngestRequest, IngestSource } from '@/api/types'
import { captureOwnersEqual, type CaptureOwner } from '@/api/capabilities'

// ── 持久化数据形状 ──────────────────────────────────────────

/**
 * 一个「进行中」采集任务的完整运行时形状。
 *
 * 它在提交前以及轮询阶段存在；终态（done/failed）一到即清除。存储层
 * 会把整个对象封装进 AES-GCM 密文；storage.session 明文中只留下所有者、
 * 阶段、轮询次数和开始时间等续跑授权所需的非敏感元数据。
 *
 * 失效自愈（fail-safe）：本记录带 `startedAt` 时间戳。读取方（看门狗续跑、
 * SW 重建后的并发去重）必须用 isInFlightStale 检查它是否已超出采集预算——
 * 一旦 `clearInFlight` 因 storage 故障被丢弃，陈旧记录会在预算到期后被自动
 * 视为失效，而不是把整个浏览器会话的后续采集永久锁死。
 */
export interface InFlightCapture {
  /** Non-secret installation activation owner; required for every replay. */
  owner: CaptureOwner
  /** 后端 Link id，续跑轮询时作为 getLink 的入参；提交前暂为空串。 */
  linkId: string
  /** 被采集页面 URL，用于续跑时构造中间态/终态快照。 */
  url: string
  /** 被采集页面标题，同上。 */
  title: string
  /** 用户备注（右键菜单为空串）。保留以便未来重提交，当前续跑不重提交。 */
  note: string
  /** Submission phase is persisted before the first POST can become ambiguous. */
  phase?: 'submitting' | 'polling'
  /** Exact normalized source used with the idempotency key during resumption. */
  source?: IngestSource
  /** Exact request body used for replay; prevents a capability re-probe from changing it. */
  ingestRequest?: IngestRequest
  /** Requested destination before/after capability downgrade. */
  destination?: CaptureRequestDestination
  /** Reused for an ambiguous network result and after a worker restart. */
  idempotencyKey?: string
  /** Optional for compatibility with in-flight records persisted by older builds. */
  requestedKind?: RequestedLibraryKind
  /**
   * 已完成的轮询次数（从 0 计）。每轮 getLink 后递增并落盘——
   * SW 被回收后看门狗据此从断点续跑，而非从头重数。
   */
  attempt: number
  /** 最近一次发布的快照，作为续跑前的展示兜底。 */
  snapshot: CaptureSnapshot
  /**
   * 本采集任务的创建时间戳（毫秒）。仅在首次写入 in-flight 记录时设定，
   * 后续每轮续跑落盘都沿用同一个值——它衡量「采集开始至今」的墙钟时长。
   *
   * 看门狗续跑与 SW 重建后的并发去重读取本记录时，用 isInFlightStale 判断
   * 它是否已超出采集预算。超出则视为失效（一个被丢弃的 clearInFlight 留下
   * 的僵尸记录），不再阻塞新采集、也不再续跑。
   */
  startedAt: number
}

/**
 * 采集任务的最大墙钟预算（毫秒）。
 *
 * = 轮询间隔 × 最大轮询次数 + 余量。看门狗 alarm 周期为 1 分钟，SW 被回收
 * 期间不计入轮询；这里给出一个相对宽裕的余量（多个看门狗周期），保证一次
 * 正常采集（即使中途多次被 SW 回收）不会被误判为陈旧。
 *
 * 超过此预算的 in-flight 记录被视为 STALE：要么对应采集早已结束、
 * `clearInFlight` 因 storage 故障被丢弃留下僵尸记录，要么采集异常卡死。
 * 两种情况都应放行新采集，而非永久锁死。
 *
 * 最大预算 = 轮询间隔 × 最大轮询次数 + 本余量常量。轮询部分由
 * capture-poll.ts 的 POLL_INTERVAL_MS / MAX_POLL_ATTEMPTS 推导，在
 * createCaptureController 处计算后注入——capture-store 不直接依赖
 * capture-poll，避免反向耦合。本常量是该相加式中的固定余量项，
 * 不存在下限校验或 clamp 逻辑。
 */
export const STALE_INFLIGHT_MARGIN_MS = 5 * 60 * 1000

/**
 * 判断一条 in-flight 记录是否已陈旧（超出采集墙钟预算）。
 *
 * @param inFlight   待检查的 in-flight 记录
 * @param maxAgeMs   采集墙钟预算上限（轮询预算 + 余量）
 * @param nowMs      当前时间戳
 * @returns true 表示记录陈旧，应被忽略（不阻塞新采集、不续跑）
 */
export function isInFlightStale(
  inFlight: Pick<InFlightCapture, 'startedAt'>,
  maxAgeMs: number,
  nowMs: number,
): boolean {
  // 缺失或非法 startedAt（老数据 / 脏数据）一律视为陈旧，避免无界阻塞。
  if (typeof inFlight.startedAt !== 'number' || inFlight.startedAt <= 0) {
    return true
  }
  return nowMs - inFlight.startedAt > maxAgeMs
}

// ── 抽象接口 ────────────────────────────────────────────────

/**
 * 采集状态存储抽象。captureHandler 只认这个接口，
 * 生产实现见 createSessionCaptureStore，测试可传内存 stub。
 */
export interface CaptureStore {
  /** Owner, null for ownerless/absent, or undefined when the read was unavailable. */
  getSnapshotOwner: () => Promise<CaptureOwner | null | undefined>
  /** 读取最近一次采集快照；从未采集过时返回 IDLE_CAPTURE_SNAPSHOT。 */
  getSnapshot: (recoveryKey?: CryptoKey) => Promise<CaptureSnapshot>
  /** 写入最近一次采集快照。 */
  setSnapshot: (
    snapshot: CaptureSnapshot,
    recoveryKey?: CryptoKey,
  ) => Promise<void>
  /** Read non-secret owner/age metadata before authorizing payload decryption. */
  getInFlightMetadata: () => Promise<InFlightCaptureMetadata | null>
  /** 读取进行中的采集任务；无则返回 null。 */
  getInFlight: (recoveryKey?: CryptoKey) => Promise<InFlightCapture | null>
  /** 写入/更新进行中的采集任务。 */
  setInFlight: (
    inFlight: InFlightCapture,
    recoveryKey?: CryptoKey,
  ) => Promise<void>
  /** 清除进行中的采集任务（采集到达终态时调用）。 */
  clearInFlight: () => Promise<void>
}

export interface InFlightCaptureMetadata {
  owner: CaptureOwner
  phase?: InFlightCapture['phase']
  attempt: number
  startedAt: number
}

// ── storage.session 键名 ────────────────────────────────────

/** storage.session 中存「最近一次采集快照」的键。 */
export const STORAGE_KEY_SNAPSHOT = 'webtag:capture-snapshot'
/** storage.session 中存「进行中采集任务」的键。 */
export const STORAGE_KEY_INFLIGHT = 'webtag:capture-inflight'

const CAPTURE_STAGES = new Set([
  'idle',
  'capturing',
  'submitted',
  'parsing',
  'done',
  'still-processing',
  'failed',
])
const REQUESTED_LIBRARY_KINDS = new Set(['auto', 'reading', 'site'])
const FINAL_LIBRARY_KINDS = new Set(['reading', 'site'])
const SOURCE_KINDS = new Set(['url', 'text', 'image', 'browser_capture'])

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

const OWNER_KEYS = new Set(['fingerprint', 'revision'])

function isCaptureOwner(value: unknown): value is CaptureOwner {
  if (!isRecord(value)) return false
  if (Object.keys(value).some((key) => !OWNER_KEYS.has(key))) return false
  return (
    typeof value.fingerprint === 'string' &&
    /^[a-f0-9]{64}$/.test(value.fingerprint) &&
    typeof value.revision === 'number' &&
    Number.isSafeInteger(value.revision) &&
    value.revision >= 0
  )
}

/** Reject corrupted session data before it can block capture recovery. */
function isCaptureSnapshot(value: unknown): value is CaptureSnapshot {
  if (
    !isRecord(value) ||
    typeof value.stage !== 'string' ||
    !CAPTURE_STAGES.has(value.stage) ||
    typeof value.url !== 'string' ||
    typeof value.title !== 'string' ||
    typeof value.updatedAt !== 'number' ||
    !Number.isFinite(value.updatedAt) ||
    value.updatedAt < 0
  ) {
    return false
  }
  const mayBeUnowned =
    value.stage === 'idle' ||
    (value.stage === 'failed' && value.url === '' && value.title === '')
  if (
    (value.owner === undefined && !mayBeUnowned) ||
    (value.owner !== undefined && !isCaptureOwner(value.owner))
  ) {
    return false
  }
  if (
    value.requestedKind !== undefined &&
    (typeof value.requestedKind !== 'string' ||
      !REQUESTED_LIBRARY_KINDS.has(value.requestedKind))
  ) {
    return false
  }
  if (
    value.libraryKind !== undefined &&
    (typeof value.libraryKind !== 'string' ||
      !FINAL_LIBRARY_KINDS.has(value.libraryKind))
  ) {
    return false
  }
  if (
    value.errorMessage !== undefined &&
    typeof value.errorMessage !== 'string'
  ) {
    return false
  }
  return value.errorKind === undefined || typeof value.errorKind === 'string'
}

const SAFE_IDLE_KEYS = new Set(['stage', 'url', 'title', 'updatedAt'])

function isSafeIdleSnapshot(value: unknown): value is CaptureSnapshot {
  return (
    isRecord(value) &&
    hasOnlyKeys(value, SAFE_IDLE_KEYS) &&
    value.stage === IDLE_CAPTURE_SNAPSHOT.stage &&
    value.url === IDLE_CAPTURE_SNAPSHOT.url &&
    value.title === IDLE_CAPTURE_SNAPSHOT.title &&
    value.updatedAt === IDLE_CAPTURE_SNAPSHOT.updatedAt
  )
}

function isPersistedSource(value: unknown): value is IngestSource {
  if (!isRecord(value) || typeof value.kind !== 'string') return false
  if (!SOURCE_KINDS.has(value.kind)) return false
  for (const key of ['url', 'title', 'text', 'html'] as const) {
    if (value[key] !== undefined && typeof value[key] !== 'string') {
      return false
    }
  }
  if (
    value.image_urls !== undefined &&
    (!Array.isArray(value.image_urls) ||
      !value.image_urls.every((url) => typeof url === 'string'))
  ) {
    return false
  }
  return value.metadata === undefined || isRecord(value.metadata)
}

function isPersistedIngestRequest(value: unknown): value is IngestRequest {
  if (
    !isRecord(value) ||
    !Array.isArray(value.sources) ||
    value.sources.length < 1 ||
    value.sources.length > 64
  ) {
    return false
  }
  if (!value.sources.every(isPersistedSource)) return false
  if (
    value.destination !== undefined &&
    value.destination !== 'library' &&
    value.destination !== 'inbox' &&
    value.destination !== 'site'
  ) {
    return false
  }
  return (
    value.requested_library_kind === undefined ||
    (typeof value.requested_library_kind === 'string' &&
      REQUESTED_LIBRARY_KINDS.has(value.requested_library_kind))
  )
}

function isPersistedInFlight(value: unknown): value is InFlightCapture {
  if (
    !isRecord(value) ||
    !isCaptureOwner(value.owner) ||
    typeof value.linkId !== 'string' ||
    typeof value.url !== 'string' ||
    typeof value.title !== 'string' ||
    typeof value.note !== 'string' ||
    typeof value.attempt !== 'number' ||
    !Number.isInteger(value.attempt) ||
    value.attempt < 0 ||
    typeof value.startedAt !== 'number' ||
    !Number.isFinite(value.startedAt) ||
    value.startedAt <= 0 ||
    !isCaptureSnapshot(value.snapshot)
  ) {
    return false
  }
  if (
    !value.snapshot.owner ||
    !captureOwnersEqual(value.owner, value.snapshot.owner)
  ) {
    return false
  }
  if (
    value.phase !== undefined &&
    value.phase !== 'submitting' &&
    value.phase !== 'polling'
  ) {
    return false
  }
  if (
    value.requestedKind !== undefined &&
    (typeof value.requestedKind !== 'string' ||
      !REQUESTED_LIBRARY_KINDS.has(value.requestedKind))
  ) {
    return false
  }
  if (
    value.destination !== undefined &&
    value.destination !== 'library' &&
    value.destination !== 'inbox' &&
    value.destination !== 'site'
  ) {
    return false
  }
  if (
    value.idempotencyKey !== undefined &&
    (typeof value.idempotencyKey !== 'string' ||
      value.idempotencyKey.trim() === '')
  ) {
    return false
  }
  if (value.source !== undefined && !isPersistedSource(value.source)) {
    return false
  }
  return (
    value.ingestRequest === undefined ||
    isPersistedIngestRequest(value.ingestRequest)
  )
}

const SEALED_FORMAT = 'webtag-capture-sealed-v1'
const SEALED_SNAPSHOT_KEYS = new Set([
  'format',
  'kind',
  'owner',
  'iv',
  'ciphertext',
])
const SEALED_INFLIGHT_KEYS = new Set([
  ...SEALED_SNAPSHOT_KEYS,
  'phase',
  'attempt',
  'startedAt',
])

interface SealedCaptureSnapshot {
  format: typeof SEALED_FORMAT
  kind: 'snapshot'
  owner: CaptureOwner
  iv: string
  ciphertext: string
}

interface SealedInFlightCapture extends InFlightCaptureMetadata {
  format: typeof SEALED_FORMAT
  kind: 'in-flight'
  iv: string
  ciphertext: string
}

function hasOnlyKeys(value: Record<string, unknown>, allowed: Set<string>) {
  return Object.keys(value).every((key) => allowed.has(key))
}

function isBase64(value: unknown): value is string {
  return (
    typeof value === 'string' &&
    value.length > 0 &&
    value.length % 4 === 0 &&
    /^[A-Za-z0-9+/]+={0,2}$/.test(value)
  )
}

function base64ByteLength(value: string): number {
  const padding = value.endsWith('==') ? 2 : value.endsWith('=') ? 1 : 0
  return (value.length / 4) * 3 - padding
}

function isSealedSnapshot(value: unknown): value is SealedCaptureSnapshot {
  return (
    isRecord(value) &&
    hasOnlyKeys(value, SEALED_SNAPSHOT_KEYS) &&
    value.format === SEALED_FORMAT &&
    value.kind === 'snapshot' &&
    isCaptureOwner(value.owner) &&
    isBase64(value.iv) &&
    base64ByteLength(value.iv) === 12 &&
    isBase64(value.ciphertext) &&
    base64ByteLength(value.ciphertext) >= 16
  )
}

function isSealedInFlight(value: unknown): value is SealedInFlightCapture {
  return (
    isRecord(value) &&
    hasOnlyKeys(value, SEALED_INFLIGHT_KEYS) &&
    value.format === SEALED_FORMAT &&
    value.kind === 'in-flight' &&
    isCaptureOwner(value.owner) &&
    (value.phase === undefined ||
      value.phase === 'submitting' ||
      value.phase === 'polling') &&
    typeof value.attempt === 'number' &&
    Number.isInteger(value.attempt) &&
    value.attempt >= 0 &&
    typeof value.startedAt === 'number' &&
    Number.isFinite(value.startedAt) &&
    value.startedAt > 0 &&
    isBase64(value.iv) &&
    base64ByteLength(value.iv) === 12 &&
    isBase64(value.ciphertext) &&
    base64ByteLength(value.ciphertext) >= 16
  )
}

function bytesToBase64(bytes: Uint8Array): string {
  let binary = ''
  const chunkSize = 0x8000
  for (let offset = 0; offset < bytes.length; offset += chunkSize) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + chunkSize))
  }
  return btoa(binary)
}

function base64ToBytes(value: string): Uint8Array<ArrayBuffer> {
  const binary = atob(value)
  const bytes = new Uint8Array(new ArrayBuffer(binary.length))
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index)
  }
  return bytes
}

function snapshotAAD(
  record: Pick<SealedCaptureSnapshot, 'owner'>,
): Uint8Array<ArrayBuffer> {
  return new TextEncoder().encode(
    JSON.stringify([SEALED_FORMAT, 'snapshot', record.owner]),
  )
}

function inFlightAAD(record: InFlightCaptureMetadata): Uint8Array<ArrayBuffer> {
  return new TextEncoder().encode(
    JSON.stringify([
      SEALED_FORMAT,
      'in-flight',
      record.owner,
      record.phase ?? null,
      record.attempt,
      record.startedAt,
    ]),
  )
}

async function encryptPayload(
  value: unknown,
  recoveryKey: CryptoKey,
  additionalData: Uint8Array<ArrayBuffer>,
): Promise<{ iv: string; ciphertext: string }> {
  const iv = crypto.getRandomValues(new Uint8Array(12))
  const plaintext = new TextEncoder().encode(JSON.stringify(value))
  const encrypted = await crypto.subtle.encrypt(
    { name: 'AES-GCM', iv, additionalData },
    recoveryKey,
    plaintext,
  )
  return {
    iv: bytesToBase64(iv),
    ciphertext: bytesToBase64(new Uint8Array(encrypted)),
  }
}

async function decryptPayload(
  record: { iv: string; ciphertext: string },
  recoveryKey: CryptoKey,
  additionalData: Uint8Array<ArrayBuffer>,
): Promise<unknown> {
  const decrypted = await crypto.subtle.decrypt(
    {
      name: 'AES-GCM',
      iv: base64ToBytes(record.iv),
      additionalData,
    },
    recoveryKey,
    base64ToBytes(record.ciphertext),
  )
  return JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(decrypted))
}

async function sealSnapshot(
  snapshot: CaptureSnapshot & { owner: CaptureOwner },
  recoveryKey: CryptoKey,
): Promise<SealedCaptureSnapshot> {
  const metadata: Pick<SealedCaptureSnapshot, 'format' | 'kind' | 'owner'> = {
    format: SEALED_FORMAT,
    kind: 'snapshot' as const,
    owner: snapshot.owner,
  }
  return {
    ...metadata,
    ...(await encryptPayload(snapshot, recoveryKey, snapshotAAD(metadata))),
  }
}

async function sealInFlight(
  inFlight: InFlightCapture,
  recoveryKey: CryptoKey,
): Promise<SealedInFlightCapture> {
  const metadata: InFlightCaptureMetadata = {
    owner: inFlight.owner,
    ...(inFlight.phase ? { phase: inFlight.phase } : {}),
    attempt: inFlight.attempt,
    startedAt: inFlight.startedAt,
  }
  return {
    format: SEALED_FORMAT,
    kind: 'in-flight',
    ...metadata,
    ...(await encryptPayload(inFlight, recoveryKey, inFlightAAD(metadata))),
  }
}

// ── 生产实现 ────────────────────────────────────────────────

/**
 * chrome.storage.session 的最小接口形状。
 *
 * 抽出来是为了：① 让 createSessionCaptureStore 可被注入假 storage 单测；
 * ② 兼容 chrome.storage.session 在部分类型定义里缺失的环境——调用方
 * 显式断言 `chrome.storage.session` 传入即可。
 */
export interface SessionStorageArea {
  get: (keys: string | string[]) => Promise<Record<string, unknown>>
  set: (items: Record<string, unknown>) => Promise<void>
  remove: (keys: string | string[]) => Promise<void>
}

async function quarantineSlot(
  area: SessionStorageArea,
  key: string,
): Promise<void> {
  try {
    await area.remove(key)
  } catch {
    console.warn('[capture-store] failed to quarantine an unsafe recovery slot')
  }
}

/**
 * 用给定的 storage.session area 构造一个生产 CaptureStore。
 *
 * 所有读写都经过 try/catch 兜底——storage.session 在极端情况下（配额、
 * SW 正在销毁）可能抛错，采集流程不应因持久化失败而崩溃。
 *
 * 失败处理策略（fail-safe，不再 fail-open 静默吞掉）：
 *   - 读失败（getSnapshot / getInFlight）：回退到空态，并 console.warn 留痕。
 *   - 写失败（setSnapshot / setInFlight / clearInFlight）：不再静默——一律
 *     console.warn 记录。持久化层「不保证成功」只是「可容忍的降级」，
 *     掩盖故障会让上层误以为状态已落盘：
 *       · setInFlight 失败 → 跨 SW 续跑对该次采集失效（但 controller 的
 *         内存级并发守卫仍能阻止本 SW 实例内的重复采集）。
 *       · clearInFlight 失败 → 留下僵尸记录，但 controller 的 isInFlightStale
 *         陈旧判定会在采集预算到期后自动放行新采集，不会永久锁死会话。
 *     这两层缓解都在 captureHandler.ts 的 controller 中——本层只负责
 *     「忠实暴露故障」，不替上层做静默兜底决策。
 *
 * @param area chrome.storage.session（生产）或内存假实现（测试）
 */
export function createSessionCaptureStore(
  area: SessionStorageArea,
): CaptureStore {
  return {
    getSnapshotOwner: async () => {
      try {
        const got = await area.get(STORAGE_KEY_SNAPSHOT)
        const raw = got[STORAGE_KEY_SNAPSHOT]
        if (isSealedSnapshot(raw)) return raw.owner
        if (isSafeIdleSnapshot(raw)) return null
        if (raw !== undefined) await area.remove(STORAGE_KEY_SNAPSHOT)
        return null
      } catch {
        console.warn(
          '[capture-store] getSnapshotOwner 读取失败，保留现有恢复状态',
        )
        return undefined
      }
    },
    getSnapshot: async (recoveryKey) => {
      try {
        const got = await area.get(STORAGE_KEY_SNAPSHOT)
        const raw = got[STORAGE_KEY_SNAPSHOT]
        if (isSafeIdleSnapshot(raw)) return { ...IDLE_CAPTURE_SNAPSHOT }
        if (isSealedSnapshot(raw) && !recoveryKey) {
          return { ...IDLE_CAPTURE_SNAPSHOT }
        }
        if (isSealedSnapshot(raw) && recoveryKey) {
          try {
            const decrypted = await decryptPayload(
              raw,
              recoveryKey,
              snapshotAAD(raw),
            )
            if (
              isCaptureSnapshot(decrypted) &&
              decrypted.owner &&
              captureOwnersEqual(decrypted.owner, raw.owner)
            ) {
              return decrypted
            }
          } catch {
            // Authentication, decoding, and validation failures all quarantine.
          }
        }
        if (raw !== undefined) await area.remove(STORAGE_KEY_SNAPSHOT)
        return { ...IDLE_CAPTURE_SNAPSHOT }
      } catch {
        console.warn('[capture-store] getSnapshot 读取失败，回退到 idle')
        return { ...IDLE_CAPTURE_SNAPSHOT }
      }
    },
    setSnapshot: async (snapshot, recoveryKey) => {
      try {
        if (!isCaptureSnapshot(snapshot)) {
          await area.remove(STORAGE_KEY_SNAPSHOT)
          return
        }
        if (!snapshot.owner) {
          if (isSafeIdleSnapshot(snapshot)) {
            await area.set({
              [STORAGE_KEY_SNAPSHOT]: { ...IDLE_CAPTURE_SNAPSHOT },
            })
          } else {
            await area.remove(STORAGE_KEY_SNAPSHOT)
          }
          return
        }
        if (!recoveryKey) {
          await area.remove(STORAGE_KEY_SNAPSHOT)
          return
        }
        await area.set({
          [STORAGE_KEY_SNAPSHOT]: await sealSnapshot(
            snapshot as CaptureSnapshot & { owner: CaptureOwner },
            recoveryKey,
          ),
        })
      } catch {
        // 持久化失败：最坏丢失上次结果展示，不影响数据正确性。但仍留痕。
        console.warn('[capture-store] setSnapshot 持久化失败')
        await quarantineSlot(area, STORAGE_KEY_SNAPSHOT)
      }
    },
    getInFlightMetadata: async () => {
      try {
        const got = await area.get(STORAGE_KEY_INFLIGHT)
        const raw = got[STORAGE_KEY_INFLIGHT]
        if (isSealedInFlight(raw)) {
          return {
            owner: raw.owner,
            ...(raw.phase ? { phase: raw.phase } : {}),
            attempt: raw.attempt,
            startedAt: raw.startedAt,
          }
        }
        if (raw !== undefined) await area.remove(STORAGE_KEY_INFLIGHT)
        return null
      } catch {
        console.warn(
          '[capture-store] getInFlightMetadata 读取失败，回退到 null',
        )
        return null
      }
    },
    getInFlight: async (recoveryKey) => {
      try {
        const got = await area.get(STORAGE_KEY_INFLIGHT)
        const raw = got[STORAGE_KEY_INFLIGHT]
        if (isSealedInFlight(raw) && !recoveryKey) return null
        if (isSealedInFlight(raw) && recoveryKey) {
          try {
            const decrypted = await decryptPayload(
              raw,
              recoveryKey,
              inFlightAAD(raw),
            )
            if (
              isPersistedInFlight(decrypted) &&
              captureOwnersEqual(decrypted.owner, raw.owner) &&
              decrypted.phase === raw.phase &&
              decrypted.attempt === raw.attempt &&
              decrypted.startedAt === raw.startedAt
            ) {
              return decrypted
            }
          } catch {
            // Authentication, decoding, and validation failures all quarantine.
          }
        }
        if (raw !== undefined) await area.remove(STORAGE_KEY_INFLIGHT)
        return null
      } catch {
        console.warn('[capture-store] getInFlight 读取失败，回退到 null')
        return null
      }
    },
    setInFlight: async (inFlight, recoveryKey) => {
      try {
        if (!isPersistedInFlight(inFlight) || !recoveryKey) {
          await area.remove(STORAGE_KEY_INFLIGHT)
          return
        }
        await area.set({
          [STORAGE_KEY_INFLIGHT]: await sealInFlight(inFlight, recoveryKey),
        })
      } catch {
        // 失败 → 跨 SW 续跑对本次采集失效；controller 的内存级并发守卫
        // 仍能阻止本 SW 实例内的重复采集。属可容忍降级，但必须留痕。
        console.warn(
          '[capture-store] setInFlight 持久化失败，跨 SW 续跑将对本次采集失效',
        )
        await quarantineSlot(area, STORAGE_KEY_INFLIGHT)
      }
    },
    clearInFlight: async () => {
      try {
        await area.remove(STORAGE_KEY_INFLIGHT)
      } catch {
        // 失败 → 留下僵尸 in-flight 记录；controller 的 isInFlightStale
        // 陈旧判定会在采集预算到期后自动放行，不会永久锁死会话。
        console.warn(
          '[capture-store] clearInFlight 清除失败，僵尸记录将由陈旧判定自愈',
        )
      }
    },
  }
}

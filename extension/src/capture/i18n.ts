import { createI18n } from 'vue-i18n'

export type CaptureLocale = 'en-US' | 'zh-CN'

const messages = {
  'zh-CN': {
    capture: {
      title: '采集当前页到 WebTag',
      pageTitleFallback: '（无标题页面）',
      notePlaceholder: '给这条链接加个备注（可选）',
      submitButton: '采集到 WebTag',
      submitting: '采集中…',
      kindLabel: '采集类型',
      kindAuto: '自动',
      kindReading: '阅读',
      kindSite: '网站',
      statusCapturing: '正在抓取页面…',
      statusSubmitted: '已提交',
      statusParsing: '解析中…',
      statusDone: '采集完成',
      statusStillProcessing: '仍在解析中',
      statusStillProcessingHint:
        '解析耗时较长，稍后可在知识库桌面的「处理中」分区查看结果',
      statusFailed: '采集失败',
      openSettings: '打开设置',
      openSettingsAria: '打开设置',
      resultPageLabel: '目标页面',
      restrictedPage: '当前页面无法采集（浏览器内置页、扩展页或应用商店）',
      error: {
        'not-configured':
          '尚未配置 WebTag 后端，请先在设置页填写后端地址与 Token',
        'network-unreachable': '无法连接 WebTag 后端，请检查后端地址与网络',
        unauthorized: '访问 Token 无效，请到设置页检查 Token',
        timeout: '请求 WebTag 后端超时，请稍后重试',
        'rate-limited': '请求过于频繁，请稍后再试',
        'capture-injection-failed': '当前页面不支持采集（如浏览器内置页）',
        'job-failed': '解析失败，请稍后到知识库桌面查看',
        other: '采集提交失败，请稍后重试',
        'capture-restricted': '该页面受限，无法采集',
        'capture-unexpected': '采集过程中发生意外错误',
        'not-modified': '后端版本不支持该采集操作',
        'identity-mismatch': '当前登录身份与后端身份不匹配',
      },
    },
    subscriptions: {
      title: '发现订阅源',
      discovering: '正在检查当前页面…',
      restricted: '浏览器限制了此页面，无法发现订阅源。',
      unavailable: '暂时无法读取当前页面的订阅源。',
      noneFound: '当前页面没有声明 RSS、Atom 或 RDF 订阅源。',
      checking: '检查中…',
      subscribe: '订阅',
      subscribing: '订阅中…',
      subscribed: '已订阅',
      retry: '重试',
      openSettings: '设置',
      openReader: '打开 Reader 订阅页',
      notConfigured: '连接 WebTag 后端后即可订阅，打开设置',
      error: {
        unauthorized: '访问 Token 无效，请检查设置。',
        'network-unreachable': '无法连接 WebTag 后端。',
        timeout: '请求超时，请重试。',
        'rate-limited': '请求过于频繁，请稍后重试。',
        other: '订阅操作失败，请重试。',
      },
    },
    webtagSettings: {
      paneTitle: 'WebTag 后端连接',
      connectionSection: '后端连接',
      backendUrlLabel: '后端地址',
      backendUrlPlaceholder: '如 http://localhost:8080',
      backendUrlTip: 'WebTag 后端服务的基础地址，知识库与采集功能依赖它',
      readerUrlLabel: 'Reader 地址',
      readerUrlPlaceholder: '如 https://reader.example.com',
      readerUrlTip:
        'Reader 前端的完整地址；若留空，扩展会尝试后端同源的 /reader 兼容路径',
      tokenLabel: '访问 Token',
      tokenPlaceholder: '后端配置的 EXTENSION_API_TOKEN',
      tokenTip:
        '与后端 EXTENSION_API_TOKEN 一致。仅当后端不暴露到公网时才可留空',
      testConnectionLabel: '连接测试',
      testConnectionButton: '测试连接',
      testTesting: '正在测试…',
      testResultSuccess: '连接成功',
      testResultUnauthorized: 'Token 无效或缺失，请检查访问 Token',
      testResultUnreachable: '无法连接到后端，请检查后端地址与服务状态',
      testResultTimeout: '连接超时，请检查后端地址与网络',
      testResultRateLimited: '请求过于频繁，请稍后再试',
      testResultOther: '连接失败，请稍后重试',
      captureDestinationLabel: '采集去向',
      captureDestinationInbox: '待确认',
      captureDestinationLibrary: '直接入库',
      captureDestinationTip:
        '默认先进入 Reader 待确认；选择直接入库后沿用原有知识库采集流程',
      captureDestinationSaving: '正在保存…',
      saveFailed: '设置保存失败，请重试',
    },
  },
  'en-US': {
    capture: {
      title: 'Capture page to WebTag',
      pageTitleFallback: '(Untitled page)',
      notePlaceholder: 'Add a note for this link (optional)',
      submitButton: 'Capture to WebTag',
      submitting: 'Capturing…',
      kindLabel: 'Capture type',
      kindAuto: 'Automatic',
      kindReading: 'Reading',
      kindSite: 'Website',
      statusCapturing: 'Capturing page…',
      statusSubmitted: 'Submitted',
      statusParsing: 'Parsing…',
      statusDone: 'Capture complete',
      statusStillProcessing: 'Still processing',
      statusStillProcessingHint:
        'Parsing is taking a while. Check the Processing section later.',
      statusFailed: 'Capture failed',
      openSettings: 'Open settings',
      openSettingsAria: 'Open settings',
      resultPageLabel: 'Target page',
      restrictedPage:
        'This page cannot be captured (browser, extension, or Web Store page)',
      error: {
        'not-configured':
          'WebTag backend not configured. Set the backend URL and token first.',
        'network-unreachable':
          'Cannot reach the WebTag backend. Check the URL and your network.',
        unauthorized: 'Invalid access token. Check your token in Settings.',
        timeout: 'Request to the WebTag backend timed out. Please try again.',
        'rate-limited': 'Too many requests. Please try again later.',
        'capture-injection-failed': 'This page cannot be captured.',
        'job-failed': 'Parsing failed. Check the knowledge base later.',
        other: 'Capture submission failed. Please try again.',
        'capture-restricted': 'This page is restricted and cannot be captured.',
        'capture-unexpected': 'An unexpected error occurred during capture.',
        'not-modified': 'This backend version does not support capture.',
        'identity-mismatch':
          'The signed-in identity does not match the backend.',
      },
    },
    subscriptions: {
      title: 'Feeds found',
      discovering: 'Checking this page…',
      restricted: 'Browser restrictions prevent feed discovery on this page.',
      unavailable: 'Feeds on this page are temporarily unavailable.',
      noneFound: 'This page does not declare an RSS, Atom, or RDF feed.',
      checking: 'Checking…',
      subscribe: 'Subscribe',
      subscribing: 'Subscribing…',
      subscribed: 'Subscribed',
      retry: 'Retry',
      openSettings: 'Settings',
      openReader: 'Open Reader subscriptions',
      notConfigured: 'Connect the WebTag backend to subscribe. Open Settings',
      error: {
        unauthorized: 'The access token is invalid. Check Settings.',
        'network-unreachable': 'Cannot reach the WebTag backend.',
        timeout: 'The request timed out. Try again.',
        'rate-limited': 'Too many requests. Try again later.',
        other: 'Subscription failed. Try again.',
      },
    },
    webtagSettings: {
      paneTitle: 'WebTag Backend Connection',
      connectionSection: 'Backend Connection',
      backendUrlLabel: 'Backend URL',
      backendUrlPlaceholder: 'e.g. http://localhost:8080',
      backendUrlTip: 'Base URL of the WebTag backend used for capture.',
      readerUrlLabel: 'Reader URL',
      readerUrlPlaceholder: 'e.g. https://reader.example.com',
      readerUrlTip:
        'Full Reader frontend URL. When empty, the extension tries the backend /reader compatibility path.',
      tokenLabel: 'Access Token',
      tokenPlaceholder: 'The backend EXTENSION_API_TOKEN',
      tokenTip:
        'Must match EXTENSION_API_TOKEN. Leave blank only for private, unauthenticated backends.',
      testConnectionLabel: 'Connection Test',
      testConnectionButton: 'Test Connection',
      testTesting: 'Testing…',
      testResultSuccess: 'Connected successfully',
      testResultUnauthorized:
        'Token invalid or missing; check the access token',
      testResultUnreachable:
        'Cannot reach the backend; check the URL and service',
      testResultTimeout: 'Connection timed out; check the URL and network',
      testResultRateLimited: 'Too many requests; please try again later',
      testResultOther: 'Connection failed; please try again later',
      captureDestinationLabel: 'Capture destination',
      captureDestinationInbox: 'Inbox',
      captureDestinationLibrary: 'Library',
      captureDestinationTip:
        'Captures go to Reader Inbox by default. Choose Library to use the direct library flow.',
      captureDestinationSaving: 'Saving…',
      saveFailed: 'Could not save the setting. Try again.',
    },
  },
} as const

export function resolveCaptureLocale(language?: string): CaptureLocale {
  return language?.toLowerCase().startsWith('zh') ? 'zh-CN' : 'en-US'
}

const browserLanguage =
  globalThis.chrome?.i18n?.getUILanguage?.() || globalThis.navigator?.language

export const captureI18n = createI18n({
  legacy: false,
  locale: resolveCaptureLocale(browserLanguage),
  fallbackLocale: 'en-US',
  messages,
})

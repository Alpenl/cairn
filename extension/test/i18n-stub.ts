/**
 * @module test/i18n-stub
 * @description 组件单测共用的 vue-i18n 测试桩。
 *
 *   被测的知识库 / 桌面组件在 setup 内调用 `useI18n()`，挂载时若无 i18n 插件会抛错。
 *   本桩提供一个最小 vue-i18n 实例：`t(key)` 直接回传 key 本身（与 test/setup.ts
 *   里 `window.$t` 的「回传 key」语义一致），让断言可以稳定地对 i18n key 做匹配，
 *   不依赖真实文案。
 *
 *   用法：
 *     import { i18nPlugin } from '@/../test/i18n-stub'
 *     mount(Comp, { global: { plugins: [i18nPlugin] } })
 *
 * @consumers src/newtab/components/**\/*.test.ts、src/newtab/desktops/*.test.ts
 */
import { createI18n } from 'vue-i18n'

/**
 * 最小 vue-i18n 插件实例。
 *
 * - legacy: false —— 启用 Composition API 模式，组件内 `useI18n()` 可用。
 * - globalInjection: true —— 把 `$t` 注册为组件全局属性（与项目真实 i18n
 *   src/common/i18n.ts 一致）。DesktopIndicator / DesktopContainer 的模板用裸
 *   `$t`（非 useI18n），没有此项时 `_ctx.$t` 不存在会抛 "$t is not a function"。
 * - missingWarn / fallbackWarn 关闭 —— messages 故意为空，`t(key)` 缺键时回传 key，
 *   正是测试想要的行为，不需要 console 噪音。
 * - locale 固定 'en' —— LinkCard 的 ingestTime 用 `locale.value` 传给 Intl，
 *   固定值保证日期格式化结果可预测。
 */
export const i18nPlugin = createI18n({
  legacy: false,
  globalInjection: true,
  locale: 'en',
  fallbackLocale: 'en',
  missingWarn: false,
  fallbackWarn: false,
  messages: { en: {} },
})

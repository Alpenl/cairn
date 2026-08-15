/**
 * 主界面外壳的纯副作用 hook，从 MainView 抽出以降低单文件体积（R2 review 遗留项）。
 *
 * 只产生副作用（监听 keydown），不持有业务状态，因此抽离零风险——MainView
 * 仍持有全部状态机，hook 只接收回调。
 */
import { useEffect } from 'react'

export interface AppShortcutHandlers {
  /** ⌘K / Ctrl+K：开关命令面板。 */
  onToggleCmdk: () => void
  /** ⌘J / Ctrl+J：开关 AI 助手。 */
  onToggleChat: () => void
}

/**
 * 全局快捷键（⌘K 命令面板 / ⌘J AI 助手）。
 * 回调用 ref 透传时调用方需自行保证稳定性；此处依赖回调引用，调用方用 useCallback。
 */
export function useAppShortcuts({ onToggleCmdk, onToggleChat }: AppShortcutHandlers): void {
  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        onToggleCmdk()
      }
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'j') {
        e.preventDefault()
        onToggleChat()
      }
    }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  }, [onToggleCmdk, onToggleChat])
}

/**
 * 轻量 Toast。
 * msg 为空时不渲染。图标缺省 sparkles。
 *
 * action 是可选的一次性操作（目前只有「撤销」在用）。带 action 的提示由
 * 调用方给更长的停留时间——2.6 秒够读完一句话，但不够看到、移动指针并点中
 * 一个按钮。
 */
import { Icon, type IconName } from './Icon'

export interface ToastAction {
  readonly label: string
  readonly onAction: () => void
}

export interface ToastProps {
  msg: string | null
  icon?: IconName
  action?: ToastAction
}

export function Toast({ msg, icon, action }: ToastProps) {
  if (!msg) return null
  return (
    <div className="toast">
      <Icon name={icon || 'sparkles'} size={16} /> {msg}
      {action && (
        <button type="button" className="toast-action" onClick={action.onAction}>
          {action.label}
        </button>
      )}
    </div>
  )
}

/**
 * 轻量 Toast。
 * msg 为空时不渲染。图标缺省 sparkles。
 */
import { Icon, type IconName } from './Icon'

export interface ToastProps {
  msg: string | null
  icon?: IconName
}

export function Toast({ msg, icon }: ToastProps) {
  if (!msg) return null
  return (
    <div className="toast">
      <Icon name={icon || 'sparkles'} size={16} /> {msg}
    </div>
  )
}

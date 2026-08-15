import { computed, onScopeDispose, ref } from 'vue'
import { darkTheme } from 'naive-ui'

export function useSystemTheme() {
  const query = window.matchMedia('(prefers-color-scheme: dark)')
  const isDark = ref(query.matches)
  const update = (event: MediaQueryListEvent) => {
    isDark.value = event.matches
  }

  query.addEventListener('change', update)
  onScopeDispose(() => query.removeEventListener('change', update))

  return computed(() => (isDark.value ? darkTheme : null))
}

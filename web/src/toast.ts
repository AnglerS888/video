import { reactive } from 'vue'

export interface ToastItem { id: number; type: 'info' | 'ok' | 'err'; text: string }

export const toasts = reactive<{ items: ToastItem[] }>({ items: [] })
let seq = 0

export function toast(text: string, type: ToastItem['type'] = 'info', ms = 2400) {
  const id = ++seq
  toasts.items.push({ id, type, text })
  window.setTimeout(() => {
    const i = toasts.items.findIndex((x) => x.id === id)
    if (i >= 0) toasts.items.splice(i, 1)
  }, ms)
}

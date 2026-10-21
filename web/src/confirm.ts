import { reactive } from 'vue'

/**
 * 全局确认弹窗（替代浏览器原生 confirm）。
 *
 * 原生 confirm 的样式不可控、移动端会整页阻塞，而且浏览器可以勾选
 * 「禁止此页再显示」—— 删文件这类话术一旦被静默跳过，用户就再也看不到警告。
 * 这里用 Promise 形式提供同样的用法：
 *
 *   if (!(await confirmDialog({ message: '删除？', danger: true }))) return
 *
 * 同一时刻只显示一个，重复触发时排队依次弹出。
 */
export interface ConfirmOptions {
  message: string
  title?: string
  okText?: string
  cancelText?: string
  /** true：确认按钮用危险色（删除类操作）；默认 false */
  danger?: boolean
}

export interface ConfirmTask extends ConfirmOptions {
  id: number
  title: string
  okText: string
  cancelText: string
  danger: boolean
  resolve: (ok: boolean) => void
}

export const confirmState = reactive<{ current: ConfirmTask | null }>({ current: null })

const queue: ConfirmTask[] = []
let seq = 0

export function confirmDialog(opts: ConfirmOptions | string): Promise<boolean> {
  const o = typeof opts === 'string' ? { message: opts } : opts
  return new Promise<boolean>((resolve) => {
    queue.push({
      ...o,
      id: ++seq,
      title: o.title || '请确认',
      okText: o.okText || '确定',
      cancelText: o.cancelText || '取消',
      danger: o.danger === true,
      resolve,
    })
    pump()
  })
}

/** 用户点了「确定 / 取消」（弹窗组件调用） */
export function answerConfirm(ok: boolean) {
  const cur = confirmState.current
  if (!cur) return
  confirmState.current = null
  cur.resolve(ok)
  pump()
}

function pump() {
  if (confirmState.current || !queue.length) return
  confirmState.current = queue.shift() as ConfirmTask
}

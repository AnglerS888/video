/**
 * 复制文本到剪贴板。
 *
 * 优先 Clipboard API（只在本机 https/localhost 可用），
 * 局域网 http 访问时浏览器不给 navigator.clipboard，用 execCommand 兜底 ——
 * 否则在 NAS 上点「复制」会静默失败。
 */
export async function copyText(t: string): Promise<boolean> {
  if (!t) return false
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(t)
      return true
    }
  } catch {
    /* 落到兜底方案 */
  }
  try {
    const ta = document.createElement('textarea')
    ta.value = t
    ta.setAttribute('readonly', '')
    ta.style.position = 'fixed'
    ta.style.top = '-1000px'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    const done = document.execCommand('copy')
    document.body.removeChild(ta)
    return done
  } catch {
    return false
  }
}

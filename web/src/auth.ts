import { reactive } from 'vue'

/**
 * 站点访问密码状态（服务端存在数据目录 ui.yml 的 auth.password）。
 *
 * required=false 表示站点没设密码，接口随便调；
 * 设了密码后浏览器要先登录（服务端下发 HttpOnly Cookie），
 * 脚本 / 第三方客户端调接口则带 X-Access-Password 头或 ?password= 参数。
 *
 * 这里是全局单例：App.vue 用它决定显示登录页还是正常页面，
 * api.ts 收到 401 时调用 markUnauthorized() 切回登录页。
 */
export const auth = reactive({
  /** 站点是否设置了访问密码 */
  required: false,
  /** 当前浏览器是否已通过验证（没设密码时恒为 true） */
  authed: true,
  /** 首次状态检查中：避免没设密码的站点闪一下登录页 */
  checking: true,
})

/** 接口返回 401（需要访问密码）时调用：直接切回登录页 */
export function markUnauthorized() {
  auth.required = true
  auth.authed = false
}

/** 查询站点是否需要密码、当前浏览器是否已登录（这个接口本身不需要密码） */
export async function checkAuth(): Promise<void> {
  try {
    const r = await fetch('/api/auth/status')
    const j = await r.json()
    if (j?.ok && j.data) {
      auth.required = !!j.data.required
      auth.authed = !!j.data.authenticated
    }
  } catch {
    // 服务端还没起来 / 网络异常：当作不需要密码，让页面照常显示错误提示，
    // 而不是永远卡在「正在检查访问权限…」
  } finally {
    auth.checking = false
  }
}

/** 登录：密码正确后服务端会下发 Cookie，后续请求自动带上 */
export async function login(password: string): Promise<{ ok: boolean; msg?: string }> {
  try {
    const r = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password }),
    })
    const j = await r.json().catch(() => null)
    if (r.ok && j?.ok) {
      auth.required = true
      auth.authed = true
      return { ok: true }
    }
    return { ok: false, msg: j?.msg || `HTTP ${r.status}` }
  } catch (e) {
    return { ok: false, msg: e instanceof Error ? e.message : String(e) }
  }
}

/** 退出登录：清掉服务端 Cookie，本地切回登录页 */
export async function logout(): Promise<void> {
  try {
    await fetch('/api/auth/logout', { method: 'POST' })
  } catch {
    // 清 Cookie 失败也无所谓：本地状态照样切回登录页
  }
  auth.authed = false
}

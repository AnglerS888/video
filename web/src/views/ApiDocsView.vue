<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { markUnauthorized } from '../auth'
import { toast } from '../toast'
import { copyText } from '../clipboard'
import { confirmDialog } from '../confirm'
import { fmtBytes } from '../format'

/**
 * 接口文档：把站点自身的 HTTP 接口开放出来，方便脚本/第三方客户端对接。
 *
 * 接口都是同源的 `/api/...`，默认无鉴权；在「信息」页 →「运行配置」里设了
 * 访问密码后，除了 /healthz 与 /api/auth/* 以外的接口都要带密码
 * （页面登录后的 Cookie，或 X-Access-Password 请求头 / ?password= 参数）。
 * 统一响应：{ ok: true, data: ... } 或 { ok: false, msg: "错误原因" }。
 *
 * 「试一下」直接在本页用 fetch 发同源请求（自动带登录 Cookie）：
 *   - 路径参数（{id} 这类）与查询参数按输入框拼进 URL，发送前校验占位是否填完；
 *   - 写接口可以编辑 JSON 请求体，默认给了示例；
 *   - 返回文件流的接口（视频/图片）不在这里发，改为新窗口打开，避免整段下载进内存。
 */
type Method = 'GET' | 'POST' | 'PUT' | 'DELETE'
type ApiParam = {
  in: 'path' | 'query'
  name: string
  desc?: string
  /** 输入框默认值（能直接发的接口给上，如分页） */
  def?: string
  /** 输入框占位提示 */
  example?: string
  required?: boolean
}
type ApiItem = {
  m: Method
  p: string
  d: string
  q?: string
  params?: ApiParam[]
  /** 请求体示例（不设置表示该接口不需要 body） */
  body?: unknown
  /** 发送前的确认文案；不设置则直接发 */
  confirm?: string
  /** 确认按钮用危险色（删除/重启/清库这类） */
  danger?: boolean
  /** 不能在当前页直接发的接口：说明原因，只给「新窗口打开」 */
  noTry?: string
}
type ApiGroup = { title: string; desc?: string; items: ApiItem[] }

const groups: ApiGroup[] = [
  {
    title: '访问控制（可选）',
    desc: '站点在「信息」页 →「运行配置」设了访问密码后，其它接口都要带密码；这三个接口本身不需要。',
    items: [
      { m: 'GET', p: '/api/auth/status', d: '当前是否启用访问密码、这个浏览器是否已登录（返回 required / authenticated）' },
      {
        m: 'POST', p: '/api/auth/login', d: '登录：密码正确后下发 HttpOnly Cookie（30 天）',
        body: { password: '访问密码' },
      },
      { m: 'POST', p: '/api/auth/logout', d: '退出登录（清除 Cookie）' },
    ],
  },
  {
    title: '影视库',
    desc: '列表按「剧名」合并多个来源，同一部剧只出现一次。',
    items: [
      {
        m: 'GET', p: '/api/videos', d: '影片列表（分页）',
        params: [
          { in: 'query', name: 'wd', desc: '关键词' },
          { in: 'query', name: 'field', desc: '搜索范围 name|actor|all', example: 'all' },
          { in: 'query', name: 'page', desc: '页码', def: '1', example: '1' },
          { in: 'query', name: 'size', desc: '每页条数 8~120', def: '12', example: '12' },
          { in: 'query', name: 'year', desc: '年份' },
          { in: 'query', name: 'area', desc: '地区' },
          { in: 'query', name: 'type', desc: '类型' },
          { in: 'query', name: 'filter', desc: 'filter=downloaded 只看已下载' },
          { in: 'query', name: 'source_id', desc: '限定来源 id' },
        ],
      },
      {
        m: 'GET', p: '/api/videos/{id}', d: '影片详情（含多来源、剧集与播放地址）',
        params: [{ in: 'path', name: 'id', desc: '影片 id（列表返回的 id）', required: true }],
      },
      { m: 'GET', p: '/api/filters', d: '筛选项：年份 / 地区 / 类型' },
      {
        m: 'GET', p: '/api/img', d: '封面图代理（解决源站防盗链）',
        params: [{ in: 'query', name: 'u', desc: '封面原地址（直接填原始 URL，本页会自动编码）', required: true }],
        noTry: '返回图片，用「新窗口打开」查看',
      },
    ],
  },
  {
    title: '来源（采集接口）',
    desc: '一个来源就是一个资源站接口（苹果 CMS 的 provide/vod 协议）。',
    items: [
      { m: 'GET', p: '/api/sources', d: '来源列表（名称、地址、启用、自动增量；不含影片统计，大库下避免卡顿）' },
      {
        m: 'POST', p: '/api/sources', d: '添加来源',
        body: { name: '我的接口', api_url: 'https://www.example.com', remark: '', enabled: 1 },
      },
      {
        m: 'PUT', p: '/api/sources/{id}', d: '修改来源（名称 / 地址 / 备注 / 启用）',
        params: [{ in: 'path', name: 'id', desc: '来源 id', required: true }],
        body: { name: '我的接口', api_url: 'https://www.example.com', remark: '', enabled: 1 },
      },
      {
        m: 'DELETE', p: '/api/sources/{id}', d: '删除来源及其采集数据',
        params: [{ in: 'path', name: 'id', desc: '来源 id', required: true }],
        confirm: '会同时删除该来源的影片、播放地址、下载记录与本地文件，不可恢复。',
        danger: true,
      },
      {
        m: 'POST', p: '/api/sources/probe', d: '检测地址是否可用（不写库）',
        body: { url: 'https://www.example.com' },
      },
      {
        m: 'GET', p: '/api/sources/{id}/test', d: '测试来源连通性',
        params: [{ in: 'path', name: 'id', desc: '来源 id', required: true }],
      },
      {
        m: 'GET', p: '/api/sources/{id}/sync-logs', d: '增量同步记录（每次手动/自动增量一条汇总，新的在前；返回 total/page/page_size/items）',
        params: [
          { in: 'path', name: 'id', desc: '来源 id', required: true },
          { in: 'query', name: 'page', desc: '页码（越界回退最后一页）', def: '1', example: '1' },
          { in: 'query', name: 'page_size', desc: '每页条数（默认 10，最多 100；兼容旧 limit）', def: '10', example: '10' },
        ],
      },
      {
        m: 'GET', p: '/api/sources/{id}/sync-logs/{logID}', d: '单条同步记录的影片明细（新增/更新了哪几部、源站更新时间）',
        params: [
          { in: 'path', name: 'id', desc: '来源 id', required: true },
          { in: 'path', name: 'logID', desc: '同步记录 id', required: true },
        ],
      },
      {
        m: 'POST', p: '/api/sources/{id}/auto-incr', d: '开关「定时增量」',
        params: [{ in: 'path', name: 'id', desc: '来源 id', required: true }],
        body: { on: 1 },
      },
      { m: 'GET', p: '/api/sources/export', d: '导出全部来源为 JSON' },
      {
        m: 'POST', p: '/api/sources/import', d: '导入来源 JSON（按 api_url 去重）',
        body: { sources: [{ name: '我的接口', api_url: 'https://www.example.com', enabled: 1 }] },
        confirm: '导入的来源默认启用并参与定时增量采集。',
      },
    ],
  },
  {
    title: '采集',
    items: [
      {
        m: 'POST', p: '/api/collect/full', d: '全量采集（可随时停止；并发已满时进排队，采完自动接上）',
        params: [
          { in: 'query', name: 'source_id', desc: '不传 = 全部启用来源' },
          { in: 'query', name: 'restart', desc: '1 = 从头重采（忽略断点）', example: '1' },
        ],
      },
      {
        m: 'POST', p: '/api/collect/incr', d: '增量采集（按更新时间拉最新几页；并发已满时进排队）',
        params: [{ in: 'query', name: 'source_id', desc: '不传 = 全部启用来源' }],
      },
      {
        m: 'POST', p: '/api/collect/stop', d: '停止采集（排队中的会取消排队）',
        params: [{ in: 'query', name: 'source_id', desc: '不传 = 停止全部并清空队列' }],
      },
      { m: 'GET', p: '/api/progress', d: '采集进度（各来源页码 / 已入库数量；phase 含 queued = 排队中）' },
      { m: 'GET', p: '/api/scheduler', d: '定时增量状态（enabled / interval_sec / next_run_unix）' },
    ],
  },
  {
    title: '下载与本地',
    items: [
      { m: 'GET', p: '/api/downloads', d: '下载任务列表' },
      {
        m: 'POST', p: '/api/download', d: '新增下载（空 ep_indices = 全部集）',
        body: { vod_id: 1, play_from: 'm3u8', ep_indices: [1] },
        confirm: '会在后台开始下载视频文件，占用磁盘空间。',
      },
      {
        m: 'POST', p: '/api/downloads/{id}/pause', d: '暂停',
        params: [{ in: 'path', name: 'id', desc: '下载任务 id', required: true }],
      },
      {
        m: 'POST', p: '/api/downloads/{id}/resume', d: '继续',
        params: [{ in: 'path', name: 'id', desc: '下载任务 id', required: true }],
      },
      {
        m: 'POST', p: '/api/downloads/{id}/retry', d: '失败重试',
        params: [{ in: 'path', name: 'id', desc: '下载任务 id', required: true }],
      },
      {
        m: 'POST', p: '/api/downloads/{id}/priority', d: '插队优先下载',
        params: [{ in: 'path', name: 'id', desc: '下载任务 id', required: true }],
      },
      {
        m: 'POST', p: '/api/downloads/batch', d: '批量操作',
        body: { action: 'pause', ids: [1, 2] },
        q: 'action=pause|resume|retry|delete',
        confirm: 'action=delete 时会连同已下载的 mp4 文件一起删除，不可恢复；pause/resume/retry 不动文件。',
        danger: true,
      },
      {
        m: 'DELETE', p: '/api/downloads/{id}', d: '删除任务与文件',
        params: [{ in: 'path', name: 'id', desc: '下载任务 id', required: true }],
        confirm: '会同时删除已下载的 mp4 文件，不可恢复。',
        danger: true,
      },
    ],
  },
  {
    title: '播放与分享',
    items: [
      { m: 'GET', p: '/api/play-token', d: '取本地播放令牌（/play/{id} 需要 ?t=令牌）' },
      {
        m: 'GET', p: '/play/{id}', d: '播放文件流',
        params: [
          { in: 'path', name: 'id', desc: '下载任务 id', required: true },
          { in: 'query', name: 't', desc: '播放令牌' },
          { in: 'query', name: 'dl', desc: '1 = 作为附件下载', example: '1' },
        ],
        noTry: '返回视频文件流，在页面内试会整段下载，用「新窗口打开」或播放器访问',
      },
      {
        m: 'GET', p: '/api/live', d: '在线播放代理（改写 m3u8 + 转发分片，绕过源站 CORS）',
        params: [{ in: 'query', name: 'u', desc: 'm3u8 地址', required: true }],
      },
      {
        m: 'GET', p: '/api/downloads/{id}/share', d: '生成分享链接（永久有效，局域网地址）',
        params: [{ in: 'path', name: 'id', desc: '下载任务 id', required: true }],
      },
      { m: 'GET', p: '/api/share/base', d: '站点根地址（用于二维码）' },
      {
        m: 'GET', p: '/api/share/{id}', d: '分享页数据（不直接暴露视频地址）',
        params: [{ in: 'path', name: 'id', desc: '分享令牌（旧版数字下载 id 需本机访问）', required: true }],
      },
    ],
  },
  {
    title: '历史与收藏',
    items: [
      {
        m: 'POST', p: '/api/watch/progress', d: '上报播放进度',
        body: { download_id: 1, position: 60, duration: 1200 },
      },
      { m: 'GET', p: '/api/watch/history', d: '观看历史' },
      {
        m: 'DELETE', p: '/api/watch/history/{id}', d: '删除单条历史（不动文件）',
        params: [{ in: 'path', name: 'id', desc: 'download_id', required: true }],
      },
      { m: 'DELETE', p: '/api/watch/history', d: '清空历史', confirm: '清空全部观看记录（已下载的文件不受影响）。' },
      { m: 'GET', p: '/api/favs', d: '收藏列表（含自动追更状态 auto_dl / auto_vod_id / auto_play_from / auto_new_eps）' },
      { m: 'POST', p: '/api/favs', d: '添加收藏', body: { vod_name: '片名' } },
      {
        m: 'DELETE', p: '/api/favs', d: '取消收藏',
        params: [{ in: 'query', name: 'name', desc: '片名', required: true }],
      },
      {
        m: 'POST', p: '/api/favs/seen', d: '把收藏标记为已看过更新',
        params: [{ in: 'query', name: 'name', desc: '片名', required: true }],
      },
      {
        m: 'POST', p: '/api/favs/auto',
        d: '开关自动追更：盯住某部剧在指定来源的某条线路，采集到新集数自动加入下载',
        body: { vod_name: '片名', vod_id: 1, play_from: 'm3u8', enabled: true },
      },
    ],
  },
  {
    title: '系统与配置',
    items: [
      {
        m: 'GET', p: '/api/sysinfo', d: '运行环境、磁盘、内存、记录数',
        params: [{ in: 'query', name: 'fresh', desc: '1 = 跳过缓存', example: '1' }],
      },
      { m: 'GET', p: '/api/version', d: '版本与更新状态' },
      { m: 'POST', p: '/api/update/check', d: '检查 Gitee / GitHub 是否有新版本（最长 45 秒）' },
      {
        m: 'POST', p: '/api/update/apply', d: '下载最新源码（仅 Docker 容器内可用）',
        params: [{ in: 'query', name: 'force', desc: '1 = 忽略版本号强制更新', example: '1' }],
        confirm: '会下载最新源码、在容器内重新编译并原地重启服务。',
        danger: true,
      },
      { m: 'GET', p: '/api/config', d: '读取配置文件（原文 + 解析结果）' },
      {
        m: 'PUT', p: '/api/config', d: '保存配置文件',
        body: { text: '# 配置 YAML 全文，重启后生效\n' },
        confirm: '会覆盖写入配置文件（yml），重启后生效。',
      },
      {
        m: 'POST', p: '/api/system/vacuum', d: '整理数据库（回收空闲页）',
        confirm: 'VACUUM 会独占数据库写锁，大库可能持续几分钟，期间接口无响应。',
        danger: true,
      },
      { m: 'GET', p: '/healthz', d: '健康检查（只做一次轻量查询）' },
    ],
  },
]

const base = computed(() => location.origin)

/* ---------- 试一下：每个接口一份可变状态 ---------- */

type TryResult = {
  status: number
  statusText: string
  ms: number
  size: number
  type: string
  body: string
  truncated: boolean
}
type TryState = {
  vars: Record<string, string>
  body: string
  sending: boolean
  result: TryResult | null
}

const openKey = ref('')
const states = reactive<Record<string, TryState>>({})

const emptyResult: TryResult = { status: 0, statusText: '', ms: 0, size: 0, type: '', body: '', truncated: false }

const keyOf = (it: ApiItem) => `${it.m} ${it.p}`
const hasBody = (it: ApiItem) => it.body !== undefined
const isOpen = (it: ApiItem) => openKey.value === keyOf(it)

function initState(it: ApiItem): TryState {
  const vars: Record<string, string> = {}
  for (const p of it.params ?? []) vars[p.name] = p.def ?? ''
  return {
    vars,
    body: it.body === undefined ? '' : JSON.stringify(it.body, null, 2),
    sending: false,
    result: null,
  }
}

function stateOf(it: ApiItem): TryState {
  const k = keyOf(it)
  if (!states[k]) states[k] = initState(it)
  return states[k]
}

function resultOf(it: ApiItem): TryResult {
  return stateOf(it).result ?? emptyResult
}
const hasResult = (it: ApiItem) => stateOf(it).result !== null

function toggleTry(it: ApiItem) {
  openKey.value = isOpen(it) ? '' : keyOf(it)
}

function reset(it: ApiItem) {
  aborts.get(keyOf(it))?.abort()
  states[keyOf(it)] = initState(it)
}

/** 按输入框拼出请求路径（未填的路径参数保留 {name}，发给后端前会再校验） */
function buildURL(it: ApiItem): string {
  const st = stateOf(it)
  let url = it.p.replace(/\{(\w+)\}/g, (m, name: string) => {
    const v = (st.vars[name] ?? '').trim()
    return v ? encodeURIComponent(v) : m
  })
  const qs = new URLSearchParams()
  for (const p of it.params ?? []) {
    if (p.in !== 'query') continue
    const v = (st.vars[p.name] ?? '').trim()
    if (v) qs.set(p.name, v)
  }
  const q = qs.toString()
  if (q) url += (url.includes('?') ? '&' : '?') + q
  return url
}

const previewURL = (it: ApiItem) => buildURL(it)
const openURL = (it: ApiItem) => base.value + buildURL(it)

/** 管理口令：服务端设了 VDL_ADMIN_TOKEN 时，写接口要带 X-Admin-Token */
const tokenKey = 'vdl_admin_token'
const adminToken = ref(localStorage.getItem(tokenKey) ?? '')
watch(adminToken, (v) => {
  if (v.trim()) localStorage.setItem(tokenKey, v)
  else localStorage.removeItem(tokenKey)
})

/** 进行中的请求：重置时先中止，避免旧请求的结果回来时写进已经被丢弃的状态 */
const aborts = new Map<string, AbortController>()

async function send(it: ApiItem) {
  const st = stateOf(it)
  if (st.sending) return
  const url = buildURL(it)
  const miss = url.match(/\{\w+\}/)
  if (miss) {
    toast(`请先填写路径参数 ${miss[0]}`, 'err', 1800)
    return
  }
  if (it.confirm) {
    const yes = await confirmDialog({
      title: `确认发送 ${it.m} 请求`,
      message: `${it.p}\n\n${it.confirm}`,
      okText: '发送',
      danger: it.danger === true,
    })
    if (!yes) return
  }

  const headers: Record<string, string> = {}
  const init: RequestInit = { method: it.m, headers }
  const bodyText = st.body.trim()
  if (hasBody(it) && bodyText) {
    headers['Content-Type'] = 'application/json'
    init.body = bodyText
  }
  const token = adminToken.value.trim()
  if (token) headers['X-Admin-Token'] = token

  // fetch 默认没有超时：vacuum / update/apply 这类接口可能几分钟不回，
  // 不设超时按钮会永久停在「发送中…」。重置也会 abort。
  const ac = new AbortController()
  aborts.set(keyOf(it), ac)
  init.signal = ac.signal
  let timedOut = false
  const timeout = window.setTimeout(() => { timedOut = true; ac.abort() }, 120_000)

  st.sending = true
  const t0 = performance.now()
  try {
    const r = await fetch(url, init)
    // 设了访问密码且 Cookie 无效（改密码 / 过期）：直接切回登录页
    if (r.status === 401) markUnauthorized()
    const raw = await r.text()
    const ms = Math.round(performance.now() - t0)
    const type = r.headers.get('content-type') ?? ''
    let text = raw
    if (type.includes('json')) {
      try {
        text = JSON.stringify(JSON.parse(raw), null, 2)
      } catch {
        /* 不是合法 JSON 就按原文显示 */
      }
    }
    // 大响应（列表/导出）只留前 50KB，避免页面卡死
    const truncated = text.length > 50_000
    if (truncated) text = text.slice(0, 50_000)
    // 明确是文本才按文本展示：m3u8（application/vnd.apple.mpegurl）等也是纯文本
    const isText = type === '' ||
      /^(text\/|application\/(json|xml|javascript|x-yaml|yaml|vnd\.apple\.mpegurl|x-mpegurl|x-ndjson|graphql))/i.test(type)
    const binary = !isText
    st.result = {
      status: r.status,
      statusText: r.statusText,
      ms,
      size: new Blob([raw]).size,
      type,
      body: binary ? `二进制响应（${fmtBytes(new Blob([raw]).size)}，${type}）` : text,
      truncated,
    }
  } catch (e) {
    const aborted = e instanceof DOMException && e.name === 'AbortError'
    st.result = {
      status: 0,
      statusText: aborted ? (timedOut ? '请求超时' : '已取消') : '请求失败',
      ms: Math.round(performance.now() - t0),
      size: 0,
      type: '',
      body: aborted
        ? (timedOut ? '超过 120 秒没有响应，已自动取消' : '请求已取消')
        : e instanceof Error ? e.message : String(e),
      truncated: false,
    }
    if (timedOut) toast('请求超时（120 秒），已取消', 'err', 2400)
    else if (!aborted) toast('请求失败', 'err', 1800)
  } finally {
    window.clearTimeout(timeout)
    // 重置后可能已经发了新请求，别把新请求的 controller 删掉
    if (aborts.get(keyOf(it)) === ac) aborts.delete(keyOf(it))
    st.sending = false
  }
}

/* ---------- 展示辅助 ---------- */

const color = (m: string) =>
  m === 'GET' ? 'bg-emerald-500/12 text-emerald-600 dark:text-emerald-400'
  : m === 'POST' ? 'bg-sky-500/12 text-sky-600 dark:text-sky-400'
  : m === 'PUT' ? 'bg-amber-500/15 text-amber-600 dark:text-amber-400'
  : 'bg-rose-500/12 text-rose-600 dark:text-rose-400'

const statusClass = (r: TryResult) =>
  r.status === 0 ? 'bg-rose-500/12 text-rose-600 dark:text-rose-300'
  : r.status >= 200 && r.status < 300 ? 'bg-emerald-500/12 text-emerald-600 dark:text-emerald-400'
  : r.status >= 400 ? 'bg-rose-500/12 text-rose-600 dark:text-rose-300'
  : 'bg-amber-500/15 text-amber-600 dark:text-amber-400'

async function copy(text: string) {
  if (await copyText(text)) toast('已复制', 'ok', 1200)
  else toast('复制失败，请手动选择', 'err', 1800)
}

function curlOf(it: ApiItem) {
  const st = stateOf(it)
  const url = `${base.value}${buildURL(it)}`
  const lines = [`curl -s -X ${it.m} '${url}'`]
  const token = adminToken.value.trim()
  if (token) lines.push(`-H 'X-Admin-Token: ${token}'`)
  if (hasBody(it) && st.body.trim()) {
    lines.push(`-H 'Content-Type: application/json'`)
    lines.push(`-d '${st.body.trim().replace(/'/g, `'\\''`)}'`)
  }
  return lines.join(' \\\n  ')
}
</script>

<template>
  <div class="space-y-3 sm:space-y-4">
    <section class="card card-pad space-y-2">
      <div class="flex flex-wrap items-center gap-2">
        <span class="title">接口</span>
        <span class="chip">同源 /api</span>
        <span class="chip">可选访问密码</span>
        <span class="chip chip-on">可直接试</span>
      </div>
      <p class="text-xs muted">
        站点自身的 HTTP 接口，脚本或第三方客户端可以直接调用。统一返回
        <code>{ ok: true, data }</code> 或 <code>{ ok: false, msg }</code>。
        默认按局域网使用设计，没设访问密码时不需要鉴权，请勿直接暴露到公网。
      </p>
      <p class="text-xs muted">
        在「信息」页 →「运行配置」里设了<strong>访问密码</strong>后，除了
        <code>/healthz</code> 与 <code>/api/auth/*</code>，所有接口（含 <code>/play/</code>）都要带密码：
        浏览器登录一次即可（服务端下发 HttpOnly Cookie，30 天有效）；
        脚本调用请在请求头带 <code>X-Access-Password</code>，或加 <code>?password=</code> 参数
        （查询参数容易进访问日志，只建议临时使用）。未通过时返回
        <code>401</code> 与 <code>{ ok: false, code: "auth_required" }</code>；
        同一 IP 连续输错 5 次会锁定 5 分钟（响应码 429，code=auth_locked）。
      </p>
      <p class="text-xs muted">
        每个接口点「试一下」即可填参数直接发请求（已登录时自动带 Cookie），响应会显示状态码、耗时与返回内容。
      </p>
      <p class="break-all text-[11px] text-slate-400">Base URL：{{ base }}</p>
      <details class="pt-1">
        <summary class="cursor-pointer text-[11px] muted">管理口令（可选，服务端设了 VDL_ADMIN_TOKEN 时需要）</summary>
        <div class="mt-1.5 flex flex-wrap items-center gap-2">
          <input
            v-model="adminToken"
            type="password"
            class="input max-w-xs"
            placeholder="X-Admin-Token"
            autocomplete="off"
            spellcheck="false"
          />
          <span class="text-[11px] text-slate-400">只保存在本机浏览器，写接口会带上这个头</span>
        </div>
      </details>
    </section>

    <section v-for="g in groups" :key="g.title" class="card card-pad space-y-2">
      <div class="flex flex-wrap items-baseline gap-2">
        <span class="title">{{ g.title }}</span>
        <span v-if="g.desc" class="text-[11px] muted">{{ g.desc }}</span>
      </div>
      <ul class="divide-y divide-slate-100 dark:divide-white/5">
        <li v-for="it in g.items" :key="it.m + it.p" class="py-2">
          <div class="flex flex-wrap items-start gap-x-3 gap-y-1">
            <span class="w-[52px] shrink-0 rounded px-1.5 py-0.5 text-center text-[10px] font-semibold" :class="color(it.m)">{{ it.m }}</span>
            <code class="shrink-0 font-mono text-xs">{{ it.p }}</code>
            <span class="min-w-0 flex-1 break-words text-xs muted">{{ it.d }}</span>
            <button
              class="btn btn-ghost btn-sm shrink-0"
              :class="{ 'chip-on': isOpen(it) }"
              @click="toggleTry(it)"
            >{{ isOpen(it) ? '收起' : '试一下' }}</button>
            <button
              class="btn btn-ghost btn-sm shrink-0"
              :title="curlOf(it)"
              @click="copy(curlOf(it))"
            >复制 curl</button>
          </div>
          <p v-if="it.q" class="w-full text-[11px] text-slate-400 sm:pl-[64px]">{{ it.q }}</p>
          <p v-if="it.confirm" class="w-full text-[11px] text-amber-600 dark:text-amber-400 sm:pl-[64px]">注意：{{ it.confirm }}</p>

          <div
            v-if="isOpen(it)"
            class="mt-2 space-y-2.5 rounded-xl border border-slate-200 bg-slate-50/70 p-3 dark:border-white/10 dark:bg-white/[0.03]"
          >
            <div v-if="it.params?.length" class="grid gap-2 sm:grid-cols-2">
              <label v-for="p in it.params" :key="p.in + p.name" class="block">
                <span class="mb-1 flex flex-wrap items-center gap-1 text-[11px] muted">
                  <span class="font-mono text-slate-600 dark:text-slate-300">{{ p.name }}</span>
                  <span class="rounded bg-slate-200/70 px-1 text-[10px] dark:bg-white/10">{{ p.in === 'path' ? '路径' : '查询' }}</span>
                  <span v-if="p.required" class="text-rose-500">必填</span>
                  <span v-if="p.desc">{{ p.desc }}</span>
                </span>
                <input
                  v-model="stateOf(it).vars[p.name]"
                  class="input font-mono text-xs"
                  :placeholder="p.example || p.name"
                  spellcheck="false"
                />
              </label>
            </div>

            <label v-if="hasBody(it)" class="block">
              <span class="mb-1 block text-[11px] muted">请求体（JSON）</span>
              <textarea
                v-model="stateOf(it).body"
                class="input font-mono text-xs leading-relaxed"
                rows="6"
                spellcheck="false"
              ></textarea>
            </label>

            <div class="flex flex-wrap items-center gap-2">
              <button
                v-if="!it.noTry"
                class="btn btn-primary btn-sm"
                :disabled="stateOf(it).sending"
                @click="send(it)"
              >{{ stateOf(it).sending ? '发送中…' : '发送请求' }}</button>
              <a
                v-else
                class="btn btn-ghost btn-sm"
                :href="openURL(it)"
                target="_blank"
                rel="noopener"
              >新窗口打开</a>
              <button class="btn btn-ghost btn-sm" @click="reset(it)">重置</button>
              <code class="min-w-0 break-all text-[11px] text-slate-400">{{ previewURL(it) }}</code>
            </div>
            <p v-if="it.noTry" class="text-[11px] text-amber-600 dark:text-amber-400">{{ it.noTry }}</p>

            <div v-if="hasResult(it)" class="space-y-1">
              <div class="flex flex-wrap items-center gap-2 text-[11px]">
                <span class="rounded px-1.5 py-0.5 font-semibold" :class="statusClass(resultOf(it))">
                  {{ resultOf(it).status }} {{ resultOf(it).statusText }}
                </span>
                <span class="muted">{{ resultOf(it).ms }} ms</span>
                <span class="muted">{{ fmtBytes(resultOf(it).size) }}</span>
                <span v-if="resultOf(it).truncated" class="text-amber-600 dark:text-amber-400">已截断（仅显示前 50KB）</span>
                <button class="btn btn-ghost btn-sm ml-auto" @click="copy(resultOf(it).body)">复制响应</button>
              </div>
              <pre class="max-h-80 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-white p-2 text-[11px] leading-relaxed dark:bg-black/20">{{ resultOf(it).body }}</pre>
            </div>
          </div>
        </li>
      </ul>
    </section>
  </div>
</template>

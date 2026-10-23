<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { api } from '../api'
import { confirmDialog } from '../confirm'
import { toast } from '../toast'
import type { Source, SourceProgress, SourceTest, SyncLog, SyncLogItem } from '../types'

/**
 * 来源管理：添加（只填域名）+ 列表（连通性 / 采集进度 / 排队状态 / 同步记录）。
 * 列表默认只读展示，点「编辑」才进编辑态 —— 避免在输入框里误改后忘记保存。
 */
const list = ref<Source[]>([])
const progress = ref<Record<number, SourceProgress>>({})
const tests = reactive<Record<number, SourceTest | undefined>>({})
const testing = reactive<Record<number, boolean>>({})
// 增量同步记录：默认收起，点「同步记录」时按需拉取，避免列表页一次请求所有来源的历史
const logs = reactive<Record<number, SyncLog[] | undefined>>({})
const logsOpen = reactive<Record<number, boolean>>({})
const logsLoading = reactive<Record<number, boolean>>({})
// 同步记录分页：每页 10 条（后端最多保留 100 条/来源），页码与总数按来源分别记录
const LOG_PAGE_SIZE = 10
const logPage = reactive<Record<number, number>>({})
const logTotal = reactive<Record<number, number>>({})
// 单条记录的影片明细：默认不加载，点开某条记录时按需拉取（最多 500 条）
const logItems = reactive<Record<number, SyncLogItem[] | undefined>>({})
const logItemsOpen = reactive<Record<number, boolean>>({})
const logItemsLoading = reactive<Record<number, boolean>>({})
// 定时增量任务状态：间隔与下一次触发时间（由 /api/scheduler 轮询）
const sched = reactive({ enabled: false, interval_sec: 0, next_run_unix: 0 })
const nowMs = ref(Date.now())
// 上一次轮询时正在采集的来源：用来在增量刚跑完时自动刷新已展开的记录面板
const wasRunning = new Set<number>()
const editing = reactive<Record<number, { name: string; url: string }>>({})
const draft = reactive({ name: '', url: '' })
const busy = ref(false)
const testingAll = ref(false)
// 添加前的预检：只有点了「测试」才有值；成功后 add() 优先用探测出的可用地址
const probing = ref(false)
const probe = ref<SourceTest | null>(null)
const probeURL = ref('')
const q = ref('')
let timer: number | undefined
let schedTimer: number | undefined
let tickTimer: number | undefined

const phaseText: Record<string, string> = {
  pending: '排队中', queued: '排队中', running: '采集中', stopping: '停止中…', stopped: '已停止',
  done: '已完成', warning: '完成（有警告）', failed: '失败',
}

const anyRunning = computed(() => Object.values(progress.value).some((p) => p.running))
// 排队等待采集的来源数（超过「同时采集的接口数」时会排队，采完一个自动接下一个）
const queuedCount = computed(() => Object.values(progress.value).filter((p) => !p.running && p.phase === 'queued').length)
const enabledCount = computed(() => list.value.filter((s) => s.enabled === 1).length)
// 会参与定时增量任务的来源数（启用 + 自动增量都打开）
const autoIncrCount = computed(() => list.value.filter((s) => s.enabled === 1 && s.auto_incr === 1).length)

// 采集间隔文案（10 秒 / 30 分钟 / 1 小时）
const schedIntervalText = computed(() => {
  const sec = sched.interval_sec
  if (!sec) return ''
  if (sec % 3600 === 0) return `${sec / 3600} 小时`
  if (sec % 60 === 0) return `${sec / 60} 分钟`
  return `${sec} 秒`
})

// 下次自动采集倒计时：本地每秒刷新，触发时间由后端提供
const countdown = computed(() => {
  if (!sched.enabled || !sched.next_run_unix) return ''
  const left = sched.next_run_unix * 1000 - nowMs.value
  if (left <= 0) return '即将开始'
  const s = Math.round(left / 1000)
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  const mm = String(m).padStart(2, '0')
  const ss = String(sec).padStart(2, '0')
  return h > 0 ? `${h}:${mm}:${ss}` : `${mm}:${ss}`
})

const filtered = computed(() => {
  const kw = q.value.trim().toLowerCase()
  if (!kw) return list.value
  return list.value.filter((s) => (s.name + ' ' + hostOf(s) + ' ' + s.api_url).toLowerCase().includes(kw))
})

function hostOf(s: Source): string {
  try {
    return new URL(s.api_url).host
  } catch {
    return s.api_url
  }
}

function num(n?: number): string {
  return (n || 0).toLocaleString('zh-CN')
}

const logStatusText: Record<string, string> = {
  running: '同步中…', done: '完成', warning: '有警告', failed: '失败', stopped: '已停止',
}

function logStatusClass(st: string): string {
  if (st === 'failed') return 'text-rose-500'
  if (st === 'warning' || st === 'stopped') return 'text-amber-600 dark:text-amber-400'
  if (st === 'running') return 'text-sky-600 dark:text-sky-400'
  return 'text-emerald-600 dark:text-emerald-400'
}

// 后端时间是 UTC，但格式可能是 RFC3339（驱动把 DATETIME 解成 time.Time）或
// SQLite 原样的 "YYYY-MM-DD HH:mm:ss"，两种都兼容，最后按本地时区展示
function logTime(v?: string): string {
  if (!v) return '—'
  const t = new Date(v.includes('T') ? v : v.replace(' ', 'T') + 'Z')
  if (Number.isNaN(t.getTime())) return v.slice(5, 16)
  return t.toLocaleString('zh-CN', {
    month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false,
  })
}

function logDur(ms?: number): string {
  const n = ms || 0
  if (n < 1000) return n + 'ms'
  if (n < 60000) return (n / 1000).toFixed(1) + 's'
  return Math.floor(n / 60000) + 'm' + Math.round((n % 60000) / 1000) + 's'
}

// 源站更新时间（vod_time）是源站本地时间，不能当 UTC 转换，只截断展示
function srcTime(v?: string): string {
  if (!v) return ''
  return v.replace('T', ' ').slice(5, 16)
}

async function loadLogs(id: number) {
  logsLoading[id] = true
  const page = logPage[id] || 1
  const r = await api.syncLogs(id, page, LOG_PAGE_SIZE)
  logsLoading[id] = false
  if (!r.ok || !r.data) {
    toast(r.msg || '同步记录读取失败', 'err', 2400)
    return
  }
  logs[id] = r.data.items || []
  logTotal[id] = r.data.total || 0
  // 后端在页码越界时会回退到最后一页，这里同步过来
  if (r.data.page && r.data.page !== page) logPage[id] = r.data.page
}

// 同步记录总页数（每页 LOG_PAGE_SIZE 条，最少 1 页）
function logPages(id: number): number {
  return Math.max(1, Math.ceil((logTotal[id] || 0) / LOG_PAGE_SIZE))
}

async function gotoLogPage(id: number, p: number) {
  const next = Math.min(logPages(id), Math.max(1, p))
  if (next === (logPage[id] || 1)) return
  logPage[id] = next
  await loadLogs(id)
}

async function loadSched() {
  const r = await api.scheduler()
  if (!r.ok || !r.data) return
  sched.enabled = !!r.data.enabled
  sched.interval_sec = r.data.interval_sec || 0
  sched.next_run_unix = r.data.next_run_unix || 0
}

async function toggleLogs(s: Source) {
  logsOpen[s.id] = !logsOpen[s.id]
  if (logsOpen[s.id]) await loadLogs(s.id)
}

// 展开/收起某条记录的影片明细（首次展开才请求）
async function toggleLogItems(s: Source, g: SyncLog) {
  if (logItemsOpen[g.id]) {
    logItemsOpen[g.id] = false
    return
  }
  logItemsOpen[g.id] = true
  if (logItems[g.id]) return
  logItemsLoading[g.id] = true
  const r = await api.syncLogItems(s.id, g.id)
  logItemsLoading[g.id] = false
  if (!r.ok) {
    toast(r.msg || '影片明细读取失败', 'err', 2400)
    return
  }
  logItems[g.id] = r.data || []
}

async function load() {
  const r = await api.sources()
  if (!r.ok || !r.data) return
  list.value = r.data
  for (const id of Object.keys(editing)) {
    if (!r.data.some((s) => s.id === Number(id))) delete editing[Number(id)]
  }
}

async function loadProgress() {
  const r = await api.progress()
  if (!r.ok || !r.data) return
  const m: Record<number, SourceProgress> = {}
  const nowRunning = new Set<number>()
  for (const p of r.data.sources || []) {
    m[p.source_id] = p
    if (p.running) nowRunning.add(p.source_id)
    // 这一轮里刚跑完的增量：已展开的记录面板自动刷新，不用手动点「刷新」
    if (wasRunning.has(p.source_id) && !p.running && p.mode === 'incr' && logsOpen[p.source_id]) {
      loadLogs(p.source_id)
    }
  }
  // 开始了一轮新的采集（可能是定时任务）→ 刷新下次触发时间
  if (nowRunning.size > 0 && wasRunning.size === 0) loadSched()
  wasRunning.clear()
  for (const id of nowRunning) wasRunning.add(id)
  progress.value = m
}

async function add() {
  const name = draft.name.trim()
  const url = draft.url.trim()
  if (!name) return toast('请填写来源名称', 'err', 1600)
  if (!url) return toast('请填写来源地址，例如 https://www.baidu.com', 'err', 2200)
  busy.value = true
  // 测试过就用探测出来的可用地址（可能是补上 .php 或换成 at/xml 的写法）
  const r = await api.addSource(name, probeURL.value || url)
  busy.value = false
  if (!r.ok) return toast(r.msg || '添加失败', 'err', 2600)
  toast('已添加：' + (r.data?.api_url || url), 'ok', 2600)
  draft.name = ''
  draft.url = ''
  probe.value = null
  probeURL.value = ''
  load()
}

/**
 * 添加前测试：只填域名也能测。
 * 后端会依次尝试 /api.php/provide/vod/、/api/provide/vod/ 以及 at=xml 两种写法，
 * 返回第一个真能解析出数据的地址 + 格式/页数/条数/detail 情况。
 */
async function probeDraft() {
  const url = draft.url.trim()
  if (!url) return toast('先填来源地址（填域名即可，如 www.baidu.com）', 'err', 2200)
  probing.value = true
  probe.value = null
  probeURL.value = ''
  const r = await api.probeSource(url)
  probing.value = false
  if (!r.ok || !r.data) return toast(r.msg || '测试失败', 'err', 2800)
  probe.value = r.data
  if (r.data.ok && r.data.api_url) probeURL.value = r.data.api_url
  if (r.data.ok) toast('可以同步，点「添加」即可', 'ok', 2200)
}

// 改了地址就作废旧结果，避免「测的是 A、添加的是 B」
watch(() => draft.url, () => {
  probe.value = null
  probeURL.value = ''
})

function startEdit(s: Source) {
  editing[s.id] = { name: s.name, url: s.api_url }
}

function cancelEdit(s: Source) {
  delete editing[s.id]
}

async function save(s: Source) {
  const e = editing[s.id]
  if (!e) return
  if (!e.name.trim()) return toast('名称不能为空', 'err', 1600)
  if (!e.url.trim()) return toast('来源地址不能为空', 'err', 1600)
  const r = await api.updateSource(s.id, { name: e.name.trim(), api_url: e.url.trim(), enabled: s.enabled })
  if (!r.ok) return toast(r.msg || '保存失败', 'err', 2400)
  toast('已保存', 'ok', 1500)
  delete editing[s.id]
  await load()
  await loadProgress()
}

async function toggleEnabled(s: Source) {
  const next = s.enabled === 1 ? 0 : 1
  const r = await api.updateSource(s.id, { name: s.name, api_url: s.api_url, enabled: next })
  if (!r.ok) return toast(r.msg || '操作失败', 'err', 2400)
  s.enabled = next
  toast(next ? '已启用' : '已停用（不再参与采集）', 'ok', 1500)
}

// 自动增量：只决定「定时任务」采不采这个来源；手动点增量/全量不受影响
function autoOn(s: Source) {
  return s.auto_incr === 1
}

async function toggleAutoIncr(s: Source) {
  const next = !autoOn(s)
  const r = await api.setAutoIncr(s.id, next)
  if (!r.ok) return toast(r.msg || '操作失败', 'err', 2400)
  s.auto_incr = next ? 1 : 0
  toast(next ? '已开启自动增量：定时任务会采这个来源' : '已关闭自动增量：仍可手动点「增量采集」', 'ok', 2400)
}

async function remove(s: Source) {
  const okc = await confirmDialog({
    title: '删除来源',
    message: `删除来源「${s.name}」？\n\n删除后会一并清空：\n` +
      `· 影片记录、播放地址\n· 下载记录与已下载文件\n· 封面缓存\n· 采集断点与增量同步记录\n\n该操作不可恢复。`,
    okText: '删除',
    danger: true,
  })
  if (!okc) return
  busy.value = true
  const r = await api.delSource(s.id)
  busy.value = false
  if (!r.ok) return toast(r.msg || '删除失败', 'err', 2600)
  toast(r.data?.msg || '已删除', 'ok', 3200)
  delete editing[s.id]
  delete tests[s.id]
  await load()
  await loadProgress()
}

async function test(s: Source) {
  testing[s.id] = true
  tests[s.id] = { ok: true, msg: '测试中…' }
  const r = await api.testSource(s.id)
  testing[s.id] = false
  tests[s.id] = r.ok ? r.data : { ok: false, msg: r.msg || '测试失败' }
}

// 导入来源列表：读取本系统导出的 JSON，后端按 api_url 去重
const importing = ref(false)
const importInput = ref<HTMLInputElement | null>(null)

async function onImportFile(e: Event) {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = '' // 允许连续导入同一个文件
  if (!file) return
  let parsed: unknown
  try {
    parsed = JSON.parse(await file.text())
  } catch {
    return toast('文件不是有效的 JSON，请选择本系统导出的来源文件', 'err', 3000)
  }
  const sources = Array.isArray(parsed) ? parsed : (parsed as { sources?: unknown[] })?.sources
  if (!Array.isArray(sources) || !sources.length) {
    return toast('文件里没有可导入的来源', 'err', 2600)
  }
  importing.value = true
  const r = await api.importSources(sources)
  importing.value = false
  if (!r.ok) return toast(r.msg || '导入失败', 'err', 3200)
  toast(r.data?.msg || '导入完成', 'ok', 3600)
  load()
}

async function testAll() {
  if (!list.value.length) return
  testingAll.value = true
  for (const s of list.value) {
    await test(s)
  }
  testingAll.value = false
  const bad = list.value.filter((s) => tests[s.id] && !tests[s.id]?.ok).length
  toast(
    bad ? `${list.value.length} 个来源已测试，${bad} 个异常` : `${list.value.length} 个来源全部正常`,
    bad ? 'err' : 'ok',
    2600,
  )
}

async function collect(s: Source, full: boolean) {
  const r = await api.collect(full, s.id)
  if (!r.ok) return toast(r.msg || '启动失败', 'err', 2400)
  toast(r.data || '已开始采集', 'ok', 2000)
  loadProgress()
}

async function collectAll(full: boolean) {
  if (full && !(await confirmDialog({
    title: '全量采集',
    message: '对全部启用来源重新全量采集，耗时较长，确认继续？',
    okText: '开始采集',
  }))) return
  const r = await api.collect(full, undefined, full)
  if (!r.ok) return toast(r.msg || '启动失败', 'err', 2600)
  toast(r.data || '已开始', 'ok', 2600)
  loadProgress()
}

// stop / stopAll 只是发停止信号：后端会在当前这一页采完后收尾，
// 进度里的 phase 先变 stopping、再变 stopped，所以这里立刻刷新一次让按钮变成「停止中…」。
async function stop(s: Source) {
  const r = await api.stopCollect(s.id)
  if (!r.ok) return toast(r.msg || '停止失败', 'err', 2400)
  toast(r.data || '已停止', 'ok', 2000)
  await loadProgress()
}

async function stopAll() {
  const r = await api.stopCollect()
  if (!r.ok) return toast(r.msg || '停止失败', 'err', 2400)
  toast(r.data || '已停止', 'ok', 2400)
  await loadProgress()
}

onMounted(async () => {
  await load()
  await loadProgress()
  await loadSched()
  timer = window.setInterval(loadProgress, 3000)
  schedTimer = window.setInterval(loadSched, 10000)
  // 倒计时每秒走一格（只算本地剩余时间，不打接口）
  tickTimer = window.setInterval(() => { nowMs.value = Date.now() }, 1000)
})
onBeforeUnmount(() => {
  if (timer) window.clearInterval(timer)
  if (schedTimer) window.clearInterval(schedTimer)
  if (tickTimer) window.clearInterval(tickTimer)
})
</script>

<template>
  <div class="space-y-4">
    <!-- 添加来源 -->
    <section class="card card-pad">
      <div class="mb-1 flex flex-wrap items-center gap-2">
        <span class="title">添加采集来源</span>
        <span class="chip">苹果CMS v10</span>
      </div>
      <p class="mb-3 text-xs muted">
        只填域名即可（如 <code class="text-amber-600 dark:text-amber-400">www.baidu.com</code>），
        会自动识别 <b class="font-medium">.php / 无 .php</b> 两种路径与 <b class="font-medium">JSON / XML</b> 两种格式，
        完整的来源地址也能直接粘贴。建议先点「测试」确认能同步，再添加。
      </p>
      <div class="flex flex-col gap-2 sm:flex-row">
        <input v-model="draft.name" class="input sm:w-52" placeholder="来源名称" />
        <input
          v-model="draft.url" class="input flex-1" placeholder="来源地址 例如https://www.baidu.com"
          inputmode="url" enterkeyhint="done" @keyup.enter="add"
        />
        <button class="btn btn-ghost sm:w-24" :disabled="probing || busy" @click="probeDraft">
          {{ probing ? '测试中…' : '测试' }}
        </button>
        <button class="btn btn-primary sm:w-24" :disabled="busy || probing" @click="add">添加</button>
      </div>

      <!-- 添加前的预检结果 -->
      <div v-if="probe" class="mt-2 rounded-xl bg-slate-50 px-2.5 py-2 text-xs dark:bg-white/5">
        <div class="flex flex-wrap items-center gap-1.5">
          <span
            class="rounded px-1.5 py-0.5 text-[11px] font-medium"
            :class="probe.ok ? 'bg-emerald-500/15 text-emerald-600 dark:text-emerald-400'
              : 'bg-rose-500/15 text-rose-600 dark:text-rose-400'"
          >{{ probe.ok ? '可以同步' : '不可用' }}</span>
          <span v-if="probe.http_code" class="chip">HTTP {{ probe.http_code }}</span>
          <span v-if="probe.format" class="chip">{{ (probe.format || '').toUpperCase() }}</span>
          <span v-if="probe.cost_ms !== undefined" class="chip">{{ probe.cost_ms }}ms</span>
          <span v-if="probe.pagecount" class="chip">{{ probe.pagecount }} 页 / {{ num(probe.total) }} 条</span>
          <span v-if="probe.pagination === true" class="chip">分页 ✓</span>
          <span v-if="probe.detail_ok === true" class="chip">detail ✓</span>
        </div>
        <p class="mt-1.5 break-all" :class="probe.ok ? 'text-slate-600 dark:text-slate-300' : 'text-rose-500'">
          {{ probe.msg }}
        </p>
        <p v-if="probe.api_url" class="mt-1 break-all text-[11px] muted">
          将使用 <code class="text-amber-600 dark:text-amber-400">{{ probe.api_url }}</code>
        </p>
        <p v-if="probe.ok && probe.parsed === 0" class="mt-1 text-amber-600 dark:text-amber-400">
          来源能连通，但首页没解析到影片（源站可能暂时为空）
        </p>
        <p v-if="probe.detail_sample" class="mt-1 truncate text-[11px] text-slate-400" :title="probe.detail_sample">
          样例：{{ probe.detail_sample }}
        </p>
        <p v-if="!probe.ok && probe.tried && probe.tried.length" class="mt-1 break-all text-[11px] text-slate-400">
          已尝试：{{ probe.tried.join(' → ') }}
        </p>
      </div>
    </section>

    <!-- 工具栏 -->
    <section class="card card-pad space-y-3">
      <div class="flex flex-wrap items-center gap-2">
        <span class="title">来源列表</span>
        <span class="chip">{{ list.length }} 个 · 启用 {{ enabledCount }}</span>
        <span
          v-if="enabledCount > 0"
          class="chip"
          :class="autoIncrCount < enabledCount ? 'chip-on' : ''"
          :title="`定时增量任务会采集 ${autoIncrCount} 个来源（启用 ${enabledCount} 个）`"
        >自动增量 {{ autoIncrCount }}/{{ enabledCount }}</span>
        <span
          v-if="autoIncrCount > 0 && sched.enabled && countdown"
          class="chip"
          :title="`定时任务每 ${schedIntervalText}自动采集一次（启用且打开自动增量的来源）`"
        >下次自动采集 <b class="font-semibold tabular-nums">{{ countdown }}</b></span>
        <span v-if="anyRunning" class="chip chip-on">采集进行中…</span>
        <span v-if="queuedCount > 0" class="chip">排队中 {{ queuedCount }}</span>

        <div class="relative ml-auto w-full sm:w-64">
          <svg viewBox="0 0 24 24" class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400" fill="none" stroke="currentColor" stroke-width="2">
            <circle cx="11" cy="11" r="7" /><path d="m20 20-3.5-3.5" />
          </svg>
          <input v-model="q" class="input py-1.5 pl-9 text-sm" type="search" placeholder="搜索来源名 / 域名" />
        </div>
      </div>

      <div class="flex flex-wrap items-center gap-2 border-t border-slate-100 pt-2 dark:border-white/5">
        <button class="btn btn-ghost btn-sm" :disabled="testingAll" @click="testAll">
          {{ testingAll ? '测试中…' : '测试全部' }}
        </button>
        <button class="btn btn-ghost btn-sm" @click="collectAll(false)">全部增量</button>
        <button class="btn btn-ghost btn-sm" @click="collectAll(true)">全部全量</button>
        <button v-if="anyRunning || queuedCount > 0" class="btn btn-warn btn-sm" @click="stopAll">停止全部采集</button>
        <!-- 换机/重装迁移：导出 JSON，导入时按地址去重 -->
        <button class="btn btn-ghost btn-sm" :disabled="!list.length" @click="api.exportSources()">导出</button>
        <button class="btn btn-ghost btn-sm" :disabled="importing" @click="importInput?.click()">
          {{ importing ? '导入中…' : '导入' }}
        </button>
        <input ref="importInput" type="file" accept=".json,application/json" class="hidden" @change="onImportFile" />
        <span class="ml-auto hidden text-[11px] muted sm:inline">全量=重新抓所有页 · 增量=只抓有变化的 · 自动增量=定时任务自动采</span>
      </div>
    </section>

    <!-- 来源卡片 -->
    <section class="space-y-3">
      <div v-if="filtered.length === 0" class="card grid place-items-center gap-2 py-16 text-center">
        <div class="text-3xl">{{ q ? '🔍' : '🔌' }}</div>
        <div class="title">{{ q ? '没有匹配的来源' : '还没有采集来源' }}</div>
        <p class="text-sm muted">{{ q ? '换个关键词试试' : '在上方填站点域名添加一个' }}</p>
      </div>

      <div v-for="s in filtered" :key="s.id" class="card p-3 sm:p-4">
        <!-- 头部 -->
        <div class="flex flex-wrap items-start gap-2">
          <span
            class="mt-1.5 h-2 w-2 shrink-0 rounded-full"
            :class="s.enabled === 1 ? 'bg-emerald-500' : 'bg-slate-300 dark:bg-slate-600'"
            :title="s.enabled === 1 ? '已启用' : '已停用'"
          ></span>

          <div class="min-w-0 flex-1">
            <div class="flex flex-wrap items-center gap-2">
              <span class="rounded-md bg-slate-100 px-1.5 py-0.5 text-[11px] text-slate-500 dark:bg-white/10 dark:text-slate-300">
                #{{ s.id }}
              </span>
              <span class="truncate text-sm font-medium">{{ s.name }}</span>
              <span class="chip">{{ hostOf(s) }}</span>
              <span
                v-if="s.enabled !== 1"
                class="rounded bg-slate-200/70 px-1.5 py-0.5 text-[11px] text-slate-500 dark:bg-white/10 dark:text-slate-400"
              >已停用</span>
            </div>

            <div class="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-slate-500 dark:text-slate-400">
              <span
                v-if="s.enabled === 1 && autoOn(s) && sched.enabled"
                class="text-sky-600 dark:text-sky-400"
                :title="`定时任务每 ${schedIntervalText}自动采集一次`"
              >下次自动 {{ countdown || '—' }}</span>
            </div>
            <div class="mt-0.5 hidden truncate text-[11px] text-slate-400 sm:block" :title="s.api_url">{{ s.api_url }}</div>
          </div>

          <button
            class="relative h-6 w-11 shrink-0 rounded-full transition before:absolute before:-inset-2.5 before:content-[''] sm:h-5 sm:w-9"
            :class="s.enabled === 1 ? 'bg-emerald-500' : 'bg-slate-300 dark:bg-slate-600'"
            role="switch" :aria-checked="s.enabled === 1" :title="s.enabled === 1 ? '点击停用' : '点击启用'"
            @click="toggleEnabled(s)"
          >
            <span
              class="absolute left-0.5 top-0.5 h-5 w-5 rounded-full bg-white shadow transition-transform sm:h-4 sm:w-4"
              :class="s.enabled === 1 ? 'translate-x-5 sm:translate-x-4' : ''"
            ></span>
          </button>
        </div>

        <!-- 操作 -->
        <div class="mt-2.5 flex flex-wrap items-center gap-1.5 border-t border-slate-100 pt-2.5 dark:border-white/5">
          <button class="btn btn-ghost btn-sm" :disabled="testing[s.id]" @click="test(s)">
            {{ testing[s.id] ? '测试中…' : '测试' }}
          </button>
          <button
            class="btn btn-primary btn-sm"
            :disabled="progress[s.id]?.running || progress[s.id]?.phase === 'queued'"
            @click="collect(s, true)"
          >全量采集</button>
          <button
            class="btn btn-ghost btn-sm"
            :disabled="progress[s.id]?.running || progress[s.id]?.phase === 'queued'"
            @click="collect(s, false)"
          >增量采集</button>
          <button
            class="btn btn-ghost btn-sm"
            :class="autoOn(s) ? '' : 'text-slate-400 dark:text-slate-500'"
            :title="autoOn(s)
              ? '定时任务会自动采集这个来源，点击关闭'
              : '已关闭自动增量：定时任务跳过它，仍可手动点「增量采集」'"
            :aria-pressed="autoOn(s)"
            @click="toggleAutoIncr(s)"
          >
            <span
              class="h-1.5 w-1.5 rounded-full"
              :class="autoOn(s) ? 'bg-emerald-500' : 'bg-slate-300 dark:bg-slate-600'"
            ></span>
            自动增量
          </button>
          <button
            class="btn btn-ghost btn-sm"
            :class="logsOpen[s.id] ? 'text-sky-600 dark:text-sky-400' : ''"
            title="每次增量同步一条记录（手动 + 定时）"
            @click="toggleLogs(s)"
          >同步记录</button>
          <button
            v-if="progress[s.id]?.running"
            class="btn btn-warn btn-sm"
            :disabled="progress[s.id]?.phase === 'stopping'"
            @click="stop(s)"
          >{{ progress[s.id]?.phase === 'stopping' ? '停止中…' : '停止采集' }}</button>
          <button
            v-else-if="progress[s.id]?.phase === 'queued'"
            class="btn btn-ghost btn-sm"
            title="从排队中移除，不会再自动开始采集"
            @click="stop(s)"
          >取消排队</button>

          <template v-if="editing[s.id]">
            <button class="btn btn-ghost btn-sm" @click="save(s)">保存</button>
            <button class="btn btn-ghost btn-sm" @click="cancelEdit(s)">取消</button>
          </template>
          <button v-else class="btn btn-ghost btn-sm" @click="startEdit(s)">编辑</button>

          <button class="btn btn-danger btn-sm ml-auto" @click="remove(s)">删除</button>
        </div>

        <!-- 编辑态 -->
        <div v-if="editing[s.id]" class="mt-2 flex flex-col gap-2 rounded-xl bg-slate-50 p-2.5 sm:flex-row dark:bg-white/5">
          <input v-model="editing[s.id].name" class="input h-9 py-1 sm:w-52" placeholder="来源名称" />
          <input v-model="editing[s.id].url" class="input h-9 min-w-0 flex-1 py-1 text-xs" placeholder="来源地址 例如https://www.baidu.com" />
        </div>

        <!-- 测试结果 -->
        <div v-if="tests[s.id]" class="mt-2 rounded-xl bg-slate-50 px-2.5 py-2 text-xs dark:bg-white/5">
          <div class="flex flex-wrap items-center gap-1.5">
            <span
              class="rounded px-1.5 py-0.5 text-[11px] font-medium"
              :class="tests[s.id]?.ok ? 'bg-emerald-500/15 text-emerald-600 dark:text-emerald-400'
                : 'bg-rose-500/15 text-rose-600 dark:text-rose-400'"
            >{{ tests[s.id]?.ok ? '正常' : '异常' }}</span>
            <span v-if="tests[s.id]?.http_code" class="chip">HTTP {{ tests[s.id]?.http_code }}</span>
            <span v-if="tests[s.id]?.format" class="chip">{{ (tests[s.id]?.format || '').toUpperCase() }}</span>
            <span v-if="tests[s.id]?.cost_ms !== undefined" class="chip">{{ tests[s.id]?.cost_ms }}ms</span>
            <span v-if="tests[s.id]?.pagecount" class="chip">
              {{ tests[s.id]?.pagecount }} 页 / {{ num(tests[s.id]?.total) }} 条
            </span>
            <span v-if="tests[s.id]?.pagination === true" class="chip">分页 ✓</span>
            <span v-if="tests[s.id]?.detail_ok === true" class="chip">detail ✓</span>
          </div>
          <p class="mt-1.5 break-all" :class="tests[s.id]?.ok ? 'text-slate-600 dark:text-slate-300' : 'text-rose-500'">
            {{ tests[s.id]?.msg }}
          </p>
          <p v-if="tests[s.id]?.pagination === false" class="mt-1 text-amber-600 dark:text-amber-400">
            ⚠ 源站忽略分页参数，全量只能采到第 1 页
          </p>
          <p v-if="tests[s.id]?.detail_ok === false" class="mt-1 text-amber-600 dark:text-amber-400">
            ⚠ detail 来源拿不到播放地址，采到的影片无法下载
          </p>
          <p v-if="tests[s.id]?.detail_sample" class="mt-1 truncate text-[11px] text-slate-400" :title="tests[s.id]?.detail_sample">
            样例：{{ tests[s.id]?.detail_sample }}
          </p>
        </div>

        <!-- 采集进度 -->
        <div v-if="progress[s.id]" class="mt-2 text-xs">
          <div class="flex flex-wrap items-center gap-2">
            <span
              :class="progress[s.id].running || progress[s.id].phase === 'stopped'
                ? 'text-amber-600 dark:text-amber-400' : 'text-slate-500'"
            >
              {{ phaseText[progress[s.id].phase] || progress[s.id].phase }}
            </span>
            <span v-if="progress[s.id].phase !== 'queued'" class="text-slate-400">
              页 {{ progress[s.id].page }}/{{ progress[s.id].page_count || '?' }}
              · 取得 {{ num(progress[s.id].fetched) }}
              · 新增 {{ num(progress[s.id].added) }}
              · 更新 {{ num(progress[s.id].updated) }}
            </span>
            <span v-else class="text-slate-400">等待前面的来源采完，会自动开始</span>
            <span v-if="progress[s.id].running" class="ml-auto text-[11px] text-slate-400">
              {{ progress[s.id].mode === 'full' ? '全量' : '增量' }}
            </span>
          </div>
          <div v-if="progress[s.id].phase !== 'queued'" class="mt-1.5">
            <div class="bar">
              <i :style="{ width: (progress[s.id].page_count ? Math.min(100, Math.round(progress[s.id].page * 100 / progress[s.id].page_count)) : (progress[s.id].running ? 30 : 100)) + '%' }"></i>
            </div>
          </div>
          <p v-if="progress[s.id].error" class="mt-1 break-all text-rose-500">{{ progress[s.id].error }}</p>
          <p v-if="progress[s.id].phase === 'stopped'" class="mt-1 text-amber-600 dark:text-amber-400">
            已手动停止，采到的数据已入库；再点「全量采集」会从断点继续。
          </p>
        </div>

        <!-- 增量同步记录：每次手动/定时增量一条汇总（新的在前），分页展示 -->
        <div v-if="logsOpen[s.id]" class="mt-2 rounded-xl bg-slate-50 px-2.5 py-2 text-xs dark:bg-white/5">
          <div class="flex flex-wrap items-center gap-2">
            <span class="font-medium">同步记录</span>
            <span class="chip" title="每个来源最多保留 100 条">仅增量 · 共 {{ num(logTotal[s.id]) }} 条</span>
            <span v-if="logPages(s.id) > 1" class="chip">第 {{ logPage[s.id] || 1 }}/{{ logPages(s.id) }} 页</span>
            <button class="btn btn-ghost btn-sm ml-auto" :disabled="logsLoading[s.id]" @click="loadLogs(s.id)">
              {{ logsLoading[s.id] ? '刷新中…' : '刷新' }}
            </button>
          </div>
          <p v-if="!logs[s.id]" class="mt-1.5 text-slate-400">
            {{ logsLoading[s.id] ? '加载中…' : '暂无记录' }}
          </p>
          <p v-else-if="!logs[s.id]?.length" class="mt-1.5 text-slate-400">
            还没有同步记录：点「增量采集」或等定时任务跑一次（全量采集不记录）。
          </p>
          <template v-else>
            <ul class="mt-1 divide-y divide-slate-100 dark:divide-white/5">
              <li v-for="g in logs[s.id]" :key="g.id" class="py-1.5 first:pt-0 last:pb-0">
                <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
                  <span class="shrink-0 tabular-nums text-slate-400">{{ logTime(g.started_at) }}</span>
                  <span class="shrink-0 rounded bg-slate-200/70 px-1 text-[10px] text-slate-500 dark:bg-white/10 dark:text-slate-300">
                    {{ g.trigger === 'auto' ? '自动' : '手动' }}
                  </span>
                  <span class="shrink-0 font-medium" :class="logStatusClass(g.status)">{{ logStatusText[g.status] || g.status }}</span>
                  <span class="text-slate-500 dark:text-slate-400">
                    抓取 <b class="font-medium text-slate-700 dark:text-slate-200">{{ num(g.fetched) }}</b>
                    · 新增 <b class="font-medium text-emerald-600 dark:text-emerald-400">{{ num(g.added) }}</b>
                    · 更新 <b class="font-medium text-amber-600 dark:text-amber-400">{{ num(g.updated) }}</b>
                    · 耗时 {{ logDur(g.duration_ms) }}
                  </span>
                  <button
                    v-if="g.items_count > 0"
                    class="ml-auto shrink-0 text-[11px] text-sky-600 hover:underline dark:text-sky-400"
                    @click="toggleLogItems(s, g)"
                  >{{ logItemsOpen[g.id] ? '收起影片' : `影片 ${num(g.items_count)} 部` }}</button>
                </div>
                <p v-if="g.error" class="mt-0.5 break-all text-rose-500">{{ g.error }}</p>
                <!-- 本次新增/更新的影片明细（点开才加载） -->
                <div v-if="logItemsOpen[g.id]" class="mt-1 rounded-lg bg-white px-2 py-1 dark:bg-black/20">
                  <p v-if="logItemsLoading[g.id]" class="text-slate-400">加载中…</p>
                  <template v-else>
                    <div
                      v-for="it in logItems[g.id] || []" :key="it.action + '-' + it.vod_id"
                      class="flex flex-wrap items-baseline gap-x-1.5 border-b border-slate-100 py-0.5 last:border-0 dark:border-white/5"
                    >
                      <span
                        class="rounded px-1 text-[10px]"
                        :class="it.action === 'new'
                          ? 'bg-emerald-500/15 text-emerald-600 dark:text-emerald-400'
                          : 'bg-amber-500/15 text-amber-600 dark:text-amber-400'"
                      >{{ it.action === 'new' ? '新增' : '更新' }}</span>
                      <span class="min-w-0 truncate font-medium">{{ it.name }}</span>
                      <span v-if="it.remarks" class="truncate text-slate-400">{{ it.remarks }}</span>
                      <span v-if="it.src_time" class="ml-auto shrink-0 tabular-nums text-slate-400">源站更新 {{ srcTime(it.src_time) }}</span>
                    </div>
                    <p v-if="logItems[g.id]?.length === 0" class="text-slate-400">本次没有新增或更新的影片</p>
                    <p v-if="g.added + g.updated > (logItems[g.id] || []).length" class="pt-0.5 text-slate-400">
                      仅显示前 {{ (logItems[g.id] || []).length }} 部，本次共 {{ num(g.added + g.updated) }} 部
                    </p>
                  </template>
                </div>
              </li>
            </ul>
            <!-- 分页：每页 10 条，记录多了在面板内翻页 -->
            <div class="mt-1.5 flex flex-wrap items-center justify-between gap-2 border-t border-slate-100 pt-1.5 dark:border-white/5">
              <span class="text-[11px] text-slate-400">
                第 {{ logPage[s.id] || 1 }} / {{ logPages(s.id) }} 页 · 共 {{ num(logTotal[s.id]) }} 条
              </span>
              <div class="flex items-center gap-1">
                <button
                  class="btn btn-ghost btn-sm"
                  :disabled="(logPage[s.id] || 1) <= 1 || logsLoading[s.id]"
                  @click="gotoLogPage(s.id, (logPage[s.id] || 1) - 1)"
                >上一页</button>
                <button
                  class="btn btn-ghost btn-sm"
                  :disabled="(logPage[s.id] || 1) >= logPages(s.id) || logsLoading[s.id]"
                  @click="gotoLogPage(s.id, (logPage[s.id] || 1) + 1)"
                >下一页</button>
              </div>
            </div>
            <p class="mt-1 text-[11px] text-slate-400">
              点记录里的「影片 N 部」可看本次新增/更新的影片与源站更新时间。
            </p>
          </template>
        </div>
      </div>
    </section>
  </div>
</template>

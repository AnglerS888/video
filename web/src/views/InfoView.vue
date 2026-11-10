<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '../api'
import { auth } from '../auth'
import { confirmDialog } from '../confirm'
import { fmtBytes } from '../format'
import { toast } from '../toast'
import type { ConfigData, ConfigInfo, CoverFixStatus, FfmpegStatus, SysInfo, UpdateInfo, UpdateStatus } from '../types'

const info = ref<SysInfo | null>(null)
const loading = ref(true)
const at = ref('')
const vacuuming = ref(false)
let timer: number | undefined

function pct(used: number, total: number) {
  if (!total) return 0
  return Math.min(100, Math.max(0, Math.round((used / total) * 100)))
}

function fmtUptime(sec: number) {
  if (!sec) return '-'
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d) return `${d} 天 ${h} 小时`
  if (h) return `${h} 小时 ${m} 分`
  return `${m} 分`
}

// 目录统计在后端是缓存 + 后台重扫，页面上可能是稍早的快照；
// at 是扫描开始的 Unix 秒，按浏览器时区显示到分钟
function fmtStatAt(sec?: number): string {
  if (!sec) return '—'
  const d = new Date(sec * 1000)
  if (Number.isNaN(d.getTime())) return '—'
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

// 构建时间/提交时间是 UTC（RFC3339 带 Z），必须经 Date 按浏览器时区转换后再展示；
// 直接 replace 掉 Z 会把它当本地时间显示，国内用户会看到早 8 小时的「未来时间差」。
function fmtLocalTime(s?: string): string {
  if (!s) return '—'
  const d = new Date(s)
  if (Number.isNaN(d.getTime())) return s.replace('T', ' ').replace('Z', '')
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

const dataParts = computed(() => {
  const d = info.value?.data
  if (!d) return []
  return [
    { label: '数据库', value: d.db_bytes, color: 'bg-sky-500' },
    { label: '封面缓存', value: d.img_bytes, color: 'bg-emerald-500' },
    { label: '视频文件', value: d.video_bytes, color: 'bg-violet-500' },
    { label: 'ffmpeg', value: d.bin_bytes, color: 'bg-slate-400' },
    { label: '其他文件', value: d.other_bytes, color: 'bg-amber-500' },
  ].filter((x) => x.value > 0)
})

async function load(fresh = false) {
  if (!info.value) loading.value = true
  const r = await api.sysinfo(fresh)
  loading.value = false
  if (!r.ok || !r.data) {
    toast(r.msg || '读取失败', 'err')
    return
  }
  info.value = r.data
  // 信息接口里也带 ffmpeg 快照，非下载态时用它同步（下载态以轮询为准，别把进度覆盖掉）
  if (!ff.value || ff.value.state !== 'downloading') ff.value = r.data.ffmpeg
  at.value = new Date().toLocaleTimeString('zh-CN', { hour12: false })
}

// 空闲页占比与可回收空间
const freelistPct = computed(() => info.value?.data.freelist_pct ?? 0)
const reclaimable = computed(() => info.value?.data.reclaimable_bytes ?? 0)

// Go 版本提示：编译期版本与运行环境版本不同（容器内自更新后重新编译）时说明差异
const goVersionTip = computed(() => {
  const app = info.value?.app
  if (!app) return ''
  return app.go_compile && app.go !== app.go_compile
    ? `编译期 ${app.go_compile}，当前运行环境工具链 ${app.go}`
    : ''
})

// 整理数据库：VACUUM 需要独占写锁，服务在整理期间不响应任何请求，
// 所以先确认、再给出"整理中"状态，轮询也暂停，避免堆一串挂住的请求。
async function vacuum() {
  if (vacuuming.value) return
  const msg =
    `整理数据库会重写整个库文件，预计回收 ${fmtBytes(reclaimable.value)}。\n` +
    '整理期间服务会暂停响应（大库可能要几分钟），采集/下载中无法执行。\n\n确定继续？'
  if (!(await confirmDialog({
    title: '整理数据库',
    message: msg,
    okText: '开始整理',
  }))) return
  vacuuming.value = true
  const t0 = Date.now()
  const r = await api.vacuum()
  vacuuming.value = false
  if (!r.ok) {
    toast(r.msg || '整理失败', 'err', 4000)
    return
  }
  const sec = Math.round((Date.now() - t0) / 1000)
  toast(`${r.data?.msg || '整理完成'}，耗时 ${sec}s`, 'ok', 4000)
  await load(true)
}

// ---- 封面补充：检查全库封面缓存，同剧多源时用其它源的封面补齐拉不到的 ----
const fix = ref<CoverFixStatus | null>(null)
const fixStarting = ref(false)
let fixTimer: number | undefined

const fixVisible = computed(() => !!fix.value && fix.value.phase !== 'idle')

const fixPhaseLabel = computed(() => {
  const f = fix.value
  if (!f) return ''
  switch (f.phase) {
    case 'scanning': return '正在扫描本地封面缓存…'
    case 'processing': return f.message || '正在处理缺失封面…'
    case 'stopping': return '正在停止…'
    case 'done': return f.missing === 0 ? '封面缓存全部正常' : '处理完成'
    case 'stopped': return '已停止'
    case 'failed': return f.message || '处理失败'
    default: return ''
  }
})

const fixProgress = computed(() => {
  const f = fix.value
  if (!f) return 0
  if (f.phase === 'scanning' || (f.phase === 'stopping' && f.missing === 0)) {
    return f.covers ? Math.min(100, Math.round((f.checked / f.covers) * 100)) : 0
  }
  if (!f.missing) return f.phase === 'done' ? 100 : 0
  return Math.min(100, Math.round((f.processed / f.missing) * 100))
})

function fmtSec(s: number) {
  if (!s || s <= 0) return '0s'
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m${s % 60 ? ` ${s % 60}s` : ''}`
  return `${Math.floor(m / 60)}h ${m % 60}m`
}

function pollFix() {
  if (fixTimer) return
  fixTimer = window.setInterval(loadFix, 1200)
}

async function loadFix() {
  const r = await api.coverFixStatus()
  if (!r.ok || !r.data) return
  const wasRunning = fix.value?.running === true
  fix.value = r.data
  if (!r.data.running) {
    // 任务结束：停轮询并刷新一次封面统计，结果留在界面上
    if (fixTimer) { window.clearInterval(fixTimer); fixTimer = undefined }
    if (wasRunning) load(true)
  }
}

async function startFix() {
  if (fixStarting.value || fix.value?.running) return
  fixStarting.value = true
  const r = await api.startCoverFix()
  fixStarting.value = false
  if (!r.ok) {
    toast(r.msg || '启动失败', 'err', 4000)
    return
  }
  if (r.data) fix.value = r.data
  toast('已开始检查封面缓存', 'ok')
  pollFix()
}

async function stopFix() {
  const r = await api.stopCoverFix()
  if (!r.ok) {
    toast(r.msg || '停止失败', 'err', 3000)
    return
  }
  if (r.data) fix.value = r.data
  toast('已请求停止', 'ok')
}

// ---- ffmpeg：系统优先，缺失时按需下载（国内镜像，约 29MB）----
const ff = ref<FfmpegStatus | null>(null)
const ffStarting = ref(false)
let ffTimer: number | undefined

const ffLabel = computed(() => {
  switch (ff.value?.state) {
    case 'ready': return '已就绪'
    case 'downloading': return '下载中'
    case 'failed': return '下载失败'
    case 'missing': return '未安装'
    default: return '检测中'
  }
})

const ffPercent = computed(() => ff.value?.progress?.percent ?? 0)

function stopFfPoll() {
  if (ffTimer) {
    window.clearInterval(ffTimer)
    ffTimer = undefined
  }
}

function pollFf() {
  if (ffTimer) return
  ffTimer = window.setInterval(async () => {
    const r = await api.ffmpegStatus()
    if (!r.ok || !r.data) return
    const prev = ff.value?.state
    ff.value = r.data
    if (r.data.state !== 'downloading') {
      stopFfPoll()
      if (prev === 'downloading') {
        if (r.data.state === 'ready') {
          toast('ffmpeg 下载完成，已就绪', 'ok', 4000)
          load(true)
        } else {
          toast(r.data.error || 'ffmpeg 下载失败', 'err', 6000)
        }
      }
    }
  }, 1000)
}

async function installFf() {
  if (ffStarting.value || ff.value?.state === 'downloading') return
  ffStarting.value = true
  const r = await api.installFfmpeg()
  ffStarting.value = false
  if (!r.ok) {
    toast(r.msg || '启动下载失败', 'err', 5000)
    return
  }
  if (r.data) ff.value = r.data
  pollFf()
}

// ---- 配置（表单，保存即生效）----
// 只放「不用重启就能改」的几组：采集 / 下载 / 封面 / 合并转码 / 访问密码。
// 其余项（监听地址、数据目录、日志级别、内存、数据库）只能在启动时定死，
// 保留在 config.yml 里手改；前端不展示、也不提交，避免误以为已经生效。
const cfgInfo = ref<ConfigInfo | null>(null)
const cfgForm = ref<ConfigData | null>(null)
const cfgOrig = ref('')
const cfgLoading = ref(false)
const cfgSaving = ref(false)
// 完整快照（还原用）；cfgOrig 只存可热更几组的序列化，用于脏检查
let cfgOrigFull: ConfigData | null = null

/** 只取可热更的几组，用于脏检查与提交 */
type HotConfig = Pick<ConfigData, 'collect' | 'download' | 'cover' | 'ffmpeg' | 'auth'>

function clone<T>(v: T): T {
  return JSON.parse(JSON.stringify(v)) as T
}

function hotOf(c: ConfigData): HotConfig {
  return {
    collect: clone(c.collect),
    download: clone(c.download),
    cover: clone(c.cover),
    ffmpeg: clone(c.ffmpeg),
    auth: clone(c.auth),
  }
}

const cfgDirty = computed(() => !!cfgForm.value && JSON.stringify(hotOf(cfgForm.value)) !== cfgOrig.value)

async function loadConfig() {
  cfgLoading.value = true
  const r = await api.configInfo()
  cfgLoading.value = false
  if (!r.ok || !r.data) return toast(r.msg || '读取配置失败', 'err', 3000)
  cfgInfo.value = r.data
  if (r.data.config) {
    cfgForm.value = clone(r.data.config)
    cfgOrigFull = clone(r.data.config)
    cfgOrig.value = JSON.stringify(hotOf(r.data.config))
  } else {
    cfgForm.value = null
    cfgOrigFull = null
    cfgOrig.value = ''
  }
}

async function saveCfg() {
  if (!cfgForm.value || !cfgDirty.value || cfgSaving.value) return
  if (!(await confirmDialog({
    title: '保存配置',
    message: '保存后立即生效：正在采集的接口、正在下载的这一集不受影响，下一轮采集 / 下一集开始用新值。\n修改访问密码后当前浏览器会保持登录，其它已登录设备需重新输入新密码。继续？',
    okText: '保存并生效',
  }))) return
  cfgSaving.value = true
  const r = await api.saveConfig(hotOf(cfgForm.value))
  cfgSaving.value = false
  if (!r.ok || !r.data) return toast(r.msg || '保存失败', 'err', 5000)
  // 服务端会把越界值夹到合法区间，以返回结果为准重新回填
  if (r.data.config) {
    cfgForm.value = clone(r.data.config)
    cfgOrigFull = clone(r.data.config)
    cfgOrig.value = JSON.stringify(hotOf(r.data.config))
    // 访问密码保存后立即生效：同步全局状态（刚设密码时服务端已给当前浏览器续上 Cookie）
    auth.required = !!r.data.config.auth?.password
    auth.authed = true
  }
  toast(r.data.msg || '已保存并生效', 'ok', 4000)
}

function resetCfg() {
  if (cfgOrigFull) cfgForm.value = clone(cfgOrigFull)
  toast('已还原为磁盘上的内容', 'info', 1500)
}

/** 低配/标准预设：只覆盖采集/下载/封面/ffmpeg 四组（访问密码与其它项不动） */
function applyPreset(kind: 'lowmem' | 'default') {
  const f = cfgForm.value
  const p = cfgInfo.value?.presets?.[kind]
  if (!f || !p) return
  f.collect = clone(p.collect)
  f.download = clone(p.download)
  f.cover = clone(p.cover)
  f.ffmpeg = clone(p.ffmpeg)
  toast(kind === 'lowmem' ? '已套用低配预设，点「保存」立即生效' : '已套用标准预设，点「保存」立即生效', 'info', 2500)
}

// ---- 表单字段描述（渲染用，免得手写几十个输入框）----
type CfgFieldType = 'text' | 'number' | 'select' | 'bool' | 'password'
interface CfgField {
  path: string
  label: string
  type: CfgFieldType
  hint?: string
  unit?: string
  min?: number
  max?: number
  options?: { v: string; t: string }[]
}
interface CfgGroup {
  title: string
  desc?: string
  fields: CfgField[]
}

const cfgGroups: CfgGroup[] = [
  {
    title: '采集',
    desc: '全量采集的分页并发与定时增量，下一轮采集开始用新值。',
    fields: [
      { path: 'collect.pages', label: '每个接口的分页并发', type: 'number', unit: '页', min: 1, max: 64 },
      { path: 'collect.sources', label: '同时采集的接口数', type: 'number', unit: '个', min: 1, max: 64 },
      { path: 'collect.incr_every', label: '定时增量间隔', type: 'text', hint: '写成 30m / 1h / 6h；留空按 1h' },
      { path: 'collect.incr_pages', label: '增量每次同步页数', type: 'number', unit: '页', min: 0, max: 1000, hint: '0 = 不限页数' },
    ],
  },
  {
    title: '下载',
    desc: '分片并发与磁盘保护，下一集开始用新值。',
    fields: [
      { path: 'download.workers', label: '同时下载的集数', type: 'number', unit: '集', min: 1, max: 8, hint: '实际是串行下载，这个值一般保持 1' },
      { path: 'download.seg_concurrency', label: '单个任务的分片并发', type: 'number', unit: '片', min: 1, max: 64 },
      { path: 'download.seg_budget', label: '全局分片并发上限', type: 'number', unit: '片', min: 0, max: 512, hint: '0 = 集数 × 分片并发' },
      { path: 'download.min_free_mb', label: '磁盘剩余保护阈值', type: 'number', unit: 'MiB', min: 0, hint: '低于该值暂停下载；0 = 关闭' },
      { path: 'download.auto_new_ep', label: '自动追更下载', type: 'bool', hint: '总开关；具体盯哪部剧要在详情页逐部开启' },
    ],
  },
  {
    title: '封面',
    desc: '封面预取的并发与代理，改完立即生效。',
    fields: [
      { path: 'cover.workers', label: '封面任务并发', type: 'number', unit: '个', min: 1, max: 8, hint: '同时处理几张封面' },
      { path: 'cover.concurrency', label: '单站点并发', type: 'number', unit: '个', min: 1, max: 16, hint: '同一个图床同时连几个' },
      { path: 'cover.img_proxy', label: '图片代理', type: 'text', hint: '留空 = 只用直连；填写后取图优先走代理，如 https://wsrv.nl/?url=' },
    ],
  },
  {
    title: '合并 / 转码（ffmpeg）',
    desc: '下一次合并开始用新值。',
    fields: [
      { path: 'ffmpeg.threads', label: '编码线程数', type: 'number', min: 0, max: 64, hint: '0 = 自动' },
      { path: 'ffmpeg.reencode', label: '需要时重新编码', type: 'bool' },
      { path: 'ffmpeg.verify', label: '合并后校验时长', type: 'bool' },
    ],
  },
  {
    title: '访问控制',
    desc: '设置后打开网站要先输入密码，接口调用也要带上密码；留空 = 关闭。',
    fields: [
      {
        path: 'auth.password',
        label: '访问密码',
        type: 'password',
        hint: '最多 128 个字符；留空 = 不需要密码。忘记密码时可直接编辑数据目录 ui.yml 里的 auth.password',
      },
    ],
  },
]

function cfgGet(path: string): any {
  let o: any = cfgForm.value
  for (const k of path.split('.')) {
    if (o == null) return undefined
    o = o[k]
  }
  return o
}

function cfgSet(path: string, val: any) {
  let o: any = cfgForm.value
  if (!o) return
  const keys = path.split('.')
  const last = keys.pop() as string
  for (const k of keys) {
    if (o[k] == null) o[k] = {}
    o = o[k]
  }
  o[last] = val
}

function onCfgText(path: string, e: Event) {
  cfgSet(path, (e.target as HTMLInputElement).value)
}

/** 数字框：空值先按 0 存，避免 NaN 把整个表单写成 null */
function onCfgNum(path: string, e: Event) {
  const raw = (e.target as HTMLInputElement).value
  cfgSet(path, raw === '' ? 0 : Number(raw))
}

function onCfgBool(path: string, e: Event) {
  cfgSet(path, (e.target as HTMLInputElement).checked)
}

// ---- 版本与在线更新 ----
// 后端 /api/version 给当前版本 + 上次检查结果；检查/下载/编译走 /api/update/*。
const upInfo = ref<UpdateInfo | null>(null)
const upStatus = ref<UpdateStatus | null>(null)
const upChecking = ref(false)
const upApplying = ref(false)
let upTimer: number | undefined
let reloadTimer: number | undefined

const isDocker = computed(() => upInfo.value?.mode === 'docker')
// 更新源显示成「gitee.com/anglers/video」这种形式：去掉协议头即可。
// 检查更新后 site_url 会是实际命中并胜出的那个仓库（Gitee 或 GitHub）。
const upSiteHost = computed(() =>
  (upInfo.value?.site_url || 'https://gitee.com').replace(/^https?:\/\//, '').replace(/\/+$/, ''),
)
// restarting 也算进行中：进程马上原地 exec 重启，接口会断几秒，新进程回来后回 done
const upBusy = computed(() => ['downloading', 'extracting', 'building', 'restarting'].includes(upStatus.value?.phase || ''))

// 「立即更新」只在版本号确实不同时可用；版本号相同（或还没检查到远端版本）只能走「强制更新」，
// 避免同一版本反复下载编译。与后端 compareSemver 一致：忽略前导 v 与 -rc 之类的后缀，只比三段数字。
function verParts(s: string): number[] {
  const core = (s || '').trim().replace(/^v/i, '').split(/[-+ ]/)[0]
  return core.split('.').map((x) => parseInt(x, 10) || 0)
}
const upVersionDiff = computed(() => {
  const cur = upInfo.value?.current_version || ''
  const latest = upInfo.value?.latest_version || ''
  if (!cur || !latest) return false
  const a = verParts(cur)
  const b = verParts(latest)
  return [0, 1, 2].some((i) => (a[i] || 0) !== (b[i] || 0))
})

const upPhaseText = computed(() => {
  const s = upStatus.value
  if (!s) return ''
  switch (s.phase) {
    case 'downloading': return '正在下载最新源码…'
    case 'extracting': return '正在解压…'
    case 'building': return '正在容器内编译…'
    case 'restarting': return '正在重启到新版本…'
    case 'done': return '更新完成'
    case 'staged': return '源码已就绪'
    case 'failed': return '更新失败'
    default: return s.message || ''
  }
})

async function loadVersion() {
  const r = await api.version()
  if (!r.ok || !r.data) return
  upInfo.value = r.data.info
  upStatus.value = r.data.status
  if (['downloading', 'extracting', 'building', 'restarting'].includes(r.data.status?.phase || '')) {
    upApplying.value = true
    pollUp()
  }
  maybeAutoCheck()
}

async function checkUp(silent = false) {
  if (upChecking.value) return
  upChecking.value = true
  const r = await api.checkUpdate()
  upChecking.value = false
  if (!r.ok || !r.data) {
    if (!silent) toast(r.msg || '检查更新失败', 'err', 4000)
    return
  }
  upInfo.value = r.data
  if (r.data.error) {
    if (!silent) toast(r.data.error, 'err', 5000)
    return
  }
  if (!silent) {
    // 版本号相同但远端有新提交时，不能误导成「可以立即更新」
    if (r.data.available && r.data.latest_version && !upVersionDiff.value) {
      toast('远端有新提交，但版本号与本机相同；如需重装请用「强制更新」', 'info', 3200)
    } else {
      toast(r.data.available ? '发现可用更新' : '当前已是最新版本', r.data.available ? 'ok' : 'info', 2400)
    }
  }
}

// 进信息页时自动检查一次：超过 30 分钟没查过才查，避免每次刷新都打接口
async function maybeAutoCheck() {
  const t = upInfo.value?.checked_at
  if (!t) return checkUp(true)
  const ts = Date.parse(t.replace(' ', 'T'))
  if (Number.isNaN(ts) || Date.now() - ts > 30 * 60 * 1000) return checkUp(true)
}

async function applyUp(force = false) {
  if (upApplying.value || upBusy.value) return
  if (!isDocker.value) {
    return toast('直接运行模式不支持自动更新，请到 Gitee / GitHub 下载最新版', 'info', 3000)
  }
  if (!force && !upVersionDiff.value) {
    return toast('当前版本号与远端相同，如需按最新提交重装请用「强制更新」', 'info', 2800)
  }
  const cur = upInfo.value?.current_version ? 'v' + upInfo.value.current_version : '未知'
  const latest = upInfo.value?.latest_version ? 'v' + upInfo.value.latest_version : '未知（以下载到的源码为准）'
  const tip = force
    ? `强制更新会忽略版本号判断：本机 ${cur}，远端 ${latest}。\n` +
      '将直接下载远端最新源码、在容器内重新编译并替换程序，然后自动重启生效。\n' +
      '期间服务会中断几秒到几分钟（编译耗时取决于机器性能）。继续？'
    : '会自动下载最新源码、在容器内编译并替换程序，然后自动重启生效。\n期间服务会中断几秒到几分钟（编译耗时取决于机器性能）。继续？'
  if (!(await confirmDialog({
    title: force ? '强制更新' : '在线更新',
    message: tip,
    okText: force ? '强制更新' : '开始更新',
    danger: force,
  }))) return
  upApplying.value = true
  const r = await api.applyUpdate(force)
  if (!r.ok) {
    upApplying.value = false
    return toast(r.msg || '启动更新失败', 'err', 4000)
  }
  if (r.data) upStatus.value = r.data
  pollUp()
}

function pollUp() {
  if (upTimer) return
  upTimer = window.setInterval(async () => {
    const r = await api.updateStatus()
    // restarting 阶段接口会断（进程原地 exec 重启，旧进程已停、新进程还没监听），失败就等下一轮
    if (!r.ok || !r.data) return
    upStatus.value = r.data
    if (['staged', 'failed', 'done'].includes(r.data.phase)) {
      if (upTimer) { window.clearInterval(upTimer); upTimer = undefined }
      upApplying.value = false
      if (r.data.phase === 'failed') toast(r.data.error || '更新失败', 'err', 6000)
      else if (r.data.phase === 'done') {
        toast(r.data.message || '更新完成，已自动重启', 'ok', 6000)
        // 新进程已就绪：等提示露个脸就整页刷新，换成本次更新带上的前端产物。
        // 旧页面里带 hash 的 chunk 已被替换，不刷新再点导航会 404。
        reloadTimer = window.setTimeout(() => location.reload(), 1800)
      } else toast(r.data.message || '源码已就绪，请到宿主机重建镜像', 'ok', 5000)
      loadVersion()
    }
  }, 1200)
}

onMounted(() => {
  load()
  loadVersion()
  loadConfig()
  loadFix().then(() => { if (fix.value?.running) pollFix() })
  // 续上「已在下载」的状态（比如刷新过页面）
  api.ffmpegStatus().then((r) => {
    if (r.ok && r.data) {
      ff.value = r.data
      if (r.data.state === 'downloading') pollFf()
    }
  })
  timer = window.setInterval(() => {
    if (!vacuuming.value) load()
  }, 15000)
})
onBeforeUnmount(() => {
  if (timer) window.clearInterval(timer)
  if (fixTimer) window.clearInterval(fixTimer)
  if (upTimer) window.clearInterval(upTimer)
  if (reloadTimer) window.clearTimeout(reloadTimer)
  stopFfPoll()
})
</script>

<template>
  <div class="space-y-4">
    <section class="flex flex-wrap items-center gap-2">
      <h2 class="text-lg font-semibold">网站信息</h2>
      <span v-if="at" class="text-xs muted">更新于 {{ at }}</span>
      <button class="btn btn-ghost btn-sm ml-auto" :disabled="loading" @click="load(true)">立即刷新</button>
    </section>

    <div v-if="loading && !info" class="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
      <div v-for="i in 6" :key="i" class="card card-pad h-28 animate-pulse bg-slate-100 dark:bg-white/5"></div>
    </div>

    <template v-else-if="info">
      <!-- 概览 -->
      <section class="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <div v-for="c in [
          { label: '影片记录', value: info.counts.videos },
          { label: '片名（合并后）', value: info.counts.names },
          { label: '播放地址', value: info.counts.plays },
          { label: '采集来源', value: info.counts.sources },
          { label: '收藏', value: info.counts.favorites },
          { label: '下载任务', value: info.counts.downloads },
        ]" :key="c.label" class="card card-pad">
          <div class="text-xs muted">{{ c.label }}</div>
          <div class="mt-1 text-xl font-semibold tabular-nums">{{ c.value.toLocaleString() }}</div>
        </div>
      </section>

      <div class="grid gap-3 xl:grid-cols-2">
        <!-- 存储 -->
        <section class="card card-pad">
          <div class="mb-3 flex flex-wrap items-center gap-2">
            <span class="title">数据目录</span>
            <span class="chip">{{ fmtBytes(info.data.total_bytes) }}</span>
            <span v-if="info.data.partial" class="chip">统计未完成</span>
            <span class="chip">统计于 {{ fmtStatAt(info.data.at) }}</span>
          </div>
          <code class="block break-all rounded-lg bg-slate-50 px-2.5 py-2 text-xs dark:bg-white/5">{{ info.data.dir }}</code>

          <div class="mt-3 space-y-2">
            <div v-for="p in dataParts" :key="p.label" class="text-xs">
              <div class="flex justify-between">
                <span class="muted">{{ p.label }}</span>
                <span class="tabular-nums">{{ fmtBytes(p.value) }}</span>
              </div>
              <div class="mt-1 h-1.5 overflow-hidden rounded-full bg-slate-200 dark:bg-white/10">
                <i class="block h-full rounded-full" :class="p.color" :style="{ width: pct(p.value, info.data.total_bytes) + '%' }"></i>
              </div>
            </div>
            <div class="flex justify-between border-t border-slate-100 pt-2 text-xs dark:border-white/5">
              <span class="muted">封面缓存张数</span>
              <span class="tabular-nums">{{ info.data.img_count.toLocaleString() }}</span>
            </div>
          </div>

          <!-- 数据库空闲页：采集与删接口会留下空洞，需整理才能还给文件系统 -->
          <div class="mt-3 border-t border-slate-100 pt-3 dark:border-white/5">
            <div class="flex flex-wrap items-center gap-2 text-xs">
              <span class="muted">数据库空闲页</span>
              <span
                class="tabular-nums"
                :class="freelistPct > 0 ? 'font-medium text-amber-600 dark:text-amber-400' : ''"
              >{{ freelistPct }}%</span>
              <span v-if="reclaimable > 0" class="chip">可回收 {{ fmtBytes(reclaimable) }}</span>
              <span v-else class="chip">无需整理</span>
              <span v-if="info.data.vacuum_suggest" class="chip chip-on">已达阈值，建议整理</span>
            </div>
            <p class="mt-1 text-[11px] muted">
              采集和删来源会在库文件里留下空闲页，SQLite 不会自动归还磁盘。
              <template v-if="info.data.vacuum_pct > 0">
                空闲时占比超过 {{ info.data.vacuum_pct }}% 会自动整理；等不到自动整理可手动点下面按钮。
              </template>
            </p>
            <button
              class="btn btn-sm mt-2"
              :class="info.data.vacuum_suggest ? 'btn-primary' : 'btn-ghost'"
              :disabled="vacuuming || reclaimable <= 0"
              @click="vacuum"
            >
              {{ vacuuming ? '整理中…（服务暂停响应）' : '整理数据库' }}
            </button>
          </div>

          <div class="mt-3 border-t border-slate-100 pt-3 dark:border-white/5">
            <div class="flex justify-between text-xs">
              <span class="muted">所在磁盘</span>
              <span class="tabular-nums" :class="info.disk.low ? 'font-medium text-rose-500' : ''">
                剩余 {{ fmtBytes(info.disk.free) }} / 共 {{ fmtBytes(info.disk.total) }}
              </span>
            </div>
            <div class="mt-1 h-2 overflow-hidden rounded-full bg-slate-200 dark:bg-white/10">
              <i
                class="block h-full rounded-full"
                :class="info.disk.low ? 'bg-rose-500' : 'bg-sky-500'"
                :style="{ width: pct(info.disk.used, info.disk.total) + '%' }"
              ></i>
            </div>
            <div class="mt-1 flex items-center justify-between text-[11px] muted">
              <span v-if="info.disk.min_free_bytes > 0">保护阈值 {{ fmtBytes(info.disk.min_free_bytes) }}</span>
              <span v-else></span>
              <span>已用 {{ pct(info.disk.used, info.disk.total) }}%</span>
            </div>
            <p v-if="info.disk.low" class="mt-2 rounded-lg bg-rose-500/10 px-2.5 py-2 text-[11px] text-rose-500">
              剩余空间低于保护阈值（{{ fmtBytes(info.disk.min_free_bytes) }}）：新下载会被拒绝，
              排队中的任务已自动暂停。清理空间后到「下载」页点「继续」恢复。
            </p>
          </div>
        </section>

        <!-- 内存 -->
        <section class="card card-pad">
          <div class="mb-3 flex items-center gap-2">
            <span class="title">内存</span>
            <span class="chip">已用 {{ pct(info.mem.used, info.mem.total) }}%</span>
          </div>

          <div class="text-xs">
            <div class="flex justify-between">
              <span class="muted">系统</span>
              <span class="tabular-nums">{{ fmtBytes(info.mem.used) }} / {{ fmtBytes(info.mem.total) }}</span>
            </div>
            <div class="mt-1 h-2 overflow-hidden rounded-full bg-slate-200 dark:bg-white/10">
              <i class="block h-full rounded-full bg-emerald-500" :style="{ width: pct(info.mem.used, info.mem.total) + '%' }"></i>
            </div>
            <div class="mt-1 flex justify-between text-[11px] muted">
              <span>可用 {{ fmtBytes(info.mem.available) }}</span>
              <span>Go 堆占用 {{ pct(info.mem.go_inuse, info.mem.total) }}%</span>
            </div>
          </div>

          <div class="mt-4 grid grid-cols-2 gap-2 text-xs sm:grid-cols-4">
            <div class="rounded-lg bg-slate-50 px-2.5 py-2 dark:bg-white/5">
              <div class="muted">Go 堆在用</div>
              <div class="mt-0.5 font-medium tabular-nums">{{ fmtBytes(info.mem.go_inuse) }}</div>
            </div>
            <div class="rounded-lg bg-slate-50 px-2.5 py-2 dark:bg-white/5">
              <div class="muted">Go 向系统申请</div>
              <div class="mt-0.5 font-medium tabular-nums">{{ fmtBytes(info.mem.go_sys) }}</div>
            </div>
            <div class="rounded-lg bg-slate-50 px-2.5 py-2 dark:bg-white/5">
              <div class="muted">协程数</div>
              <div class="mt-0.5 font-medium tabular-nums">{{ info.mem.goroutines }}</div>
            </div>
            <div class="rounded-lg bg-slate-50 px-2.5 py-2 dark:bg-white/5">
              <div class="muted">GC 次数</div>
              <div class="mt-0.5 font-medium tabular-nums">{{ info.mem.gc_count }}</div>
            </div>
          </div>
        </section>

        <!-- 运行环境 -->
        <section class="card card-pad">
          <div class="title mb-3">运行环境</div>
          <dl class="grid grid-cols-1 gap-x-4 gap-y-2 text-xs sm:grid-cols-2">
            <div class="flex justify-between gap-3"><dt class="muted">程序版本</dt><dd class="font-medium">{{ info.app.version }}</dd></div>
            <div class="flex justify-between gap-3">
              <dt class="muted">Go 版本</dt>
              <dd class="min-w-0 truncate font-medium" :title="goVersionTip">{{ info.app.go }}</dd>
            </div>
            <div class="flex justify-between gap-3"><dt class="muted">系统 / 架构</dt><dd>{{ info.app.os }} / {{ info.app.arch }}</dd></div>
            <div class="flex justify-between gap-3"><dt class="muted">进程 PID</dt><dd class="tabular-nums">{{ info.app.pid }}</dd></div>
            <div class="flex justify-between gap-3"><dt class="muted">启动时间</dt><dd>{{ info.app.started_at }}</dd></div>
            <div class="flex justify-between gap-3 sm:col-span-2">
              <dt class="shrink-0 muted">配置文件</dt>
              <dd class="min-w-0 truncate text-right" :title="info.app.config_file">{{ info.app.config_file || '—' }}</dd>
            </div>
            <div class="flex justify-between gap-3"><dt class="muted">已运行</dt><dd>{{ fmtUptime(info.app.uptime_sec) }}</dd></div>
          </dl>

          <div class="mt-4 rounded-xl bg-slate-50 p-3 dark:bg-white/5">
            <div class="flex flex-wrap items-center gap-2 text-xs">
              <span class="title">ffmpeg</span>
              <span
                class="chip"
                :class="ff?.state === 'ready' ? 'chip-on' : ff?.state === 'failed' ? 'chip-err' : ''"
              >{{ ffLabel }}</span>
              <span v-if="ff?.source" class="chip">来源：{{ ff.source === 'system' ? '系统已装' : ff.source === 'download' ? '页面下载' : ff.source === 'cache' ? '下载缓存' : ff.source === 'env' ? '环境变量' : ff.source }}</span>
              <!-- 只有未安装/失败时才给下载按钮：已就绪没必要重复下 -->
              <button
                v-if="ff && ff.state !== 'ready' && ff.supported"
                class="btn btn-primary btn-sm ml-auto"
                :disabled="ffStarting || ff.state === 'downloading'"
                @click="installFf"
              >{{ ff.state === 'downloading' ? '下载中…' : ff.state === 'failed' ? '重新下载' : '下载 ffmpeg' }}</button>
            </div>

            <p class="mt-1.5 break-all text-xs muted">{{ ff?.version || ff?.path || '未检测到' }}</p>
            <p v-if="ff?.path && ff?.version" class="mt-0.5 break-all text-[11px] text-slate-400">{{ ff.path }}</p>

            <!-- 下载进度 -->
            <div v-if="ff?.state === 'downloading'" class="mt-2">
              <div class="h-2 overflow-hidden rounded-full bg-slate-200 dark:bg-white/10">
                <i class="block h-full rounded-full bg-sky-500 transition-[width] duration-300" :style="{ width: Math.max(2, ffPercent) + '%' }"></i>
              </div>
              <div class="mt-1 flex justify-between text-[11px] muted">
                <span>正在从国内镜像下载…</span>
                <span class="tabular-nums">
                  {{ fmtBytes(ff.progress?.downloaded || 0) }}<span v-if="ff.progress?.total"> / {{ fmtBytes(ff.progress.total) }} · {{ ffPercent }}%</span>
                </span>
              </div>
            </div>

            <!-- 失败信息与重试提示 -->
            <p v-if="ff?.state === 'failed' && ff.error" class="mt-2 break-all rounded-lg bg-rose-500/10 px-2.5 py-2 text-[11px] text-rose-500">
              下载失败：{{ ff.error }}
            </p>
            <p v-if="ff && !ff.supported" class="mt-2 text-[11px] text-amber-600 dark:text-amber-400">
              当前平台（{{ ff.platform }}）暂不支持自动下载，请自行安装 ffmpeg，或设置环境变量 <code>VDL_FFMPEG</code> 指向可执行文件。
            </p>
            <p v-if="ff && ff.state === 'missing' && ff.supported" class="mt-1.5 text-[11px] muted">
              系统未检测到 ffmpeg。点击下载后会自动解压到数据目录 <code>bin/</code>，约 29MB，只下一次，之后直接复用（合并、封面缩放都需要它）。
            </p>
          </div>
        </section>

        <!-- 版本与在线更新 -->
        <section class="card card-pad space-y-3">
          <div class="flex flex-wrap items-center gap-2">
            <span class="title">版本与更新</span>
            <span class="chip">{{ upInfo?.current_version ? 'v' + upInfo.current_version : '—' }}</span>
            <span v-if="upInfo?.current_commit" class="chip">提交 {{ upInfo.current_commit.slice(0, 7) }}</span>
            <span class="chip">{{ isDocker ? 'Docker 容器' : '直接运行' }}</span>
            <span v-if="upInfo?.available" class="chip chip-on">{{ upVersionDiff ? '有新版本' : '有新提交' }}</span>
            <!-- 注意要写成 checkUp()：直接写 checkUp 会把 MouseEvent 传进 silent 参数，导致提示被吞 -->
            <button class="btn btn-ghost btn-sm ml-auto" :disabled="upChecking || upBusy" @click="checkUp()">
              {{ upChecking ? '检查中…' : '检查更新' }}
            </button>
          </div>

          <dl class="grid grid-cols-1 gap-x-4 gap-y-1.5 text-xs sm:grid-cols-2">
            <div class="flex justify-between gap-3">
              <dt class="muted">构建时间</dt>
              <dd class="tabular-nums">{{ fmtLocalTime(upInfo?.build_time) }}</dd>
            </div>
            <div class="flex justify-between gap-3">
              <dt class="muted">更新源</dt>
              <dd class="truncate" :title="upInfo?.site_url || ''">{{ upSiteHost }}/{{ upInfo?.repo || '—' }}@{{ upInfo?.branch || '—' }}</dd>
            </div>
            <div class="flex justify-between gap-3">
              <dt class="muted">最新提交</dt>
              <dd class="tabular-nums">{{ upInfo?.latest_commit ? upInfo.latest_commit.slice(0, 7) : '—' }}</dd>
            </div>
            <div class="flex justify-between gap-3">
              <dt class="muted">提交时间</dt>
              <dd class="tabular-nums">{{ fmtLocalTime(upInfo?.latest_commit_at) }}</dd>
            </div>
          </dl>
          <p v-if="upInfo?.latest_commit_msg" class="truncate text-[11px] muted" :title="upInfo.latest_commit_msg">
            最新说明：{{ upInfo.latest_commit_msg }}
          </p>

          <ul v-if="upInfo?.reasons?.length" class="space-y-0.5 rounded-lg bg-amber-500/10 px-2.5 py-2 text-[11px] text-amber-700 dark:text-amber-300">
            <li v-for="r in upInfo.reasons" :key="r">· {{ r }}</li>
          </ul>
          <p v-if="upInfo?.error" class="break-all rounded-lg bg-rose-500/10 px-2.5 py-2 text-[11px] text-rose-500">{{ upInfo.error }}</p>
          <p v-if="upInfo?.checked_at" class="text-[11px] muted">
            上次检查：{{ upInfo.checked_at }}<span v-if="upInfo.via"> · 通道 {{ upInfo.via === 'github' ? 'GitHub' : upInfo.via === 'gitee' ? 'Gitee' : upInfo.via }}</span>
          </p>

          <!-- 进行中 -->
          <div v-if="upBusy" class="rounded-lg bg-slate-50 px-2.5 py-2 dark:bg-white/5">
            <div class="flex items-center justify-between text-xs">
              <span class="font-medium">{{ upPhaseText }}</span>
              <span class="tabular-nums muted">{{ upStatus?.percent || 0 }}%</span>
            </div>
            <div class="mt-1.5 h-1.5 overflow-hidden rounded-full bg-slate-200 dark:bg-white/10">
              <i class="block h-full rounded-full bg-sky-500 transition-all" :style="{ width: Math.max(2, upStatus?.percent || 0) + '%' }"></i>
            </div>
            <p v-if="upStatus?.total" class="mt-1 text-[11px] muted">{{ fmtBytes(upStatus.bytes) }} / {{ fmtBytes(upStatus.total) }}</p>
          </div>

          <!-- 结果：更新完成 / 源码已就位（旧镜像） / 失败 -->
          <div v-else-if="upStatus && ['done', 'staged', 'failed'].includes(upStatus.phase)" class="rounded-lg bg-slate-50 px-2.5 py-2 dark:bg-white/5">
            <p class="text-xs" :class="upStatus.phase === 'failed' ? 'text-rose-500' : 'font-medium'">{{ upStatus.message }}</p>
            <ul v-if="upStatus.instructions?.length" class="mt-1 space-y-0.5 break-all text-[11px] muted">
              <li v-for="(t, i) in upStatus.instructions" :key="i">{{ t }}</li>
            </ul>
            <p v-if="upStatus.source_dir" class="mt-1 break-all text-[11px] text-slate-400">{{ upStatus.source_dir }}</p>
          </div>

          <p v-if="isDocker && upInfo?.available && !upVersionDiff" class="rounded-lg bg-slate-50 px-2.5 py-2 text-[11px] muted dark:bg-white/5">
            远端有更新，但版本号与本机相同{{ upInfo?.latest_version ? '（' + upInfo.latest_version + '）' : '' }}；
            「立即更新」只在版本号不同时可用，想按最新提交重装请点「强制更新」。
          </p>

          <div class="flex flex-wrap items-center gap-2">
            <template v-if="isDocker">
              <button
                class="btn btn-primary btn-sm"
                :disabled="!upVersionDiff || upApplying || upBusy"
                :title="upVersionDiff ? '' : '版本号与远端相同（或还没检查到远端版本），请用「强制更新」'"
                @click="applyUp()"
              >
                {{ upApplying || upBusy ? '更新中…' : '立即更新' }}
              </button>
              <button
                class="btn btn-ghost btn-sm"
                :disabled="upApplying || upBusy"
                title="忽略版本号判断，直接按远端最新源码重新编译并替换（同版本重装用）"
                @click="applyUp(true)"
              >
                强制更新
              </button>
            </template>
            <a
              class="btn btn-primary btn-sm"
              v-else
              :href="upInfo?.site_url || 'https://gitee.com/anglers/video'"
              target="_blank" rel="noopener"
            >打开仓库手动更新</a>
            <a
              class="btn btn-ghost btn-sm"
              :href="upInfo?.site_url || 'https://gitee.com/anglers/video'"
              target="_blank" rel="noopener"
            >项目仓库</a>
          </div>
          <p v-if="isDocker" class="text-[11px] muted">
            自动更新只在 Docker 容器内提供：「立即更新」仅在版本号与远端不同时可用；
            「强制更新」不看版本号，随时可以按远端最新源码重装。两者都会自动下载源码、在容器内编译并替换程序，
            随后进程原地重启生效（PID 不变，容器不会退出；期间服务短暂中断），数据目录不受影响，无需登录宿主机。
            若镜像不含 Go 工具链（旧版精简镜像），则只下载源码并给出宿主机重建镜像的步骤。
          </p>
          <p v-else class="text-[11px] muted">
            当前为<strong>直接运行</strong>模式，不支持自动更新：请到 Gitee / GitHub 下载最新版（或最新源码自行编译），
            替换程序文件后重启即可；数据目录与配置不受影响。
          </p>
        </section>

        <!-- 运行配置（表单，保存即生效） -->
        <section class="card card-pad space-y-3">
          <div class="flex flex-wrap items-center gap-2">
            <span class="title">运行配置</span>
            <span class="chip">保存即生效</span>
            <span v-if="cfgDirty" class="chip chip-on">有未保存修改</span>
            <div class="ml-auto flex flex-wrap items-center gap-1.5">
              <button class="btn btn-ghost btn-sm" :disabled="!cfgForm" @click="applyPreset('default')">标准预设</button>
              <button class="btn btn-ghost btn-sm" :disabled="!cfgForm" @click="applyPreset('lowmem')">低配预设</button>
              <button class="btn btn-ghost btn-sm" :disabled="cfgLoading || cfgSaving" @click="loadConfig">
                {{ cfgLoading ? '读取中…' : '重新读取' }}
              </button>
              <button class="btn btn-ghost btn-sm" :disabled="!cfgDirty || cfgSaving" @click="resetCfg">还原</button>
              <button class="btn btn-primary btn-sm" :disabled="!cfgDirty || cfgSaving" @click="saveCfg">
                {{ cfgSaving ? '保存中…' : '保存' }}
              </button>
            </div>
          </div>

          <template v-if="cfgForm">
            <!-- 固定高度 + 内部滚动：几组控件全铺开太长，把页面撑得没法看 -->
            <div class="max-h-[46vh] min-w-0 overflow-y-auto overscroll-contain pr-1 sm:max-h-[420px]">
              <div
                v-for="g in cfgGroups"
                :key="g.title"
                class="border-t border-slate-100 pt-3 first:border-t-0 first:pt-0 dark:border-white/5"
              >
                <div class="mb-2 flex flex-wrap items-baseline gap-2">
                  <span class="text-xs font-semibold text-slate-700 dark:text-slate-200">{{ g.title }}</span>
                  <span v-if="g.desc" class="text-[11px] muted">{{ g.desc }}</span>
                </div>
                <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <div v-for="f in g.fields" :key="f.path">
                    <label class="mb-1 block text-[11px] muted" :for="'cfg-' + f.path">{{ f.label }}</label>

                    <select
                      v-if="f.type === 'select'"
                      :id="'cfg-' + f.path"
                      class="select"
                      :value="cfgGet(f.path)"
                      @change="onCfgText(f.path, $event)"
                    >
                      <option v-for="o in f.options" :key="o.v" :value="o.v">{{ o.t }}</option>
                    </select>

                    <label
                      v-else-if="f.type === 'bool'"
                      class="flex min-h-9 cursor-pointer items-center gap-2 text-xs"
                      :for="'cfg-' + f.path"
                    >
                      <input
                        :id="'cfg-' + f.path"
                        type="checkbox"
                        class="h-5 w-5 rounded accent-amber-500"
                        :checked="cfgGet(f.path)"
                        @change="onCfgBool(f.path, $event)"
                      />
                      <span>{{ cfgGet(f.path) ? '开启' : '关闭' }}</span>
                    </label>

                    <div v-else class="flex items-center gap-1.5">
                      <input
                        :id="'cfg-' + f.path"
                        :type="f.type === 'number' ? 'number' : f.type === 'password' ? 'password' : 'text'"
                        class="input min-w-0 flex-1"
                        :min="f.min"
                        :max="f.max"
                        autocomplete="new-password"
                        :value="cfgGet(f.path)"
                        @input="f.type === 'number' ? onCfgNum(f.path, $event) : onCfgText(f.path, $event)"
                      />
                      <span v-if="f.unit" class="shrink-0 text-[11px] muted">{{ f.unit }}</span>
                    </div>

                    <p v-if="f.hint" class="mt-1 text-[11px] text-slate-400">{{ f.hint }}</p>
                  </div>
                </div>
              </div>
            </div>
          </template>

          <p v-else-if="cfgInfo" class="rounded-lg bg-rose-500/10 px-2.5 py-2 text-[11px] text-rose-500">
            配置文件无法解析，无法用表单编辑：{{ cfgInfo.path }}
          </p>

          <p class="break-all text-[11px] muted">
            这里只有<strong>不用重启</strong>的几组（采集 / 下载 / 封面 / ffmpeg / 访问密码）：
            点保存后立即生效，正在跑的那一轮采集、正在下的那一集不受影响，下一轮 / 下一集开始用新值。
            设了访问密码后，打开网站要先登录，接口调用也要带 Cookie / <code>X-Access-Password</code> /
            <code>?password=</code>；当前浏览器在保存时会自动保持登录，其它已登录设备需重新输入新密码。
            这些值单独存在数据目录的
            <code class="break-all">{{ cfgInfo?.ui_path || 'ui.yml' }}</code>（旧文件备份为 <code>.bak</code>），
            不碰 <code>config.yml</code>。监听地址、数据目录、日志级别、内存限制、数据库等
            <strong>需要重启</strong>的项留在 <code class="break-all">{{ cfgInfo?.path || 'config.yml' }}</code>，页面不涉及。
          </p>
        </section>

        <!-- 封面 / 备份 -->
        <section class="card card-pad space-y-3">
          <div>
            <div class="title mb-2">封面缓存</div>
            <div class="grid grid-cols-2 gap-2 text-xs">
              <div class="rounded-lg bg-slate-50 px-2.5 py-2 dark:bg-white/5">
                <div class="muted">本次成功</div>
                <div class="mt-0.5 text-base font-semibold tabular-nums text-emerald-600 dark:text-emerald-400">{{ info.cover.ok }}</div>
              </div>
              <div class="rounded-lg bg-slate-50 px-2.5 py-2 dark:bg-white/5">
                <div class="muted">本次失败</div>
                <div class="mt-0.5 text-base font-semibold tabular-nums" :class="info.cover.failed ? 'text-rose-500' : ''">{{ info.cover.failed }}</div>
              </div>
            </div>
            <p v-if="info.cover.last_error" class="mt-2 break-all rounded-lg bg-rose-500/10 px-2.5 py-2 text-[11px] text-rose-500">
              最近失败：{{ info.cover.last_error }}
            </p>
            <p class="mt-1 text-[11px] muted">页面优先读本地封面，拉不到时显示内置占位图。</p>
          </div>

          <!-- 封面补充：全库检查，同剧多源互相兜底 -->
          <div class="border-t border-slate-100 pt-3 dark:border-white/5">
            <div class="flex flex-wrap items-center gap-2">
              <span class="title">封面补充</span>
              <span v-if="fix?.running" class="chip chip-on">进行中</span>
              <button
                class="btn btn-ghost btn-sm ml-auto"
                :disabled="fixStarting || !!fix?.running"
                @click="startFix"
              >
                {{ fixStarting ? '启动中…' : fix?.running ? '补充中…' : '封面补充' }}
              </button>
            </div>
            <p class="mt-1 text-[11px] muted">
              检查全库封面缓存；同一部剧在多个来源都有记录时，某个源的封面拉不到就自动用其它源的封面补上。
              配置了图片代理时取图优先走代理（失败再直连），未配置时直连都失败再走内置代理（每次间隔 2~3 秒）。
            </p>

            <div v-if="fixVisible" class="mt-2 rounded-lg bg-slate-50 px-2.5 py-2 dark:bg-white/5">
              <div class="flex items-center justify-between gap-2 text-xs">
                <span :class="fix?.phase === 'failed' ? 'text-rose-500' : 'font-medium'">{{ fixPhaseLabel }}</span>
                <span class="tabular-nums muted">{{ fixProgress }}%</span>
              </div>
              <div class="mt-1.5 h-1.5 overflow-hidden rounded-full bg-slate-200 dark:bg-white/10">
                <i
                  class="block h-full rounded-full transition-all"
                  :class="fix?.phase === 'failed' ? 'bg-rose-500' : fix?.running ? 'bg-sky-500' : 'bg-emerald-500'"
                  :style="{ width: fixProgress + '%' }"
                ></i>
              </div>

              <div class="mt-1.5 flex justify-between gap-2 text-[11px] muted">
                <span v-if="fix?.phase === 'scanning' || (fix && fix.missing === 0 && fix.processed === 0)">
                  已检查 {{ (fix?.checked || 0).toLocaleString() }} / {{ (fix?.covers || 0).toLocaleString() }} 个封面
                </span>
                <span v-else>
                  已处理 {{ (fix?.processed || 0).toLocaleString() }} / {{ (fix?.missing || 0).toLocaleString() }} 张缺失封面
                </span>
                <span class="tabular-nums">{{ fmtSec(fix?.elapsed_sec || 0) }}</span>
              </div>

              <div class="mt-2 grid grid-cols-2 gap-1.5 text-[11px] sm:grid-cols-3">
                <div class="rounded-md bg-white px-2 py-1 dark:bg-white/5">
                  <div class="muted">影片 / 封面</div>
                  <div class="mt-0.5 tabular-nums">{{ (fix?.videos || 0).toLocaleString() }} / {{ (fix?.covers || 0).toLocaleString() }}</div>
                </div>
                <div class="rounded-md bg-white px-2 py-1 dark:bg-white/5">
                  <div class="muted">本地已有</div>
                  <div class="mt-0.5 tabular-nums text-emerald-600 dark:text-emerald-400">{{ (fix?.healthy || 0).toLocaleString() }}</div>
                </div>
                <div class="rounded-md bg-white px-2 py-1 dark:bg-white/5">
                  <div class="muted">重新下载</div>
                  <div class="mt-0.5 tabular-nums">{{ (fix?.fetched || 0).toLocaleString() }}</div>
                </div>
                <div class="rounded-md bg-white px-2 py-1 dark:bg-white/5">
                  <div class="muted">其它源补</div>
                  <div class="mt-0.5 tabular-nums text-sky-600 dark:text-sky-400">{{ (fix?.filled || 0).toLocaleString() }}</div>
                </div>
                <div class="rounded-md bg-white px-2 py-1 dark:bg-white/5">
                  <div class="muted">代理补</div>
                  <div class="mt-0.5 tabular-nums text-violet-600 dark:text-violet-400">{{ (fix?.proxied || 0).toLocaleString() }}</div>
                </div>
                <div class="rounded-md bg-white px-2 py-1 dark:bg-white/5">
                  <div class="muted">仍失败</div>
                  <div class="mt-0.5 tabular-nums" :class="fix?.failed ? 'text-rose-500' : ''">{{ (fix?.failed || 0).toLocaleString() }}</div>
                </div>
              </div>

              <p v-if="fix?.last_error" class="mt-2 break-all rounded-md bg-rose-500/10 px-2 py-1.5 text-[11px] text-rose-500">
                最近失败：{{ fix.last_error }}
              </p>
              <p v-if="fix?.finished_at && !fix?.running" class="mt-1 text-[11px] muted">
                结束于 {{ fix.finished_at }}，耗时 {{ fmtSec(fix.elapsed_sec) }}
              </p>
              <button v-if="fix?.running" class="btn btn-ghost btn-sm mt-2" :disabled="fix.phase === 'stopping'" @click="stopFix">
                {{ fix.phase === 'stopping' ? '正在停止…' : '停止' }}
              </button>
            </div>
          </div>

        </section>
      </div>
    </template>
  </div>
</template>

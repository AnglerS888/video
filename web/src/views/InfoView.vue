<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '../api'
import { toast } from '../toast'
import type { CoverFixStatus, FfmpegStatus, SysInfo } from '../types'

const info = ref<SysInfo | null>(null)
const loading = ref(true)
const at = ref('')
const vacuuming = ref(false)
let timer: number | undefined

function fmtBytes(n?: number): string {
  if (!n || n <= 0) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${u[i]}`
}

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

const dataParts = computed(() => {
  const d = info.value?.data
  if (!d) return []
  return [
    { label: '数据库', value: d.db_bytes, color: 'bg-sky-500' },
    { label: '封面缓存', value: d.img_bytes, color: 'bg-emerald-500' },
    { label: '视频文件', value: d.video_bytes, color: 'bg-violet-500' },
    { label: 'ffmpeg', value: d.bin_bytes, color: 'bg-slate-400' },
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

// 整理数据库：VACUUM 需要独占写锁，服务在整理期间不响应任何请求，
// 所以先确认、再给出"整理中"状态，轮询也暂停，避免堆一串挂住的请求。
async function vacuum() {
  if (vacuuming.value) return
  const msg =
    `整理数据库会重写整个库文件，预计回收 ${fmtBytes(reclaimable.value)}。\n` +
    '整理期间服务会暂停响应（大库可能要几分钟），采集/下载中无法执行。\n\n确定继续？'
  if (!confirm(msg)) return
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

onMounted(() => {
  load()
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
          { label: '采集接口', value: info.counts.sources },
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
          <div class="mb-3 flex items-center gap-2">
            <span class="title">数据目录</span>
            <span class="chip">{{ fmtBytes(info.data.total_bytes) }}</span>
            <span v-if="info.data.partial" class="chip">统计未完成</span>
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
              采集和删接口会在库文件里留下空闲页，SQLite 不会自动归还磁盘。
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
            <div class="flex justify-between gap-3"><dt class="muted">Go 版本</dt><dd>{{ info.app.go }}</dd></div>
            <div class="flex justify-between gap-3"><dt class="muted">系统 / 架构</dt><dd>{{ info.app.os }} / {{ info.app.arch }}</dd></div>
            <div class="flex justify-between gap-3"><dt class="muted">进程 PID</dt><dd class="tabular-nums">{{ info.app.pid }}</dd></div>
            <div class="flex justify-between gap-3"><dt class="muted">启动时间</dt><dd>{{ info.app.started_at }}</dd></div>
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
            <p class="mt-1 text-[11px] muted">页面只显示已成功下载并压缩到本地的封面，拉不到就不显示。</p>
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
              检查全库封面缓存；同一部剧在多个接口源都有记录时，某个源的封面拉不到就自动用其它源的封面补上。
              直连都失败时再走图片代理（wsrv.nl，每次间隔 2~3 秒）。
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

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import CoverImg from '../components/CoverImg.vue'
import { api } from '../api'
import { copyText } from '../clipboard'
import { toast } from '../toast'
import type { DownloadTask, WatchHistoryItem } from '../types'

/**
 * 本地：已经下载完成的内容。
 * 上半部分是「继续观看」（有观看进度、还没看完的），下半部分按剧分组列出全部已下载集。
 */
const router = useRouter()
const tasks = ref<DownloadTask[]>([])
const history = ref<WatchHistoryItem[]>([])
const q = ref('')
const loading = ref(true)

const done = computed(() => tasks.value.filter((t) => t.status === 'done'))

const histMap = computed(() => {
  const m = new Map<number, WatchHistoryItem>()
  for (const h of history.value) m.set(h.download_id, h)
  return m
})

function fmt(sec: number): string {
  if (!sec || !isFinite(sec)) return '00:00'
  const s = Math.max(0, Math.floor(sec))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const ss = s % 60
  const mm = String(m).padStart(2, '0')
  return h > 0 ? `${h}:${mm}:${String(ss).padStart(2, '0')}` : `${mm}:${String(ss).padStart(2, '0')}`
}

// 继续观看：有进度、还没看完
const continuing = computed(() =>
  history.value
    .filter((h) => !h.finished && h.position > 5)
    .slice(0, 12)
    .map((h) => ({
      ...h,
      pct: h.duration > 0 ? Math.min(99, Math.round((h.position / h.duration) * 100)) : 0,
    })),
)

interface Group {
  key: string
  name: string
  pic: string
  vodId: number
  episodes: DownloadTask[]
  sizeMB: number
}

const groups = computed<Group[]>(() => {
  const kw = q.value.trim().toLowerCase()
  const m = new Map<string, Group>()
  for (const t of done.value) {
    if (kw && !(`${t.vod_name} ${t.source_name}`.toLowerCase().includes(kw))) continue
    const key = t.vod_name
    let g = m.get(key)
    if (!g) {
      g = { key, name: t.vod_name, pic: t.vod_pic || '', vodId: t.vod_id, episodes: [], sizeMB: 0 }
      m.set(key, g)
    }
    if (!g.pic && t.vod_pic) g.pic = t.vod_pic
    g.sizeMB += t.size_mb || 0
    g.episodes.push(t)
  }
  const list = [...m.values()]
  for (const g of list) g.episodes.sort((a, b) => a.ep_index - b.ep_index)
  list.sort((a, b) => b.episodes.length - a.episodes.length)
  return list
})

const totalSize = computed(() => done.value.reduce((n, t) => n + (t.size_mb || 0), 0))

function epLabel(t: DownloadTask | WatchHistoryItem): string {
  return t.ep_index >= 0 ? `第${t.ep_index + 1}集` : '整条'
}

// 某集上次看到第几秒（没有记录/已看完都返回 0）
function watchedAt(id: number): number {
  const h = histMap.value.get(id)
  return h && !h.finished && h.position > 5 ? h.position : 0
}

function play(id: number) {
  router.push(`/watch/${id}`)
}

/**
 * 文件直连：/play/{id} 直接返回 mp4 本体（支持 Range），
 * 浏览器会用它自带的播放器打开，不经过播放页，适合投屏/外部播放器/发给别人。
 */
function fileURL(id: number, base = location.origin): string {
  return `${base.replace(/\/+$/, '')}/play/${id}`
}

// 局域网根地址（后端会把 localhost 换成内网 IP）：复制给别人时不能用 127.0.0.1
let basePromise: Promise<string> | null = null
function lanBase(): Promise<string> {
  if (!basePromise) {
    basePromise = api.shareBase().then((r) => {
      const b = r.ok && r.data?.base_url ? r.data.base_url : location.origin
      return b.replace(/\/+$/, '')
    })
  }
  return basePromise
}

async function copyLink(t: DownloadTask) {
  const ok = await copyText(fileURL(t.id, await lanBase()))
  toast(ok ? `${epLabel(t)} 直链已复制` : '复制失败，请手动复制', ok ? 'ok' : 'err', 1800)
}

function openDirect(t: DownloadTask) {
  window.open(fileURL(t.id), '_blank', 'noopener')
}

// 同一部剧里的来源列表（多个源都下载过时在标题上标出来）
function srcList(tasks: DownloadTask[]): string[] {
  const set = new Set<string>()
  for (const t of tasks) if (t.source_name) set.add(t.source_name)
  return [...set]
}

// 删除单集：文件存在就删文件，文件不存在就只删记录（后端统一处理）
async function delEp(t: DownloadTask) {
  if (!confirm(`删除「${t.vod_name} ${epLabel(t)}」？\n本地文件存在会一并删除，文件已不存在则只删记录。`)) return
  const r = await api.delDownload(t.id)
  if (!r.ok) return toast(r.msg || '删除失败', 'err')
  toast('已删除', 'ok', 1500)
  load()
}

// 删除整部（该剧名下的全部已下载集）
async function delGroup(g: Group) {
  const n = g.episodes.length
  if (!confirm(`删除「${g.name}」的全部 ${n} 集？文件和记录都会删除，不可恢复。`)) return
  const r = await api.batchDownload('delete', g.episodes.map((e) => e.id))
  if (!r.ok) return toast(r.msg || '删除失败', 'err')
  toast(r.data || `已删除 ${n} 集`, 'ok')
  load()
}

async function load() {
  loading.value = true
  const [d, h] = await Promise.all([api.downloads(), api.history()])
  loading.value = false
  if (!d.ok || !d.data) {
    toast(d.msg || '读取本地文件失败', 'err')
    return
  }
  tasks.value = d.data.tasks || []
  history.value = h.ok && h.data ? h.data : []
}

onMounted(load)
</script>

<template>
  <div class="space-y-4">
    <!-- 顶部统计 -->
    <section class="card card-pad flex flex-wrap items-center gap-2">
      <span class="title">本地</span>
      <span class="chip">{{ done.length }} 集 · {{ groups.length }} 部</span>
      <span class="chip">{{ totalSize.toFixed(1) }} MB</span>
      <div class="relative ml-auto w-full sm:w-64">
        <svg viewBox="0 0 24 24" class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400" fill="none" stroke="currentColor" stroke-width="2">
          <circle cx="11" cy="11" r="7" /><path d="m20 20-3.5-3.5" />
        </svg>
        <input v-model="q" class="input py-1.5 pl-9 text-sm" type="search" placeholder="搜索已下载的剧" />
      </div>
      <button class="btn btn-ghost btn-sm min-h-10 px-3 xl:min-h-0" @click="load">刷新</button>
      <RouterLink to="/history" class="btn btn-ghost btn-sm min-h-10 px-3 xl:min-h-0">观看历史</RouterLink>
    </section>

    <!-- 继续观看 -->
    <section v-if="continuing.length" class="space-y-2">
      <div class="flex items-center gap-2 px-1">
        <span class="title">继续观看</span>
        <span class="chip">{{ continuing.length }}</span>
        <RouterLink to="/history" class="ml-auto inline-flex min-h-10 items-center text-xs text-amber-600 hover:underline dark:text-amber-400">全部观看历史 →</RouterLink>
      </div>
      <div class="grid gap-2 sm:grid-cols-2 xl:grid-cols-3">
        <button
          v-for="h in continuing" :key="h.download_id"
          class="card flex items-center gap-3 p-2.5 text-left transition hover:border-amber-400/60"
          @click="play(h.download_id)"
        >
          <div class="w-12 shrink-0 overflow-hidden rounded-lg">
            <CoverImg :src="h.vod_pic" :alt="h.vod_name" :retries="2" />
          </div>
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-1.5">
              <span class="truncate text-sm font-medium">{{ h.vod_name }}</span>
              <span class="shrink-0 text-[11px] text-slate-400">{{ epLabel(h) }}</span>
            </div>
            <div class="mt-1 flex items-center gap-2 text-[11px] muted">
              <span class="shrink-0">{{ fmt(h.position) }} / {{ fmt(h.duration) }}</span>
              <span class="shrink-0 text-amber-600 dark:text-amber-400">{{ h.pct }}%</span>
            </div>
            <div class="mt-1"><div class="bar h-1"><i :style="{ width: h.pct + '%' }"></i></div></div>
          </div>
          <span class="btn btn-primary btn-sm shrink-0">继续</span>
        </button>
      </div>
    </section>

    <!-- 已下载（按剧分组） -->
    <section class="space-y-2">
      <div class="flex items-center gap-2 px-1">
        <span class="title">已下载</span>
        <span class="chip">{{ groups.length }} 部 / {{ done.length }} 集</span>
      </div>

      <div v-if="loading" class="grid gap-2 sm:grid-cols-2">
        <div v-for="i in 4" :key="i" class="card h-24 animate-pulse bg-slate-100 dark:bg-white/5"></div>
      </div>

      <div v-else-if="groups.length === 0" class="card grid place-items-center gap-2 py-16 text-center">
        <div class="text-3xl">{{ q ? '🔍' : '📁' }}</div>
        <div class="title">{{ q ? '没有匹配的内容' : '本地还没有视频' }}</div>
        <p class="text-sm muted">{{ q ? '换个关键词试试' : '去影视库挑一集下载，完成后会出现在这里' }}</p>
        <RouterLink v-if="!q" to="/" class="btn btn-ghost btn-sm mt-1">去选片</RouterLink>
      </div>

      <div v-for="g in groups" :key="g.key" class="card p-3">
        <div class="flex items-start gap-3">
          <RouterLink :to="`/v/${g.vodId}`" class="w-10 shrink-0 overflow-hidden rounded-lg">
            <CoverImg :src="g.pic" :alt="g.name" :retries="2" />
          </RouterLink>
          <div class="min-w-0 flex-1">
            <div class="flex flex-wrap items-center gap-2">
              <RouterLink :to="`/v/${g.vodId}`" class="inline-flex min-h-10 items-center truncate text-sm font-medium hover:text-amber-500">{{ g.name }}</RouterLink>
              <span class="chip">{{ g.episodes.length }} 集</span>
              <span class="text-[11px] muted">{{ g.sizeMB.toFixed(1) }} MB</span>
              <span
                v-if="srcList(g.episodes).length > 1"
                class="chip border-amber-400/60 text-amber-600 dark:text-amber-400"
                :title="'这些集来自不同源：' + srcList(g.episodes).join(' / ')"
              >多源 {{ srcList(g.episodes).length }}</span>
              <button class="btn btn-ghost btn-sm ml-auto min-h-10 px-3 xl:min-h-0" title="删除整部的文件和记录" @click="delGroup(g)">删除</button>
            </div>
            <!-- 集列表：手机上每集占整行、按钮 40px 高（原来 12px 图标 + py-1 只有 20px，手指点不中）；
                 桌面端（sm 以上）仍还原成紧凑的小胶囊 -->
            <div class="mt-1.5 grid grid-cols-1 gap-1.5 xl:flex xl:flex-wrap">
              <span
                v-for="ep in g.episodes" :key="ep.id"
                class="flex items-stretch overflow-hidden rounded-lg border transition"
                :class="watchedAt(ep.id)
                  ? 'border-amber-400/70 bg-amber-500/10'
                  : 'border-slate-200 dark:border-white/10'"
              >
                <button
                  class="flex min-h-11 flex-1 items-center px-3 text-xs transition xl:min-h-0 xl:flex-none xl:px-2 xl:py-1 xl:text-[11px]"
                  :class="watchedAt(ep.id)
                    ? 'text-amber-700 dark:text-amber-300'
                    : 'hover:text-amber-500'"
                  :title="(watchedAt(ep.id) ? `上次看到 ${fmt(watchedAt(ep.id))}` : '点击播放')
                    + ` · 来源：${ep.source_name || ep.play_from || '未知'}`"
                  @click="play(ep.id)"
                >
                  {{ epLabel(ep) }}<span v-if="watchedAt(ep.id)" class="ml-0.5">·</span>
                </button>
                <button
                  class="grid w-11 shrink-0 place-items-center border-l border-slate-200 text-slate-400 transition hover:bg-sky-500/10 hover:text-sky-500 dark:border-white/10 xl:w-7 xl:py-1"
                  :title="`复制「${epLabel(ep)}」文件直链`"
                  :aria-label="`复制${epLabel(ep)}直链`"
                  @click.stop="copyLink(ep)"
                >
                  <svg viewBox="0 0 24 24" class="h-4 w-4 xl:h-3 xl:w-3" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
                    <rect x="9" y="9" width="11" height="11" rx="2" /><path d="M5 15V5a2 2 0 0 1 2-2h10" />
                  </svg>
                </button>
                <button
                  class="grid w-11 shrink-0 place-items-center border-l border-slate-200 text-slate-400 transition hover:bg-sky-500/10 hover:text-sky-500 dark:border-white/10 xl:w-7 xl:py-1"
                  :title="`新窗口直连打开「${epLabel(ep)}」（浏览器原生播放）`"
                  :aria-label="`新窗口打开${epLabel(ep)}`"
                  @click.stop="openDirect(ep)"
                >
                  <svg viewBox="0 0 24 24" class="h-4 w-4 xl:h-3 xl:w-3" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                    <path d="M14 4h6v6" /><path d="M20 4 11 13" /><path d="M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5" />
                  </svg>
                </button>
                <button
                  class="grid w-11 shrink-0 place-items-center border-l border-slate-200 text-slate-400 transition hover:bg-rose-500/10 hover:text-rose-500 dark:border-white/10 xl:w-7 xl:py-1"
                  :title="`删除「${epLabel(ep)}」（文件 + 记录）`"
                  :aria-label="`删除${epLabel(ep)}`"
                  @click.stop="delEp(ep)"
                >
                  <svg viewBox="0 0 24 24" class="h-4 w-4 xl:h-3 xl:w-3" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                    <path d="M4 7h16" /><path d="M9 7V4h6v3" /><path d="M6 7l1 13h10l1-13" />
                  </svg>
                </button>
              </span>
            </div>
          </div>
        </div>
      </div>
    </section>
  </div>
</template>

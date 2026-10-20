<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import CoverImg from '../components/CoverImg.vue'
import { api } from '../api'
import { toast } from '../toast'
import type { DownloadTask } from '../types'

/**
 * 下载管理：只显示「还没下完」的任务（进行中/排队/合并中/暂停/失败）。
 * 下载完成的已经分离到「本地」模块，这里不再堆几百条已完成记录。
 */

const all = ref<DownloadTask[]>([])
const tab = ref<'active' | 'failed' | 'all'>('active')
const q = ref('')
const picked = ref<Set<number>>(new Set())
const expanded = ref<Set<string>>(new Set())
const autoExpanded = ref(true) // 首次进入时自动展开"有进行中任务"的剧
let timer: number | undefined

const ACTIVE = ['queued', 'downloading', 'merging', 'paused']

const statusText: Record<string, string> = {
  queued: '排队中', downloading: '下载中', merging: '合并中', paused: '已暂停', done: '已完成', failed: '失败',
}

function epLabel(t: DownloadTask) {
  return t.ep_index >= 0 ? `第${t.ep_index + 1}集` : '整条'
}

// 已完成的都归「本地」模块，这里一条都不显示
const pending = computed(() => all.value.filter((t) => t.status !== 'done'))

const counts = computed(() => ({
  active: pending.value.filter((t) => ACTIVE.includes(t.status)).length,
  failed: pending.value.filter((t) => t.status === 'failed').length,
  done: all.value.filter((t) => t.status === 'done').length, // 只用于顶部提示
}))

// 标签页筛选
const tabbed = computed(() => {
  if (tab.value === 'active') return pending.value.filter((t) => ACTIVE.includes(t.status))
  if (tab.value === 'failed') return pending.value.filter((t) => t.status === 'failed')
  return pending.value
})

// 搜索（剧名 / 来源 / 集名 / 状态）
const filtered = computed(() => {
  const kw = q.value.trim().toLowerCase()
  if (!kw) return tabbed.value
  return tabbed.value.filter((t) => {
    const hay = [t.vod_name, t.source_name, epLabel(t), statusText[t.status] || t.status]
      .join(' ')
      .toLowerCase()
    return hay.includes(kw)
  })
})

interface Group {
  key: string
  name: string
  vodId: number
  pic: string
  tasks: DownloadTask[]
  done: number
  active: number
  failed: number
  sizeMB: number
  running: DownloadTask | null
  latest: number
}

// 按剧名分组
const groups = computed<Group[]>(() => {
  const m = new Map<string, Group>()
  for (const t of filtered.value) {
    const key = t.vod_name || `#${t.vod_id}`
    let g = m.get(key)
    if (!g) {
      g = {
        key, name: t.vod_name, vodId: t.vod_id, pic: t.vod_pic || '',
        tasks: [], done: 0, active: 0, failed: 0, sizeMB: 0,
        running: null, latest: 0,
      }
      m.set(key, g)
    }
    g.tasks.push(t)
    if (!g.pic && t.vod_pic) g.pic = t.vod_pic
    if (t.status === 'done') {
      g.done++
      g.sizeMB += t.size_mb || 0
    } else if (ACTIVE.includes(t.status)) {
      g.active++
      if (!g.running && (t.status === 'downloading' || t.status === 'merging')) g.running = t
    } else if (t.status === 'failed') {
      g.failed++
    }
    if (t.id > g.latest) g.latest = t.id
  }
  const list = [...m.values()]
  for (const g of list) g.tasks.sort((a, b) => a.ep_index - b.ep_index)
  // 有在跑的排前面，其次最近操作的
  list.sort((a, b) => Number(!!b.running) - Number(!!a.running) || b.latest - a.latest)
  return list
})

function isOpen(g: Group) {
  if (q.value.trim()) return true // 搜索时全部展开，直接看到命中的集
  if (autoExpanded.value && g.running) return true
  return expanded.value.has(g.key)
}

function toggleGroup(g: Group) {
  const s = new Set(expanded.value)
  if (s.has(g.key)) s.delete(g.key)
  else s.add(g.key)
  expanded.value = s
}

function expandAll() {
  autoExpanded.value = false
  expanded.value = new Set(groups.value.map((g) => g.key))
}
function collapseAll() {
  autoExpanded.value = false
  expanded.value = new Set()
}

async function load() {
  const r = await api.downloads()
  if (r.ok && r.data) {
    all.value = r.data.tasks || []
    const ids = new Set(all.value.map((t) => t.id))
    picked.value = new Set([...picked.value].filter((id) => ids.has(id)))
  }
}

function toggle(id: number) {
  const s = new Set(picked.value)
  if (s.has(id)) s.delete(id)
  else s.add(id)
  picked.value = s
}

function groupPicked(g: Group) {
  return g.tasks.every((t) => picked.value.has(t.id))
}

function toggleGroupPick(g: Group) {
  const s = new Set(picked.value)
  const on = groupPicked(g)
  for (const t of g.tasks) {
    if (on) s.delete(t.id)
    else s.add(t.id)
  }
  picked.value = s
}

function toggleAll() {
  picked.value = picked.value.size === filtered.value.length ? new Set() : new Set(filtered.value.map((t) => t.id))
}

async function act(t: DownloadTask, action: 'pause' | 'resume' | 'retry' | 'priority' | 'delete') {
  if (action === 'delete' && !confirm(`删除「${t.vod_name} ${epLabel(t)}」会同时删除本地文件，继续？`)) return
  const r = action === 'pause' ? await api.pauseDownload(t.id)
    : action === 'resume' ? await api.resumeDownload(t.id)
      : action === 'retry' ? await api.retryDownload(t.id)
        : action === 'priority' ? await api.priorityDownload(t.id)
          : await api.delDownload(t.id)
  if (!r.ok) return toast(r.msg || '操作失败', 'err')
  const msg = action === 'delete' ? '已删除' : action === 'pause' ? '已暂停'
    : action === 'resume' ? '已继续' : action === 'retry' ? '已重新加入队列'
    : (r.data || '已设为优先下载')
  toast(msg, 'ok', 1800)
  load()
}

// 同一部剧里的来源列表（多个源都下载过时标出来）
function srcList(tasks: DownloadTask[]): string[] {
  const s = new Set<string>()
  for (const t of tasks) if (t.source_name) s.add(t.source_name)
  return [...s]
}

async function batch(action: 'pause' | 'resume' | 'retry' | 'delete') {
  const ids = [...picked.value]
  if (!ids.length) return toast('先选择任务', 'info', 1500)
  if (action === 'delete' && !confirm(`删除选中的 ${ids.length} 个任务会同时删除本地文件，继续？`)) return
  const r = await api.batchDownload(action, ids)
  if (!r.ok) return toast(r.msg || '操作失败', 'err')
  toast(r.data || '操作成功', 'ok')
  picked.value = new Set()
  load()
}

// 一键重试当前全部失败任务（不翻页逐个勾）
async function retryAllFailed() {
  const ids = pending.value.filter((t) => t.status === 'failed').map((t) => t.id)
  if (!ids.length) return toast('没有失败的任务', 'info', 1500)
  const r = await api.batchDownload('retry', ids)
  if (!r.ok) return toast(r.msg || '操作失败', 'err')
  toast(r.data || '已重新加入队列', 'ok')
  load()
}

async function delGroup(g: Group) {
  const done = g.tasks.filter((t) => t.status === 'done')
  const tip = done.length
    ? `删除「${g.name}」的 ${g.tasks.length} 个任务（含 ${done.length} 个已下载文件）？不可恢复。`
    : `删除「${g.name}」的 ${g.tasks.length} 个任务？`
  if (!confirm(tip)) return
  const r = await api.batchDownload('delete', g.tasks.map((t) => t.id))
  if (!r.ok) return toast(r.msg || '操作失败', 'err')
  toast(r.data || '已删除', 'ok')
  load()
}

onMounted(() => {
  load()
  timer = window.setInterval(load, 2500)
})
onBeforeUnmount(() => {
  if (timer) window.clearInterval(timer)
})
</script>

<template>
  <div class="space-y-4">
    <!-- 顶部：切换 + 搜索 + 批量 -->
    <section class="card card-pad space-y-3">
      <div class="flex flex-wrap items-center gap-2">
        <div class="flex rounded-xl bg-slate-100 p-1 dark:bg-white/5">
          <button class="rounded-lg px-3 py-1.5 text-xs font-medium tap" :class="tab === 'active' ? 'bg-white shadow-sm dark:bg-white/10' : 'text-slate-500'" @click="tab = 'active'">
            进行中 {{ counts.active }}
          </button>
          <button class="rounded-lg px-3 py-1.5 text-xs font-medium tap" :class="tab === 'failed' ? 'bg-white shadow-sm dark:bg-white/10' : 'text-slate-500'" @click="tab = 'failed'">
            失败 {{ counts.failed }}
          </button>
          <button class="rounded-lg px-3 py-1.5 text-xs font-medium tap" :class="tab === 'all' ? 'bg-white shadow-sm dark:bg-white/10' : 'text-slate-500'" @click="tab = 'all'">
            全部 {{ pending.length }}
          </button>
        </div>

        <div class="relative min-w-[180px] flex-1">
          <svg viewBox="0 0 24 24" class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400" fill="none" stroke="currentColor" stroke-width="2">
            <circle cx="11" cy="11" r="7" /><path d="m20 20-3.5-3.5" />
          </svg>
          <input v-model="q" class="input py-1.5 pl-9 text-sm" type="search" placeholder="搜索剧名 / 来源 / 集数" />
        </div>

        <div class="flex flex-wrap items-center gap-2">
          <button class="btn btn-ghost btn-sm" @click="expandAll">展开全部</button>
          <button class="btn btn-ghost btn-sm" @click="collapseAll">收起全部</button>
        </div>
      </div>

      <div class="flex flex-wrap items-center gap-2 border-t border-slate-100 pt-2 dark:border-white/5">
        <label class="flex min-h-10 items-center gap-1.5 pr-2 text-xs muted">
          <input type="checkbox" class="h-5 w-5" :checked="picked.size > 0 && picked.size === filtered.length" @change="toggleAll" />
          全选当前 {{ filtered.length }} 集
        </label>
        <span v-if="picked.size" class="chip chip-on">已选 {{ picked.size }}</span>
        <div class="ml-auto flex flex-wrap items-center gap-2">
          <button class="btn btn-ghost btn-sm" :disabled="!picked.size" @click="batch('pause')">批量暂停</button>
          <button class="btn btn-ghost btn-sm" :disabled="!picked.size" @click="batch('resume')">批量继续</button>
          <button class="btn btn-ghost btn-sm" :disabled="!picked.size" @click="batch('retry')">批量重试</button>
          <button class="btn btn-danger btn-sm" :disabled="!picked.size" @click="batch('delete')">批量删除</button>
        </div>
      </div>
      <div v-if="counts.failed" class="flex flex-wrap items-center gap-2 text-xs text-rose-500">
        <span>有 {{ counts.failed }} 个任务失败，展开后点「重试」单个重跑。</span>
        <button class="btn btn-ghost btn-sm" @click="retryAllFailed">重试全部失败</button>
      </div>
      <p v-if="counts.done" class="text-xs muted">
        已下载完成的 {{ counts.done }} 个任务已移到
        <RouterLink to="/local" class="text-amber-600 hover:underline dark:text-amber-400">「本地」</RouterLink>
        模块，这里只显示还没下完的。
      </p>
    </section>

    <!-- 分组列表 -->
    <section class="space-y-2">
      <div v-if="filtered.length === 0" class="card grid place-items-center gap-2 py-16 text-center">
        <div class="text-3xl">{{ q ? '🔍' : '📥' }}</div>
        <div class="title">{{ q ? '没有匹配的任务' : '没有进行中的下载' }}</div>
        <p class="text-sm muted">{{ q ? '换个关键词试试' : '去影视库挑一集下载吧；已下载的都在「本地」' }}</p>
        <RouterLink v-if="!q" to="/" class="btn btn-ghost btn-sm mt-1">去选片</RouterLink>
      </div>

      <div v-for="g in groups" :key="g.key" class="card overflow-hidden">
        <!-- 剧集分组头 -->
        <div class="flex items-center gap-3 p-3">
          <input type="checkbox" class="h-5 w-5 shrink-0" :checked="groupPicked(g)" @change="toggleGroupPick(g)" />

          <button class="w-9 shrink-0 overflow-hidden rounded-lg" title="展开/收起" @click="toggleGroup(g)">
            <CoverImg :src="g.pic" :alt="g.name" :retries="2" />
          </button>

          <div class="min-w-0 flex-1 cursor-pointer" @click="toggleGroup(g)">
            <div class="flex items-center gap-2">
              <span class="truncate text-sm font-medium">{{ g.name }}</span>
              <span class="chip shrink-0">{{ g.tasks.length }} 集</span>
              <span
                v-if="srcList(g.tasks).length > 1"
                class="chip shrink-0"
                :title="'这些集来自不同源：' + srcList(g.tasks).join(' / ')"
              >多源 {{ srcList(g.tasks).length }}</span>
              <span v-if="g.active" class="shrink-0 text-[11px] text-amber-600 dark:text-amber-400">进行中 {{ g.active }}</span>
              <span v-if="g.failed" class="shrink-0 text-[11px] text-rose-500">失败 {{ g.failed }}</span>
            </div>

            <div class="mt-1 flex items-center gap-2 text-[11px] text-slate-500 dark:text-slate-400">
              <template v-if="g.running">
                <span class="shrink-0">{{ statusText[g.running.status] }} {{ epLabel(g.running) }}</span>
                <span class="shrink-0 text-amber-500">{{ g.running.percent }}%</span>
                <span v-if="g.running.speed" class="shrink-0">{{ g.running.speed }}</span>
              </template>
              <span v-else class="shrink-0">未开始</span>
            </div>
          </div>

          <div class="flex shrink-0 items-center gap-1.5">
            <button class="btn btn-ghost btn-sm" @click="toggleGroup(g)">
              {{ isOpen(g) ? '收起' : '展开' }}
            </button>
            <button class="btn btn-danger btn-sm" @click="delGroup(g)">删除</button>
          </div>
        </div>

        <!-- 分组进度条 -->
        <div v-if="g.running" class="px-3 pb-2">
          <div class="bar" :class="g.running.status === 'merging' ? '[&>i]:bg-sky-500' : ''">
            <i :style="{ width: (g.running.percent || 0) + '%' }"></i>
          </div>
          <div v-if="g.running.status === 'merging'" class="mt-0.5 text-[11px] text-sky-600 dark:text-sky-400">
            下载完成，正在合并为 mp4…{{ g.running.percent > 0 ? ' ' + g.running.percent + '%' : '' }}
          </div>
        </div>

        <!-- 展开后的每一集 -->
        <div v-if="isOpen(g)" class="border-t border-slate-100 bg-slate-50/60 px-3 py-2 dark:border-white/5 dark:bg-white/[0.02]">
          <div class="grid gap-1.5 sm:grid-cols-2 xl:grid-cols-3">
            <div
              v-for="t in g.tasks" :key="t.id"
              class="flex items-center gap-2 rounded-lg bg-white px-2 py-1.5 text-xs dark:bg-white/5"
            >
              <input type="checkbox" class="h-5 w-5 shrink-0" :checked="picked.has(t.id)" @change="toggle(t.id)" />

              <span class="w-14 shrink-0 truncate font-medium">{{ epLabel(t) }}</span>

              <span
                class="shrink-0 rounded px-1.5 py-0.5 text-[11px]"
                :class="t.status === 'done' ? 'bg-emerald-500/15 text-emerald-600 dark:text-emerald-400'
                  : t.status === 'failed' ? 'bg-rose-500/15 text-rose-600 dark:text-rose-400'
                  : t.status === 'merging' ? 'bg-sky-500/15 text-sky-600 dark:text-sky-400'
                  : 'bg-amber-500/15 text-amber-600 dark:text-amber-400'"
              >{{ statusText[t.status] || t.status }}</span>

              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-1.5 text-[11px] text-slate-500 dark:text-slate-400">
                  <span v-if="t.size_mb > 0" class="shrink-0">{{ t.size_mb.toFixed(1) }} MB</span>
                  <span v-if="['downloading', 'queued', 'merging', 'paused'].includes(t.status)" class="shrink-0">{{ t.percent }}%</span>
                  <span v-if="t.error" class="truncate text-rose-500">{{ t.error }}</span>
                </div>
                <div v-if="['downloading', 'queued', 'merging', 'paused'].includes(t.status)" class="mt-1">
                  <div class="bar h-1" :class="t.status === 'merging' ? '[&>i]:bg-sky-500' : ''">
                    <i :style="{ width: (t.percent || 0) + '%' }"></i>
                  </div>
                </div>
              </div>

              <div class="flex shrink-0 items-center gap-1">
                <span v-if="t.priority === 1" class="chip shrink-0 text-amber-600 dark:text-amber-400">优先</span>
                <button
                  v-if="['queued', 'paused', 'failed'].includes(t.status) && t.priority !== 1"
                  class="btn btn-ghost btn-sm px-2! py-1!"
                  title="插队：当前这一集下完后先下这个"
                  @click="act(t, 'priority')"
                >优先</button>
                <button v-if="t.status === 'downloading' || t.status === 'queued'" class="btn btn-ghost btn-sm px-2! py-1!" @click="act(t, 'pause')">暂停</button>
                <button v-else-if="t.status === 'paused'" class="btn btn-ghost btn-sm px-2! py-1!" @click="act(t, 'resume')">继续</button>
                <button v-else-if="t.status === 'failed'" class="btn btn-ghost btn-sm px-2! py-1!" title="重新排队下载（已下好的分片会复用）" @click="act(t, 'retry')">重试</button>
                <button class="btn btn-danger btn-sm px-2! py-1!" @click="act(t, 'delete')">删</button>
              </div>
            </div>
          </div>
        </div>
      </div>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import CoverImg from '../components/CoverImg.vue'
import { api } from '../api'
import { toast } from '../toast'
import type { WatchHistoryItem } from '../types'

/**
 * 观看历史（独立页）
 *
 * 数据来自 vod_watch_history（/api/watch/history，已按最近观看倒序、最多 200 条）。
 * 从这里可以：
 * - 直接继续播放（/watch/:id 会自动从上次位置续播）
 * - 只看没看完的（等价于「继续观看」的完整版）
 * - 移除单条 / 清空历史（只删进度记录，不动已下载的文件）
 */
const router = useRouter()

const all = ref<WatchHistoryItem[]>([])
const loading = ref(true)
const q = ref('')
const tab = ref<'all' | 'unfinished' | 'finished'>('all')

const filtered = computed(() => {
  const kw = q.value.trim().toLowerCase()
  return all.value.filter((h) => {
    if (tab.value === 'unfinished' && h.finished) return false
    if (tab.value === 'finished' && !h.finished) return false
    if (!kw) return true
    return `${h.vod_name} ${h.source_name} 第${h.ep_index + 1}集`.toLowerCase().includes(kw)
  })
})

const counts = computed(() => ({
  all: all.value.length,
  unfinished: all.value.filter((h) => !h.finished).length,
  finished: all.value.filter((h) => h.finished).length,
}))

function fmt(sec: number): string {
  if (!sec || !isFinite(sec)) return '00:00'
  const s = Math.max(0, Math.floor(sec))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const ss = s % 60
  const mm = String(m).padStart(2, '0')
  return h > 0 ? `${h}:${mm}:${String(ss).padStart(2, '0')}` : `${mm}:${String(ss).padStart(2, '0')}`
}

function pct(h: WatchHistoryItem): number {
  if (h.duration <= 0) return 0
  return Math.min(100, Math.round((h.position / h.duration) * 100))
}

/** 后端返回的是 SQLite 的 "YYYY-MM-DD HH:MM:SS"（UTC） */
function when(s: string): string {
  if (!s) return ''
  const t = new Date(s.includes('T') ? s : s.replace(' ', 'T') + 'Z')
  if (isNaN(t.getTime())) return s
  const now = Date.now()
  const diff = now - t.getTime()
  if (diff < 60_000) return '刚刚'
  if (diff < 3600_000) return `${Math.floor(diff / 60_000)} 分钟前`
  const pad = (n: number) => String(n).padStart(2, '0')
  const hm = `${pad(t.getHours())}:${pad(t.getMinutes())}`
  const today = new Date()
  const sameDay = t.toDateString() === today.toDateString()
  if (sameDay) return `今天 ${hm}`
  const y = new Date(today.getTime() - 86400_000)
  if (t.toDateString() === y.toDateString()) return `昨天 ${hm}`
  return `${t.getFullYear()}-${pad(t.getMonth() + 1)}-${pad(t.getDate())} ${hm}`
}

function epLabel(h: WatchHistoryItem): string {
  return h.ep_index >= 0 ? `第${h.ep_index + 1}集` : '整条'
}

function play(h: WatchHistoryItem) {
  router.push(`/watch/${h.download_id}`)
}

async function load() {
  loading.value = true
  const r = await api.history()
  loading.value = false
  if (!r.ok || !r.data) {
    toast(r.msg || '读取观看历史失败', 'err')
    return
  }
  all.value = r.data
}

async function remove(h: WatchHistoryItem) {
  const r = await api.delHistory(h.download_id)
  if (!r.ok) return toast(r.msg || '移除失败', 'err')
  all.value = all.value.filter((x) => x.download_id !== h.download_id)
  toast('已从历史移除', 'ok', 1500)
}

async function clearAll() {
  if (!all.value.length) return
  if (!confirm(`清空全部 ${all.value.length} 条观看记录？\n只会删除进度，已下载的视频文件不受影响。`)) return
  const r = await api.clearHistory()
  if (!r.ok) return toast(r.msg || '清空失败', 'err')
  all.value = []
  toast(r.data?.msg || '已清空观看历史', 'ok')
}

onMounted(load)
</script>

<template>
  <div class="space-y-3 sm:space-y-4">
    <!-- 顶部：统计 + 搜索 + 清理 -->
    <section class="card card-pad space-y-3">
      <div class="flex flex-wrap items-center gap-2">
        <span class="title">观看历史</span>
        <span class="chip">{{ counts.all }} 条</span>
        <span v-if="counts.unfinished" class="chip chip-on">{{ counts.unfinished }} 条未看完</span>
        <button class="btn btn-ghost btn-sm ml-auto" @click="load">刷新</button>
        <button class="btn btn-danger btn-sm" :disabled="!counts.all" @click="clearAll">清空历史</button>
      </div>

      <div class="flex flex-col gap-2 sm:flex-row sm:items-center">
        <div class="flex rounded-xl bg-slate-100 p-1 text-xs dark:bg-white/5">
          <button class="rounded-lg px-3 py-1.5 font-medium tap" :class="tab === 'all' ? 'bg-white shadow-sm dark:bg-white/10' : 'text-slate-500'" @click="tab = 'all'">
            全部 {{ counts.all }}
          </button>
          <button class="rounded-lg px-3 py-1.5 font-medium tap" :class="tab === 'unfinished' ? 'bg-white shadow-sm dark:bg-white/10' : 'text-slate-500'" @click="tab = 'unfinished'">
            未看完 {{ counts.unfinished }}
          </button>
          <button class="rounded-lg px-3 py-1.5 font-medium tap" :class="tab === 'finished' ? 'bg-white shadow-sm dark:bg-white/10' : 'text-slate-500'" @click="tab = 'finished'">
            已看完 {{ counts.finished }}
          </button>
        </div>

        <div class="relative min-w-[180px] flex-1">
          <svg viewBox="0 0 24 24" class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400" fill="none" stroke="currentColor" stroke-width="2">
            <circle cx="11" cy="11" r="7" /><path d="m20 20-3.5-3.5" />
          </svg>
          <input v-model="q" class="input py-1.5 pl-9 text-sm" type="search" placeholder="搜索剧名 / 来源 / 集数" />
        </div>
      </div>
    </section>

    <!-- 列表 -->
    <section class="space-y-2">
      <div v-if="loading" class="space-y-2">
        <div v-for="i in 5" :key="i" class="card flex animate-pulse items-center gap-3 p-3">
          <div class="h-20 w-14 shrink-0 rounded-lg bg-slate-200/80 dark:bg-white/5"></div>
          <div class="flex-1 space-y-2">
            <div class="h-3 w-1/3 rounded bg-slate-200 dark:bg-white/5"></div>
            <div class="h-2 w-2/3 rounded bg-slate-200 dark:bg-white/5"></div>
            <div class="h-2 w-1/4 rounded bg-slate-200 dark:bg-white/5"></div>
          </div>
        </div>
      </div>

      <div v-else-if="!filtered.length" class="card grid place-items-center gap-2 py-16 text-center">
        <div class="text-3xl">{{ q || tab !== 'all' ? '🔍' : '🕘' }}</div>
        <div class="title">{{ q || tab !== 'all' ? '没有匹配的记录' : '还没有观看记录' }}</div>
        <p class="text-sm muted">{{ q || tab !== 'all' ? '换个关键词或切回「全部」看看' : '去「本地」随便播一集，进度就会记在这里' }}</p>
        <RouterLink v-if="!q && tab === 'all'" to="/local" class="btn btn-ghost btn-sm mt-1">去本地看看</RouterLink>
      </div>

      <!-- 手机：剧名独占整行（最多两行，不截断），操作按钮换到下一行右侧；
           平板 / 桌面：按钮回到右侧，剧名与 chip 同行 -->
      <div
        v-for="h in filtered" :key="h.download_id"
        class="card flex flex-wrap items-start gap-3 p-3 transition hover:border-slate-300 dark:hover:border-white/20"
      >
        <button class="w-14 shrink-0 overflow-hidden rounded-lg" :title="'播放 ' + epLabel(h)" @click="play(h)">
          <CoverImg :src="h.vod_pic" :alt="h.vod_name" :retries="2" />
        </button>

        <div class="min-w-0 flex-1">
          <button
            class="line-clamp-2 min-h-10 w-full min-w-0 text-left text-sm font-medium leading-5 hover:text-amber-600 sm:w-auto sm:max-w-[62%] dark:hover:text-amber-400"
            :title="h.vod_name"
            @click="play(h)"
          >
            {{ h.vod_name }}
          </button>

          <div class="flex flex-wrap items-center gap-1.5">
            <span class="chip shrink-0">{{ epLabel(h) }}</span>
            <span v-if="h.finished" class="chip shrink-0">已看完</span>
            <span v-if="h.source_name" class="chip hidden shrink-0 sm:inline-flex">{{ h.source_name }}</span>
          </div>

          <div class="mt-1.5 max-w-md">
            <div class="bar h-1" :class="h.finished ? '[&>i]:bg-emerald-500' : ''">
              <i :style="{ width: pct(h) + '%' }"></i>
            </div>
            <div class="mt-1 flex flex-wrap items-center gap-x-2 text-[11px] muted">
              <span v-if="h.duration > 0">看到 {{ fmt(h.position) }} / {{ fmt(h.duration) }} · {{ pct(h) }}%</span>
              <span v-else>看到 {{ fmt(h.position) }}</span>
              <span>· {{ when(h.updated_at) }}</span>
            </div>
          </div>
        </div>

        <div class="ml-auto flex w-full shrink-0 items-center justify-end gap-1.5 sm:w-auto">
          <button class="btn btn-ghost btn-sm" @click="play(h)">{{ h.finished ? '重看' : '继续播放' }}</button>
          <button class="btn btn-danger btn-sm" title="只移除这条进度记录，不影响视频文件" @click="remove(h)">移除</button>
        </div>
      </div>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import VideoPlayer from '../components/VideoPlayer.vue'
import ShareDialog from '../components/ShareDialog.vue'
import { api } from '../api'
import { toast } from '../toast'
import type { DownloadTask } from '../types'

/**
 * 播放页：/watch/:id（id = 下载任务 id）
 * 下面列出「同一部剧」所有已下载的集，可以直接切换，播完自动下一集。
 */
const route = useRoute()
const router = useRouter()

const curID = computed(() => Number(route.params.id) || 0)
const tasks = ref<DownloadTask[]>([])
const loading = ref(true)
const notFound = ref('')
const posMap = ref(new Map<number, number>()) // download_id -> 上次看到第几秒
const showShare = ref(false)

const current = computed(() => tasks.value.find((t) => t.id === curID.value) || null)

// 同名（同一部剧）里已下载的集：按集号排序，同集多份时保留体积最大的那份
const episodes = computed<DownloadTask[]>(() => {
  const name = current.value?.vod_name
  if (!name) return []
  const map = new Map<number, DownloadTask>()
  for (const t of tasks.value) {
    if (t.status !== 'done' || t.vod_name !== name) continue
    const prev = map.get(t.ep_index)
    if (!prev || (t.size_mb || 0) > (prev.size_mb || 0)) map.set(t.ep_index, t)
  }
  return [...map.values()].sort((a, b) => a.ep_index - b.ep_index)
})

// 从上次位置继续（看完的集从头播）
const startAt = computed(() => {
  const p = posMap.value.get(curID.value) || 0
  return p > 5 ? p : 0
})

const idx = computed(() => episodes.value.findIndex((t) => t.id === curID.value))
const prevEp = computed(() => (idx.value > 0 ? episodes.value[idx.value - 1] : null))
const nextEp = computed(() =>
  idx.value >= 0 && idx.value + 1 < episodes.value.length ? episodes.value[idx.value + 1] : null,
)

function epLabel(t: DownloadTask | null): string {
  if (!t) return ''
  return t.ep_index >= 0 ? `第${t.ep_index + 1}集` : '整条'
}

function go(t: DownloadTask | null) {
  if (t && t.id !== curID.value) router.replace(`/watch/${t.id}`)
}

async function load() {
  loading.value = true
  const [r, h] = await Promise.all([api.downloads(), api.history()])
  loading.value = false
  if (h.ok && h.data) {
    const m = new Map<number, number>()
    for (const it of h.data) m.set(it.download_id, it.finished ? 0 : it.position)
    posMap.value = m
  }
  if (!r.ok || !r.data) {
    notFound.value = r.msg || '读取下载记录失败'
    return
  }
  tasks.value = r.data.tasks || []
  const t = tasks.value.find((x) => x.id === curID.value)
  if (!t) notFound.value = '这个视频不在了（可能已被删除）'
  else if (t.status !== 'done') notFound.value = '这一集还没下载完成，回到下载页看看进度'
  else notFound.value = ''
}

// 播放进度：播放器每 5 秒回调一次，直接落库（本地也更新，切集回来能接上）
let lastPos = 0
let lastDur = 0
function onTime(pos: number, dur: number) {
  lastPos = pos
  lastDur = dur
  posMap.value.set(curID.value, dur > 0 && pos >= dur * 0.95 ? 0 : pos)
  api.saveProgress(curID.value, pos, dur)
}

function saveNow() {
  if (lastDur > 0) api.saveProgress(curID.value, lastPos, lastDur)
}

// 键盘左右切集（播放器自己的快捷键不冲突时很实用）
function onKey(e: KeyboardEvent) {
  const el = e.target as HTMLElement | null
  if (el && /input|textarea|select/i.test(el.tagName)) return
  if (e.key === 'ArrowLeft') go(prevEp.value)
  else if (e.key === 'ArrowRight') go(nextEp.value)
}

onMounted(() => {
  load()
  window.addEventListener('keydown', onKey)
  window.addEventListener('pagehide', saveNow)
})
onBeforeUnmount(() => {
  saveNow()
  window.removeEventListener('keydown', onKey)
  window.removeEventListener('pagehide', saveNow)
})
watch(curID, () => {
  if (!notFound.value) return
  load()
})

// 删除/切集后如果记录消失，提示一下
watch(tasks, () => {
  if (tasks.value.length === 0) toast('没有下载记录', 'err', 1600)
})
</script>

<template>
  <div class="space-y-4">
    <!-- 顶部信息 -->
    <section class="flex flex-wrap items-center gap-2">
      <button class="btn btn-ghost btn-sm" @click="router.back()">← 返回</button>
      <div class="min-w-0">
        <div class="truncate text-base font-semibold">{{ current?.vod_name || '播放' }}</div>
        <div class="mt-0.5 flex flex-wrap items-center gap-2 text-[11px] muted">
          <span v-if="current">{{ epLabel(current) }}</span>
          <span v-if="current?.source_name">{{ current.source_name }}</span>
          <span v-if="current && current.size_mb > 0">{{ current.size_mb.toFixed(1) }} MB</span>
          <span v-if="episodes.length">共 {{ episodes.length }} 集已下载</span>
        </div>
      </div>
      <div class="ml-auto flex items-center gap-1.5">
        <button v-if="current" class="btn btn-primary btn-sm" @click="showShare = true">分享</button>
        <a
          v-if="current"
          :href="`/play/${current.id}`"
          target="_blank"
          rel="noopener"
          class="btn btn-ghost btn-sm"
          title="用浏览器直接打开视频文件（不经过播放页，可配合投屏/外部播放器）"
        >直连</a>
        <RouterLink v-if="current" :to="`/v/${current.vod_id}`" class="btn btn-ghost btn-sm">影片详情</RouterLink>
      </div>
    </section>

    <!-- 播放器 -->
    <section v-if="current && !notFound" class="card overflow-hidden">
      <VideoPlayer :url="`/play/${current.id}`" :start-at="startAt" @time="onTime" @ended="go(nextEp)" />
    </section>

    <section v-else-if="!loading" class="card grid place-items-center gap-2 py-16 text-center">
      <div class="text-3xl">🎬</div>
      <div class="title">{{ notFound || '找不到这一集' }}</div>
      <RouterLink to="/downloads" class="btn btn-ghost btn-sm mt-1">去下载页</RouterLink>
    </section>

    <!-- 同剧集数切换 -->
    <section v-if="current && episodes.length" class="card card-pad space-y-3">
      <div class="flex flex-wrap items-center gap-2">
        <span class="title">剧集</span>
        <span class="chip">{{ episodes.length }} 集</span>
        <div class="ml-auto flex items-center gap-1.5">
          <button class="btn btn-ghost btn-sm" :disabled="!prevEp" @click="go(prevEp)">上一集</button>
          <button class="btn btn-ghost btn-sm" :disabled="!nextEp" @click="go(nextEp)">下一集</button>
        </div>
      </div>

      <div class="grid grid-cols-3 gap-2 sm:grid-cols-5 md:grid-cols-8 xl:grid-cols-12">
        <button
          v-for="ep in episodes" :key="ep.id"
          class="truncate rounded-lg border px-2 py-1.5 text-xs transition tap"
          :class="ep.id === curID
            ? 'border-amber-400 bg-amber-500/15 font-medium text-amber-700 dark:text-amber-300'
            : 'border-slate-200 hover:border-slate-300 dark:border-white/10 dark:hover:border-white/25'"
          :title="`${epLabel(ep)}${ep.size_mb ? ' · ' + ep.size_mb.toFixed(1) + 'MB' : ''}`"
          @click="go(ep)"
        >
          {{ epLabel(ep) }}
        </button>
      </div>

      <p class="text-[11px] muted">提示：播放器内按 ← → 也能切集；点屏幕左侧快退 15 秒、右侧快进 15 秒；长按右侧 2 倍速播放，松手恢复；播完会自动播下一集。</p>
    </section>
  </div>

  <Teleport to="body">
    <ShareDialog v-if="showShare && current" :id="current.id" @close="showShare = false" />
  </Teleport>
</template>

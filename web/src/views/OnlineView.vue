<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import VideoPlayer from '../components/VideoPlayer.vue'
import { api } from '../api'
import { toast } from '../toast'
import type { Episode, Line, VideoDetail, VideoSource } from '../types'

/**
 * 在线播放：/online/:vid?from=<线路>&ep=<集号>
 *
 * vid 是 vod_info.id（某个采集源下的这一部片），from 是 play_from（线路）。
 * 播放地址走本服务代理 /api/live?u=...：源站 m3u8 不带 CORS 头，
 * 浏览器里 hls.js 直接拉会被拦，必须由后端改写播放列表再转发。
 *
 * 与 /watch/:id（本地已下载 mp4）分开：一个是直接看源站，一个是看下载好的文件。
 */
const route = useRoute()
const router = useRouter()

const vid = computed(() => Number(route.params.vid) || 0)
const detail = ref<VideoDetail | null>(null)
const loading = ref(true)
const errMsg = ref('')
const lineIdx = ref(0)

const src = computed<VideoSource | null>(() => {
  const list = detail.value?.sources || []
  return list.find((s) => s.vod_id === vid.value) || list[0] || null
})
const lines = computed<Line[]>(() => src.value?.lines || [])
const line = computed<Line | null>(() => lines.value[lineIdx.value] || null)
const episodes = computed<Episode[]>(() => line.value?.episodes || [])

// 集号从 URL 来；找不到就退回这一线路的第一集
const epIdx = computed(() => {
  const want = Number(route.query.ep)
  if (!Number.isFinite(want) || episodes.value.length === 0) return 0
  const i = episodes.value.findIndex((e) => e.ep_index === want)
  return i >= 0 ? i : 0
})
const cur = computed<Episode | null>(() => episodes.value[epIdx.value] || null)
const prevEp = computed(() => (epIdx.value > 0 ? episodes.value[epIdx.value - 1] : null))
const nextEp = computed(() =>
  epIdx.value + 1 < episodes.value.length ? episodes.value[epIdx.value + 1] : null,
)
const playErr = ref(false)

// 播放地址：交给后端代理（改写 m3u8 里的分片地址）
const playURL = computed(() => (cur.value?.url ? '/api/live?u=' + encodeURIComponent(cur.value.url) : ''))

function epName(ep: Episode): string {
  return ep.ep_name || '第' + (ep.ep_index + 1) + '集'
}

async function load() {
  loading.value = true
  const r = await api.video(vid.value)
  loading.value = false
  if (!r.ok || !r.data) {
    errMsg.value = r.msg || '影片不存在'
    return
  }
  errMsg.value = ''
  detail.value = r.data
  // 按 URL 上的 from 选中线路
  const s = r.data.sources?.find((x) => x.vod_id === vid.value) || r.data.sources?.[0]
  const from = String(route.query.from || '')
  const i = from ? (s?.lines || []).findIndex((l) => l.play_from === from) : -1
  lineIdx.value = i >= 0 ? i : 0
}

function go(i: number) {
  const ep = episodes.value[i]
  if (!ep) return
  playErr.value = false
  router.replace({
    path: `/online/${vid.value}`,
    query: { from: line.value?.play_from || '', ep: String(ep.ep_index) },
  })
}

function pickLine(i: number) {
  const want = cur.value?.ep_index ?? 0
  lineIdx.value = i
  playErr.value = false
  const list = lines.value[i]?.episodes || []
  const j = list.findIndex((e) => e.ep_index === want)
  const ep = list[j >= 0 ? j : 0]
  if (ep) {
    router.replace({
      path: `/online/${vid.value}`,
      query: { from: lines.value[i].play_from, ep: String(ep.ep_index) },
    })
  }
}

// 源站 m3u8 打不开（常见于防盗链：部分源只允许它自己的播放页来拉）
function onPlayError() {
  if (playErr.value) return
  playErr.value = true
  toast('这条线路播放失败，换一条线路试试（或下载到本地看）', 'err', 3000)
}

function onEnded() {
  if (nextEp.value) go(epIdx.value + 1)
  else toast('已经是最后一集了', 'info', 1600)
}

function onKey(e: KeyboardEvent) {
  const el = e.target as HTMLElement | null
  if (el && /input|textarea|select/i.test(el.tagName)) return
  if (e.key === 'ArrowLeft' && prevEp.value) go(epIdx.value - 1)
  else if (e.key === 'ArrowRight' && nextEp.value) go(epIdx.value + 1)
}

onMounted(() => {
  load()
  window.addEventListener('keydown', onKey)
})
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))
watch(playURL, () => { playErr.value = false })
</script>

<template>
  <div class="space-y-4">
    <!-- 顶部 -->
    <section class="flex flex-wrap items-center gap-2">
      <button class="btn btn-ghost btn-sm" @click="router.back()">← 返回</button>
      <div class="min-w-0">
        <div class="truncate text-base font-semibold">{{ detail?.vod_name || '在线播放' }}</div>
        <div class="mt-0.5 flex flex-wrap items-center gap-2 text-[11px] muted">
          <span v-if="cur">{{ epName(cur) }}</span>
          <span v-if="src?.source_name">{{ src.source_name }}</span>
          <span v-if="line">线路 {{ line.play_from }}</span>
          <span v-if="episodes.length">共 {{ episodes.length }} 集</span>
          <span v-if="cur" class="text-amber-600 dark:text-amber-400">在线播放（不占用磁盘）</span>
        </div>
      </div>
      <div class="ml-auto flex items-center gap-1.5">
        <RouterLink v-if="src" :to="`/v/${src.vod_id}`" class="btn btn-ghost btn-sm">影片详情</RouterLink>
      </div>
    </section>

    <!-- 播放器 -->
    <section v-if="cur && playURL" class="card overflow-hidden">
      <VideoPlayer :url="playURL" :hls="true" @ended="onEnded" @error="onPlayError" />
      <div v-if="playErr" class="flex flex-wrap items-center gap-2 border-t border-slate-100 bg-rose-50 px-3 py-2 text-xs
        text-rose-600 dark:border-white/5 dark:bg-rose-500/10 dark:text-rose-300">
        <span>这条线路打不开（源站可能限制了外链播放）。</span>
        <button v-if="lines.length > 1" class="btn btn-ghost btn-sm" @click="pickLine((lineIdx + 1) % lines.length)">
          换下一条线路
        </button>
        <RouterLink v-if="src" :to="`/v/${src.vod_id}`" class="btn btn-ghost btn-sm">去下载</RouterLink>
      </div>
    </section>

    <section v-else-if="!loading" class="card grid place-items-center gap-2 py-16 text-center">
      <div class="text-3xl">📺</div>
      <div class="title">{{ errMsg || (cur ? '这一集没有可用的 m3u8 地址' : '这条线路没有剧集') }}</div>
      <RouterLink v-if="src" :to="`/v/${src.vod_id}`" class="btn btn-ghost btn-sm mt-1">回到详情页</RouterLink>
    </section>

    <!-- 线路 / 集数 -->
    <section v-if="episodes.length" class="card card-pad space-y-3">
      <div v-if="lines.length > 1" class="flex flex-wrap items-center gap-2">
        <span class="text-xs muted">线路</span>
        <button
          v-for="(l, i) in lines" :key="l.play_from"
          class="chip" :class="i === lineIdx ? 'chip-on' : ''" @click="pickLine(i)"
        >{{ l.play_from }}（{{ l.episode_count }}）</button>
      </div>

      <div class="flex flex-wrap items-center gap-2">
        <span class="title">剧集</span>
        <span class="chip">{{ episodes.length }} 集</span>
        <div class="ml-auto flex items-center gap-1.5">
          <button class="btn btn-ghost btn-sm" :disabled="!prevEp" @click="go(epIdx - 1)">上一集</button>
          <button class="btn btn-ghost btn-sm" :disabled="!nextEp" @click="go(epIdx + 1)">下一集</button>
        </div>
      </div>

      <div class="grid grid-cols-3 gap-2 sm:grid-cols-5 md:grid-cols-8 xl:grid-cols-12">
        <button
          v-for="(ep, i) in episodes" :key="ep.ep_index"
          class="truncate rounded-lg border px-2 py-1.5 text-xs transition tap"
          :class="i === epIdx
            ? 'border-amber-400 bg-amber-500/15 font-medium text-amber-700 dark:text-amber-300'
            : 'border-slate-200 hover:border-slate-300 dark:border-white/10 dark:hover:border-white/25'"
          @click="go(i)"
        >{{ epName(ep) }}</button>
      </div>

      <p class="text-[11px] muted">
        在线播放直接读取源站 m3u8（经本机转发），卡顿可以换线路；想稳定观看建议「下载」到本地。
        播放器内按 ← → 也能切集。
      </p>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import CoverImg from '../components/CoverImg.vue'
import QrShareDialog from '../components/QrShareDialog.vue'
import { api } from '../api'
import { copyText } from '../clipboard'
import { toast } from '../toast'
import type { Episode, VideoDetail } from '../types'

const route = useRoute()
const router = useRouter()

const id = computed(() => Number(route.params.id))
const detail = ref<VideoDetail | null>(null)
const loading = ref(true)
const faved = ref(false)
const favBusy = ref(false)
const expanded = ref(false)

const srcIdx = ref(0)
const lineIdx = ref(0)
const selected = ref<Set<number>>(new Set())
const busy = ref(false)
let poll: number | undefined

const curSrc = computed(() => detail.value?.sources?.[srcIdx.value])
const curLine = computed(() => curSrc.value?.lines?.[lineIdx.value])
const episodes = computed<Episode[]>(() => curLine.value?.episodes || [])

const altPics = computed(() => (detail.value?.sources || []).map((s) => s.pic).filter(Boolean))

const statusText: Record<string, string> = {
  queued: '排队中', downloading: '下载中', merging: '合并中', paused: '已暂停', done: '已完成', failed: '失败',
}

function epKey(ep: Episode) {
  return ep.ep_index
}

function isDone(ep: Episode) {
  return ep.download?.status === 'done'
}

function toggle(ep: Episode) {
  if (isDone(ep)) return
  const s = new Set(selected.value)
  if (s.has(epKey(ep))) s.delete(epKey(ep))
  else s.add(epKey(ep))
  selected.value = s
}

async function load(silent = false) {
  if (!silent) loading.value = true
  const r = await api.video(id.value)
  if (!silent) loading.value = false
  if (!r.ok || !r.data) {
    if (!silent) {
      toast(r.msg || '影片不存在', 'err')
      router.replace('/')
    }
    return
  }
  detail.value = r.data
  if (srcIdx.value >= (r.data.sources?.length || 0)) srcIdx.value = 0
  const lines = r.data.sources?.[srcIdx.value]?.lines || []
  if (lineIdx.value >= lines.length) lineIdx.value = 0
  syncPolling()
}

function syncPolling() {
  const active = episodes.value.some((e) => {
    const st = e.download?.status
    return st === 'queued' || st === 'downloading' || st === 'merging'
  })
  if (active && poll === undefined) {
    poll = window.setInterval(() => load(true), 4000)
  } else if (!active && poll !== undefined) {
    window.clearInterval(poll)
    poll = undefined
  }
}

async function loadFav() {
  const r = await api.favs()
  if (r.ok && r.data && detail.value) {
    faved.value = r.data.list.some((f) => f.vod_name === detail.value?.vod_name)
  }
}

async function toggleFav() {
  if (!detail.value || favBusy.value) return
  favBusy.value = true
  const r = faved.value ? await api.delFav(detail.value.vod_name) : await api.addFav(detail.value.vod_name)
  favBusy.value = false
  if (!r.ok) return toast(r.msg || '操作失败', 'err')
  faved.value = !faved.value
  toast(faved.value ? '已收藏' : '已取消收藏', 'ok', 1500)
}

function pickSource(i: number) {
  srcIdx.value = i
  lineIdx.value = 0
  selected.value = new Set()
  syncPolling()
}

function pickLine(i: number) {
  lineIdx.value = i
  selected.value = new Set()
  syncPolling()
}

function selectAll() {
  const all = new Set<number>()
  for (const ep of episodes.value) if (!isDone(ep)) all.add(epKey(ep))
  selected.value = selected.value.size === all.size ? new Set() : all
}

async function doDownload(all: boolean) {
  if (!curSrc.value || !curLine.value || busy.value) return
  const idxs = all ? [] : Array.from(selected.value)
  if (!all && idxs.length === 0) {
    toast('先选要下载的集数', 'info', 1600)
    return
  }
  busy.value = true
  const r = await api.download(curSrc.value.vod_id, curLine.value.play_from, idxs)
  busy.value = false
  if (!r.ok) return toast(r.msg || '加入下载失败', 'err')
  toast(r.data?.msg || '已加入下载队列', 'ok')
  selected.value = new Set()
  load(true)
}

// 播放走独立播放页（下面是同剧已下载集数，可直接切换）
function play(ep: Episode) {
  if (ep.download) router.push(`/watch/${ep.download.id}`)
}

// 在线播放：直接播源站 m3u8（后端会代理改写），不需要先下载
function playOnline(ep: Episode) {
  const sid = curSrc.value?.vod_id
  const from = curLine.value?.play_from
  if (!sid || !from) return
  if (!ep.url) return toast('这一集没有可用的 m3u8 地址', 'err', 1800)
  const q = new URLSearchParams({ from, ep: String(ep.ep_index) })
  router.push(`/online/${sid}?${q.toString()}`)
}

// 头部「在线播放」：从当前线路第一集有地址的开始播
function playFirst() {
  const ep = episodes.value.find((e) => e.url)
  if (!ep) return toast('当前线路没有可播放的地址', 'err', 1800)
  playOnline(ep)
}

// 单集直接下载（不用先勾选）
const epBusy = ref<number | null>(null)
async function downloadOne(ep: Episode) {
  if (!curSrc.value || !curLine.value || isDone(ep)) return
  if (epBusy.value !== null) return
  epBusy.value = ep.ep_index
  const r = await api.download(curSrc.value.vod_id, curLine.value.play_from, [ep.ep_index])
  epBusy.value = null
  if (!r.ok) return toast(r.msg || '加入下载失败', 'err')
  toast('已加入下载队列', 'ok', 1500)
  load(true)
}

// 复制文本：优先 Clipboard API（需 https/localhost），
// 局域网 http 访问时用 execCommand 兜底，否则 NAS 上点复制会静默失败
async function copyURL(ep: Episode) {
  if (!ep.url) return toast('该集没有可用的 m3u8 地址', 'err', 1800)
  const ok = await copyText(ep.url)
  toast(ok ? 'm3u8 链接已复制' : '复制失败，请手动复制', ok ? 'ok' : 'err', 1800)
}

// 详情页二维码分享：地址用后端推断的「外部可访问地址」（本机 localhost 会换成内网 IP）
const shareURL = ref('')
async function sharePage() {
  const r = await api.shareBase()
  const base = (r.ok && r.data?.base_url ? r.data.base_url : location.origin).replace(/\/+$/, '')
  shareURL.value = `${base}/v/${id.value}`
}

watch(id, async () => {
  selected.value = new Set()
  srcIdx.value = 0
  lineIdx.value = 0
  await load()
  await loadFav()
})

load().then(loadFav)
onBeforeUnmount(() => { if (poll !== undefined) window.clearInterval(poll) })
</script>

<template>
  <div v-if="loading" class="space-y-4">
    <div class="card card-pad animate-pulse">
      <div class="flex gap-4">
        <div class="cover-box w-28 shrink-0 rounded-xl bg-slate-200 dark:bg-white/5"></div>
        <div class="flex-1 space-y-2">
          <div class="h-5 w-2/3 rounded bg-slate-200 dark:bg-white/5"></div>
          <div class="h-3 w-1/2 rounded bg-slate-200 dark:bg-white/5"></div>
          <div class="h-3 w-3/4 rounded bg-slate-200 dark:bg-white/5"></div>
        </div>
      </div>
    </div>
  </div>

  <div v-else-if="detail" class="space-y-4">
    <!-- 头部 -->
    <section class="card overflow-hidden">
      <div class="flex flex-col gap-4 p-4 sm:flex-row sm:p-5">
        <div class="mx-auto w-32 shrink-0 overflow-hidden rounded-xl sm:mx-0 sm:w-40">
          <CoverImg :src="detail.vod_pic" :alts="altPics" :alt="detail.vod_name" />
        </div>

        <div class="min-w-0 flex-1">
          <h1 class="text-lg font-semibold sm:text-xl">{{ detail.vod_name }}</h1>
          <div class="mt-2 flex flex-wrap items-center gap-1.5 text-xs">
            <span v-if="detail.vod_score" class="chip border-amber-400/50 text-amber-600 dark:text-amber-300">
              ★ {{ detail.vod_score }}
            </span>
            <span v-if="detail.vod_year" class="chip">{{ detail.vod_year }}</span>
            <span v-if="detail.vod_area" class="chip">{{ detail.vod_area }}</span>
            <span v-if="detail.type_name" class="chip">{{ detail.type_name }}</span>
            <span v-if="detail.vod_remarks" class="chip">{{ detail.vod_remarks }}</span>
            <span v-if="detail.vod_lang" class="chip">{{ detail.vod_lang }}</span>
          </div>

          <p class="mt-3 text-sm leading-6 muted" :class="expanded ? '' : 'line-clamp-3'">
            {{ detail.vod_content || '暂无简介' }}
          </p>
          <button v-if="(detail.vod_content || '').length > 90" class="mt-1 inline-flex min-h-9 items-center px-1 text-xs text-amber-600 dark:text-amber-400" @click="expanded = !expanded">
            {{ expanded ? '收起' : '展开' }}
          </button>

          <div class="mt-3 space-y-1 text-xs muted">
            <div v-if="detail.vod_actor"><span class="opacity-70">主演：</span>{{ detail.vod_actor }}</div>
            <div v-if="detail.vod_director"><span class="opacity-70">导演：</span>{{ detail.vod_director }}</div>
          </div>

          <div class="mt-4 flex flex-wrap gap-2">
            <button class="btn btn-primary btn-sm" @click="playFirst">▶ 在线播放</button>
            <button class="btn btn-sm" :class="faved ? 'btn-primary' : 'btn-ghost'" :disabled="favBusy" @click="toggleFav">
              {{ faved ? '★ 已收藏' : '☆ 收藏' }}
            </button>
            <button class="btn btn-ghost btn-sm" title="生成这一页的二维码，手机扫码直接打开" @click="sharePage">
              <svg viewBox="0 0 24 24" class="mr-1 h-3.5 w-3.5" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><path d="M14 14h3v3h-3zM20 14v3M14 20h3M20 20h1"/></svg>
              分享
            </button>
            <button class="btn btn-ghost btn-sm" @click="load()">刷新</button>
          </div>
        </div>
      </div>
    </section>

    <!-- 来源 / 线路 -->
    <section class="card card-pad space-y-3">
      <div class="flex flex-wrap items-center gap-2">
        <span class="text-xs muted">来源</span>
        <button
          v-for="(s, i) in detail.sources" :key="s.source_id"
          class="chip tap" :class="i === srcIdx ? 'chip-on' : ''" @click="pickSource(i)"
        >{{ s.source_name }}</button>
      </div>

      <div v-if="(curSrc?.lines || []).length > 1" class="flex flex-wrap items-center gap-2">
        <span class="text-xs muted">线路</span>
        <button
          v-for="(l, i) in curSrc?.lines || []" :key="l.play_from"
          class="chip tap" :class="i === lineIdx ? 'chip-on' : ''" @click="pickLine(i)"
        >{{ l.play_from }}（{{ l.episode_count }}）</button>
      </div>

      <div class="flex flex-wrap items-center gap-2 border-t border-slate-100 pt-3 dark:border-white/5">
        <button class="btn btn-ghost btn-sm" @click="selectAll">全选/反选</button>
        <button class="btn btn-primary btn-sm" :disabled="busy || selected.size === 0" @click="doDownload(false)">
          下载选中（{{ selected.size }}）
        </button>
        <button class="btn btn-ghost btn-sm" :disabled="busy" @click="doDownload(true)">下载整条线路</button>
        <span class="ml-auto text-xs muted">每集一个 mp4</span>
      </div>

      <!-- 剧集 -->
      <div class="grid grid-cols-2 gap-2 sm:grid-cols-3 md:grid-cols-5 xl:grid-cols-7">
        <div
          v-for="ep in episodes" :key="ep.ep_index"
          class="relative cursor-pointer rounded-xl border p-2 text-xs transition"
          :class="[
            isDone(ep)
              ? 'border-emerald-400/50 bg-emerald-500/10'
              : selected.has(epKey(ep))
                ? 'border-amber-400 bg-amber-500/15'
                : 'border-slate-200 hover:border-slate-300 dark:border-white/10 dark:hover:border-white/20',
          ]"
          @click="isDone(ep) ? play(ep) : toggle(ep)"
        >
          <div class="truncate text-center font-medium">{{ ep.ep_name || '第' + (ep.ep_index + 1) + '集' }}</div>
          <div class="mt-0.5 truncate text-center text-[11px]" :class="isDone(ep) ? 'text-emerald-600 dark:text-emerald-400' : 'text-slate-400'">
            <template v-if="isDone(ep)">▶ 已下载</template>
            <template v-else-if="ep.download && ep.download.status !== 'failed'">
              {{ statusText[ep.download.status] || ep.download.status }}
              <span v-if="ep.download.status === 'downloading' || ep.download.status === 'merging'">{{ ep.download.percent }}%</span>
            </template>
            <template v-else-if="ep.download?.status === 'failed'">失败（点此重试）</template>
            <template v-else>未下载</template>
          </div>

          <!-- 每集操作：在线播放 + 单集下载/播放 + 复制 m3u8 -->
          <div class="mt-1.5 flex items-stretch gap-1">
            <button
              class="flex-1 rounded-md bg-amber-500/15 px-0.5 py-1 text-[11px] font-medium tap text-xs text-amber-700
                transition hover:bg-amber-500/25 dark:text-amber-300"
              title="直接在线播放这一集（不下载）"
              @click.stop="playOnline(ep)"
            >在线</button>
            <button
              v-if="isDone(ep)"
              class="flex-1 rounded-md bg-emerald-500/15 px-0.5 py-1 text-[11px] font-medium tap text-xs text-emerald-600
                transition hover:bg-emerald-500/25 dark:text-emerald-400"
              title="播放已下载的这一集"
              @click.stop="play(ep)"
            >播放</button>
            <button
              v-else
              class="flex-1 rounded-md bg-slate-100 px-0.5 py-1 text-[11px] font-medium tap text-xs text-slate-700
                transition hover:bg-slate-200 disabled:opacity-50
                dark:bg-white/10 dark:text-slate-200 dark:hover:bg-white/20"
              :disabled="epBusy === ep.ep_index || ep.download?.status === 'downloading' || ep.download?.status === 'merging' || ep.download?.status === 'queued'"
              title="只下载这一集"
              @click.stop="downloadOne(ep)"
            >{{ epBusy === ep.ep_index ? '…' : '下载' }}</button>

            <button
              class="flex-1 rounded-md bg-slate-100 px-0.5 py-1 text-[11px] font-medium tap text-xs text-slate-700
                transition hover:bg-slate-200 dark:bg-white/10 dark:text-slate-200 dark:hover:bg-white/20"
              title="复制这一集的 m3u8 链接"
              @click.stop="copyURL(ep)"
            >复制</button>
          </div>

          <div v-if="ep.download && (ep.download.status === 'downloading' || ep.download.status === 'merging')" class="absolute inset-x-2 bottom-0.5">
            <div class="bar h-1"><i :style="{ width: ep.download.percent + '%' }"></i></div>
          </div>
        </div>
      </div>
      <p v-if="episodes.length === 0" class="py-6 text-center text-sm muted">该来源没有可用剧集</p>
    </section>
  </div>

  <Teleport to="body">
    <QrShareDialog
      v-if="shareURL"
      :url="shareURL"
      :title="detail?.vod_name"
      @close="shareURL = ''"
    />
  </Teleport>

</template>

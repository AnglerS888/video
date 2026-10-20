<script setup lang="ts">
import { onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Pager from '../components/Pager.vue'
import QrShareDialog from '../components/QrShareDialog.vue'
import VideoCard from '../components/VideoCard.vue'
import { api } from '../api'
import { toast } from '../toast'
import type { Filters, VideoItem } from '../types'

const route = useRoute()
const router = useRouter()

const size = 48
const state = reactive({ page: 1, wd: '', year: '', area: '', type: '', onlyDone: false })
const filters = ref<Filters>({ years: [], areas: [], types: [] })
const items = ref<VideoItem[]>([])
const total = ref(0)
const loading = ref(false)
const favSet = ref<Set<string>>(new Set())
let debounce: number | undefined
let seq = 0

function readQuery() {
  const q = route.query
  state.page = Number(q.page || 1) || 1
  state.wd = String(q.wd || '')
  state.year = String(q.year || '')
  state.area = String(q.area || '')
  state.type = String(q.type || '')
  state.onlyDone = q.filter === 'downloaded'
}

function syncQuery() {
  const q: Record<string, string> = {}
  if (state.page > 1) q.page = String(state.page)
  if (state.wd) q.wd = state.wd
  if (state.year) q.year = state.year
  if (state.area) q.area = state.area
  if (state.type) q.type = state.type
  if (state.onlyDone) q.filter = 'downloaded'
  router.replace({ query: q })
}

async function load() {
  const my = ++seq
  loading.value = true
  const r = await api.videos({
    page: state.page, size,
    wd: state.wd.trim(), year: state.year, area: state.area, type: state.type,
    filter: state.onlyDone ? 'downloaded' : '',
  })
  if (my !== seq) return
  loading.value = false
  if (!r.ok || !r.data) {
    toast(r.msg || '加载失败', 'err')
    return
  }
  items.value = r.data.list || []
  total.value = r.data.total
}

async function loadFavs() {
  const r = await api.favs()
  if (r.ok && r.data) favSet.value = new Set(r.data.list.map((f) => f.vod_name))
}

function onSearch() {
  window.clearTimeout(debounce)
  debounce = window.setTimeout(() => {
    state.page = 1
    syncQuery()
    load()
  }, 320)
}

function setFilter(key: 'year' | 'area' | 'type', v: string) {
  state[key] = state[key] === v ? '' : v
  state.page = 1
  syncQuery()
  load()
}

function changePage(p: number) {
  state.page = p
  syncQuery()
  load()
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

function resetAll() {
  state.wd = ''
  state.year = ''
  state.area = ''
  state.type = ''
  state.onlyDone = false
  state.page = 1
  syncQuery()
  load()
}

function onFav(name: string, on: boolean) {
  if (on) favSet.value.add(name)
  else favSet.value.delete(name)
}

// 首页二维码分享：让手机直接打开影视库首页（本机访问时后端会换成内网 IP）
const shareURL = ref('')
async function shareSite() {
  const r = await api.shareBase()
  const base = (r.ok && r.data?.base_url ? r.data.base_url : location.origin).replace(/\/+$/, '')
  shareURL.value = `${base}/`
}

// 地址栏变化（前进/后退）时按 query 重新加载
watch(() => route.fullPath, () => { readQuery(); load() })

onMounted(async () => {
  readQuery()
  await Promise.all([load(), loadFavs()])
  const f = await api.filters()
  if (f.ok && f.data) filters.value = f.data
})
</script>

<template>
  <div class="space-y-3 sm:space-y-4">
    <!-- 搜索与筛选 -->
    <section class="card card-pad">
      <div class="flex flex-col gap-3 lg:flex-row lg:items-center">
        <div class="relative flex-1">
          <svg viewBox="0 0 24 24" class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400" fill="none" stroke="currentColor" stroke-width="2">
            <circle cx="11" cy="11" r="7" /><path d="m20 20-3.5-3.5" />
          </svg>
          <input
            v-model="state.wd" class="input pl-9" type="search" enterkeyhint="search"
            placeholder="搜索片名 / 演员 / 类型" @input="onSearch" @keyup.enter="onSearch"
          />
        </div>
        <div class="flex items-center gap-2">
          <button class="btn btn-ghost btn-sm whitespace-nowrap" :class="state.onlyDone ? 'chip-on' : ''" @click="state.onlyDone = !state.onlyDone; state.page = 1; syncQuery(); load()">
            仅看已下载
          </button>
          <button class="btn btn-ghost btn-sm whitespace-nowrap" @click="resetAll">重置</button>
          <button class="btn btn-ghost btn-sm whitespace-nowrap" title="生成首页二维码，手机扫码直接打开影视库" @click="shareSite">
            <svg viewBox="0 0 24 24" class="mr-1 h-3.5 w-3.5" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><path d="M14 14h3v3h-3zM20 14v3M14 20h3M20 20h1"/></svg>
            分享
          </button>
          <span class="ml-auto whitespace-nowrap text-xs muted lg:ml-0">共 {{ total }} 部</span>
        </div>
      </div>

      <!-- 筛选行：横向滚动，不挤压布局 -->
      <div class="-mx-1 mt-3 flex gap-3 overflow-x-auto px-1 pb-1">
        <div class="min-w-[132px] shrink-0">
          <select class="select py-1.5 text-xs" :value="state.year" @change="setFilter('year', ($event.target as HTMLSelectElement).value)">
            <option value="">全部年份</option>
            <option v-for="y in filters.years" :key="y" :value="y">{{ y }}</option>
          </select>
        </div>
        <div class="min-w-[132px] shrink-0">
          <select class="select py-1.5 text-xs" :value="state.area" @change="setFilter('area', ($event.target as HTMLSelectElement).value)">
            <option value="">全部地区</option>
            <option v-for="a in filters.areas" :key="a" :value="a">{{ a }}</option>
          </select>
        </div>
        <div class="min-w-[132px] shrink-0">
          <select class="select py-1.5 text-xs" :value="state.type" @change="setFilter('type', ($event.target as HTMLSelectElement).value)">
            <option value="">全部类型</option>
            <option v-for="t in filters.types" :key="t" :value="t">{{ t }}</option>
          </select>
        </div>
      </div>
    </section>

    <!-- 列表 -->
    <section>
      <div v-if="loading && items.length === 0" class="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4 xl:grid-cols-5 2xl:grid-cols-6">
        <div v-for="i in 12" :key="i" class="animate-pulse">
          <div class="cover-box rounded-2xl bg-slate-200/80 dark:bg-white/5"></div>
          <div class="mt-2 h-3 w-3/4 rounded bg-slate-200 dark:bg-white/5"></div>
        </div>
      </div>

      <div
        v-else-if="items.length"
        class="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4 xl:grid-cols-5 2xl:grid-cols-6"
      >
        <VideoCard v-for="v in items" :key="v.id + v.vod_name" :item="v" :faved="favSet.has(v.vod_name)" @fav="onFav" />
      </div>

      <div v-else class="card grid place-items-center gap-2 py-16 text-center">
        <div class="text-3xl">🎬</div>
        <div class="title">没有匹配的影片</div>
        <p class="text-sm muted">换个关键词或清空筛选条件试试</p>
        <button class="btn btn-ghost btn-sm mt-1" @click="resetAll">清空条件</button>
      </div>

      <Pager :page="state.page" :size="size" :total="total" @change="changePage" />
    </section>
  </div>

  <Teleport to="body">
    <QrShareDialog v-if="shareURL" :url="shareURL" title="影视库" @close="shareURL = ''" />
  </Teleport>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Pager from '../components/Pager.vue'
import VideoCard from '../components/VideoCard.vue'
import { api } from '../api'
import { toast } from '../toast'
import type { Filters, VideoItem } from '../types'

const route = useRoute()
const router = useRouter()

const size = 48
// field：搜索范围——all=片名+演员+类型，name=只搜片名，actor=只搜演员
const state = reactive({
  page: 1, wd: '', field: 'all' as 'all' | 'name' | 'actor',
  year: '', area: '', type: '', onlyDone: false,
})
const filters = ref<Filters>({ years: [], areas: [], types: [], other: { year: 0, area: 0, type: 0 } })
const items = ref<VideoItem[]>([])
const total = ref(0)
const loading = ref(false)
const favSet = ref<Set<string>>(new Set())
let debounce: number | undefined
let seq = 0

// 「其他」胶囊在 URL 里的哨兵值，和后端 filterOtherSentinel 保持一致。
// 不能用"其他"两个字：库里真的有叫"其它"/"其他"的地区取值，会和正常取值撞车。
const OTHER = '__other__'

// 筛选行：年份 / 地区 / 类型。
// 后端已按「片量最多的前 20 个」排好序并把其余收进 other 桶，这里只负责渲染，
// 并在每行末尾补一个「其他」胶囊（other 为 0 时不显示）。
const filterRows = computed(() => {
  const f = filters.value
  return [
    { key: 'year' as const, label: '年份', options: f.years, other: f.other.year },
    { key: 'area' as const, label: '地区', options: f.areas, other: f.other.area },
    { key: 'type' as const, label: '类型', options: f.types, other: f.other.type },
  ]
})

function readQuery() {
  const q = route.query
  state.page = Number(q.page || 1) || 1
  state.wd = String(q.wd || '')
  state.field = q.field === 'name' || q.field === 'actor' ? q.field : 'all'
  state.year = String(q.year || '')
  state.area = String(q.area || '')
  state.type = String(q.type || '')
  state.onlyDone = q.filter === 'downloaded'
}

function syncQuery() {
  const q: Record<string, string> = {}
  if (state.page > 1) q.page = String(state.page)
  if (state.wd) q.wd = state.wd
  if (state.field !== 'all') q.field = state.field
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
    wd: state.wd.trim(), field: state.field, year: state.year, area: state.area, type: state.type,
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
  state.field = 'all'
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
          <!-- 搜索范围：点这里切换「全部 / 片名 / 演员」 -->
          <div class="absolute left-8 top-1/2 flex -translate-y-1/2 items-center">
            <select
              v-model="state.field"
              class="cursor-pointer appearance-none rounded-md bg-slate-100 py-1 pl-2 pr-6 text-xs font-medium text-slate-600 outline-none
                hover:bg-slate-200 dark:bg-white/10 dark:text-slate-300 dark:hover:bg-white/15"
              aria-label="搜索范围"
              @change="onSearch"
            >
              <option value="all">全部</option>
              <option value="name">片名</option>
              <option value="actor">演员</option>
            </select>
            <svg viewBox="0 0 24 24" class="pointer-events-none absolute right-1.5 h-3 w-3 text-slate-400" fill="none" stroke="currentColor" stroke-width="2.5"><path d="m6 9 6 6 6-6" /></svg>
          </div>
          <input
            v-model="state.wd" class="input pl-[7.5rem]" type="search" enterkeyhint="search"
            :placeholder="state.field === 'actor' ? '搜索演员' : state.field === 'name' ? '搜索片名' : '搜索片名 / 演员 / 类型'"
            @input="onSearch" @keyup.enter="onSearch"
          />
        </div>
        <div class="flex items-center gap-2">
          <button class="btn btn-ghost btn-sm whitespace-nowrap" :class="state.onlyDone ? 'chip-on' : ''" @click="state.onlyDone = !state.onlyDone; state.page = 1; syncQuery(); load()">
            仅看已下载
          </button>
          <button class="btn btn-ghost btn-sm whitespace-nowrap" @click="resetAll">重置</button>
          <span class="ml-auto whitespace-nowrap text-xs muted lg:ml-0">共 {{ total }} 部</span>
        </div>
      </div>

      <!-- 筛选行：年份 / 地区 / 类型。
           每个维度只列片量最多的前 20 个，其余（不规范的取值，如 "英国,美国" 这种
           逗号合成值、脏年份 "22025"、"1988~" 区间值）收进末尾的「其他」胶囊。
           点「其他」= 该列的值不在前 20 名里，所以规范的取值一个都不会漏掉。

           排版：手机/平板/小笔记本（< xl）横向滚动；桌面（xl 起，1280+）自动换行 ——
           电脑上没有触摸横滑手势，横向滚动的话右边的取值根本看不见（2026-10-20 用户反馈）。
           断点选 xl 而不是 lg：横屏平板（1024）也保持横滑，不至于一个地区行撑到几十行。 -->
      <div class="mt-3 space-y-2 border-t border-slate-100 pt-3 dark:border-white/5">
        <div v-for="row in filterRows" :key="row.key" class="flex items-start gap-2">
          <span class="w-8 shrink-0 pt-1 text-[11px] leading-none text-slate-400">{{ row.label }}</span>
          <div
            class="-mx-1 flex min-w-0 flex-1 gap-x-1.5 gap-y-1 overflow-x-auto px-1 py-0.5 no-scrollbar
              xl:flex-wrap xl:overflow-visible"
          >
            <button
              class="chip shrink-0 whitespace-nowrap"
              :class="state[row.key] === '' ? 'chip-on' : ''"
              @click="setFilter(row.key, '')"
            >全部</button>
            <button
              v-for="o in row.options" :key="o.v"
              class="chip shrink-0 whitespace-nowrap"
              :class="state[row.key] === o.v ? 'chip-on' : ''"
              :title="`${o.c} 部`"
              @click="setFilter(row.key, o.v)"
            >{{ o.v }}</button>
            <!-- 「其他」：不规范的取值都在这里，鼠标悬停能看到有多少部 -->
            <button
              v-if="row.other > 0"
              class="chip shrink-0 whitespace-nowrap border-dashed"
              :class="state[row.key] === OTHER ? 'chip-on' : ''"
              :title="`其余 ${row.other} 部（不规范/长尾取值）`"
              @click="setFilter(row.key, OTHER)"
            >其他 {{ row.other }}</button>
          </div>
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
  </Teleport>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import CoverImg from '../components/CoverImg.vue'
import { api } from '../api'
import { confirmDialog } from '../confirm'
import { toast } from '../toast'
import type { FavItem } from '../types'

const list = ref<FavItem[]>([])
const updates = ref(0)
const loading = ref(true)

const sorted = computed(() => [...list.value].sort((a, b) => Number(b.has_update) - Number(a.has_update)))

async function load() {
  loading.value = true
  const r = await api.favs()
  loading.value = false
  if (!r.ok || !r.data) return toast(r.msg || '加载失败', 'err')
  list.value = r.data.list || []
  updates.value = r.data.update_count || 0
}

async function seen(f: FavItem) {
  const r = await api.seenFav(f.vod_name)
  if (!r.ok) return toast(r.msg || '操作失败', 'err')
  load()
}

async function remove(f: FavItem) {
  if (!(await confirmDialog({
    title: '取消收藏',
    message: `取消收藏「${f.vod_name}」？`,
    okText: '取消收藏',
  }))) return
  const r = await api.delFav(f.vod_name)
  if (!r.ok) return toast(r.msg || '操作失败', 'err')
  toast('已取消收藏', 'ok', 1500)
  load()
}

// 关闭自动追更：只清开关，已经加入队列的下载不受影响
async function offAuto(f: FavItem) {
  if (!(await confirmDialog({
    title: '关闭自动追更',
    message: `关闭「${f.vod_name}」在 ${f.auto_play_from} 线路的自动追更？已加入队列的下载不受影响。`,
    okText: '关闭追更',
  }))) return
  const r = await api.setFavAuto(f.vod_name, 0, '', false)
  if (!r.ok) return toast(r.msg || '操作失败', 'err')
  toast('已关闭自动追更', 'ok', 1500)
  load()
}

onMounted(load)
</script>

<template>
  <div class="space-y-4">
    <section class="card card-pad flex flex-wrap items-center gap-2">
      <span class="title">我的收藏</span>
      <span class="chip">{{ list.length }} 部</span>
      <span v-if="updates" class="chip chip-on">有 {{ updates }} 部更新</span>
      <button class="btn btn-ghost btn-sm ml-auto" @click="load">刷新</button>
    </section>

    <div v-if="loading" class="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4 xl:grid-cols-5 2xl:grid-cols-6">
      <div v-for="i in 6" :key="i" class="animate-pulse">
        <div class="cover-box rounded-2xl bg-slate-200/80 dark:bg-white/5"></div>
        <div class="mt-2 h-3 w-3/4 rounded bg-slate-200 dark:bg-white/5"></div>
      </div>
    </div>

    <div v-else-if="!list.length" class="card grid place-items-center gap-2 py-16 text-center">
      <div class="text-3xl">⭐</div>
      <div class="title">还没有收藏</div>
      <p class="text-sm muted">在影视库点封面右上角的星标即可收藏</p>
      <RouterLink to="/" class="btn btn-ghost btn-sm mt-1">去逛逛</RouterLink>
    </div>

    <div v-else class="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4 xl:grid-cols-5 2xl:grid-cols-6">
      <div v-for="f in sorted" :key="f.vod_name" class="group relative overflow-hidden rounded-2xl border border-slate-200 bg-white shadow-sm dark:border-white/10 dark:bg-white/[0.035]">
        <RouterLink :to="f.id ? `/v/${f.id}` : '#'" class="block">
          <div class="relative">
            <CoverImg :src="f.vod_pic" :alt="f.vod_name" />
            <span v-if="f.has_update" class="absolute left-2 top-2 rounded-md bg-rose-500/95 px-1.5 py-0.5 text-[11px] font-medium text-white">
              有更新
            </span>
            <span v-if="f.gone" class="absolute left-2 top-2 rounded-md bg-slate-700/90 px-1.5 py-0.5 text-[11px] text-white">
              已下架
            </span>
            <span
              v-if="f.auto_dl"
              class="absolute right-2 top-2 rounded-md bg-emerald-500/95 px-1.5 py-0.5 text-[11px] font-medium text-white"
              title="已开启自动追更：这条线路更新新集数会自动加入下载"
            >追更{{ f.auto_new_eps > 0 ? ` +${f.auto_new_eps}` : '' }}</span>
          </div>
          <div class="p-2.5">
            <div class="truncate text-sm font-medium">{{ f.vod_name }}</div>
            <div class="mt-1 truncate text-[11px] muted">
              {{ [f.vod_year, f.vod_area, f.type_name].filter(Boolean).join(' · ') }}
            </div>
            <div v-if="f.vod_remarks" class="mt-0.5 truncate text-[11px] text-amber-600 dark:text-amber-400">{{ f.vod_remarks }}</div>
          </div>
        </RouterLink>
        <div v-if="f.auto_dl" class="mx-2.5 mb-1 flex items-center gap-1 rounded-lg bg-emerald-500/10 px-2 py-1 text-[11px] text-emerald-700 dark:text-emerald-300">
          <span class="truncate" :title="`自动追更线路：${f.auto_play_from}`">
            追更 {{ f.auto_play_from || '整部剧' }}<template v-if="f.auto_new_eps > 0"> · {{ f.auto_new_eps }} 集待下载</template>
          </span>
          <button class="ml-auto shrink-0 underline decoration-dotted" @click.stop="offAuto(f)">关闭</button>
        </div>
        <div class="flex items-center gap-1 px-2.5 pb-2.5">
          <button v-if="f.has_update" class="btn btn-soft btn-sm" @click.stop="seen(f)">标记已读</button>
          <button class="btn btn-danger btn-sm ml-auto" @click.stop="remove(f)">取消收藏</button>
        </div>
      </div>
    </div>
  </div>
</template>

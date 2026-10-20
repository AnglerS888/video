<script setup lang="ts">
import { computed, ref } from 'vue'
import CoverImg from './CoverImg.vue'
import { api } from '../api'
import { toast } from '../toast'
import type { VideoItem } from '../types'

const props = defineProps<{ item: VideoItem; faved?: boolean }>()
const emit = defineEmits<{ (e: 'fav', name: string, on: boolean): void }>()

const star = ref(!!props.faved)
const busy = ref(false)

const meta = computed(() => {
  const parts = [props.item.vod_year, props.item.vod_area, props.item.type_name].filter(Boolean)
  return parts.join(' · ')
})

async function toggleFav(e: Event) {
  e.preventDefault()
  e.stopPropagation()
  if (busy.value) return
  busy.value = true
  const r = star.value ? await api.delFav(props.item.vod_name) : await api.addFav(props.item.vod_name)
  busy.value = false
  if (!r.ok) {
    toast(r.msg || '操作失败', 'err')
    return
  }
  star.value = !star.value
  emit('fav', props.item.vod_name, star.value)
  toast(star.value ? '已收藏' : '已取消收藏', 'ok', 1500)
}
</script>

<template>
  <RouterLink
    :to="`/v/${item.id}`"
    class="group block overflow-hidden rounded-2xl border border-slate-200 bg-white shadow-sm
      transition hover:-translate-y-0.5 hover:shadow-md
      dark:border-white/10 dark:bg-white/[0.035] dark:shadow-none dark:hover:border-white/20"
  >
    <div class="relative">
      <CoverImg :src="item.vod_pic" :alts="item.pics" :alt="item.vod_name" />

      <!-- 角标 -->
      <div class="pointer-events-none absolute left-2 top-2 flex flex-wrap gap-1">
        <span v-if="item.downloaded_count > 0" class="rounded-md bg-emerald-500/90 px-1.5 py-0.5 text-[11px] font-medium text-white">
          已下载 {{ item.downloaded_count }}
        </span>
        <span v-else-if="item.source_count > 1" class="rounded-md bg-black/55 px-1.5 py-0.5 text-[11px] text-white">
          {{ item.source_count }} 源
        </span>
      </div>

      <button
        class="absolute right-2 top-2 grid h-11 w-11 place-items-center rounded-full xl:h-9 xl:w-9
          bg-black/45 text-white backdrop-blur transition hover:bg-black/65"
        :class="star ? 'text-amber-300' : 'text-white/85'"
        :title="star ? '取消收藏' : '收藏'"
        @click="toggleFav"
      >
        <svg viewBox="0 0 24 24" class="h-5 w-5 xl:h-4 xl:w-4" :fill="star ? 'currentColor' : 'none'" stroke="currentColor" stroke-width="1.8">
          <path d="m12 3.6 2.6 5.3 5.9.9-4.3 4.1 1 5.8-5.2-2.8-5.2 2.8 1-5.8L3.5 9.8l5.9-.9z" stroke-linejoin="round" />
        </svg>
      </button>

      <div v-if="item.vod_score" class="absolute bottom-2 right-2 rounded-md bg-black/55 px-1.5 py-0.5 text-[11px] font-semibold text-amber-300">
        {{ item.vod_score }}
      </div>
    </div>

    <div class="p-2.5">
      <div class="truncate text-sm font-medium text-slate-900 dark:text-slate-100" :title="item.vod_name">
        {{ item.vod_name }}
      </div>
      <div class="mt-1 flex items-center justify-between gap-2 text-[11px] text-slate-500 dark:text-slate-400">
        <span class="truncate">{{ meta }}</span>
        <span v-if="item.vod_remarks" class="shrink-0 truncate">{{ item.vod_remarks }}</span>
      </div>
    </div>
  </RouterLink>
</template>

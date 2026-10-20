<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import CoverImg from '../components/CoverImg.vue'
import VideoPlayer from '../components/VideoPlayer.vue'
import { api, coverURL } from '../api'
import type { SharePage } from '../types'

/**
 * 分享页 /s/:id（meta.bare，不显示站点导航）
 *
 * 收到的是一条页面链接，而不是视频直链：
 * 页面里给出剧名/集数/封面，用户自己点「播放」或「下载」；
 * 同剧其它已下载的集也能直接切换（换页时只换 URL，不跳回影视库）。
 */
const route = useRoute()
const router = useRouter()

const id = computed(() => Number(route.params.id) || 0)
const info = ref<SharePage | null>(null)
const loading = ref(true)
const errMsg = ref('')
const playing = ref(false)

const poster = computed(() => (info.value?.vod_pic ? coverURL(info.value.vod_pic) : ''))
const sizeText = computed(() => (info.value && info.value.size_mb > 0 ? info.value.size_mb.toFixed(1) + ' MB' : ''))

function epLabel(i: number): string {
  return i >= 0 ? `第${i + 1}集` : '整条'
}

async function load() {
  loading.value = true
  const r = await api.shareInfo(id.value)
  loading.value = false
  if (!r.ok || !r.data) {
    errMsg.value = r.msg || '分享的内容不存在'
    info.value = null
    return
  }
  errMsg.value = ''
  info.value = r.data
  // 只有一集可看时就没必要让用户再点一次
  playing.value = r.data.episodes.length <= 1
}

function goEp(eid: number) {
  if (eid === id.value) return
  playing.value = false
  router.replace(`/s/${eid}`)
}

onMounted(load)
watch(id, () => {
  if (id.value > 0) load()
})
</script>

<template>
  <div class="mx-auto w-full max-w-3xl space-y-3 py-1">
    <!-- 页头：品牌 + 主题提示（分享页不显示站点导航） -->
    <div class="flex items-center gap-2 px-1">
      <span class="grid h-8 w-8 place-items-center rounded-lg bg-amber-500 text-slate-950">
        <svg viewBox="0 0 24 24" class="h-4 w-4" fill="currentColor"><path d="M4 5.5A1.5 1.5 0 0 1 5.5 4h13A1.5 1.5 0 0 1 20 5.5v13A1.5 1.5 0 0 1 18.5 20h-13A1.5 1.5 0 0 1 4 18.5zM10 8.5v7l6-3.5z" /></svg>
      </span>
      <span class="text-sm font-semibold">影视库 · 分享</span>
      <span class="ml-auto text-[11px] muted">局域网内可播放</span>
    </div>

    <div v-if="loading" class="card grid place-items-center gap-3 py-20">
      <div class="h-6 w-6 animate-spin rounded-full border-2 border-amber-500 border-t-transparent"></div>
      <span class="text-sm muted">加载中…</span>
    </div>

    <section v-else-if="errMsg" class="card grid place-items-center gap-2 px-4 py-16 text-center">
      <div class="text-3xl">🔗</div>
      <div class="title">{{ errMsg }}</div>
      <p class="text-sm muted">分享链接可能已失效，或者这一集还没下载完成。</p>
    </section>

    <template v-else-if="info">
      <!-- 影片信息 -->
      <section class="card overflow-hidden">
        <div class="flex gap-3 p-3">
          <div class="w-20 shrink-0 overflow-hidden rounded-xl sm:w-24">
            <CoverImg :src="info.vod_pic" :alt="info.name" />
          </div>
          <div class="min-w-0 flex-1">
            <div class="truncate text-base font-semibold">{{ info.name }}</div>
            <div class="mt-1 flex flex-wrap items-center gap-1.5">
              <span class="chip">{{ info.episode }}</span>
              <span v-if="info.source_name" class="chip">{{ info.source_name }}</span>
              <span v-if="sizeText" class="chip">{{ sizeText }}</span>
            </div>
            <p class="mt-2 text-[11px] muted">
              共 {{ info.episodes.length }} 集已下载，可直接播放或保存到手机/电脑。
            </p>
          </div>
        </div>

        <div class="flex flex-wrap items-center gap-2 border-t border-slate-100 px-3 py-2.5 dark:border-white/5">
          <button class="btn btn-primary btn-sm" @click="playing = true">
            <svg viewBox="0 0 24 24" class="h-4 w-4" fill="currentColor"><path d="M8 5v14l11-7z" /></svg>
            播放
          </button>
          <a class="btn btn-ghost btn-sm" :href="info.file_url">
            <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
              <path d="M12 4v10m0 0 3.5-3.5M12 14l-3.5-3.5M5 18h14" />
            </svg>
            下载到本地
          </a>
          <span class="ml-auto text-[11px] muted">{{ info.play_from }}</span>
        </div>
      </section>

      <!-- 播放器：点击播放后才创建，避免手机上自动播放被拦 -->
      <section v-if="playing" class="card overflow-hidden">
        <VideoPlayer :url="info.play_url" :poster="poster" />
      </section>

      <!-- 同剧其它已下载的集 -->
      <section v-if="info.episodes.length > 1" class="card card-pad space-y-2">
        <div class="flex items-center gap-2">
          <span class="title">本剧其他集</span>
          <span class="chip">{{ info.episodes.length }} 集</span>
        </div>
        <div class="grid grid-cols-3 gap-2 sm:grid-cols-5 md:grid-cols-8">
          <button
            v-for="e in info.episodes" :key="e.id"
            class="truncate rounded-lg border px-2 py-1.5 text-xs transition tap"
            :class="e.id === info.id
              ? 'border-amber-400 bg-amber-500/15 font-medium text-amber-700 dark:text-amber-300'
              : 'border-slate-200 hover:border-slate-300 dark:border-white/10 dark:hover:border-white/25'"
            @click="goEp(e.id)"
          >
            {{ epLabel(e.ep_index) }}
          </button>
        </div>
      </section>

      <p class="px-1 text-center text-[11px] muted">由「影视库」分享 · 仅在同一局域网内可访问</p>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import QRCode from 'qrcode'
import { api } from '../api'
import { toast } from '../toast'
import type { ShareInfo } from '../types'

/**
 * 分享弹窗：生成「分享页」外链 + 二维码。
 *
 * 分享出去的是 /s/{id} 这个页面，不是 /play/{id} 的视频直链：
 * 对方打开能看到剧名/集数/封面，自己决定播放还是下载，同剧其它已下载的集也能直接切；
 * 而且直链一旦发出去就收不回来，页面至少还能在剧被删后给出「内容不存在」的提示。
 */
const props = defineProps<{ id: number }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const info = ref<ShareInfo | null>(null)
const qr = ref('')
const loading = ref(true)

const link = computed(() => info.value?.page_url || '')

function hostOf(u: string): string {
  try {
    return new URL(u).host
  } catch {
    return u
  }
}

async function load() {
  loading.value = true
  const r = await api.share(props.id)
  loading.value = false
  if (!r.ok || !r.data) {
    toast(r.msg || '生成分享链接失败', 'err')
    emit('close')
    return
  }
  info.value = r.data
  try {
    qr.value = await QRCode.toDataURL(r.data.page_url, {
      width: 240,
      margin: 1,
      color: { dark: '#0f172a', light: '#ffffff' },
    })
  } catch {
    qr.value = ''
  }
}

async function copy() {
  const t = link.value
  if (!t) return
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(t)
    } else {
      const ta = document.createElement('textarea')
      ta.value = t
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      document.execCommand('copy')
      document.body.removeChild(ta)
    }
    toast('分享链接已复制', 'ok', 1500)
  } catch {
    toast('复制失败，请手动选中复制', 'err', 2000)
  }
}

onMounted(load)
</script>

<template>
  <div class="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-3 sm:p-6" @click.self="emit('close')">
    <div class="max-h-[calc(100dvh-1.5rem)] w-full max-w-md overflow-y-auto rounded-2xl bg-white shadow-2xl dark:bg-[#121826]">
      <div class="flex items-center gap-2 border-b border-slate-100 px-4 py-3 dark:border-white/10">
        <span class="text-sm font-semibold">分享</span>
        <span v-if="info" class="chip truncate">{{ info.name }} · {{ info.episode }}</span>
        <button class="ml-auto grid h-11 w-11 shrink-0 place-items-center rounded-lg hover:bg-slate-100 xl:h-8 xl:w-8 dark:hover:bg-white/10" @click="emit('close')">
          <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2"><path d="M6 6l12 12M18 6 6 18" /></svg>
        </button>
      </div>

      <div v-if="loading" class="grid place-items-center py-16 text-sm muted">生成中…</div>

      <div v-else-if="info" class="space-y-3 p-4">
        <div class="flex justify-center rounded-xl bg-slate-50 p-3 dark:bg-white/5">
          <img v-if="qr" :src="qr" alt="二维码" class="h-56 w-56 rounded-lg bg-white p-1" />
          <div v-else class="grid h-56 w-56 place-items-center text-xs muted">二维码生成失败，请复制链接</div>
        </div>
        <p class="text-center text-[11px] muted">
          手机扫码打开分享页（需与服务器在同一局域网：{{ hostOf(link) }}）
        </p>

        <div class="flex items-center gap-2">
          <input class="input select-all text-xs" :value="link" readonly />
          <button class="btn btn-primary btn-sm shrink-0" @click="copy">复制</button>
        </div>
        <div class="flex items-center gap-2">
          <a :href="link" target="_blank" rel="noopener" class="btn btn-ghost btn-sm">预览分享页</a>
          <span class="text-[11px] muted">对方打开后可自行播放或下载，不会直接拿到视频文件地址。</span>
        </div>
      </div>
    </div>
  </div>
</template>

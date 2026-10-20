<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import QRCode from 'qrcode'
import { copyText } from '../clipboard'
import { toast } from '../toast'

/**
 * 通用二维码分享弹窗：给任意页面地址生成二维码 + 复制链接。
 * 用于首页（整站）与影片详情页（/v/{id}）；视频文件的分享仍走 ShareDialog（分享页 /s/{id}）。
 */
const props = defineProps<{ url: string; title?: string; tip?: string }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const qr = ref('')
const busy = ref(false)

const host = computed(() => {
  try {
    return new URL(props.url).host
  } catch {
    return props.url
  }
})

async function render() {
  if (!props.url) return
  try {
    qr.value = await QRCode.toDataURL(props.url, {
      width: 240,
      margin: 1,
      color: { dark: '#0f172a', light: '#ffffff' },
    })
  } catch {
    qr.value = ''
  }
}

async function copy() {
  if (busy.value) return
  busy.value = true
  const ok = await copyText(props.url)
  busy.value = false
  toast(ok ? '链接已复制' : '复制失败，请手动选中复制', ok ? 'ok' : 'err', 1600)
}

watch(() => props.url, render, { immediate: true })
</script>

<template>
  <div class="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-3 sm:p-6" @click.self="emit('close')">
    <div class="max-h-[calc(100dvh-1.5rem)] w-full max-w-md overflow-y-auto rounded-2xl bg-white shadow-2xl dark:bg-[#121826]">
      <div class="flex items-center gap-2 border-b border-slate-100 px-4 py-3 dark:border-white/10">
        <span class="text-sm font-semibold">分享</span>
        <span v-if="title" class="chip truncate">{{ title }}</span>
        <button class="ml-auto grid h-11 w-11 shrink-0 place-items-center rounded-lg hover:bg-slate-100 xl:h-8 xl:w-8 dark:hover:bg-white/10" @click="emit('close')">
          <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2"><path d="M6 6l12 12M18 6 6 18" /></svg>
        </button>
      </div>

      <div class="space-y-3 p-4">
        <div class="flex justify-center rounded-xl bg-slate-50 p-3 dark:bg-white/5">
          <img v-if="qr" :src="qr" alt="二维码" class="h-56 w-56 rounded-lg bg-white p-1" />
          <div v-else class="grid h-56 w-56 place-items-center text-xs muted">二维码生成失败，请复制链接</div>
        </div>
        <p class="text-center text-[11px] muted">
          {{ tip || '手机扫码打开（需与服务器在同一局域网：' + host + '）' }}
        </p>

        <div class="flex items-center gap-2">
          <input class="input select-all text-xs" :value="url" readonly @focus="($event.target as HTMLInputElement).select()" />
          <button class="btn btn-primary btn-sm shrink-0" @click="copy">复制</button>
        </div>
        <a :href="url" target="_blank" rel="noopener" class="btn btn-ghost btn-sm">在浏览器中打开</a>
      </div>
    </div>
  </div>
</template>

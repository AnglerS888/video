<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { coverURL } from '../api'

/**
 * 封面：
 *  - 只显示"服务端已成功下载并压缩落盘"的本地图片（/api/img 命中本地文件才 200）
 *  - 加载失败先换同名其它源的封面；都失败就等一会儿重试（单源等源站刷新）
 *  - 全程不显示任何占位图：没图就留空底色
 */
const props = withDefaults(defineProps<{
  src?: string
  alts?: string[]
  alt?: string
  /** 单源/全部失败后的重试次数 */
  retries?: number
}>(), { alts: () => [], alt: '', retries: 5 })

// 只有单个来源（或所有来源都拉不到）时的重试节奏：
// 先快后慢，最后拉到 5 分钟一次，覆盖源站稍后恢复/下一次数据刷新的场景
const DELAYS = [1500, 4000, 10000, 60000, 300000]

const list = computed(() => {
  const out: string[] = []
  if (props.src) out.push(props.src)
  for (const u of props.alts || []) {
    if (u && !out.includes(u)) out.push(u)
  }
  return out
})

const idx = ref(0)
const nonce = ref(0)
const shown = ref(false)
const dead = ref(false)
let retry = 0
let timer: number | undefined

const current = computed(() => list.value[idx.value] || '')

function clearTimer() {
  if (timer !== undefined) {
    window.clearTimeout(timer)
    timer = undefined
  }
}

function reset() {
  clearTimer()
  idx.value = 0
  nonce.value = 0
  shown.value = false
  dead.value = false
  retry = 0
}

function onLoad() {
  shown.value = true
  dead.value = false
  clearTimer()
}

function onError() {
  shown.value = false
  clearTimer()
  // 1) 还有其它源的同名封面：直接换
  if (idx.value + 1 < list.value.length) {
    idx.value += 1
    return
  }
  // 2) 只有这一个源：退避等待后再试（服务端会忽略失败缓存重新拉取）
  if (retry >= props.retries) {
    dead.value = true
    return
  }
  const d = DELAYS[Math.min(retry, DELAYS.length - 1)]
  retry += 1
  timer = window.setTimeout(() => {
    nonce.value = Date.now()
  }, d)
}

watch(() => [props.src, (props.alts || []).join('|')], reset)
onBeforeUnmount(clearTimer)
</script>

<template>
  <div class="cover-box">
    <img
      v-if="current && !dead"
      :key="current + '#' + nonce"
      :src="coverURL(current, nonce)"
      :alt="alt"
      loading="lazy"
      decoding="async"
      referrerpolicy="no-referrer"
      :class="shown ? 'opacity-100' : 'opacity-0'"
      class="transition-opacity duration-300"
      @load="onLoad"
      @error="onError"
    />
  </div>
</template>

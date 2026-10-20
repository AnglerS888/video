<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{ page: number; size: number; total: number }>()
const emit = defineEmits<{ (e: 'change', page: number): void }>()

const pages = computed(() => Math.max(1, Math.ceil(props.total / Math.max(1, props.size))))

const items = computed(() => {
  const n = pages.value
  const cur = props.page
  const out: (number | '...')[] = []
  const push = (v: number | '...') => {
    if (out[out.length - 1] !== v) out.push(v)
  }
  for (let i = 1; i <= n; i++) {
    if (i === 1 || i === n || Math.abs(i - cur) <= 1) push(i)
    else if (Math.abs(i - cur) === 2) push('...')
  }
  return out
})

function go(p: number) {
  if (p < 1 || p > pages.value || p === props.page) return
  emit('change', p)
}
</script>

<template>
  <div v-if="pages > 1" class="flex flex-wrap items-center justify-center gap-1.5 py-4">
    <button class="btn btn-ghost btn-sm" :disabled="page <= 1" @click="go(page - 1)">上一页</button>
    <template v-for="(p, i) in items" :key="i">
      <span v-if="p === '...'" class="px-1 text-slate-400">…</span>
      <button
        v-else
        class="btn btn-sm min-w-10"
        :class="p === page ? 'btn-primary' : 'btn-ghost'"
        @click="go(p as number)"
      >{{ p }}</button>
    </template>
    <button class="btn btn-ghost btn-sm" :disabled="page >= pages" @click="go(page + 1)">下一页</button>
  </div>
</template>

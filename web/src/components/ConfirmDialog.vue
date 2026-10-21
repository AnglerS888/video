<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { answerConfirm, confirmState } from '../confirm'

/**
 * 全局确认弹窗：替代浏览器原生 confirm()。
 * 逻辑在 confirm.ts（confirmDialog），这里只负责渲染与交互。
 */
const cancelBtn = ref<HTMLButtonElement | null>(null)

function onKey(e: KeyboardEvent) {
  if (!confirmState.current) return
  if (e.key === 'Escape') {
    e.preventDefault()
    answerConfirm(false)
  }
}

watch(
  () => confirmState.current,
  async (cur) => {
    if (!cur) {
      document.removeEventListener('keydown', onKey)
      return
    }
    document.addEventListener('keydown', onKey)
    await nextTick()
    // 默认焦点给「取消」：确认键多是删除类不可恢复操作，连按回车不应该误删
    cancelBtn.value?.focus()
  },
)

onBeforeUnmount(() => document.removeEventListener('keydown', onKey))
</script>

<template>
  <Teleport to="body">
    <div
      v-if="confirmState.current"
      class="fixed inset-0 z-[60] flex items-center justify-center bg-black/60 p-4"
      @click.self="answerConfirm(false)"
    >
      <div
        class="max-h-[calc(100dvh-2rem)] w-full max-w-sm overflow-y-auto rounded-2xl bg-white p-4 shadow-2xl dark:bg-[#121826]"
        role="dialog"
        aria-modal="true"
        :aria-label="confirmState.current.title"
      >
        <div class="text-[15px] font-semibold">{{ confirmState.current.title }}</div>
        <p class="mt-2 whitespace-pre-line text-sm leading-relaxed text-slate-600 dark:text-slate-300">
          {{ confirmState.current.message }}
        </p>
        <div class="mt-4 flex items-center justify-end gap-2">
          <button ref="cancelBtn" class="btn btn-ghost min-w-[5.5rem]" @click="answerConfirm(false)">
            {{ confirmState.current.cancelText }}
          </button>
          <button
            class="btn min-w-[5.5rem]"
            :class="confirmState.current.danger ? 'btn-danger' : 'btn-primary'"
            @click="answerConfirm(true)"
          >
            {{ confirmState.current.okText }}
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>

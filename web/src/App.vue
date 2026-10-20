<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { isDark, toggleTheme } from './theme'
import { toasts } from './toast'

const route = useRoute()

const nav = [
  { to: '/', label: '影视库', icon: 'M3 5.5A1.5 1.5 0 0 1 4.5 4h15A1.5 1.5 0 0 1 21 5.5v13A1.5 1.5 0 0 1 19.5 20h-15A1.5 1.5 0 0 1 3 18.5zM10 9l5 3-5 3z' },
  { to: '/local', label: '本地', icon: 'M4 6.5A2.5 2.5 0 0 1 6.5 4h11A2.5 2.5 0 0 1 20 6.5v11a2.5 2.5 0 0 1-2.5 2.5h-11A2.5 2.5 0 0 1 4 17.5zM4 10h16M9 15h6' },
  { to: '/downloads', label: '下载', icon: 'M12 4v10m0 0 3.5-3.5M12 14l-3.5-3.5M5 18h14' },
  { to: '/history', label: '历史', icon: 'M12 7v5l3 2M3.5 12a8.5 8.5 0 1 0 2.6-6.1M3 4v4h4' },
  { to: '/favorites', label: '收藏', icon: 'm12 3.6 2.6 5.3 5.9.9-4.3 4.1 1 5.8-5.2-2.8-5.2 2.8 1-5.8L3.5 9.8l5.9-.9z' },
  { to: '/sources', label: '接口', icon: 'M4 7h16M4 12h10M4 17h7' },
  { to: '/info', label: '信息', icon: 'M12 8h.01M11 12h1v5h1M12 21a9 9 0 1 1 0-18 9 9 0 0 1 0 18z' },
]

const title = computed(() => (route.meta.title as string) || '影视库')
const isDetail = computed(() => route.name === 'detail')
// 分享页（/s/:id）独立成页：不显示站点导航，避免收到链接的人被带进后台
const bare = computed(() => route.meta.bare === true)
</script>

<template>
  <div class="flex min-h-dvh flex-col">
    <!-- 顶部导航：大屏横向 / 小屏标题栏；分享页不显示 -->
    <header
      v-if="!bare"
      class="safe-top sticky top-0 z-30 border-b border-slate-200 bg-white/80 backdrop-blur
        dark:border-white/10 dark:bg-[#0b0f17]/85"
    >
      <!-- 小屏：标题栏（返回 / logo / 标题 / 主题切换） -->
      <div class="flex h-14 items-center gap-2 px-3 lg:hidden">
        <RouterLink v-if="isDetail" to="/" class="btn btn-ghost btn-sm -ml-1 shrink-0">
          <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2"><path d="M15 6l-6 6 6 6" /></svg>
          返回
        </RouterLink>
        <RouterLink v-else to="/" class="grid h-11 w-11 shrink-0 place-items-center" aria-label="回到影视库首页">
          <span class="grid h-8 w-8 place-items-center rounded-lg bg-amber-500 text-slate-950">
            <svg viewBox="0 0 24 24" class="h-4 w-4" fill="currentColor"><path d="M4 5.5A1.5 1.5 0 0 1 5.5 4h13A1.5 1.5 0 0 1 20 5.5v13a1.5 1.5 0 0 1-1.5 1.5h-13A1.5 1.5 0 0 1 4 18.5zM10 8.5v7l6-3.5z" /></svg>
          </span>
        </RouterLink>
        <div class="min-w-0 flex-1 truncate text-[15px] font-semibold">{{ title }}</div>
        <button
          class="btn btn-ghost btn-icon ml-auto"
          :title="isDark ? '切换到浅色' : '切换到深色'"
          :aria-label="isDark ? '切换到浅色' : '切换到深色'"
          @click="toggleTheme"
        >
          <svg v-if="isDark" viewBox="0 0 24 24" class="h-5 w-5" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round">
            <circle cx="12" cy="12" r="4" />
            <path d="M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
          </svg>
          <svg v-else viewBox="0 0 24 24" class="h-5 w-5" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
            <path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z" />
          </svg>
        </button>
      </div>

      <!-- 大屏：logo + 横向菜单 -->
      <div class="mx-auto hidden h-16 max-w-[1680px] items-center gap-6 px-6 lg:flex">
        <RouterLink to="/" class="tap flex shrink-0 items-center gap-2">
          <span class="grid h-9 w-9 place-items-center rounded-xl bg-amber-500 text-slate-950">
            <svg viewBox="0 0 24 24" class="h-5 w-5" fill="currentColor"><path d="M4 5.5A1.5 1.5 0 0 1 5.5 4h13A1.5 1.5 0 0 1 20 5.5v13a1.5 1.5 0 0 1-1.5 1.5h-13A1.5 1.5 0 0 1 4 18.5zM10 8.5v7l6-3.5z" /></svg>
          </span>
          <span class="text-[16px] font-semibold tracking-wide">影视库</span>
        </RouterLink>

        <nav class="flex flex-1 items-center gap-1">
          <RouterLink
            v-for="n in nav" :key="n.to" :to="n.to"
            class="tap flex items-center gap-2 rounded-xl px-3.5 py-2 text-sm text-slate-600 transition
              hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-white/5"
            active-class="bg-amber-500/12! text-amber-600! dark:text-amber-300! font-medium"
          >
            <svg viewBox="0 0 24 24" class="h-[18px] w-[18px]" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">
              <path :d="n.icon" />
            </svg>
            {{ n.label }}
          </RouterLink>
        </nav>

        <div class="flex shrink-0 items-center gap-3">
          <button
            class="btn btn-ghost btn-icon"
            :title="isDark ? '切换到浅色' : '切换到深色'"
            :aria-label="isDark ? '切换到浅色' : '切换到深色'"
            @click="toggleTheme"
          >
            <svg v-if="isDark" viewBox="0 0 24 24" class="h-[18px] w-[18px]" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round">
              <circle cx="12" cy="12" r="4" />
              <path d="M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
            </svg>
            <svg v-else viewBox="0 0 24 24" class="h-[18px] w-[18px]" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
              <path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z" />
            </svg>
          </button>
          <span class="text-[11px] text-slate-400">v1.0</span>
        </div>
      </div>
    </header>

    <main
      class="mx-auto w-full max-w-[1680px] flex-1 px-3 sm:px-4 lg:px-6"
      :class="bare ? 'py-3 lg:py-6' : 'pb-28 pt-3 lg:pb-8 lg:pt-5'"
    >
      <RouterView />
    </main>

    <!-- 底部导航（手机 / 平板）；分享页不显示。
         7 项在 360px 宽下每项约 51px，图标 20px + 11px 文字，整项高 ≥56px -->
    <nav
      v-if="!bare"
      class="safe-bottom fixed inset-x-0 bottom-0 z-30 grid grid-cols-7 border-t border-slate-200
        bg-white/90 backdrop-blur lg:hidden dark:border-white/10 dark:bg-[#0b0f17]/92"
    >
      <RouterLink
        v-for="n in nav" :key="n.to" :to="n.to"
        class="flex min-h-14 flex-col items-center justify-center gap-0.5 px-0.5 pt-1.5 text-[11px] text-slate-500 dark:text-slate-400"
        active-class="text-amber-500!"
        :aria-label="n.label"
      >
        <svg viewBox="0 0 24 24" class="h-5 w-5" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">
          <path :d="n.icon" />
        </svg>
        {{ n.label }}
      </RouterLink>
    </nav>

    <!-- 轻提示：手机放在标题栏下方（不再盖住页面标题），大屏放右上角 -->
    <div class="pointer-events-none fixed inset-x-0 top-16 z-50 flex flex-col items-center gap-2 px-3
      lg:left-auto lg:right-6 lg:top-20 lg:items-end">
      <TransitionGroup name="fade">
        <div
          v-for="t in toasts.items" :key="t.id"
          class="pointer-events-auto max-w-[92vw] rounded-xl px-3.5 py-2 text-sm shadow-lg backdrop-blur
            lg:max-w-sm"
          :class="t.type === 'err'
            ? 'bg-rose-500/95 text-white'
            : t.type === 'ok' ? 'bg-emerald-500/95 text-white' : 'bg-slate-900/90 text-white dark:bg-white/90 dark:text-slate-900'"
        >
          {{ t.text }}
        </div>
      </TransitionGroup>
    </div>
  </div>
</template>

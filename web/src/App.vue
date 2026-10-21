<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api } from './api'
import QrShareDialog from './components/QrShareDialog.vue'
import { isDark, toggleTheme } from './theme'
import { toasts } from './toast'

const route = useRoute()

// 顶部 / 底部导航共用这一份。数组加减项时，底部 tab 的 grid-cols-N 必须同步改，
// 否则多的那项会被挤到第二行（见模板里的注释）。
// 「接口」(/api-docs) 是站点自身的接口文档，不属于日常导航，放在页面底部超链接。
const nav = [
  { to: '/', label: '影视库', icon: 'M3 5.5A1.5 1.5 0 0 1 4.5 4h15A1.5 1.5 0 0 1 21 5.5v13A1.5 1.5 0 0 1 19.5 20h-15A1.5 1.5 0 0 1 3 18.5zM10 9l5 3-5 3z' },
  { to: '/local', label: '本地', icon: 'M4 6.5A2.5 2.5 0 0 1 6.5 4h11A2.5 2.5 0 0 1 20 6.5v11a2.5 2.5 0 0 1-2.5 2.5h-11A2.5 2.5 0 0 1 4 17.5zM4 10h16M9 15h6' },
  { to: '/downloads', label: '下载', icon: 'M12 4v10m0 0 3.5-3.5M12 14l-3.5-3.5M5 18h14' },
  { to: '/history', label: '历史', icon: 'M12 7v5l3 2M3.5 12a8.5 8.5 0 1 0 2.6-6.1M3 4v4h4' },
  { to: '/favorites', label: '收藏', icon: 'm12 3.6 2.6 5.3 5.9.9-4.3 4.1 1 5.8-5.2-2.8-5.2 2.8 1-5.8L3.5 9.8l5.9-.9z' },
  { to: '/sources', label: '来源', icon: 'M4 7h16M4 12h10M4 17h7' },
  { to: '/info', label: '信息', icon: 'M12 8h.01M11 12h1v5h1M12 21a9 9 0 1 1 0-18 9 9 0 0 1 0 18z' },
]

const title = computed(() => (route.meta.title as string) || '影视库')
const isDetail = computed(() => route.name === 'detail')
// 分享页（/s/:id）独立成页：不显示站点导航，避免收到链接的人被带进后台
const bare = computed(() => route.meta.bare === true)
// 首页二维码分享：放在导航栏右侧（只在首页显示）
const isHome = computed(() => route.path === '/')
const shareURL = ref('')
async function shareSite() {
  const r = await api.shareBase()
  const base = (r.ok && r.data?.base_url ? r.data.base_url : location.origin).replace(/\/+$/, '')
  shareURL.value = `${base}/`
}

// 版本号：顶栏右下角显示真实版本（来自 /api/version，取不到就不显示）
const appVersion = ref('')
onMounted(async () => {
  const r = await api.version()
  const v = r.ok ? r.data?.info?.current_version : ''
  if (v) appVersion.value = 'v' + v
})
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
          v-if="isHome"
          class="btn btn-ghost btn-icon ml-auto"
          title="生成首页二维码，手机扫码直接打开影视库"
          aria-label="分享影视库"
          @click="shareSite"
        >
          <svg viewBox="0 0 24 24" class="h-5 w-5" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
            <rect x="3" y="3" width="7" height="7" rx="1" /><rect x="14" y="3" width="7" height="7" rx="1" /><rect x="3" y="14" width="7" height="7" rx="1" /><path d="M14 14h3v3h-3zM20 14v3M14 20h3M20 20h1" />
          </svg>
        </button>
        <button
          class="btn btn-ghost btn-icon"
          :class="isHome ? '' : 'ml-auto'"
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
            v-if="isHome"
            class="btn btn-ghost btn-icon"
            title="生成首页二维码，手机扫码直接打开影视库"
            aria-label="分享影视库"
            @click="shareSite"
          >
            <svg viewBox="0 0 24 24" class="h-[18px] w-[18px]" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="3" width="7" height="7" rx="1" /><rect x="14" y="3" width="7" height="7" rx="1" /><rect x="3" y="14" width="7" height="7" rx="1" /><path d="M14 14h3v3h-3zM20 14v3M14 20h3M20 20h1" />
            </svg>
          </button>
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
          <span v-if="appVersion" class="text-[11px] text-slate-400">{{ appVersion }}</span>
        </div>
      </div>
    </header>

    <QrShareDialog v-if="shareURL" :url="shareURL" title="影视库" @close="shareURL = ''" />

    <main
      class="mx-auto w-full max-w-[1680px] flex-1 px-3 sm:px-4 lg:px-6"
      :class="bare ? 'py-3 lg:py-6' : 'pb-2 pt-3 lg:pb-3 lg:pt-5'"
    >
      <RouterView />
    </main>

    <!-- 页脚：放「接口文档」超链接（原本是导航栏第 8 项，用户要求挪到这里）。
         分享页（bare）不显示。

         下边距由页脚负责（pb-28），不是 main —— main 原来自己带 pb-28 是为了
         避开 fixed 底部导航，现在页脚是最后一个元素，让它来避让才不会
         出现 112+112px 两段空白。桌面端没有固定导航，用正常的 pb-8。

         链接高度：min-h-8 不能只靠 tap-sm —— .tap-sm 的 36px 只在
         `@media (pointer: coarse), (max-width:639px)` 下生效，
         桌面端鼠标环境下不触发，实测只有 16px 高（纯文字行高），太难点。 -->
    <footer
      v-if="!bare"
      class="mx-auto w-full max-w-[1680px] px-3 text-center text-xs muted sm:px-4 lg:px-6
        pb-28 pt-2 lg:pb-8"
    >
      <RouterLink
        to="/api-docs"
        class="tap-sm inline-flex min-h-8 items-center gap-1.5 rounded-lg px-2.5 hover:text-amber-500"
        active-class="text-amber-500!"
      >
        <svg viewBox="0 0 24 24" class="h-3.5 w-3.5 shrink-0" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <path d="M9 4 4 12l5 8M15 4l5 8-5 8" />
        </svg>
        接口文档
      </RouterLink>
      <span v-if="appVersion" class="mx-1">·</span>
      <span v-if="appVersion">{{ appVersion }}</span>
    </footer>

    <!-- 底部导航（手机 / 平板）；分享页不显示。
         一共 7 项（影视库/本地/下载/历史/收藏/来源/信息），
         所以是 grid-cols-7：nav 加减项必须同步改这里，
         否则多的那项换到第二行、少的留下空位。
         360px 宽下每项 51px，图标 18px + 10px 文字，整项高 ≥56px -->
    <nav
      v-if="!bare"
      class="safe-bottom fixed inset-x-0 bottom-0 z-30 grid grid-cols-7 border-t border-slate-200
        bg-white/90 backdrop-blur lg:hidden dark:border-white/10 dark:bg-[#0b0f17]/92"
    >
      <RouterLink
        v-for="n in nav" :key="n.to" :to="n.to"
        class="flex min-h-14 min-w-0 flex-col items-center justify-center gap-1 px-0.5 pt-1.5 text-[10px]
          leading-none text-slate-500 dark:text-slate-400"
        active-class="text-amber-500!"
        :aria-label="n.label"
      >
        <svg viewBox="0 0 24 24" class="h-[18px] w-[18px] shrink-0" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">
          <path :d="n.icon" />
        </svg>
        <span class="w-full truncate text-center">{{ n.label }}</span>
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

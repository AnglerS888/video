import { ref } from 'vue'

const KEY = 'vd_theme'
const saved = localStorage.getItem(KEY)

// 默认浅色（用户要求）；只在用户手动切换过之后才读 localStorage
export const isDark = ref(saved ? saved === 'dark' : false)

function apply() {
  document.documentElement.classList.toggle('dark', isDark.value)
  const meta = document.querySelector('meta[name="theme-color"]')
  meta?.setAttribute('content', isDark.value ? '#0b0f17' : '#f1f5f9')
}

export function toggleTheme() {
  isDark.value = !isDark.value
  localStorage.setItem(KEY, isDark.value ? 'dark' : 'light')
  apply()
}

apply()

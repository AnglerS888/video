<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import DPlayer from 'dplayer'

/**
 * 内嵌播放器（DPlayer）。
 * - 本地播放：url 是 /play/{下载id}，服务端已产出 mp4
 * - 在线播放：url 是 /api/live?u=...（HLS 代理），此时要传 hls，走 hls.js
 *
 * - startAt：从上次观看位置继续（秒）
 * - 每 5 秒 emit 一次 time(position, duration)，由播放页负责上报
 * - 换集时整体重建实例：DPlayer 不支持运行时换源
 */
// 注意：Vue 对 Boolean prop 的默认转换是 false —— 不显式给默认值的话，
// 调用方不传 autoplay 时这里拿到的是 false，换集后就不会自动起播。
const props = withDefaults(
  defineProps<{ url: string; autoplay?: boolean; startAt?: number; hls?: boolean; poster?: string }>(),
  { autoplay: true },
)
const emit = defineEmits<{
  (e: 'ended'): void
  (e: 'time', position: number, duration: number): void
  (e: 'error'): void
}>()

const box = ref<HTMLElement | null>(null)
let dp: any = null
let hlsInst: any = null // customType 路径下 DPlayer 不管销毁，自己记着
let tick: number | undefined
let kickTimer: number | undefined
let seq = 0 // 换源竞态：动态加载 hls.js 期间可能已经换了下一集

/**
 * 仿 iOS 播放器：点屏幕左侧快退 15 秒、右侧快进 15 秒，中间仍是点击暂停/播放。
 * 只在容器上挂捕获阶段的 click：两侧 stopPropagation，中间放行给 DPlayer 自己的 toggle。
 * 底部控制条 / 设置菜单 / 提示条上的点击不参与。
 *
 * 防误触 + 长按倍速（pointer 状态机，click 只做最终裁决）：
 * - 按下后移动超过容差（滑动/拖拽）→ 本次点击作废，不 seek 也不 toggle；
 * - 按住右侧 ≥450ms → 进入 2 倍速（提示 ⏩ 2倍速），松手恢复原倍速，
 *   且这次的 click 被吞掉（长按不算快进）；
 * - 长按只放右侧：左键长按松手仍按普通点击快退，行为可预期。
 */
const SEEK_STEP = 15
const LONG_PRESS_MS = 450
const MOVE_TOLERANCE = 12 // px，超过视为滑动
let tipEl: HTMLDivElement | null = null
let tipTimer: number | undefined

function showSeekTip(dir: 'back' | 'fwd', label = '15 秒', hold = false) {
  if (!tipEl) return
  tipEl.dataset.dir = dir
  const lab = tipEl.querySelector('.vdl-seek-label')
  if (lab) lab.textContent = label
  tipEl.classList.add('vdl-show')
  if (tipTimer) {
    window.clearTimeout(tipTimer)
    tipTimer = undefined
  }
  if (!hold) tipTimer = window.setTimeout(hideSeekTip, 700)
}

function hideSeekTip() {
  tipEl?.classList.remove('vdl-show')
  if (tipTimer) {
    window.clearTimeout(tipTimer)
    tipTimer = undefined
  }
}

// ---- 长按 2 倍速 ----
let boosting = false
let boostPrevRate = 1

function enterBoost() {
  const v = dp?.video as HTMLVideoElement | undefined
  if (!v || boosting || !isFinite(v.duration) || v.duration <= 0) return
  boosting = true
  boostPrevRate = v.playbackRate || 1
  try {
    v.playbackRate = 2
  } catch {
    boosting = false
    return
  }
  showSeekTip('fwd', '2 倍速', true)
}

function exitBoost() {
  if (!boosting) return
  boosting = false
  const v = dp?.video as HTMLVideoElement | undefined
  if (v) {
    try {
      v.playbackRate = boostPrevRate
    } catch {
      /* ignore */
    }
  }
  hideSeekTip()
}

// ---- 按压状态（pointerdown 在容器上，move/up/cancel 挂 window 兜底拖出播放器的情况）----
const press = { active: false, x: 0, y: 0, moved: false, suppress: false, longFired: false, timer: 0 as number | undefined }

function pressZone(e: PointerEvent): '' | 'back' | 'fwd' {
  const host = box.value
  if (!host) return ''
  const rect = host.getBoundingClientRect()
  if (!rect.width) return ''
  const x = (e.clientX - rect.left) / rect.width
  if (x <= 0.3) return 'back'
  if (x >= 0.7) return 'fwd'
  return ''
}

function onPointerDown(e: PointerEvent) {
  if (e.pointerType === 'mouse' && e.button !== 0) return
  const t = e.target as HTMLElement | null
  if (t && t.closest('.dplayer-controller, .dplayer-menu, .dplayer-setting, .dplayer-notice, .dplayer-bezel')) return
  press.active = true
  press.x = e.clientX
  press.y = e.clientY
  press.moved = false
  press.suppress = false
  press.longFired = false
  // 只有右侧支持长按倍速（左侧长按松手按普通快退处理）
  if (pressZone(e) !== 'fwd') return
  const v = dp?.video as HTMLVideoElement | undefined
  if (!v || !isFinite(v.duration) || v.duration <= 0) return
  press.timer = window.setTimeout(() => {
    press.timer = undefined
    if (!press.active || press.moved) return
    press.longFired = true
    enterBoost()
  }, LONG_PRESS_MS)
}

function onPointerMove(e: PointerEvent) {
  if (!press.active || press.moved) return
  if (Math.abs(e.clientX - press.x) > MOVE_TOLERANCE || Math.abs(e.clientY - press.y) > MOVE_TOLERANCE) {
    press.moved = true
    if (press.timer) {
      window.clearTimeout(press.timer)
      press.timer = undefined
    }
  }
}

function onPointerUp() {
  if (!press.active) return
  press.active = false
  if (press.timer) {
    window.clearTimeout(press.timer)
    press.timer = undefined
  }
  if (press.longFired) {
    exitBoost()
    press.suppress = true // 长按结束的那次 click 不算快进
  }
}

function onPointerCancel() {
  press.active = false
  if (press.timer) {
    window.clearTimeout(press.timer)
    press.timer = undefined
  }
  if (press.longFired) exitBoost()
}

/**
 * 右键菜单：DPlayer 1.27 会强制往菜单里塞「视频统计信息 / 关于作者 / 版本」三项，
 * 传 contextmenu:[] 也关不掉（内部 concat 了默认项），浏览器还会叠一层原生菜单。
 * 这里在 document 捕获阶段先吃掉：只拦播放器内部的右键，页面别处不受影响。
 */
function onContextMenu(e: MouseEvent) {
  const host = box.value
  const t = e.target as Node | null
  if (!host || !t || !host.contains(t)) return
  e.preventDefault()
  e.stopPropagation()
}

function onContainerClick(e: MouseEvent) {
  const host = box.value
  const v = dp?.video as HTMLVideoElement | undefined
  if (!host || !v) return
  // 滑动/长按结束时的 click 一律吞掉：既不 seek 也不让 DPlayer toggle
  if (press.suppress || press.moved || press.longFired) {
    press.suppress = false
    press.moved = false
    press.longFired = false
    e.stopPropagation()
    e.preventDefault()
    return
  }
  // 控制条/菜单/提示上的点击交给 DPlayer
  const t = e.target as HTMLElement | null
  if (t && t.closest('.dplayer-controller, .dplayer-menu, .dplayer-setting, .dplayer-notice, .dplayer-bezel')) return

  const rect = host.getBoundingClientRect()
  if (!rect.width) return
  const x = (e.clientX - rect.left) / rect.width
  if (x > 0.3 && x < 0.7) return // 中间不拦截：DPlayer 自己 toggle

  e.stopPropagation()
  e.preventDefault()
  // 还没拿到时长（未起播/加载失败）就别动进度，避免误触把画面弄黑
  if (!isFinite(v.duration) || v.duration <= 0) return
  const delta = x <= 0.3 ? -SEEK_STEP : SEEK_STEP
  const to = Math.min(Math.max(0, (v.currentTime || 0) + delta), Math.max(0, v.duration - 0.1))
  try {
    v.currentTime = to
  } catch {
    /* ignore */
  }
  showSeekTip(delta < 0 ? 'back' : 'fwd')
}

/**
 * 倍速记忆：DPlayer 每次重建实例都会回到 1×，
 * 这里把用户选的倍速存进 localStorage，新实例起播后立刻套用。
 * 只记「播放速度」这一个全局偏好（不按剧分），符合大多数播放器的行为。
 */
const RATE_KEY = 'vdl.playback-rate'
const RATES = [0.5, 0.75, 1, 1.25, 1.5, 1.75, 2, 2.5, 3]
let rateVideo: HTMLVideoElement | null = null
let rateListener: (() => void) | null = null

function readRate(): number {
  try {
    const v = Number(localStorage.getItem(RATE_KEY))
    if (Number.isFinite(v) && v >= 0.5 && v <= 3) return v
  } catch {
    /* 隐私模式/禁用存储：退回 1× */
  }
  return 1
}

function writeRate(v: number) {
  if (!Number.isFinite(v) || v < 0.5 || v > 3) return
  try {
    localStorage.setItem(RATE_KEY, String(v))
  } catch {
    /* ignore */
  }
}

/** 把记住的倍速应用到当前视频元素（DPlayer 的 speed() 只是改 playbackRate） */
function applyRate() {
  const v = dp?.video as HTMLVideoElement | undefined
  const r = readRate()
  if (!v || r === 1) return
  if (Math.abs((v.playbackRate || 1) - r) > 0.001) {
    try {
      dp.speed(r)
    } catch {
      try {
        v.playbackRate = r
      } catch {
        /* ignore */
      }
    }
  }
}

/** 用户拖动倍速菜单时保存；原生 ratechange 是唯一能覆盖 DPlayer 内部按钮的入口。
 *  长按 2 倍速是临时状态，不能写进 localStorage（boosting 期间跳过）。 */
function watchRate(v: HTMLVideoElement) {
  if (rateVideo === v) return
  if (rateVideo && rateListener) rateVideo.removeEventListener('ratechange', rateListener)
  rateVideo = v
  rateListener = () => {
    if (boosting) return
    writeRate(v.playbackRate)
  }
  v.addEventListener('ratechange', rateListener)
}

function unwatchRate() {
  if (rateVideo && rateListener) rateVideo.removeEventListener('ratechange', rateListener)
  rateVideo = null
  rateListener = null
}

function report() {
  const v = dp?.video as HTMLVideoElement | undefined
  if (!v || !isFinite(v.duration) || v.duration <= 0) return
  emit('time', v.currentTime || 0, v.duration)
}

function destroy() {
  // 先摘引用再销毁：销毁旧实例时视频元素会触发 error（MSE 被拆、blob 被回收），
  // 那个 error 属于旧播放器，不能算到新一集头上（换集误报「线路失败」的根因）
  unwatchRate()
  tipEl = null
  boosting = false
  press.active = false
  if (press.timer) {
    window.clearTimeout(press.timer)
    press.timer = undefined
  }
  const old = dp
  dp = null
  if (kickTimer) {
    window.clearInterval(kickTimer)
    kickTimer = undefined
  }
  try {
    hlsInst?.destroy?.()
  } catch {
    /* ignore */
  }
  hlsInst = null
  try {
    old?.destroy?.()
  } catch {
    /* ignore */
  }
}

async function create() {
  if (!box.value || !props.url) return
  const mine = ++seq
  destroy()
  // DPlayer 的 type='hls' 用的是 window.Hls：这里按需加载，
  // 本地 mp4 播放就不用白白背上 hls.js 那几百 KB
  if (props.hls) {
    const mod: any = await import('hls.js')
    if (mine !== seq) return
    ;(window as any).Hls = mod.default || mod
  }
  if (mine !== seq) return
  // 注意：不能用 DPlayer 内置的 type:'hls' —— 它发现浏览器自称能原生播 m3u8
  // （新版 Chrome/Edge 会返回 "maybe"）就降级成 video.src 直连，结果既不走 hls.js
  // 也解不了伪装分片。用 customType 绕过这个判断，强制走 hls.js；
  // 仅在真没有 MSE 的环境（iOS Safari）退回原生播放。
  const hlsConfig = {
    // 源站分片偶发超时：放宽重试与缓冲，避免一卡就整段黑屏
    enableWorker: true,
    lowLatencyMode: false,
    manifestLoadingTimeOut: 15000,
    manifestLoadingMaxRetry: 3,
    levelLoadingTimeOut: 15000,
    fragLoadingTimeOut: 30000,
    fragLoadingMaxRetry: 4,
  }
  dp = new DPlayer({
    container: box.value,
    autoplay: props.autoplay !== false,
    theme: '#f59e0b',
    lang: 'zh-cn',
    hotkey: true,
    screenshot: false,
    playbackSpeed: RATES,
    video: props.hls
      ? {
          url: props.url,
          ...(props.poster ? { poster: props.poster } : {}),
          type: 'customHls',
          customType: {
            customHls: (video: HTMLVideoElement, player: any) => {
              const Hls = (window as any).Hls
              const src = player.options?.video?.url || props.url
              if (Hls?.isSupported()) {
                const inst = new Hls(hlsConfig)
                hlsInst = inst
                inst.on(Hls.Events.ERROR, (_e: unknown, data: any) => {
                  // 换集时旧实例会被 destroy，仍在飞的请求被中止会报 fatal；
                  // 只要它已经不是当前实例就忽略，否则每次换集都误报「线路失败」
                  if (inst !== hlsInst) return
                  if (data?.fatal) {
                    console.warn('[hls] fatal', data.type, data.details, data.reason || '')
                    emit('error')
                  }
                })
                inst.on(Hls.Events.MANIFEST_PARSED, () => {
                  // 换集后新实例不一定能自动起播（旧元素销毁/自动播放策略），补一次
                  if (props.autoplay !== false && video.paused) {
                    video.play().catch(() => {
                      /* 浏览器拦自动播放时等用户点播放键 */
                    })
                  }
                })
                inst.loadSource(src)
                inst.attachMedia(video)
              } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
                video.src = src // iOS Safari：没有 MSE，只能原生播
              } else {
                player.notice?.('当前浏览器不支持 HLS 播放')
                emit('error')
              }
            },
          },
        }
      : { url: props.url, type: 'auto', ...(props.poster ? { poster: props.poster } : {}) },
  })
  const self = dp // 旧实例销毁时也会冒 error，只认当前这个

  // iOS 风格的快退/快进指示器。必须挂在 .dplayer 内部：
  // 全屏的是 .dplayer 这个元素，挂在它外面的浮层会被裁掉。
  const holder = (dp.container as HTMLElement) || box.value
  if (holder) {
    tipEl = document.createElement('div')
    tipEl.className = 'vdl-seek-tip'
    // 图标直接用 emoji（⏪ / ⏩），两个方向各自独立渲染，尺寸由 .vdl-seek-ic 字号统一控制
    tipEl.innerHTML =
      '<span class="vdl-seek-ic" data-ic="back" aria-hidden="true">⏪</span>' +
      '<span class="vdl-seek-ic" data-ic="fwd" aria-hidden="true">⏩</span>' +
      '<span class="vdl-seek-label">15 秒</span>'
    holder.appendChild(tipEl)
  }
  const v = dp.video as HTMLVideoElement | undefined
  if (v) {
    watchRate(v)
    // 起播前套用记住的倍速；canplay 再补一次（部分浏览器换源后会把 rate 重置回 1）
    applyRate()
    v.addEventListener('canplay', applyRate)
  }
  dp.on('loadedmetadata', () => {
    applyRate()
    // 上次看到一半：跳回原位置（太靠前就不跳，避免刚点开就从头闪一下）
    if (props.startAt && props.startAt > 5) {
      try {
        dp.seek(props.startAt)
      } catch {
        /* ignore */
      }
    }
  })
  dp.on('pause', report)
  dp.on('error', () => {
    if (dp !== self) return
    console.warn('[player] video element error', (dp?.video as HTMLVideoElement)?.error?.code)
    emit('error')
  })
  dp.on('ended', () => {
    report()
    emit('ended')
  })

  // 换集时新实例常常起不了播：DPlayer 在构造末尾就调了一次 play()，那时 MSE 还没
  // attach 完，play() 被后续 load 打断（AbortError）后就再没人重试 —— 表现为
  // 缓冲够了但 paused、进度停在 0。构造后 5 秒内补几次，起播就停手。
  if (props.autoplay !== false) {
    let tries = 0
    kickTimer = window.setInterval(() => {
      const v = dp?.video as HTMLVideoElement | undefined
      if (++tries > 10 || dp !== self || !v) {
        window.clearInterval(kickTimer)
        kickTimer = undefined
        return
      }
      if (!v.paused) {
        window.clearInterval(kickTimer)
        kickTimer = undefined
        return
      }
      if (v.readyState >= 2) v.play().catch(() => {})
    }, 500)
  }
}

onMounted(() => {
  create()
  // 捕获阶段先拿到点击：两侧自己处理快进/快退，中间放行
  box.value?.addEventListener('click', onContainerClick, true)
  // pointer 状态机：down 在容器上，move/up/cancel 挂 window（手指拖出播放器也要能收尾）
  box.value?.addEventListener('pointerdown', onPointerDown, true)
  document.addEventListener('pointermove', onPointerMove, true)
  document.addEventListener('pointerup', onPointerUp, true)
  document.addEventListener('pointercancel', onPointerCancel, true)
  // 右键菜单也在捕获阶段吃掉（DPlayer 的内置菜单 + 浏览器原生菜单；
  // 顺带压掉 Android 长按弹出的原生菜单，不干扰长按倍速）
  document.addEventListener('contextmenu', onContextMenu, true)
  tick = window.setInterval(report, 5000)
})
watch(() => props.url, create)
onBeforeUnmount(() => {
  report()
  if (tick) window.clearInterval(tick)
  if (tipTimer) window.clearTimeout(tipTimer)
  box.value?.removeEventListener('click', onContainerClick, true)
  box.value?.removeEventListener('pointerdown', onPointerDown, true)
  document.removeEventListener('pointermove', onPointerMove, true)
  document.removeEventListener('pointerup', onPointerUp, true)
  document.removeEventListener('pointercancel', onPointerCancel, true)
  document.removeEventListener('contextmenu', onContextMenu, true)
  destroy()
})
</script>

<template>
  <div ref="box" class="vdl-player-host aspect-video w-full bg-black"></div>
</template>

<!-- 指示器是运行时插进 .dplayer 的 DOM，作用域样式加不上去，这里用唯一前缀的全局样式 -->
<style>
.vdl-seek-tip {
  position: absolute;
  top: 50%;
  left: 50%;
  transform: translate(-50%, -50%);
  z-index: 15;
  display: none;
  flex-direction: column;
  align-items: center;
  gap: 2px;
  padding: 10px 16px;
  border-radius: 14px;
  background: rgba(0, 0, 0, 0.55);
  color: #fff;
  font-size: 11px;
  line-height: 1;
  pointer-events: none;
  backdrop-filter: blur(2px);
}

.vdl-seek-tip.vdl-show {
  display: flex;
  animation: vdl-seek-pop 0.16s ease-out;
}

/* 长按倍速需要压掉系统长按行为（文本选择 / iOS 长按呼出菜单） */
.vdl-player-host {
  user-select: none;
  -webkit-user-select: none;
  -webkit-touch-callout: none;
}

/* 快退/快进图标：emoji 各自独立节点，不镜像、不缩放，字号统一 */
.vdl-seek-ic {
  display: none;
  font-size: 20px;
  line-height: 1;
}

.vdl-seek-tip[data-dir='back'] .vdl-seek-ic[data-ic='back'],
.vdl-seek-tip[data-dir='fwd'] .vdl-seek-ic[data-ic='fwd'] {
  display: inline-block;
}

/* 左三分之一 / 右三分之一，跟点击区域一致 */
.vdl-seek-tip[data-dir='back'] {
  left: 15%;
}

.vdl-seek-tip[data-dir='fwd'] {
  left: 85%;
}

@keyframes vdl-seek-pop {
  from {
    opacity: 0;
    transform: translate(-50%, -50%) scale(0.86);
  }
  to {
    opacity: 1;
    transform: translate(-50%, -50%) scale(1);
  }
}
</style>

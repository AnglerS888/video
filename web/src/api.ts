import type {
  ApiResp, CoverFixStatus, Downloads, FavItem, FfmpegStatus, Filters, Progress, ShareInfo, SharePage, Source, SourceTest,
  SysInfo, UpdateInfo, UpdateStatus, VersionResp, VideoDetail, VideoList, WatchHistoryItem,
} from './types'

async function req<T>(path: string, init?: RequestInit): Promise<ApiResp<T>> {
  try {
    const r = await fetch(path, init)
    if (!r.ok && r.headers.get('content-type')?.includes('json') === false) {
      return { ok: false, msg: `HTTP ${r.status}` }
    }
    return (await r.json()) as ApiResp<T>
  } catch (e) {
    return { ok: false, msg: e instanceof Error ? e.message : String(e) }
  }
}

const json = (body: unknown): RequestInit => ({
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
})

export const api = {
  /* ---- 影视 ---- */
  videos(q: Record<string, string | number | undefined>) {
    const p = new URLSearchParams()
    for (const [k, v] of Object.entries(q)) {
      if (v !== undefined && v !== '' && v !== 0) p.set(k, String(v))
    }
    return req<VideoList>(`/api/videos?${p.toString()}`)
  },
  video(id: number | string) {
    return req<VideoDetail>(`/api/videos/${id}`)
  },
  filters() {
    return req<Filters>('/api/filters')
  },

  /* ---- 收藏 ---- */
  favs() {
    return req<{ list: FavItem[]; update_count: number }>('/api/favs')
  },
  addFav(vod_name: string) {
    return req<unknown>('/api/favs', json({ vod_name }))
  },
  delFav(vod_name: string) {
    return req<unknown>(`/api/favs?name=${encodeURIComponent(vod_name)}`, { method: 'DELETE' })
  },
  seenFav(vod_name: string) {
    return req<unknown>(`/api/favs/seen?name=${encodeURIComponent(vod_name)}`, { method: 'POST' })
  },

  /* ---- 下载 ---- */
  downloads() {
    return req<Downloads>('/api/downloads')
  },
  download(vod_id: number, play_from: string, ep_indices: number[]) {
    return req<{ created: number; skipped: number; msg: string }>(
      '/api/download',
      json({ vod_id, play_from, ep_indices }),
    )
  },
  pauseDownload(id: number) {
    return req<unknown>(`/api/downloads/${id}/pause`, { method: 'POST' })
  },
  resumeDownload(id: number) {
    return req<unknown>(`/api/downloads/${id}/resume`, { method: 'POST' })
  },
  /** 重试失败的任务（failed → 重新排队，已下好的分片复用） */
  retryDownload(id: number) {
    return req<unknown>(`/api/downloads/${id}/retry`, { method: 'POST' })
  },
  /** 优先下载：当前这一集下完后立刻轮到它 */
  priorityDownload(id: number) {
    return req<string>(`/api/downloads/${id}/priority`, { method: 'POST' })
  },
  delDownload(id: number) {
    return req<unknown>(`/api/downloads/${id}`, { method: 'DELETE' })
  },
  batchDownload(action: 'pause' | 'resume' | 'retry' | 'delete', ids: number[]) {
    return req<string>('/api/downloads/batch', json({ action, ids }))
  },

  /* ---- 本地 / 分享 / 观看历史 ---- */
  share(id: number, hours = 0) {
    return req<ShareInfo>(`/api/downloads/${id}/share${hours > 0 ? `?hours=${hours}` : ''}`)
  },
  /** 站点根地址（二维码分享用；本机 localhost 访问时后端会换成内网 IP） */
  shareBase() {
    return req<{ base_url: string }>('/api/share/base')
  },
  /** 分享页数据：打开 /s/{id} 时读取 */
  shareInfo(id: number | string) {
    return req<SharePage>(`/api/share/${id}`)
  },
  saveProgress(download_id: number, position: number, duration: number) {
    return req<{ finished: number }>('/api/watch/progress', json({ download_id, position, duration }))
  },
  history() {
    return req<WatchHistoryItem[]>('/api/watch/history')
  },
  /** 从观看历史移除一条（不影响已下载的文件） */
  delHistory(download_id: number) {
    return req<string>(`/api/watch/history/${download_id}`, { method: 'DELETE' })
  },
  /** 清空观看历史 */
  clearHistory() {
    return req<{ deleted: number; msg: string }>('/api/watch/history', { method: 'DELETE' })
  },

  /* ---- 接口管理 ---- */
  sources() {
    return req<Source[]>('/api/sources')
  },
  addSource(name: string, api_url: string) {
    return req<{ id: number; api_url: string }>('/api/sources', json({ name, api_url, enabled: 1 }))
  },
  updateSource(id: number, body: { name: string; api_url: string; enabled: number }) {
    return req<{ api_url: string }>(`/api/sources/${id}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
  },
  delSource(id: number) {
    return req<{ name: string; videos: number; plays: number; downloads: number; covers: number; msg: string }>(
      `/api/sources/${id}`,
      { method: 'DELETE' },
    )
  },
  testSource(id: number) {
    return req<SourceTest>(`/api/sources/${id}/test`)
  },
  /** 添加前预检：填域名即可，后端会依次尝试 .php / 无 .php / at=xml 等写法 */
  probeSource(url: string) {
    return req<SourceTest>('/api/sources/probe', json({ url }))
  },
  // 接口级「自动增量」开关（只影响定时任务，手动增量/全量不受影响）
  setAutoIncr(id: number, on: boolean) {
    return req<{ auto_incr: number }>(`/api/sources/${id}/auto-incr`,
      json({ on: on ? 1 : 0 }))
  },
  /** 导出全部接口为 JSON 文件（浏览器直接下载） */
  exportSources() {
    const a = document.createElement('a')
    a.href = '/api/sources/export'
    a.download = 'vod-sources.json'
    document.body.appendChild(a)
    a.click()
    a.remove()
  },
  /** 导入接口列表（本系统导出的 JSON），按 api_url 去重 */
  importSources(sources: unknown[]) {
    return req<{ added: number; skipped: number; msg: string }>(
      '/api/sources/import',
      json({ sources }),
    )
  },
  collect(full: boolean, sourceId?: number, restart = false) {
    const q = new URLSearchParams()
    if (sourceId) q.set('source_id', String(sourceId))
    if (restart) q.set('restart', '1')
    const qs = q.toString()
    return req<string>(`/api/collect/${full ? 'full' : 'incr'}${qs ? '?' + qs : ''}`, { method: 'POST' })
  },
  progress() {
    return req<Progress>('/api/progress')
  },
  // 停止采集：不传 sourceId 则停止全部正在采集的接口
  stopCollect(sourceId?: number) {
    const qs = sourceId ? `?source_id=${sourceId}` : ''
    return req<string>(`/api/collect/stop${qs}`, { method: 'POST' })
  },

  /* ---- 网站信息 ---- */
  sysinfo(fresh = false) {
    return req<SysInfo>(`/api/sysinfo${fresh ? '?fresh=1' : ''}`)
  },
  // 整理数据库：回收删接口/重采集留下的空闲页。
  // VACUUM 要独占写锁，期间接口会一直挂着直到整理完成（大库可能几分钟），
  // 所以 fetch 这里不能加超时。
  vacuum() {
    return req<{ freed_bytes: number; msg: string }>('/api/system/vacuum', { method: 'POST' })
  },

  /* ---- ffmpeg（系统优先，缺失时按需下载） ---- */
  ffmpegStatus() {
    return req<FfmpegStatus>('/api/ffmpeg/status')
  },
  // 触发下载：接口立即返回，进度靠 ffmpegStatus() 轮询
  installFfmpeg() {
    return req<FfmpegStatus>('/api/ffmpeg/install', { method: 'POST' })
  },

  /* ---- 版本与在线更新 ---- */
  /** 当前版本 + 上次检查结果（不联网） */
  version() {
    return req<VersionResp>('/api/version')
  },
  /** 主动联网检查 GitHub 最新提交 */
  checkUpdate() {
    return req<UpdateInfo>('/api/update/check', { method: 'POST' })
  },
  /** 下载源码（直接运行模式会自动编译） */
  applyUpdate() {
    return req<UpdateStatus>('/api/update/apply', { method: 'POST' })
  },
  updateStatus() {
    return req<UpdateStatus>('/api/update/status')
  },
  /** 应用新版本并重启（仅直接运行模式） */
  restartUpdate() {
    return req<{ msg: string; relaunch: boolean }>('/api/update/restart', { method: 'POST' })
  },

  /* ---- 封面补充 ---- */
  // 检查全库封面缓存，同剧多源时用其它源的封面补齐拉不到的
  coverFixStatus() {
    return req<CoverFixStatus>('/api/cover/fix')
  },
  startCoverFix() {
    return req<CoverFixStatus>('/api/cover/fix', { method: 'POST' })
  },
  stopCoverFix() {
    return req<CoverFixStatus>('/api/cover/fix/stop', { method: 'POST' })
  },
}

/** 封面地址（r>0 时带一个破坏缓存的时间戳，让服务端忽略失败负缓存重试） */
export function coverURL(u: string, r = 0): string {
  const base = `/api/img?u=${encodeURIComponent(u)}`
  return r > 0 ? `${base}&r=${r}` : base
}

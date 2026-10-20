export interface ApiResp<T> { ok: boolean; data?: T; msg?: string }

export interface Source {
  id: number
  name: string
  api_url: string
  enabled: number
  /** 是否参与定时自动增量（0=只能手动点增量） */
  auto_incr: number
  remark: string
  created_at: string
  updated_at: string
  /** 以下统计由 /api/sources 一并返回（30 秒缓存） */
  videos?: number
  names?: number
  plays?: number
  last_update?: string
}

export interface SourceTest {
  ok: boolean
  msg?: string
  cost_ms?: number
  http_code?: number
  /** 源站实际返回格式：json / xml（苹果CMS 的 at=xml 接口） */
  format?: string
  /** 实际可用的接口地址（预检时可能和输入的不同：自动补 .php 或换 at/xml） */
  api_url?: string
  /** 预检时依次尝试过的地址 */
  tried?: string[]
  parsed?: number
  page?: number
  pagecount?: number
  total?: number
  pagination?: boolean
  detail_ok?: boolean
  detail_sample?: string
}

export interface SourceProgress {
  source_id: number
  name: string
  mode: string
  phase: string
  page: number
  page_count: number
  fetched: number
  added: number
  updated: number
  details: number
  running: boolean
  started_at?: string
  finished_at?: string
  error?: string
}

export interface Progress {
  mode: string
  running: boolean
  running_count: number
  finished_at?: string
  sources: SourceProgress[]
}

export interface VideoItem {
  id: number
  vod_name: string
  source_count: number
  vod_pic: string
  type_name: string
  vod_remarks: string
  vod_year: string
  vod_area: string
  source_names: string[]
  downloaded_count: number
  updated_at: string
  pics?: string[]
}

export interface VideoList { total: number; page: number; size: number; list: VideoItem[] }

export interface EpisodeDownload {
  id: number
  status: string
  /** 1 = 点了「优先下载」，当前这一集下完就轮到它 */
  priority?: number
  percent: number
  file: string
  size_mb: number
  episodes: number
  error: string
  ep_index: number
}

export interface Episode {
  ep_index: number
  ep_name: string
  url: string
  download: EpisodeDownload | null
}

export interface Line {
  play_from: string
  episode_count: number
  episodes: Episode[]
}

export interface VideoSource {
  vod_id: number
  source_id: number
  source_name: string
  remarks: string
  pic: string
  lines: Line[]
}

export interface VideoDetail {
  vod_name: string
  type_name: string
  vod_pic: string
  vod_content: string
  vod_year: string
  vod_area: string
  vod_score: string
  vod_remarks: string
  vod_actor: string
  vod_director: string
  vod_lang: string
  sources: VideoSource[]
}

export interface Filters { years: string[]; areas: string[]; types: string[] }

export interface FavItem {
  vod_name: string
  id: number
  vod_pic: string
  type_name: string
  vod_year: string
  vod_area: string
  vod_remarks: string
  source_count: number
  updated_at: string
  created_at: string
  has_update: boolean
  gone: boolean
}

export interface DownloadTask {
  id: number
  vod_id: number
  vod_name: string
  source_name: string
  play_from: string
  ep_index: number
  status: string
  /** 1 = 点了「优先下载」，当前这一集下完就轮到它 */
  priority?: number
  percent: number
  speed?: string
  error?: string
  file?: string
  size_mb: number
  episodes: number
  vod_pic?: string
  updated_at?: string
}

export interface Downloads {
  done: DownloadTask[]
  tasks: DownloadTask[]
  active: DownloadTask[]
}

export interface ShareInfo {
  id: number
  name: string
  episode: string
  /** 分享页地址（给别人打开用，页面里再播放/下载） */
  page_url: string
  play_url: string
  file_url: string
  /** 分享失效时间（UTC "YYYY-MM-DD HH:mm:ss"），空 = 永久有效 */
  expires_at?: string
}

/** 分享页 /s/{id} 的数据 */
export interface SharePage {
  id: number
  vod_id: number
  name: string
  episode: string
  source_name: string
  vod_pic: string
  play_from: string
  size_mb: number
  play_url: string
  file_url: string
  /** 同剧其它已下载的集（按集号升序，多源同集已去重） */
  episodes: { id: number; ep_index: number }[]
  /** 分享失效时间（UTC），空 = 永久有效 */
  expires_at?: string
}

export interface WatchHistoryItem {
  download_id: number
  vod_id: number
  vod_name: string
  ep_index: number
  source_name: string
  vod_pic: string
  position: number
  duration: number
  finished: number
  updated_at: string
}

/** ffmpeg 状态机：missing=未安装 / downloading=下载中 / ready=已就绪 / failed=下载失败 */
export interface FfmpegStatus {
  state: 'ready' | 'missing' | 'downloading' | 'failed'
  available: boolean
  /** env=环境变量指定 / cache=程序下载的 / system=系统已装 */
  source?: string
  path?: string
  version?: string
  error?: string
  platform: string
  /** 当前平台的下载地址，空表示该平台不支持自动下载 */
  download_url: string
  supported: boolean
  progress?: { downloaded: number; total: number; percent: number }
}

export interface SysInfo {
  app: { version: string; go: string; os: string; arch: string; pid: number; uptime_sec: number; started_at: string }
  ffmpeg: FfmpegStatus
  data: {
    dir: string; db_bytes: number; img_bytes: number; img_count: number
    video_bytes: number; backup_bytes: number; bin_bytes: number; total_bytes: number; partial: boolean
    /** 数据库空闲页占比（%）：采集/删接口留下的空洞，SQLite 不会自动归还 */
    freelist_pct: number
    /** 整理数据库可回收的字节数 */
    reclaimable_bytes: number
    /** 自动整理的空闲页占比阈值（%），0=未启用 */
    vacuum_pct: number
    /** 空闲页占比已达阈值：建议手动点「整理数据库」 */
    vacuum_suggest: boolean
  }
  disk: {
    total: number; free: number; used: number
    /** 磁盘保护阈值（字节）：低于它新下载会被拒绝、队列自动暂停；0=未启用 */
    min_free_bytes: number
    /** 剩余空间已低于保护阈值 */
    low: boolean
  }
  mem: {
    total: number; available: number; used: number
    go_alloc: number; go_inuse: number; go_sys: number; goroutines: number; gc_count: number
  }
  counts: {
    sources: number; enabled_sources: number; videos: number; names: number
    plays: number; downloads: number; downloads_active: number; favorites: number
  }
  cover: { ok: number; failed: number; last_error: string }
}

/** 封面补充任务进度（信息页手动触发） */
export interface CoverFixStatus {
  running: boolean
  /** idle / scanning / processing / stopping / done / stopped / failed */
  phase: string
  /** 影片记录数、去重后的封面地址数 */
  videos: number
  covers: number
  /** 已检查本地缓存的封面数 */
  checked: number
  /** 本地已有缓存的封面数；missing = 需要处理的封面数（本次总量） */
  healthy: number
  missing: number
  /** 已处理数；fetched 重新下载成功；filled 用其它源封面补上；proxied 走图片代理补上；failed 仍失败 */
  processed: number
  fetched: number
  filled: number
  proxied: number
  failed: number
  started_at: string
  finished_at: string
  elapsed_sec: number
  last_error: string
  message: string
}

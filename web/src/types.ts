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

/** 同步记录里的一部影片明细 */
export interface SyncLogItem {
  vod_id: number
  name: string
  /** new=新增 / update=更新 */
  action: string
  /** 源站备注（如「更新至12集」） */
  remarks: string
  /** 源站更新时间（vod_time，源站时区，不做时区换算） */
  src_time: string
}

/** 一次增量同步的汇总记录（GET /api/sources/{id}/sync-logs，新的在前） */
export interface SyncLog {
  id: number
  source_id: number
  /** manual=手动点「增量采集」 / auto=定时任务 */
  trigger: string
  /** UTC 时间（RFC3339 或 SQLite 格式） */
  started_at: string
  finished_at: string
  duration_ms: number
  /** 本次比对过的影片数 */
  fetched: number
  added: number
  updated: number
  /** 补详情请求的批次数 */
  details: number
  /** running/done/warning/failed/stopped */
  status: string
  error: string
  /** 本次新增/更新的影片明细条数（明细最多保存 500 条） */
  items_count: number
}

/** 同步记录分页结果（GET /api/sources/{id}/sync-logs?page=&page_size=） */
export interface SyncLogPage {
  /** 该来源累计的同步记录总数（最多保留 100 条） */
  total: number
  /** 当前页（越界时后端会回退到最后一页） */
  page: number
  page_size: number
  /** 当前页记录（新的在前） */
  items: SyncLog[]
}

/** 定时增量任务状态（GET /api/scheduler） */
export interface SchedulerInfo {
  enabled: boolean
  /** 采集间隔（秒） */
  interval_sec: number
  /** 下次触发时间（unix 秒），0 = 未知 */
  next_run_unix: number
  next_run?: string
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

// 一个筛选取值 + 它覆盖的影片数（后端只返回片量最多的前 20 个）
export interface FilterItem { v: string; c: number }

// 每个维度最多返回 20 个取值，其余全部归入「其他」。
// other 里是三个维度各自的「其他」桶影片数，为 0 时前端不显示「其他」胶囊。
export interface Filters {
  years: FilterItem[]
  areas: FilterItem[]
  types: FilterItem[]
  other: { year: number; area: number; type: number }
}

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
  /** 1 = 已开启自动追更（指定来源 + 线路出现新集数时自动下载） */
  auto_dl: number
  /** 追更盯的 vod_info.id（决定来源） */
  auto_vod_id: number
  /** 追更盯的播放线路 */
  auto_play_from: string
  /** 该线路里还没入队的新集数（0 = 已追平） */
  auto_new_eps: number
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
}

/** 分享页 /s/{token} 的数据（旧版 /s/{数字id} 仅本机可看） */
export interface SharePage {
  id: number
  vod_id: number
  name: string
  episode: string
  /** 当前集的集号（-1=整条） */
  ep_index: number
  source_name: string
  vod_pic: string
  play_from: string
  size_mb: number
  play_url: string
  file_url: string
  /** 同剧其它已下载的集（按集号升序，多源同集已去重） */
  episodes: { ep_index: number }[]
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
  app: {
    version: string; go: string; os: string; arch: string; pid: number; uptime_sec: number; started_at: string
    /** 当前使用的配置文件路径 */
    config_file: string
  }
  ffmpeg: FfmpegStatus
  data: {
    dir: string; db_bytes: number; img_bytes: number; img_count: number
    video_bytes: number; backup_bytes: number; bin_bytes: number; total_bytes: number; partial: boolean
    /** 这组数字的统计时间（Unix 秒）：目录体积在后台重扫，可能比页面刷新时间早一点 */
    at: number
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

/** 版本与在线更新：检查结果 */
export interface UpdateInfo {
  current_version: string
  current_commit: string
  /** 构建时间（RFC3339） */
  build_time: string
  /** docker / direct */
  mode: string
  /** 更新源仓库网页地址（检查后为命中的那个仓库：Gitee 或 GitHub） */
  site_url: string
  repo: string
  branch: string
  latest_version: string
  latest_commit: string
  latest_commit_at: string
  latest_commit_msg: string
  available: boolean
  reasons?: string[]
  checked_at: string
  /** 本次检查实际走通的通道：direct 或镜像地址 */
  via?: string
  error?: string
}

/** 版本与在线更新：任务进度 */
export interface UpdateStatus {
  /** idle/checking/downloading/extracting/building/staged/ready/applying/failed */
  phase: string
  percent: number
  message: string
  bytes: number
  total: number
  source_dir?: string
  /** 在宿主机重建镜像的步骤（仅 Docker 模式） */
  instructions?: string[]
  error?: string
  updated_at: string
}

/** 配置文件（在线编辑） */
/** 配置文件的结构（与 internal/config.Config 的 json 标签一一对应） */
export interface ConfigData {
  server: { addr: string }
  data_dir: string
  log_level: string
  lowmem: boolean
  mem_limit_mb: number
  max_procs: number
  collect: { pages: number; sources: number; incr_every: string; incr_pages: number }
  download: { workers: number; seg_concurrency: number; seg_budget: number; min_free_mb: number; auto_new_ep: boolean }
  cover: { workers: number; concurrency: number; img_proxy: string }
  ffmpeg: { threads: number; reencode: boolean; verify: boolean }
  /** 访问密码（存在 ui.yml；留空 = 不启用）：启用后站点要登录，接口要带密码 */
  auth: { password: string }
  db: { cache_mb: number; mmap_mb: number; conns: number; page_size: number; vacuum_pct: number }
}

export interface ConfigInfo {
  path: string
  /** 页面配置（ui.yml）在数据目录下的路径 */
  ui_path?: string
  text: string
  config?: ConfigData | null
  editable: boolean
  presets?: { default: ConfigData; lowmem: ConfigData }
}

export interface VersionResp {
  info: UpdateInfo
  status: UpdateStatus
  mode: string
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

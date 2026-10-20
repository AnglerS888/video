package web

import (
	"io/fs"
	"net/http"
)

// Handler 注册所有 HTTP 路由。
//
// 单独成文件：路由表是接口契约，改动频率低但要一眼看全；
// 具体实现分散在 web.go（视频/接口/下载等 handler）与 live.go（HLS 代理）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 接口管理
	mux.HandleFunc("GET /api/sources", s.listSources)
	mux.HandleFunc("POST /api/sources", s.addSource)
	// 预检：输入域名/地址后先试能不能同步（不写库），给添加表单的「测试」按钮用
	mux.HandleFunc("POST /api/sources/probe", s.probeSource)
	mux.HandleFunc("PUT /api/sources/{id}", s.updateSource)
	mux.HandleFunc("DELETE /api/sources/{id}", s.deleteSource)
	mux.HandleFunc("GET /api/sources/{id}/test", s.testSource)
	mux.HandleFunc("POST /api/sources/{id}/auto-incr", s.setAutoIncr)
	// 接口列表导出/导入（JSON）：换机/重装迁移用，导入按 api_url 去重
	mux.HandleFunc("GET /api/sources/export", s.exportSources)
	mux.HandleFunc("POST /api/sources/import", s.importSources)

	// 采集
	mux.HandleFunc("POST /api/collect/full", s.collectFull)
	mux.HandleFunc("POST /api/collect/incr", s.collectIncr)
	mux.HandleFunc("POST /api/collect/stop", s.stopCollect)
	mux.HandleFunc("GET /api/progress", s.progress)

	// 视频（按名称合并）/ 详情（多源）
	mux.HandleFunc("GET /api/videos", s.listVideos)
	mux.HandleFunc("GET /api/videos/{id}", s.videoDetail)
	// 筛选项：年份 / 国家地区 / 类型
	mux.HandleFunc("GET /api/filters", s.listFilters)

	// 封面图代理（解决防盗链导致的封面空白）
	mux.HandleFunc("GET /api/img", s.proxyImage)
	// 在线播放：HLS 代理（改写 m3u8 + 转发分片，解决源站无 CORS）
	mux.HandleFunc("GET /api/live", s.liveProxy)

	// 收藏（按剧名）
	mux.HandleFunc("GET /api/favs", s.listFavs)
	mux.HandleFunc("POST /api/favs", s.addFav)
	mux.HandleFunc("DELETE /api/favs", s.delFav)
	mux.HandleFunc("POST /api/favs/seen", s.seenFav)

	// 下载（单位：视频 + 播放源）
	mux.HandleFunc("POST /api/download", s.enqueueDownload)
	mux.HandleFunc("GET /api/downloads", s.listDownloads)
	mux.HandleFunc("DELETE /api/downloads/{id}", s.deleteDownload)
	mux.HandleFunc("POST /api/downloads/{id}/pause", s.pauseDownload)
	mux.HandleFunc("POST /api/downloads/{id}/resume", s.resumeDownload)
	// 失败任务重试：重置为 queued 重跑（已下好的分片复用）
	mux.HandleFunc("POST /api/downloads/{id}/retry", s.retryDownload)
	// 优先下载：当前这一集下完后立刻轮到它（插队）
	mux.HandleFunc("POST /api/downloads/{id}/priority", s.priorityDownload)
	// 批量：暂停 / 继续 / 删除选中任务
	mux.HandleFunc("POST /api/downloads/batch", s.batchDownload)

	// 播放：只允许下载完成的 mp4；?dl=1 直接作为附件下载（分享用）
	mux.HandleFunc("GET /play/{id}", s.playVideo)
	// 分享：返回局域网可访问的外链（前端再生成二维码）
	mux.HandleFunc("GET /api/downloads/{id}/share", s.shareDownload)
	// 站点根地址（首页/详情页二维码分享用，本机访问时换成内网 IP）
	mux.HandleFunc("GET /api/share/base", s.shareBase)
	// 分享页数据：别人打开 /s/{id} 页面时读取（页面里再点播放/下载，不直接暴露视频地址）
	mux.HandleFunc("GET /api/share/{id}", s.sharePage)
	// 观看历史：记录/续播位置
	mux.HandleFunc("POST /api/watch/progress", s.saveWatchProgress)
	mux.HandleFunc("GET /api/watch/history", s.watchHistory)
	// 观看历史：删单条 / 清空
	mux.HandleFunc("DELETE /api/watch/history/{id}", s.delWatchHistory)
	mux.HandleFunc("DELETE /api/watch/history", s.clearWatchHistory)

	mux.HandleFunc("GET /api/system", s.system)
	// 网站信息：运行环境/数据占用/磁盘/内存/记录数
	mux.HandleFunc("GET /api/sysinfo", s.sysInfo)
	// 手动整理数据库（回收删接口/重采集留下的空闲页）
	mux.HandleFunc("POST /api/system/vacuum", s.vacuumDB)

	// ffmpeg：已不再内嵌，系统没有时由用户在信息页点按钮按需下载
	mux.HandleFunc("GET /api/ffmpeg/status", s.ffmpegStatus)
	mux.HandleFunc("POST /api/ffmpeg/install", s.installFFmpeg)

	// 封面补充：检查全库封面缓存，同剧多源时用其它源的封面补上拉不到的
	mux.HandleFunc("GET /api/cover/fix", s.coverFixStatus)
	mux.HandleFunc("POST /api/cover/fix", s.startCoverFix)
	mux.HandleFunc("POST /api/cover/fix/stop", s.stopCoverFix)

	// 容器健康检查：只做一次极轻量的 DB 探活，不扫全表（/api/system 会 COUNT 三张表）
	mux.HandleFunc("GET /healthz", s.healthz)

	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /", spaHandler(sub))
	return logMiddleware(mux)
}

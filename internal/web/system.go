package web

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	"videoDownLoad/internal/collector"
	db "videoDownLoad/internal/db"
	"videoDownLoad/internal/ffmpeg"
	"videoDownLoad/internal/sysinfo"
)

// sysInfo 网站信息页数据：ffmpeg、数据目录与占用、磁盘、内存、记录数、运行时长
func (s *Server) sysInfo(w http.ResponseWriter, r *http.Request) {
	fresh := r.URL.Query().Get("fresh") == "1"
	// ffmpeg 下载中时进度每秒都在变，不能被 10 秒的页面缓存挡掉
	ff := ffmpeg.Status(s.DataDir)
	if ff.State == ffmpeg.StateDownloading {
		fresh = true
	}
	infoMu.Lock()
	if !fresh && infoCache != nil && time.Now().Before(infoExp) {
		c := infoCache
		infoMu.Unlock()
		ok(w, c)
		return
	}
	infoMu.Unlock()

	var sources, enabled, videos, names, plays, downloads, dlActive, favs int
	s.DB.QueryRow(`SELECT COUNT(*) FROM vod_source`).Scan(&sources)
	s.DB.QueryRow(`SELECT COUNT(*) FROM vod_source WHERE enabled=1`).Scan(&enabled)
	s.DB.QueryRow(`SELECT COUNT(*) FROM vod_info`).Scan(&videos)
	s.DB.QueryRow(`SELECT COUNT(*) FROM vod_name_agg`).Scan(&names)
	s.DB.QueryRow(`SELECT COUNT(*) FROM vod_download`).Scan(&downloads)
	s.DB.QueryRow(`SELECT COUNT(*) FROM vod_download WHERE status IN ('queued','downloading','merging','paused')`).Scan(&dlActive)
	s.DB.QueryRow(`SELECT COUNT(*) FROM vod_fav`).Scan(&favs)
	// 播放地址表 226 万行，COUNT(*) 要全索引扫描（弱机上 1s+），走 5 分钟缓存
	plays = db.PlaysCount(s.DB)

	du := sysinfo.DirUsageOf(s.DataDir)
	diskTotal, diskFree := sysinfo.DiskOf(s.DataDir)
	memTotal, memAvail := sysinfo.MemOf()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	// ffmpeg 状态：ready/missing/downloading/failed，前端据此显示下载按钮与进度
	coverOK, coverFail, coverErr := collector.CoverStats()

	// 空闲页占比：采集重插播放地址 / 删接口都会不断产生空闲页，SQLite 不会主动还回去
	fl, _ := db.FreelistStatsOf(s.DB)
	reclaim := fl.ReclaimableBytes()

	uptime := int64(0)
	if !s.StartedAt.IsZero() {
		uptime = int64(time.Since(s.StartedAt).Seconds())
	}
	data := map[string]any{
		"app": map[string]any{
			"version": s.Version, "go": runtime.Version(),
			"os": runtime.GOOS, "arch": runtime.GOARCH,
			"pid": os.Getpid(), "uptime_sec": uptime,
			"started_at": s.StartedAt.Format("2006-01-02 15:04:05"),
		},
		"ffmpeg": ff,
		"data": map[string]any{
			"dir": s.DataDir, "db_bytes": du.DBBytes, "img_bytes": du.ImgBytes,
			"img_count": du.ImgCount, "video_bytes": du.VideoBytes,
			"backup_bytes": du.BackupBytes, "bin_bytes": du.BinBytes,
			"total_bytes": du.TotalBytes, "partial": du.Partial,
			"freelist_pct": fl.FreeRatio(), "reclaimable_bytes": reclaim,
			// 空闲页占比达到自动整理阈值时提示用户（自动整理只在服务空闲时跑，等不到可以手动点）
			"vacuum_pct":     s.VacuumPct,
			"vacuum_suggest": s.VacuumPct > 0 && reclaim > 0 && fl.FreeRatio() >= s.VacuumPct,
		},
		"disk": map[string]any{
			"total": diskTotal, "free": diskFree, "used": diskTotal - diskFree,
			// 磁盘保护阈值与告警：低于阈值时新下载会被拒绝、队列自动暂停
			"min_free_bytes": collector.MinFreeBytes(),
			"low":            collector.MinFreeBytes() > 0 && diskFree > 0 && diskFree < uint64(collector.MinFreeBytes()),
		},
		"mem": map[string]any{
			"total": memTotal, "available": memAvail, "used": memTotal - memAvail,
			"go_alloc": ms.HeapAlloc, "go_inuse": ms.HeapInuse, "go_sys": ms.Sys,
			"goroutines": runtime.NumGoroutine(), "gc_count": ms.NumGC,
		},
		"counts": map[string]any{
			"sources": sources, "enabled_sources": enabled, "videos": videos,
			"names": names, "plays": plays, "downloads": downloads,
			"downloads_active": dlActive, "favorites": favs,
		},
		"cover": map[string]any{"ok": coverOK, "failed": coverFail, "last_error": coverErr},
	}
	infoMu.Lock()
	infoCache, infoExp = data, time.Now().Add(10*time.Second)
	infoMu.Unlock()
	ok(w, data)
}

// vacuumDB 手动整理数据库，把删接口/重采集留下的空闲页还给文件系统。
// VACUUM 要独占写锁（本项目只有 1 个 DB 连接），期间所有查询都会排队，
// 所以采集或下载进行中一律拒绝。
func (s *Server) vacuumDB(w http.ResponseWriter, r *http.Request) {
	if len(collector.GlobalProgress.RunningIDs()) > 0 {
		fail(w, "正在采集，请停止后再整理")
		return
	}
	var active int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM vod_download
		WHERE status IN ('queued','downloading','merging','paused')`).Scan(&active)
	if active > 0 {
		fail(w, "有下载任务在进行中，请稍后再整理")
		return
	}
	freed, err := db.Vacuum(s.DB)
	if err != nil {
		fail(w, "整理失败: "+err.Error())
		return
	}
	db.InvalidatePlaysCount()
	// 目录占用统计有 60 秒缓存，不清掉的话用户整理完刷新页面看到的还是旧大小
	sysinfo.InvalidateDirUsage()
	ok(w, map[string]any{
		"freed_bytes": freed,
		"msg":         "已回收 " + db.HumanBytes(freed),
	})
}

// coverFixStatus 封面补充任务进度（前端点击后轮询）
func (s *Server) coverFixStatus(w http.ResponseWriter, r *http.Request) {
	ok(w, collector.CoverSupplementStatus())
}

// startCoverFix 启动封面补充：检查全库封面缓存，同剧多源时用其它源的封面补齐拉不到的
func (s *Server) startCoverFix(w http.ResponseWriter, r *http.Request) {
	if err := s.Col.StartCoverSupplement(); err != nil {
		fail(w, err.Error())
		return
	}
	ok(w, collector.CoverSupplementStatus())
}

// stopCoverFix 停止封面补充（当前这张处理完就退出）
func (s *Server) stopCoverFix(w http.ResponseWriter, r *http.Request) {
	if !collector.StopCoverSupplement() {
		fail(w, "当前没有正在进行的封面补充")
		return
	}
	ok(w, collector.CoverSupplementStatus())
}

// healthz 容器健康检查：SELECT 1 探活即可，避免健康检查把 CPU 花在全表 COUNT 上
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.QueryRow(`SELECT 1`).Scan(new(int)); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "db error: %v\n", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// ffmpegStatus ffmpeg 状态（ready/missing/downloading/failed）+ 下载地址
func (s *Server) ffmpegStatus(w http.ResponseWriter, r *http.Request) {
	ok(w, ffmpeg.Status(s.DataDir))
}

// installFFmpeg 触发按需下载：立即返回，前端轮询 /api/ffmpeg/status 看进度。
// 下载完成后由 ffmpeg 包的回调把新路径交给下载器与图片处理器，无需重启。
func (s *Server) installFFmpeg(w http.ResponseWriter, r *http.Request) {
	if err := ffmpeg.StartInstall(s.DataDir); err != nil {
		fail(w, err.Error())
		return
	}
	// 页面缓存里存的还是 missing，立刻失效，前端下一次轮询才能看到「下载中」
	infoMu.Lock()
	infoExp = time.Time{}
	infoMu.Unlock()
	s.statMu.Lock()
	s.sysExp = time.Time{}
	s.statMu.Unlock()
	ok(w, ffmpeg.Status(s.DataDir))
}

func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	// 统计要扫全表（vod_info 8 万行 / vod_play 36 万行），前端每 30 秒轮询一次、
	// 健康检查每分钟一次；做 15 秒缓存避免同一次打开页面并发打多个全表 COUNT。
	fresh := r.URL.Query().Get("fresh") == "1"
	s.statMu.Lock()
	if !fresh && s.sysCache != nil && time.Now().Before(s.sysExp) {
		c := s.sysCache
		s.statMu.Unlock()
		ok(w, c)
		return
	}
	s.statMu.Unlock()

	var vodCount, nameCount, playCount, doneCount int
	s.DB.QueryRow(`SELECT COUNT(*) FROM vod_info`).Scan(&vodCount)
	s.DB.QueryRow(`SELECT COUNT(DISTINCT vod_name) FROM vod_info`).Scan(&nameCount)
	// 播放地址表 226 万行，COUNT(*) 要全索引扫描（弱机上 1s+），走 5 分钟缓存
	playCount = db.PlaysCount(s.DB)
	s.DB.QueryRow(`SELECT COUNT(*) FROM vod_download WHERE status='done'`).Scan(&doneCount)

	// ffmpeg 已不再内嵌，改为系统优先 + 按需下载，这里直接给状态快照
	ff := ffmpeg.Status(s.DataDir)
	// 封面缓存情况：能直接看出"没图"是拉取失败还是根本没触发
	coverOK, coverFail, coverErr := collector.CoverStats()
	data := map[string]any{
		"ffmpeg": ff, "vod_count": vodCount, "name_count": nameCount,
		"play_count": playCount, "done_count": doneCount,
		"cover": map[string]any{"ok": coverOK, "failed": coverFail, "last_error": coverErr},
	}
	s.statMu.Lock()
	s.sysCache, s.sysExp = data, time.Now().Add(15*time.Second)
	s.statMu.Unlock()
	ok(w, data)
}

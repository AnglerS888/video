package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"videoDownLoad/internal/collector"
	"videoDownLoad/internal/db"
	"videoDownLoad/internal/downloader"
	"videoDownLoad/internal/ffmpeg"
	"videoDownLoad/internal/imgproc"
	"videoDownLoad/internal/logx"
	"videoDownLoad/internal/scheduler"
	"videoDownLoad/internal/updater"
	"videoDownLoad/internal/version"
	"videoDownLoad/internal/web"
)

func main() {
	var (
		addr    = flag.String("addr", ":5200", "HTTP 监听地址（容器内默认 5200）")
		dataDir = flag.String("data", defaultDataDir, "数据目录（SQLite、ffmpeg 缓存与下载文件）；不指定时自动匹配/迁移旧目录")
		ver     = flag.Bool("version", false, "打印版本后退出")
		logLvl  = flag.String("log-level", "", "日志级别 debug/info/warn/error（默认读 VDL_LOG_LEVEL，否则 info）")

		pages   = flag.Int("pages", 6, "每个采集接口的分页并发数（全量同步的多线程数）")
		sources = flag.Int("sources", 8, "同时采集的接口数上限（点不同接口的全量/增量可并行）")
		// 定时增量：默认每小时一次，只同步每个接口的前 5 页（按更新时间倒序，即最新数据）
		incrEvery = flag.Duration("incr-every", time.Hour, "定时增量采集间隔")
		incrPages = flag.Int("incr-pages", 5, "定时增量每次同步的页数（0=不限页数）")

		// ---- 资源占用相关（NAS / 小内存机器）----
		lowmem = flag.Bool("lowmem", false, "低配模式：自动下调采集/下载/封面并发、关闭全量重编码、限制 GC 内存（NAS 推荐）")
		memMB  = flag.Int("memlimit", 0, "GC 软内存上限（MiB），0=不限制；lowmem 默认 768")
		procs  = flag.Int("maxprocs", 0, "GOMAXPROCS，0=自动；lowmem 默认 min(核数,3)，用于给系统留出余量")

		dlWorkers    = flag.Int("dl-workers", 1, "同时下载的集数（固定串行=1：一集合完再下一集；并发会被强制忽略）")
		coverWorkers = flag.Int("cover-workers", 2, "封面预取的协程数（下载本身由全局限速控制，2 个足够）")
		coverConc    = flag.Int("cover-concurrency", 4, "封面同时连接源站/解码的并发上限")
		segConc      = flag.Int("seg-concurrency", 16, "单个下载任务内的分片并发数")
		segBudget    = flag.Int("seg-budget", 0, "全局分片并发上限（跨任务），0=dl-workers×seg-concurrency")

		imgProxy = flag.String("img-proxy", "", "封面图片代理前缀（如 https://wsrv.nl/?url=）；源站封面拉不到时用")

		ffThreads  = flag.Int("ffmpeg-threads", 0, "ffmpeg 线程数，0=默认；NAS 建议 1~2，避免转码占满整机")
		ffReencode = flag.Bool("ffmpeg-reencode", true, "音画仍不同步时是否全量重编码视频（弱 CPU 上会很慢，可关）")
		ffVerify   = flag.Bool("ffmpeg-verify", true, "合并后是否做音画同步自检（每次多跑 2 个短 ffmpeg 进程）")

		// ---- 数据库占用（SQLite）----
		// cache_size 是「每连接」的：4 连接 × 64MB ≈ 256MB 上限（按需分配）。
		// 8G 内存的 NAS 建议 64~128；4G 机器用 -lowmem（自动降到 24）。
		dbCacheMB = flag.Int("db-cache-mb", 64, "SQLite 每个连接的页缓存（MiB）；8G 内存建议 64~128，4G 用 -lowmem")
		// mmap 让读请求直接由 OS 页缓存服务，少一次内存拷贝；本地盘才开，网络盘（SMB/NFS）建议 0。
		dbMmapMB = flag.Int("db-mmap-mb", 1024, "SQLite 内存映射大小（MiB，0=关闭）；数据库放网络盘时建议设 0")
		// WAL 下多读并发安全（一写多读）；1 个连接会让慢查询堵住整站
		dbConns = flag.Int("db-conns", 4, "SQLite 最大连接数（WAL 下一个写者 + 多个读者）")
		// 页大小：vod_play 行均 ~82B，8192 页可省 5~10% 空间、B 树更浅。
		// 对已存在的库会触发一次全库 VACUUM 重写（大库几分钟、期间阻塞），仅在显式指定时执行
		dbPageSize = flag.Int("db-page-size", 0, "SQLite 页大小字节数（如 8192）；0=不改。改已有库会先 VACUUM 重写一次")
		// 采集重插播放地址、删接口都会持续产生空闲页，SQLite 不会主动还回去。
		// 超过阈值且服务空闲时自动整理（0=关闭，只手动调 POST /api/system/vacuum）
		vacuumPct = flag.Int("vacuum-pct", 15, "空闲页占比超过该百分比时自动整理数据库（0=关闭）")

		minFreeMB = flag.Int("min-free-mb", 2048, "磁盘剩余空间保护阈值（MiB）：低于该值拒绝新下载并把排队任务自动暂停（0=关闭）")

		// 在线更新：默认跟踪 AnglerS888/video 的 main 分支。
		// 国内直连 github.com 经常失败，下载/检查都会自动回退到内置镜像；
		// 要换镜像或 fork 自建，用 -update-proxy / -update-repo 或对应环境变量覆盖。
		updateProxy = flag.String("update-proxy", "", "GitHub 镜像前缀，多个用逗号分隔（默认 ghproxy.net、gh-proxy.com；也可用 VDL_UPDATE_PROXY）")
		updateRepo  = flag.String("update-repo", "", "更新源仓库 owner/name（默认 AnglerS888/video；也可用 VDL_UPDATE_REPO）")
	)
	flag.Parse()

	if *ver {
		bt := "-"
		if t := version.Time(); !t.IsZero() {
			bt = t.Format("2006-01-02 15:04:05")
		}
		os.Stdout.WriteString(fmt.Sprintf("videodl %s\n  commit: %s\n  build : %s\n  go     : %s %s/%s\n  mode   : %s\n",
			version.Short(), orDash(version.Commit), bt, runtime.Version(), runtime.GOOS, runtime.GOARCH, version.ModeText()))
		return
	}

	// 日志级别：命令行 > 环境变量 > 默认 info
	if *logLvl == "" {
		*logLvl = os.Getenv("VDL_LOG_LEVEL")
	}
	if lv, ok := logx.ParseLevel(*logLvl); ok {
		logx.SetLevel(lv)
	} else {
		logx.Warnf("[startup] 无法识别 -log-level=%q，已回退为 info", *logLvl)
	}

	// 记录命令行真正显式指定的参数，低配模式只覆盖"没被显式指定"的项
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if *lowmem {
		preset := func(name string, dst *int, v int) {
			if !set[name] {
				*dst = v
			}
		}
		preset("pages", pages, 3)
		preset("sources", sources, 2)
		preset("dl-workers", dlWorkers, 1)
		preset("cover-workers", coverWorkers, 1)
		preset("cover-concurrency", coverConc, 2)
		preset("seg-concurrency", segConc, 6)
		preset("ffmpeg-threads", ffThreads, 1)
		preset("db-cache-mb", dbCacheMB, 24)
		preset("db-mmap-mb", dbMmapMB, 256)
		preset("db-conns", dbConns, 2)
		if !set["ffmpeg-reencode"] {
			*ffReencode = false
		}
		if !set["memlimit"] {
			*memMB = 768
		}
	}
	if *procs == 0 && *lowmem {
		n := runtime.NumCPU()
		if n > 3 {
			n = 3
		}
		*procs = n
	}
	if *procs > 0 {
		runtime.GOMAXPROCS(*procs)
	}
	// GC 软上限：把 Go 的堆控制在 NAS 可用内存之内（超了会触发更积极的回收，
	// 而不是让容器被内核 OOM kill）
	if *memMB > 0 {
		debug.SetMemoryLimit(int64(*memMB) << 20)
	}
	if *segBudget <= 0 {
		*segBudget = *dlWorkers * *segConc
	}

	// 数据目录：显式指定优先；否则按"已有数据库的目录"自动匹配，
	// 避免直接双击 exe / 在别的目录启动时新建空库（表现为没有影片也没有封面）
	resolved, dataWhy := resolveDataDir(*dataDir, set["data"])
	*dataDir = resolved
	// 统一转绝对路径：下载/合并过程中会生成 ffmpeg concat 清单，
	// 相对路径会被 ffmpeg 按"清单所在目录"二次拼接（曾导致 Impossible to open <路径重复>）
	if abs, aerr := filepath.Abs(*dataDir); aerr == nil {
		*dataDir = abs
	}

	logx.Infof("[startup] videodl %s | %s | %s | pid=%d", version.Short(), runtime.Version(), version.ModeText(), os.Getpid())
	logx.Infof("[startup] 监听 %s | 数据目录 %s（%s）", *addr, *dataDir, dataWhy)

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		logx.Fatalf("[startup] 创建数据目录失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(*dataDir, "videos"), 0o755); err != nil {
		logx.Fatalf("[startup] 创建视频目录失败: %v", err)
	}

	logx.Infof("[runtime] GOMAXPROCS=%d 内存软上限=%dMiB 低配模式=%v",
		runtime.GOMAXPROCS(0), *memMB, *lowmem)
	if *memMB == 0 {
		logx.Infof("[runtime] 提示：NAS 上建议加 -memlimit 768（或 -lowmem），让 GC 主动控制堆大小")
	}

	// ffmpeg：不再内嵌二进制，按 环境变量 → data/bin 缓存 → 系统 PATH 的顺序解析；
	// 都没有时不阻塞启动，由用户在「信息」页点下载，装好后回调这里补上路径。
	ffBin, ferr := ffmpeg.Ensure(*dataDir)
	if ferr != nil {
		logx.Warnf("[ffmpeg] 未检测到可用 ffmpeg（%v），合并将退化为纯 Go 方式；可在信息页点击下载", ferr)
	} else {
		downloader.Init(ffBin)
		// 超大封面（>12MP）交给 ffmpeg 在子进程里缩放，主进程不承担解码大图的像素缓存
		imgproc.SetFFmpegPath(ffBin)
		logx.Infof("[ffmpeg] 已就绪（来源=%s）: %s", ffmpeg.SourceOf(*dataDir), ffmpeg.Version(ffBin))
	}
	// 页面下载安装完成后，把新路径交给下载器与图片处理器（不用重启）
	ffmpeg.SetOnReady(func(p string) {
		downloader.Init(p)
		imgproc.SetFFmpegPath(p)
		logx.Infof("[ffmpeg] 下载完成并就绪: %s", ffmpeg.Version(p))
	})
	downloader.SetTranscodeOptions(*ffThreads, *ffReencode, *ffVerify)
	downloader.SetSegmentBudget(*segBudget)

	dbPath := filepath.Join(*dataDir, "videodl.db")
	database := db.InitDB(dbPath, db.Options{
		CacheMB:  *dbCacheMB,
		PageSize: *dbPageSize,
		MmapMB:   *dbMmapMB,
		Conns:    *dbConns,
	})
	logx.Infof("[db] 每连接页缓存 %d MiB × %d 连接，mmap %d MiB，自动整理阈值 空闲页>%d%%",
		*dbCacheMB, *dbConns, *dbMmapMB, *vacuumPct)
	col := collector.New(database)
	col.PageWorkers = *pages
	col.SourceWorkers = *sources
	// 封面压缩图目录：同步时预取到 data/img，页面访问直接读本地
	imgDir := filepath.Join(*dataDir, "img")
	col.CoverDir = imgDir
	collector.SetCoverConcurrency(*coverConc)
	if *imgProxy != "" {
		collector.SetImageProxy(*imgProxy)
	}
	logx.Infof("[cover] 封面缓存目录 %s（并发 %d，代理=%s）", imgDir, *coverConc, orNo(*imgProxy))
	col.StartCoverWorkers(*coverWorkers)
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		logx.Fatalf("[collect] 创建封面目录失败: %v", err)
	}
	logx.Infof("[collect] 并发配置：每接口 %d 个分页线程，最多同时采集 %d 个接口", col.PageWorkers, col.SourceWorkers)
	// 下载任务并发，文件保存到 data/videos
	collector.SetDownloadConcurrency(*segConc)
	collector.SetMinFreeBytes(int64(*minFreeMB) << 20)
	if *minFreeMB > 0 {
		logx.Infof("[download] 磁盘保护：剩余空间低于 %d MiB 时拒绝新下载并自动暂停队列", *minFreeMB)
	}
	col.StartDownloadWorkers(*dlWorkers, filepath.Join(*dataDir, "videos"))
	logx.Infof("[download] 并发配置：%d 个任务并行，每任务 %d 分片，全局分片上限 %d，ffmpeg 线程 %d（全量重编码=%v）",
		*dlWorkers, *segConc, *segBudget, *ffThreads, *ffReencode)

	// 定时增量采集：默认每 1 小时一次，只同步各接口前 5 页（最新数据）。
	// 只在「启用 + 自动增量」都打开的接口上跑；单个接口可以在接口页关掉自动增量。
	logx.Infof("[scheduler] 定时增量：每 %v 一次，每次同步前 %d 页（0=不限）", *incrEvery, *incrPages)
	sch := scheduler.New(*incrEvery, func() {
		srcs := col.AutoIncrSources()
		if len(srcs) == 0 {
			return // 所有接口都关了自动增量：不发请求，也不刷日志
		}
		started, skipped := col.RunIncremental(srcs, *incrPages)
		if started > 0 || len(skipped) > 0 {
			logx.Infof("[scheduler] 定时增量：启动 %d 个接口，跳过 %d 个", started, len(skipped))
		}
	})
	sch.Start()

	// 数据库整理：删接口、采集重插播放地址都会不断产生空闲页（SQLite 不会主动还回去），
	// 实测 915MB 的库里能有一半是空的。超阈值且服务空闲时自动 VACUUM 回收。
	// 延迟 2 分钟是为了避开启动时的下载复位/断点恢复；之后每 6 小时检查一次。
	db.StartAutoVacuum(database, *vacuumPct, 2*time.Minute, 6*time.Hour, func() bool {
		if len(collector.GlobalProgress.RunningIDs()) > 0 {
			return true
		}
		var active int
		_ = database.QueryRow(`SELECT COUNT(*) FROM vod_download
			WHERE status IN ('queued','downloading','merging','paused')`).Scan(&active)
		return active > 0
	})
	// 预热播放地址计数（226 万行的 COUNT 要走 1s+），别让第一个访问者撞上
	db.WarmupCounts(database)

	// 在线更新器：数据目录下 update/ 里放下载的源码与编译产物
	upRepo := *updateRepo
	if upRepo == "" {
		upRepo = os.Getenv("VDL_UPDATE_REPO")
	}
	if upRepo == "" {
		upRepo = version.Repo // 与 updater.New 的默认值保持一致，日志里不要打成空
	}
	upProxy := *updateProxy
	if upProxy == "" {
		upProxy = os.Getenv("VDL_UPDATE_PROXY")
	}
	var upMirrors []string
	for _, m := range strings.Split(upProxy, ",") {
		if m = strings.TrimSpace(m); m != "" {
			upMirrors = append(upMirrors, m)
		}
	}
	up := updater.New(*dataDir, upRepo, version.Branch, upMirrors)
	logx.Infof("[update] 在线更新：仓库 %s（分支 %s），运行方式 %s", upRepo, version.Branch, version.ModeText())

	// 容器里 docker stop / 重启会先发 SIGTERM：优雅退出，避免 SQLite 留下 -wal 尾巴、
	// 下载中的分片文件被硬切断（下次启动会按"已暂停"恢复）。
	// 提前创建：更新接口「重启并应用」也复用同一个 stop，走同一条优雅退出路径。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &web.Server{
		DB: database, Col: col, DataDir: *dataDir,
		Version: version.Version, StartedAt: time.Now(),
		VacuumPct: *vacuumPct,
		Up:        up, Shutdown: stop,
	}
	// 显式设置超时：不带超时的裸 ListenAndServe 会让半开连接一直占着 goroutine 和内存
	// （NAS 上被扫描/移动端切网时很常见）。写超时不设，避免中断大文件播放/下载。
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		<-ctx.Done()
		logx.Infof("[shutdown] 收到退出信号，停止接收新请求（最多等待 20s）…")
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(sctx); err != nil {
			logx.Warnf("[shutdown] HTTP 服务关闭超时: %v", err)
		}
	}()

	logx.Infof("视频采集服务已启动: http://localhost%s", *addr)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logx.Fatalf("[startup] 监听失败: %v", err)
	}

	sch.Stop()
	if err := database.Close(); err != nil {
		logx.Warnf("[shutdown] 关闭数据库失败: %v", err)
	} else {
		logx.Infof("[shutdown] 数据库已关闭，进程退出")
	}

	// 更新已替换二进制（Unix）：把当前进程直接换成新版本，端口不中断太久
	if up.RelaunchPending() {
		exe := up.ExePath()
		logx.Infof("[update] 正在用新版本重新启动: %s", exe)
		if err := relaunch(exe); err != nil {
			logx.Errorf("[update] 重新启动失败，请手动运行 %s：%v", exe, err)
		}
	}
}

// orDash 空字符串时显示 "-"
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// orNo 空字符串时显示 "无"，让启动日志更直观
func orNo(s string) string {
	if strings.TrimSpace(s) == "" {
		return "无"
	}
	return s
}

// defaultDataDir 默认数据目录名（SQLite、封面、下载文件、ffmpeg 缓存都在这里）
const defaultDataDir = "./videodata"

// legacyDataDirs 历史版本用过的目录名，启动时按顺序尝试迁移到 videodata：
// videos_data（上一版） → data（更早的版本）
var legacyDataDirs = []string{"videos_data", "data"}

// resolveDataDir 决定数据目录，并把历史目录名自动迁移到 videodata。
//
// 查找顺序（都指向 videodata）：
//  1. 当前目录 ./videodata
//  2. 程序所在目录/videodata
//  3. 程序上级目录/videodata（bin/videodl 配数据目录的常见布局）
//
// 某个位置存在旧的 videos_data / data 时（有 videodl.db 或非空），直接改名为 videodata；
// 改名失败（被占用/跨盘）就继续用旧目录，保证程序能起来。
func resolveDataDir(v string, explicit bool) (path string, why string) {
	if explicit {
		return v, "命令行 -data 指定"
	}
	type cand struct{ path, why string }
	cands := []cand{{defaultDataDir, "当前目录"}}
	if exe, err := os.Executable(); err == nil {
		if real, e2 := filepath.EvalSymlinks(exe); e2 == nil {
			exe = real
		}
		dir := filepath.Dir(exe)
		cands = append(cands,
			cand{filepath.Join(dir, "videodata"), "程序所在目录"},
			cand{filepath.Join(dir, "..", "videodata"), "程序上级目录"},
		)
	}

	// 1) 已经存在 videodata：直接用（优先含数据库的）
	for _, c := range cands {
		if fi, err := os.Stat(filepath.Join(c.path, "videodl.db")); err == nil && !fi.IsDir() {
			return c.path, c.why + "，已存在数据库"
		}
	}
	for _, c := range cands {
		if fi, err := os.Stat(c.path); err == nil && fi.IsDir() {
			return c.path, c.why + "，已存在数据目录"
		}
	}

	// 2) 没有 videodata，但旁边有旧目录（videos_data / data）：迁移过来
	for _, c := range cands {
		for _, old := range legacyDataDirs {
			legacy := filepath.Join(filepath.Dir(c.path), old)
			if !looksLikeDataDir(legacy) {
				continue
			}
			rerr := os.Rename(legacy, c.path)
			if rerr == nil {
				logx.Infof("[startup] 数据目录已迁移：%s -> %s", legacy, c.path)
				return c.path, c.why + "，已从旧目录 " + old + " 迁移"
			}
			logx.Warnf("[startup] 数据目录迁移失败（%v），继续使用旧目录 %s", rerr, legacy)
			return legacy, c.why + "，旧目录 " + old + "（迁移失败）"
		}
	}
	return v, "新建目录"
}

// looksLikeDataDir 目录是否存在且看着像数据目录（有库文件，或不是空目录）
func looksLikeDataDir(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || !fi.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(p, "videodl.db")); err == nil {
		return true
	}
	ents, err := os.ReadDir(p)
	return err == nil && len(ents) > 0
}

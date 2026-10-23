package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	// 把 IANA 时区库编进二进制（约 450KB）：运行镜像不再需要 apk 装 tzdata，
	// TZ=Asia/Shanghai 在精简/无 tzdata 的容器里也能正确解析（2026-10-21 起）
	_ "time/tzdata"

	"videoDownLoad/internal/collector"
	"videoDownLoad/internal/config"
	"videoDownLoad/internal/db"
	"videoDownLoad/internal/downloader"
	"videoDownLoad/internal/ffmpeg"
	"videoDownLoad/internal/imgproc"
	"videoDownLoad/internal/logx"
	"videoDownLoad/internal/scheduler"
	"videoDownLoad/internal/sysinfo"
	"videoDownLoad/internal/updater"
	"videoDownLoad/internal/version"
	"videoDownLoad/internal/web"
)

func main() {
	// 唯一保留的命令行开关：打印版本。所有配置都写在 config.yml 里
	// （路径可用环境变量 VDL_CONFIG 指定），不再有 -addr / -data 这类参数。
	// 旧版本用 -addr / -data / -lowmem 等参数，现在全部搬进 config.yml：
	// 传了旧参数就给出明确提示，而不是甩一句 "flag provided but not defined"。
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" || a == "-h" || a == "--help" {
			continue
		}
		if len(a) > 0 && a[0] == '-' {
			fmt.Fprintf(os.Stderr, "命令行参数已废弃：全部配置都在 config.yml（可用 VDL_CONFIG 指定路径）。\n未知参数：%s\n", a)
			os.Exit(2)
		}
	}
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		bt := "-"
		if t := version.Time(); !t.IsZero() {
			bt = t.Format("2006-01-02 15:04:05")
		}
		os.Stdout.WriteString(fmt.Sprintf("videodl %s\n  commit: %s\n  build : %s\n  go     : %s %s/%s\n  mode   : %s\n",
			version.Short(), orDash(version.Commit), bt, runtime.Version(), runtime.GOOS, runtime.GOARCH, version.ModeText()))
		return
	}

	// 读取配置：找不到会自动生成一份默认 config.yml
	cfg, cfgPath, cfgErr := config.Load()
	if cfgErr != nil {
		logx.Warnf("[config] %v（已回退为默认配置）", cfgErr)
	}

	if lv, ok := logx.ParseLevel(cfg.LogLevel); ok {
		logx.SetLevel(lv)
	} else {
		logx.Warnf("[config] 无法识别 log_level=%q，已回退为 info", cfg.LogLevel)
	}

	// GOMAXPROCS / GC 软上限：lowmem 模式已在配置默认值里调好
	if cfg.MaxProcs > 0 {
		runtime.GOMAXPROCS(cfg.MaxProcs)
	}
	if cfg.MemLimitMB > 0 {
		debug.SetMemoryLimit(int64(cfg.MemLimitMB) << 20)
	}
	// 全局分片并发：0 = 单任务并发 × 任务数
	segBudget := cfg.Download.SegBudget
	if segBudget <= 0 {
		segBudget = cfg.Download.Workers * cfg.Download.SegConcurrency
	}
	incrEvery := cfg.IncrEveryDuration()

	// 数据目录：配置里显式写了就用它；否则按"已有数据库的目录"自动匹配，
	// 避免直接双击 exe / 在别的目录启动时新建空库（表现为没有影片也没有封面）
	dataDir, dataWhy := resolveDataDir(cfg.DataDir, cfg.DataDirExplicit())
	// 统一转绝对路径：下载/合并过程中会生成 ffmpeg concat 清单，
	// 相对路径会被 ffmpeg 按"清单所在目录"二次拼接（曾导致 Impossible to open <路径重复>）
	if abs, aerr := filepath.Abs(dataDir); aerr == nil {
		dataDir = abs
	}

	logx.Infof("[startup] videodl %s | %s | %s | pid=%d", version.Short(), runtime.Version(), version.ModeText(), os.Getpid())
	logx.Infof("[startup] 配置文件 %s", orDash(cfgPath))
	logx.Infof("[startup] 监听 %s | 数据目录 %s（%s）", cfg.Server.Addr, dataDir, dataWhy)

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		logx.Fatalf("[startup] 创建数据目录失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "videos"), 0o755); err != nil {
		logx.Fatalf("[startup] 创建视频目录失败: %v", err)
	}

	// 页面可改的配置（采集 / 下载 / 封面 / ffmpeg）单独放在数据目录的 ui.yml：
	// 跟着数据走（换机器/换容器只要带上数据目录），config.yml 只剩需要重启的项。
	// 第一次运行没有 ui.yml 时按当前生效值生成一份；旧版 ui.json 会自动迁移。
	uiPath := config.UIFilePath(dataDir)
	switch err := config.LoadUI(uiPath, cfg); {
	case err == nil:
		logx.Infof("[config] 页面配置 %s", uiPath)
	case errors.Is(err, os.ErrNotExist):
		legacy := config.LegacyUIFilePath(dataDir)
		if e := config.LoadUI(legacy, cfg); e == nil {
			if e2 := config.SaveUI(uiPath, cfg); e2 != nil {
				logx.Warnf("[config] 从 %s 迁移失败: %v", legacy, e2)
			} else {
				logx.Infof("[config] 页面配置已从 %s 迁移到 %s", legacy, uiPath)
			}
		} else if e2 := config.SaveUI(uiPath, cfg); e2 != nil {
			logx.Warnf("[config] 生成页面配置 %s 失败: %v", uiPath, e2)
		} else {
			logx.Infof("[config] 页面配置已生成 %s", uiPath)
		}
	default:
		logx.Warnf("[config] 读取页面配置 %s 失败（用默认值）: %v", uiPath, err)
	}

	// 老版本的 config.yml 里可能还留着 collect/download/cover/ffmpeg 或 ui 段，
	// 现在这些只认 ui.yml：这里把它们从 config.yml 里摘掉（旧文件备份 .bak）。
	if cfgPath != "" {
		switch changed, err := config.CleanUIFromConfig(cfgPath, cfg); {
		case err != nil:
			logx.Warnf("[config] 清理 %s 里的页面配置失败: %v", cfgPath, err)
		case changed:
			logx.Infof("[config] 已从 %s 移除页面配置（只保留 advanced 段），页面项以 %s 为准", cfgPath, uiPath)
		}
	}

	logx.Infof("[runtime] GOMAXPROCS=%d 内存软上限=%dMiB 低配模式=%v",
		runtime.GOMAXPROCS(0), cfg.MemLimitMB, cfg.LowMem)
	if cfg.MemLimitMB == 0 {
		logx.Infof("[runtime] 提示：NAS 上建议把 mem_limit_mb 设为 768（或把 lowmem 设为 true），让 GC 主动控制堆大小")
	}

	// ffmpeg：不再内嵌二进制，按 环境变量 → data/bin 缓存 → 系统 PATH 的顺序解析；
	// 都没有时不阻塞启动，由用户在「信息」页点下载，装好后回调这里补上路径。
	ffBin, ferr := ffmpeg.Ensure(dataDir)
	if ferr != nil {
		logx.Warnf("[ffmpeg] 未检测到可用 ffmpeg（%v），合并将退化为纯 Go 方式；可在信息页点击下载", ferr)
	} else {
		downloader.Init(ffBin)
		// 超大封面（>12MP）交给 ffmpeg 在子进程里缩放，主进程不承担解码大图的像素缓存
		imgproc.SetFFmpegPath(ffBin)
		logx.Infof("[ffmpeg] 已就绪（来源=%s）: %s", ffmpeg.SourceOf(dataDir), ffmpeg.Version(ffBin))
	}
	// 页面下载安装完成后，把新路径交给下载器与图片处理器（不用重启）
	ffmpeg.SetOnReady(func(p string) {
		downloader.Init(p)
		imgproc.SetFFmpegPath(p)
		logx.Infof("[ffmpeg] 下载完成并就绪: %s", ffmpeg.Version(p))
	})
	downloader.SetTranscodeOptions(cfg.FFmpeg.Threads, cfg.FFmpeg.Reencode, cfg.FFmpeg.Verify)
	downloader.SetSegmentBudget(segBudget)

	// 容器里 docker stop / 重启会先发 SIGTERM：优雅退出，避免 SQLite 留下 -wal 尾巴、
	// 下载中的分片文件被硬切断（下次启动会按"已暂停"恢复）。
	// 提前创建（在起 worker 之前）：信号注册晚于 worker 启动的话，
	// 启动阶段收到 Ctrl-C 就没有退路，只能被硬杀。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 后台 goroutine 的停止信号：关库之前先让它们停下来（否则它们会访问已关闭的连接池）
	bgStop := make(chan struct{})
	var bgStopOnce sync.Once
	stopBG := func() { bgStopOnce.Do(func() { close(bgStop) }) }
	defer stopBG()

	// 端口预检：先探一下端口能不能 bind。
	// 原来直接 ListenAndServe，端口被占时是走到最后一步才 Fatalf，
	// 那时库已经建好、迁移也跑过（可能触发全库 VACUUM），白跑一遍还留下 -wal/-shm。
	ln, lerr := net.Listen("tcp", cfg.Server.Addr)
	if lerr != nil {
		logx.Fatalf("[startup] 监听 %s 失败: %v（端口可能已被占用）", cfg.Server.Addr, lerr)
	}
	defer func() { _ = ln.Close() }()

	dbPath := filepath.Join(dataDir, "videodl.db")
	database := db.InitDB(dbPath, db.Options{
		CacheMB:  cfg.DB.CacheMB,
		PageSize: cfg.DB.PageSize,
		MmapMB:   cfg.DB.MmapMB,
		Conns:    cfg.DB.Conns,
	})
	// 关库兜底：后面任何一步 Fatalf 退出，defer 也能把 SQLite 正常关掉（不留 -wal/-shm）。
	// 更新后的原地重启（exec）不会执行 defer，所以收尾路径会先显式调一次 closeDB。
	dbClosed := false
	closeDB := func() {
		if dbClosed {
			return
		}
		dbClosed = true
		if err := database.Close(); err != nil {
			logx.Warnf("[shutdown] 关闭数据库失败: %v", err)
		} else {
			logx.Infof("[shutdown] 数据库已关闭")
		}
	}
	defer closeDB()
	logx.Infof("[db] 每连接页缓存 %d MiB × %d 连接，mmap %d MiB，自动整理阈值 空闲页>%d%%",
		cfg.DB.CacheMB, cfg.DB.Conns, cfg.DB.MmapMB, cfg.DB.VacuumPct)
	col := collector.New(database)
	col.SetWorkers(cfg.Collect.Pages, cfg.Collect.Sources)
	// 封面压缩图目录：同步时预取到 data/img，页面访问直接读本地
	imgDir := filepath.Join(dataDir, "img")
	// 目录要在起封面 worker 之前建好（worker 一启动就可能往里写）
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		logx.Fatalf("[collect] 创建封面目录失败: %v", err)
	}
	col.CoverDir = imgDir
	collector.SetCoverConcurrency(cfg.Cover.Concurrency)
	if cfg.Cover.ImgProxy != "" {
		collector.SetImageProxy(cfg.Cover.ImgProxy)
	}
	logx.Infof("[cover] 封面缓存目录 %s（并发 %d，代理=%s）", imgDir, cfg.Cover.Concurrency, orNo(cfg.Cover.ImgProxy))
	col.StartCoverWorkers(cfg.Cover.Workers)
	logx.Infof("[collect] 并发配置：每接口 %d 个分页线程，最多同时采集 %d 个接口", col.PageWorkersCount(), col.SourceWorkersCount())
	// 下载任务并发，文件保存到 data/videos
	collector.SetDownloadConcurrency(cfg.Download.SegConcurrency)
	collector.SetMinFreeBytes(int64(cfg.Download.MinFreeMB) << 20)
	if cfg.Download.MinFreeMB > 0 {
		logx.Infof("[download] 磁盘保护：剩余空间低于 %d MiB 时拒绝新下载并自动暂停队列", cfg.Download.MinFreeMB)
	}
	col.StartDownloadWorkers(cfg.Download.Workers, filepath.Join(dataDir, "videos"))
	logx.Infof("[download] 并发配置：%d 个任务并行，每任务 %d 分片，全局分片上限 %d，ffmpeg 线程 %d（全量重编码=%v）",
		cfg.Download.Workers, cfg.Download.SegConcurrency, segBudget, cfg.FFmpeg.Threads, cfg.FFmpeg.Reencode)

	// 定时增量采集：默认每 1 小时一次，只同步各接口前 5 页（最新数据）。
	// 只在「启用 + 自动增量」都打开的接口上跑；单个接口可以在接口页关掉自动增量。
	logx.Infof("[scheduler] 定时增量：每 %v 一次，每次同步前 %d 页（0=不限）", incrEvery, cfg.Collect.IncrPages)
	// 增量页数支持热更新：调度协程每次触发时读原子量，信息页保存后立即生效
	var incrPages atomic.Int32
	incrPages.Store(int32(cfg.Collect.IncrPages))
	sch := scheduler.New(incrEvery, func() {
		srcs := col.AutoIncrSources()
		if len(srcs) == 0 {
			return // 所有接口都关了自动增量：不发请求，也不刷日志
		}
		started, queued, skipped := col.RunIncremental(srcs, int(incrPages.Load()), "auto")
		if started > 0 || queued > 0 || len(skipped) > 0 {
			logx.Infof("[scheduler] 定时增量：启动 %d 个接口，排队 %d 个，跳过 %d 个", started, queued, len(skipped))
		}
	})
	sch.Start()

	// 信息页「保存即生效」：把不用重启就能改的项直接套到运行中的组件上
	// （采集 / 下载 / 合并转码）。server、data_dir、log_level、lowmem、封面、数据库
	// 这些只能在启动时一次定死，仍然需要改配置文件后重启。
	hotApply := func(c *config.Config) []string {
		col.SetWorkers(c.Collect.Pages, c.Collect.Sources)
		incrPages.Store(int32(c.Collect.IncrPages))
		sch.SetInterval(c.IncrEveryDuration())

		collector.SetDownloadConcurrency(c.Download.SegConcurrency)
		collector.SetMinFreeBytes(int64(c.Download.MinFreeMB) << 20)
		budget := c.Download.SegBudget
		if budget <= 0 {
			budget = c.Download.Workers * c.Download.SegConcurrency
		}
		downloader.SetSegmentBudget(budget)
		downloader.SetTranscodeOptions(c.FFmpeg.Threads, c.FFmpeg.Reencode, c.FFmpeg.Verify)

		// 封面：并发张数 / 单站点并发 / 代理都是运行时变量，改完立即生效
		collector.SetCoverWorkers(c.Cover.Workers)
		collector.SetCoverConcurrency(c.Cover.Concurrency)
		collector.SetImageProxy(c.Cover.ImgProxy)

		logx.Infof("[config] 已热更新：采集 %d 分页 × %d 接口 / 增量每 %v 前 %d 页 | 下载分片并发 %d，全局上限 %d，磁盘保护 %d MiB | 封面 %d 任务 × %d 并发（代理=%s）| ffmpeg 线程 %d（全量重编码=%v，校验=%v）",
			c.Collect.Pages, c.Collect.Sources, c.IncrEveryDuration(), c.Collect.IncrPages,
			c.Download.SegConcurrency, budget, c.Download.MinFreeMB,
			c.Cover.Workers, c.Cover.Concurrency, orNo(c.Cover.ImgProxy),
			c.FFmpeg.Threads, c.FFmpeg.Reencode, c.FFmpeg.Verify)
		return []string{"collect", "download", "cover", "ffmpeg"}
	}

	// 数据库整理：删接口、采集重插播放地址都会不断产生空闲页（SQLite 不会主动还回去），
	// 实测 915MB 的库里能有一半是空的。超阈值且服务空闲时自动 VACUUM 回收。
	// 延迟 2 分钟是为了避开启动时的下载复位/断点恢复；之后每 6 小时检查一次。
	db.StartAutoVacuum(database, cfg.DB.VacuumPct, 2*time.Minute, 6*time.Hour, func() bool {
		if len(collector.GlobalProgress.RunningIDs()) > 0 {
			return true
		}
		var active int
		_ = database.QueryRow(`SELECT COUNT(*) FROM vod_download
			WHERE status IN ('queued','downloading','merging','paused')`).Scan(&active)
		return active > 0
	}, bgStop)
	// 预热播放地址计数（226 万行的 COUNT 要走 1s+），别让第一个访问者撞上
	db.WarmupCounts(database, bgStop)
	// 预热数据目录体积统计：封面几十万小文件，首次遍历要几秒到十几秒，
	// 先在后台跑完，别让第一个打开「信息」页的请求同步等全盘扫描
	go sysinfo.DirUsageOf(dataDir)

	// 在线更新器：更新源固定为 Gitee（anglers/video@master），配置里不再保留 update 段。
	// 匿名下载偶尔被 Gitee 要求验证，可用环境变量 VDL_UPDATE_TOKEN 提供私人令牌。
	up := updater.New(dataDir, version.Repo, version.Branch, os.Getenv("VDL_UPDATE_TOKEN"))
	logx.Infof("[update] 在线更新：gitee // %s（分支 %s），运行方式 %s",
		version.Repo, version.Branch, version.ModeText())

	srv := &web.Server{
		DB: database, Col: col, DataDir: dataDir,
		Version: version.Version, StartedAt: time.Now(), ConfigPath: cfgPath,
		VacuumPct: cfg.DB.VacuumPct,
		Up:        up,
		Sch:       sch,
		HotApply:  hotApply,
	}
	// 访问密码（ui.yml 里的 auth.password）：空 = 不启用；
	// 设置后站点要登录，接口要带 Cookie / X-Access-Password / ?password=
	srv.SetAccessPassword(cfg.Auth.Password)
	// 显式设置超时：不带超时的裸 ListenAndServe 会让半开连接一直占着 goroutine 和内存
	// （NAS 上被扫描/移动端切网时很常见）。写超时不设，避免中断大文件播放/下载。
	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		<-ctx.Done()
		logx.Infof("[shutdown] 收到退出信号，停止接收新请求（最多等待 15s）…")
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(sctx); err != nil {
			logx.Warnf("[shutdown] HTTP 服务关闭超时: %v", err)
		}
	}()

	logx.Infof("视频采集服务已启动: http://localhost%s", cfg.Server.Addr)
	// 服务真的起来了 → 清掉回滚哨兵（说明这次自更新成功，没有崩）
	updater.MarkHealthy(dataDir)
	// 用前面预检时建好的 listener：端口占用在启动早期就暴露出来，
	// 不会再等到 ListenAndServe 才报错（那时库已经建好、迁移也跑过了）。
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logx.Fatalf("[startup] 监听失败: %v", err)
	}

	// 退出顺序：先停后台 goroutine（不再访问 DB）→ 再停调度器 → 最后关库（defer 里做）。
	// 反过来（先关库）会让还在跑的 goroutine 拿到 "sql: database is closed"。
	stopBG()
	sch.Stop()
	// exec 替换进程后 defer 不会再执行：先真正关库（WAL 正常收尾），
	// 避免新进程启动时接手一个没关干净的 SQLite 连接。
	closeDB()
	if updater.RestartRequested() {
		// 更新触发：用新二进制原地 exec，PID 不变、容器不退出，
		// 不依赖 docker 的 restart 策略（手动 docker run 没配 --restart 也能更新）。
		if err := updater.Relaunch(); err != nil {
			logx.Warnf("[update] 原地重启失败（%v），改为退出进程，交给容器重启策略", err)
		}
	}
	logx.Infof("[shutdown] 进程退出")
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
		return v, "配置文件 data_dir 指定"
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

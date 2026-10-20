package collector

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"videoDownLoad/internal/db"
	"videoDownLoad/internal/downloader"
	"videoDownLoad/internal/imgproc"
	"videoDownLoad/internal/logx"
	"videoDownLoad/internal/sysinfo"
)

// ============================================================================
// 五、下载队列
//
// 下载粒度 = 每一集一个独立 mp4（勾选多集时会批量创建多条任务，每集一条记录）。
// 支持 暂停 / 继续 / 删除：
//   - 暂停：置取消标记，worker 在当前分片结束后停下，已下好的分片保留（<out>.segs 复用）
//   - 继续：重新入队，跳过已下好的分片续传
//   - 删除：删除记录 + 该集 mp4 + 压缩封面 + 中间文件
// ============================================================================

type DownloadTask struct {
	ID         int64   `json:"id"`
	VodID      int64   `json:"vod_id"`
	VodName    string  `json:"vod_name"`
	SourceName string  `json:"source_name"`
	PlayFrom   string  `json:"play_from"`
	EpIndex    int     `json:"ep_index"`
	Status     string  `json:"status"`   // queued/downloading/paused/done/failed
	Priority   int     `json:"priority"` // 1=用户点了「优先下载」
	Percent    int     `json:"percent"`
	Speed      string  `json:"speed,omitempty"`
	Error      string  `json:"error,omitempty"`
	File       string  `json:"file,omitempty"`
	SizeMB     float64 `json:"size_mb"`
	Episodes   int     `json:"episodes"`
	VodPic     string  `json:"vod_pic,omitempty"` // 影视封面（用于已下载文件夹展示）
	UpdatedAt  string  `json:"updated_at,omitempty"`
}

var (
	dlQueue = make(chan int64, 500)
	// dlPriWake 「优先下载」的唤醒信号：worker 平时阻塞在 dlQueue 上，
	// 用户点优先时发一个信号让它立刻改去看数据库里的优先任务。
	dlPriWake    = make(chan struct{}, 1)
	dlActive     sync.Map // downloadID -> struct{}（防止暂停后立刻继续导致两个 goroutine 抢同一批文件）
	dlCtl        sync.Map // downloadID -> *atomic.Bool（true=请求取消/暂停）
	dlMu         sync.Mutex
	downloadDir  string
	collectorRef *Collector

	// dlSegConcurrency 单个下载任务的分片并发数。
	// 弱 CPU 的 NAS 上默认 16 会让"2 个任务 × 16 分片"同时吃满带宽与内存，
	// 由 main 按机器规格下调（-seg-concurrency）。
	dlSegConcurrency = 16

	// dlMinFreeBytes 磁盘剩余空间保护阈值（0=关闭）：
	// 低于它时拒绝新建下载任务，已排队的任务在起跑前自动暂停，
	// 防止一部剧把数据盘彻底写满（盘满后 SQLite 也会跟着写失败）。
	dlMinFreeBytes int64
)

// SetMinFreeBytes 设置磁盘保护阈值（字节，0=关闭），main 启动时调用
func SetMinFreeBytes(n int64) { dlMinFreeBytes = n }

// MinFreeBytes 当前磁盘保护阈值（字节），信息页展示/告警用
func MinFreeBytes() int64 { return dlMinFreeBytes }

// diskSpaceLow 数据盘剩余空间是否已低于保护阈值。
// 平台不支持读取（返回 0）时不误伤，当作空间充足。
func diskSpaceLow() (uint64, bool) {
	if dlMinFreeBytes <= 0 || downloadDir == "" {
		return 0, false
	}
	_, free := sysinfo.DiskOf(downloadDir)
	if free == 0 {
		return 0, false
	}
	return free, free < uint64(dlMinFreeBytes)
}

// SetDownloadConcurrency 设置单任务分片并发（main 调用）
func SetDownloadConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	if n > 64 {
		n = 64
	}
	dlSegConcurrency = n
}

func dlControl(id int64) *atomic.Bool {
	if v, ok := dlCtl.Load(id); ok {
		return v.(*atomic.Bool)
	}
	b := &atomic.Bool{}
	dlCtl.Store(id, b)
	return b
}

// taskTempDir 单任务中间文件目录（暂停后保留，续传复用）
func taskTempDir(id int64) string {
	return filepath.Join(downloadDir, fmt.Sprintf(".dl_%d", id))
}

func epFileName(vodName, playFrom string, epIndex int) string {
	return downloader.Sanitize(vodName) + "_" + downloader.Sanitize(playFrom) +
		fmt.Sprintf("_第%02d集", epIndex+1) + ".mp4"
}

// EnqueueDownload 单集下载
func EnqueueDownload(vodID int64, playFrom string, epIndex int) (int64, error) {
	created, skipped, err := EnqueueDownloadBatch(vodID, playFrom, []int{epIndex})
	if err != nil {
		return 0, err
	}
	if created == 0 {
		return 0, fmt.Errorf("该集已下载或正在下载")
	}
	_ = skipped
	// 取刚创建的任务 id
	var id int64
	collectorRef.DB.QueryRow(`SELECT id FROM vod_download WHERE vod_id=? AND play_from=? AND ep_index=?`,
		vodID, playFrom, epIndex).Scan(&id)
	return id, nil
}

// EnqueueDownloadBatch 批量创建下载任务：每个勾选的集一条记录（每集一个独立 mp4）。
// epIndices 为空表示全部集。已下载完成/正在下载的集会跳过。
// 返回 (新创建的任务数, 跳过的集数, 错误)
func EnqueueDownloadBatch(vodID int64, playFrom string, epIndices []int) (int, int, error) {
	if collectorRef == nil {
		return 0, 0, fmt.Errorf("下载服务未就绪")
	}
	// 磁盘保护：剩余空间已低于阈值时直接拒绝，别等下到一半才把盘写满
	if free, low := diskSpaceLow(); low {
		return 0, 0, fmt.Errorf("磁盘剩余空间不足（%s），低于保护阈值 %s，请先清理空间再下载",
			db.HumanBytes(int64(free)), db.HumanBytes(dlMinFreeBytes))
	}
	c := collectorRef
	var vodName, srcName string
	var srcID int64
	if err := c.DB.QueryRow(`SELECT v.vod_name, v.source_id, COALESCE(s.name,'')
		FROM vod_info v LEFT JOIN vod_source s ON s.id=v.source_id WHERE v.id=?`, vodID).
		Scan(&vodName, &srcID, &srcName); err != nil {
		return 0, 0, fmt.Errorf("视频不存在")
	}

	// 勾选为空 = 全部集
	if len(epIndices) == 0 {
		rows, err := c.DB.Query(`SELECT ep_index FROM vod_play WHERE vod_id=? AND play_from=? AND path<>''`,
			vodID, playFrom)
		if err != nil {
			return 0, 0, err
		}
		for rows.Next() {
			var idx int
			if err := rows.Scan(&idx); err == nil {
				epIndices = append(epIndices, idx)
			}
		}
		rows.Close()
		if len(epIndices) == 0 {
			return 0, 0, fmt.Errorf("该播放源没有可用地址")
		}
	}
	// 去重
	seen := map[int]bool{}
	var idxs []int
	for _, x := range epIndices {
		if x >= 0 && !seen[x] {
			seen[x] = true
			idxs = append(idxs, x)
		}
	}

	dlMu.Lock()
	defer dlMu.Unlock()
	created, skipped := 0, 0
	var toQueue []int64
	for _, idx := range idxs {
		var cnt int
		c.DB.QueryRow(`SELECT COUNT(*) FROM vod_play WHERE vod_id=? AND play_from=? AND ep_index=? AND path<>''`,
			vodID, playFrom, idx).Scan(&cnt)
		if cnt == 0 {
			skipped++
			continue
		}
		var id int64
		var status, file string
		err := c.DB.QueryRow(`SELECT id, status, file_path FROM vod_download
			WHERE vod_id=? AND play_from=? AND ep_index=?`, vodID, playFrom, idx).Scan(&id, &status, &file)
		switch {
		case err == sql.ErrNoRows:
			res, ierr := c.DB.Exec(`INSERT INTO vod_download(vod_id, vod_name, source_id, source_name, play_from, ep_index, status, progress)
				VALUES(?,?,?,?,?,?,'queued',0)`, vodID, vodName, srcID, srcName, playFrom, idx)
			if ierr != nil {
				return created, skipped, ierr
			}
			id, _ = res.LastInsertId()
			created++
		case err != nil:
			return created, skipped, err
		default:
			if status == "done" && file != "" {
				if _, serr := os.Stat(file); serr == nil {
					skipped++ // 已下载完成
					continue
				}
			}
			if status == "downloading" || status == "queued" {
				skipped++ // 已在下载中
				continue
			}
			c.DB.Exec(`UPDATE vod_download SET status='queued', error='', updated_at=CURRENT_TIMESTAMP WHERE id=?`, id)
			created++
		}
		toQueue = append(toQueue, id)
	}
	for _, id := range toQueue {
		dlControl(id).Store(false)
		select {
		case dlQueue <- id:
		default:
		}
	}
	return created, skipped, nil
}

// PauseDownload 暂停下载（保留已下载部分）
func PauseDownload(id int64) error {
	if collectorRef == nil {
		return fmt.Errorf("服务未就绪")
	}
	var st string
	if err := collectorRef.DB.QueryRow(`SELECT status FROM vod_download WHERE id=?`, id).Scan(&st); err != nil {
		return fmt.Errorf("下载记录不存在")
	}
	if st != "downloading" && st != "queued" {
		return fmt.Errorf("当前状态不可暂停（%s）", statusText(st))
	}
	dlControl(id).Store(true)
	collectorRef.setDl(id, `status='paused', speed=''`)
	if st == "queued" {
		collectorRef.setDl(id, `status='paused', error=''`)
	}
	return nil
}

// ResumeDownload 继续下载（跳过已下好的分片）
func ResumeDownload(id int64) error {
	if collectorRef == nil {
		return fmt.Errorf("服务未就绪")
	}
	var st string
	if err := collectorRef.DB.QueryRow(`SELECT status FROM vod_download WHERE id=?`, id).Scan(&st); err != nil {
		return fmt.Errorf("下载记录不存在")
	}
	if st != "paused" && st != "failed" {
		return fmt.Errorf("当前状态不可继续（%s）", statusText(st))
	}
	// 等待旧 worker 真正退出（最多 5 秒）
	for i := 0; i < 50; i++ {
		if _, busy := dlActive.Load(id); !busy {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, busy := dlActive.Load(id); busy {
		return fmt.Errorf("任务正在停止中，请稍后再点继续")
	}

	dlMu.Lock()
	defer dlMu.Unlock()
	dlControl(id).Store(false)
	collectorRef.setDl(id, `status='queued', error='', updated_at=CURRENT_TIMESTAMP`)
	select {
	case dlQueue <- id:
		return nil
	default:
		return fmt.Errorf("下载队列已满")
	}
}

// RetryDownload 重试失败的任务：只接受 failed，重置为 queued 重新排队。
// 与 ResumeDownload 共用一个入口实现（已下好的分片会复用，不会从头再下），
// 单独成函数是为了让「重试」语义明确：暂停的任务走继续，失败的任务走重试。
func RetryDownload(id int64) error {
	if collectorRef == nil {
		return fmt.Errorf("服务未就绪")
	}
	var st string
	if err := collectorRef.DB.QueryRow(`SELECT status FROM vod_download WHERE id=?`, id).Scan(&st); err != nil {
		return fmt.Errorf("下载记录不存在")
	}
	if st != "failed" {
		return fmt.Errorf("只有失败的任务才能重试（当前：%s）", statusText(st))
	}
	return ResumeDownload(id)
}

func statusText(s string) string {
	switch s {
	case "queued":
		return "排队中"
	case "downloading":
		return "下载中"
	case "merging":
		return "合并中"
	case "paused":
		return "已暂停"
	case "done":
		return "已完成"
	case "failed":
		return "失败"
	}
	return s
}

// SnapshotDownloads 从数据库读全部下载记录（刷新页面不丢进度）
func SnapshotDownloads() []DownloadTask {
	if collectorRef == nil {
		return nil
	}
	// LEFT JOIN vod_info 取封面：已下载列表按剧名折叠后直接显示影视封面
	rows, err := collectorRef.DB.Query(`SELECT d.id, d.vod_id, d.vod_name, d.source_name, d.play_from, d.ep_index, d.status,
		d.progress, d.speed, d.error, d.file_path, d.episodes, d.updated_at, COALESCE(v.vod_pic,''), d.priority
		FROM vod_download d LEFT JOIN vod_info v ON v.id = d.vod_id
		ORDER BY CASE d.status WHEN 'downloading' THEN 0 WHEN 'queued' THEN 1 WHEN 'paused' THEN 2 ELSE 3 END,
		CASE WHEN d.priority=1 THEN 0 ELSE 1 END, d.updated_at DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []DownloadTask{}
	for rows.Next() {
		var t DownloadTask
		var file string
		var size int64
		if err := rows.Scan(&t.ID, &t.VodID, &t.VodName, &t.SourceName, &t.PlayFrom, &t.EpIndex, &t.Status,
			&t.Percent, &t.Speed, &t.Error, &file, &t.Episodes, &t.UpdatedAt, &t.VodPic, &t.Priority); err == nil {
			if fi, serr := os.Stat(file); serr == nil {
				size = fi.Size()
			}
			t.File = file
			t.SizeMB = float64(size) / 1024 / 1024
			out = append(out, t)
		}
	}
	return out
}

// DeleteDownload 删除下载任务：记录 + 该集 mp4 + 压缩封面 + 中间文件
func DeleteDownload(id int64) error {
	if collectorRef == nil {
		return fmt.Errorf("服务未就绪")
	}
	c := collectorRef
	var cur string
	if err := c.DB.QueryRow(`SELECT status FROM vod_download WHERE id=?`, id).Scan(&cur); err != nil {
		return fmt.Errorf("下载记录不存在")
	}
	if cur == "downloading" || cur == "queued" || cur == "paused" {
		dlControl(id).Store(true)
		for i := 0; i < 40; i++ { // 最多等 4 秒让 worker 停下
			var st string
			if err := c.DB.QueryRow(`SELECT status FROM vod_download WHERE id=?`, id).Scan(&st); err == nil && st != "downloading" {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	dlMu.Lock()
	defer dlMu.Unlock()

	var file, vodName, playFrom string
	var epIndex int
	if err := c.DB.QueryRow(`SELECT COALESCE(file_path,''), vod_name, play_from, ep_index
		FROM vod_download WHERE id=?`, id).Scan(&file, &vodName, &playFrom, &epIndex); err != nil {
		return fmt.Errorf("下载记录不存在")
	}
	if file != "" && !filepath.IsAbs(file) { // 兼容旧版本相对路径
		if wd, werr := os.Getwd(); werr == nil {
			file = filepath.Join(wd, file)
		}
	}
	// 未完成的任务 file_path 为空，按命名规则推出产物路径：
	// 否则删任务会残留 <片名>_<线路>_第NN集.mp4 / 同名 .segs 分片目录
	targets := []string{}
	if file != "" {
		targets = append(targets, file)
	} else if vodName != "" && downloadDir != "" {
		targets = append(targets, filepath.Join(downloadDir, epFileName(vodName, playFrom, epIndex)))
	}
	for _, f := range targets {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除本地文件失败（可能正在播放，请先关闭播放器）: %v", err)
		}
		// 半成品也要清：part 临时文件、分片续传目录、压缩封面
		os.Remove(f + ".part")
		os.RemoveAll(f + ".segs") // 已下好的分片 + raw.mp4/raw.ts 都在这里面
		os.Remove(strings.TrimSuffix(f, filepath.Ext(f)) + ".jpg")
	}
	if err := os.RemoveAll(taskTempDir(id)); err != nil {
		logx.Printf("[download] 清理临时目录失败 id=%d: %v", id, err)
	}
	// 观看历史跟着一起删（外键有 ON DELETE CASCADE，这里显式删一次更保险）
	c.DB.Exec(`DELETE FROM vod_watch_history WHERE download_id=?`, id)
	if _, err := c.DB.Exec(`DELETE FROM vod_download WHERE id=?`, id); err != nil {
		return err
	}
	dlCtl.Delete(id)
	return nil
}

// ResetStaleDownloads 启动时复位上次异常退出的任务（可点「继续」恢复）
func (c *Collector) ResetStaleDownloads() {
	c.DB.Exec(`UPDATE vod_download SET status='paused', error='上次运行被中断，点「继续」可恢复下载'
		WHERE status IN ('queued','downloading','merging')`)
}

// nextPriorityDownload 取一个排队中的优先任务（用户点了「优先下载」）
func (c *Collector) nextPriorityDownload() (int64, bool) {
	var id int64
	err := c.DB.QueryRow(`SELECT id FROM vod_download
		WHERE status='queued' AND priority=1 ORDER BY updated_at LIMIT 1`).Scan(&id)
	return id, err == nil && id > 0
}

// wakeDownloader 叫醒下载 worker，让它重新检查优先队列
func wakeDownloader() {
	select {
	case dlPriWake <- struct{}{}:
	default:
	}
}

// SetDownloadPriority 把一集标记为「优先下载」：当前正在下的那集结束后立刻轮到它。
// 排队中/已暂停/失败的都能插队（暂停和失败的会顺带恢复）。
func SetDownloadPriority(id int64) error {
	if collectorRef == nil {
		return fmt.Errorf("服务未就绪")
	}
	c := collectorRef
	var st string
	if err := c.DB.QueryRow(`SELECT status FROM vod_download WHERE id=?`, id).Scan(&st); err != nil {
		return fmt.Errorf("下载记录不存在")
	}
	switch st {
	case "queued":
		c.setDl(id, `priority=1`)
	case "paused", "failed":
		dlMu.Lock()
		dlControl(id).Store(false)
		c.setDl(id, `status='queued', error='', priority=1`)
		dlMu.Unlock()
	case "downloading", "merging":
		return fmt.Errorf("这一集正在下载，无需插队")
	case "done":
		return fmt.Errorf("这一集已经下载完成")
	default:
		return fmt.Errorf("当前状态不可优先（%s）", statusText(st))
	}
	wakeDownloader()
	return nil
}

// StartDownloadWorkers 启动后台下载协程
func (c *Collector) StartDownloadWorkers(n int, saveDir string) {
	collectorRef = c
	downloadDir = saveDir
	c.ResetStaleDownloads()
	// 串行下载：一集下完（并合并完）再开始下一集。
	// 并发下多集时，带宽和 ffmpeg 会被摊薄，每集都卡在 30%~70% 很久，
	// 而且合并阶段互相抢 CPU，进度看起来像卡住。
	if n != 1 {
		logx.Printf("[download] 下载任务改为串行执行（忽略 -dl-workers=%d）", n)
	}
	n = 1
	for i := 0; i < n; i++ {
		go func() {
			for {
				// 优先任务插队：当前这一集下完后，先跑用户点过「优先下载」的
				if id, ok := c.nextPriorityDownload(); ok {
					c.downloadOne(id)
					continue
				}
				select {
				case id := <-dlQueue:
					c.downloadOne(id)
				case <-dlPriWake: // 新点了优先（此刻正好空闲），回头再查一次
				}
			}
		}()
	}
}

func (c *Collector) setDl(id int64, q string, args ...any) {
	c.DB.Exec(`UPDATE vod_download SET `+q+`, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		append(args, id)...)
}

func (c *Collector) downloadOne(id int64) {
	// 同一任务只允许一个 worker：暂停后马上点继续时，旧 goroutine 可能还在收尾
	if _, loaded := dlActive.LoadOrStore(id, struct{}{}); loaded {
		logx.Printf("[download] 任务 %d 仍在收尾，跳过本次调度", id)
		return
	}
	defer dlActive.Delete(id)

	var t DownloadTask
	var st string
	err := c.DB.QueryRow(`SELECT id, vod_id, vod_name, source_name, play_from, ep_index, status FROM vod_download WHERE id=?`, id).
		Scan(&t.ID, &t.VodID, &t.VodName, &t.SourceName, &t.PlayFrom, &t.EpIndex, &st)
	if err != nil {
		return
	}
	ctl := dlControl(id)
	if st == "paused" || ctl.Load() { // 入队后被暂停
		return
	}
	// 只处理排队中的任务：优先任务会被处理两次（数据库插队 + 通道里原本那份），
	// 已经完成/失败的直接跳过，避免重复下载。
	if st != "queued" && st != "downloading" {
		return
	}
	// 磁盘保护：起跑前再查一次剩余空间，不足就自动暂停（队列里后面的任务也会逐个被拦下）。
	// 用户清理空间后点「继续」即可恢复，不丢任务。
	if free, low := diskSpaceLow(); low {
		msg := fmt.Sprintf("磁盘剩余空间不足（%s），低于保护阈值 %s，已自动暂停；清理后点「继续」",
			db.HumanBytes(int64(free)), db.HumanBytes(dlMinFreeBytes))
		c.setDl(id, `status='paused', speed='', error=?`, msg)
		logx.Printf("[download] 任务 %d 自动暂停：%s", id, msg)
		return
	}
	c.setDl(id, `status='downloading', error='', speed='', priority=0`)

	var playURL, epName, vodName string
	err = c.DB.QueryRow(`SELECT `+db.PlayURLExpr+`, `+db.EpNameExpr+`, v.vod_name
		FROM vod_play p LEFT JOIN play_host h ON h.id=p.host_id
		JOIN vod_info v ON v.id=p.vod_id
		WHERE p.vod_id=? AND p.play_from=? AND p.ep_index=?`,
		t.VodID, t.PlayFrom, t.EpIndex).Scan(&playURL, &epName, &vodName)
	if err != nil || playURL == "" || !strings.Contains(playURL, "://") {
		// 主机字典里查不到（极端情况下采集事务回滚过）时拼出来的不是完整地址，
		// 这种情况直接判失败，重新采集即可恢复
		c.setDl(id, `status='failed', error='该集没有可用播放地址'`)
		return
	}

	out := filepath.Join(downloadDir, epFileName(vodName, t.PlayFrom, t.EpIndex))
	epDir := taskTempDir(id)
	if err := os.MkdirAll(epDir, 0o755); err != nil {
		c.setDl(id, `status='failed', error=?`, "创建临时目录失败: "+err.Error())
		return
	}

	start := time.Now()
	perr := downloader.DownloadToMP4(playURL, out, downloader.Options{
		Concurrency: dlSegConcurrency,
		OnProgress:  func(pct int) { c.setDl(id, `progress=?`, pct) },
		OnSpeed:     func(sp string) { c.setDl(id, `speed=?`, sp) },
		OnPhase: func(phase string) {
			// 下载完成 → 合并阶段：状态切到 merging，进度从 0 重新走
			// （合并一个 2GB 的片子要几十秒，旧实现在这段里一直显示"下载中 100%"）
			if phase == "merge" {
				c.setDl(id, `status='merging', progress=0, speed=''`)
				logx.Printf("[download] %s 第%d集 下载完成，开始合并", vodName, t.EpIndex+1)
			}
		},
		IsCanceled: func() bool { return ctl.Load() },
	})
	if errors.Is(perr, downloader.ErrCanceled) {
		c.setDl(id, `status='paused', speed=''`)
		logx.Printf("[download] %s 第%d集 已暂停（已下部分保留）", vodName, t.EpIndex+1)
		return
	}
	if perr != nil {
		c.setDl(id, `status='failed', error=?`, perr.Error())
		logx.Printf("[download] %s 第%d集失败: %v", vodName, t.EpIndex+1, perr)
		return
	}
	fi, _ := os.Stat(out)
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	os.RemoveAll(epDir)
	c.setDl(id, `status='done', progress=100, speed='', error='', file_path=?, file_size=?, episodes=1`, out, size)
	c.saveCover(t.VodID, out)
	logx.Printf("[download] %s 第%d集 -> %s（耗时 %v）", vodName, t.EpIndex+1, out, time.Since(start))
}

// saveCover 下载封面并压缩为 jpg，存到 mp4 同目录同名文件
func (c *Collector) saveCover(vodID int64, mp4Path string) {
	var pic string
	if err := c.DB.QueryRow(`SELECT vod_pic FROM vod_info WHERE id=?`, vodID).Scan(&pic); err != nil || pic == "" {
		return
	}
	req, err := http.NewRequest("GET", pic, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0 Safari/537.36")
	req.Header.Set("Accept", "image/jpeg,image/png,image/gif,image/webp;q=0.9,*/*;q=0.5")
	if i := strings.Index(pic, "://"); i > 0 {
		if j := strings.Index(pic[i+3:], "/"); j > 0 {
			req.Header.Set("Referer", pic[:i+3+j]+"/")
		}
	}
	// 复用包级 client：旧实现每次下载都 new 一个 Client（等于每次新建连接池），
	// 长期运行会持续泄漏空闲连接和 socket
	resp, err := coverHTTP.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || len(raw) == 0 {
		return
	}
	out, _, cerr := imgproc.CompressImage(raw, 480, 80)
	if cerr != nil {
		return
	}
	coverPath := strings.TrimSuffix(mp4Path, filepath.Ext(mp4Path)) + ".jpg"
	if err := os.WriteFile(coverPath, out, 0o644); err == nil {
		logx.Printf("[download] 封面已压缩保存 %dKB -> %s", len(out)/1024, coverPath)
	}
}

package db

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"videoDownLoad/internal/logx"
)

// 为什么需要这套东西：
// 采集时 replacePlays 每次更新一部片子都要 DELETE 再重插播放地址，删接口时又是几十万行的
// 批量 DELETE。SQLite 只把这些页标记成"空闲"、不还给文件系统，所以库文件会一路涨上去 ——
// 实测 915MB 的库里有 447MB（47%）是空闲页。VACUUM 能把它们还回去，但它需要独占写锁，
// 而且第一次还得重写整个库，所以只能挑服务空闲的时候做。

// FreelistStats 数据库空间统计。
type FreelistStats struct {
	PageSize   int
	PageCount  int
	FreeCount  int
	AutoVacuum int // 0=NONE 1=FULL 2=INCREMENTAL
}

// FreeRatio 空闲页占比（整数百分比）。
func (s FreelistStats) FreeRatio() int {
	if s.PageCount <= 0 {
		return 0
	}
	return s.FreeCount * 100 / s.PageCount
}

// ReclaimableMB 可回收空间（MiB，向下取整，仅用于日志）。
func (s FreelistStats) ReclaimableMB() int64 {
	return s.ReclaimableBytes() / (1024 * 1024)
}

// ReclaimableBytes 可回收空间（字节，精确）。
// 暴露给界面用：小库只有一两百 KB 时如果按 MB 取整会变成 0，
// 界面上会出现「空闲页 45% 但可回收 0MB」这种自相矛盾的显示。
func (s FreelistStats) ReclaimableBytes() int64 {
	return int64(s.FreeCount) * int64(s.PageSize)
}

// UsedMB 文件当前占用（MiB，含空闲页）。
func (s FreelistStats) UsedMB() int64 {
	return int64(s.PageCount) * int64(s.PageSize) / (1024 * 1024)
}

// FreelistStatsOf 读取空间统计。
func FreelistStatsOf(d *sql.DB) (FreelistStats, error) {
	var s FreelistStats
	s.PageSize = 4096
	if err := d.QueryRow(`PRAGMA page_size`).Scan(&s.PageSize); err != nil {
		return s, err
	}
	if err := d.QueryRow(`PRAGMA page_count`).Scan(&s.PageCount); err != nil {
		return s, err
	}
	if err := d.QueryRow(`PRAGMA freelist_count`).Scan(&s.FreeCount); err != nil {
		return s, err
	}
	// auto_vacuum 读失败不影响使用，按 NONE 处理
	_ = d.QueryRow(`PRAGMA auto_vacuum`).Scan(&s.AutoVacuum)
	if s.PageSize <= 0 {
		s.PageSize = 4096
	}
	return s, nil
}

// Vacuum 整理数据库，把空闲页还给文件系统。
//
// 注意：VACUUM 需要独占写锁。本项目的连接是 SetMaxOpenConns(1)，执行期间所有其它查询
// 都会排队等它 —— 只有在没有采集、没有下载时可调用（见 StartAutoVacuum 的 busy 判定）。
// 返回实际回收的字节数。
func Vacuum(d *sql.DB) (int64, error) {
	st, err := FreelistStatsOf(d)
	if err != nil {
		return 0, err
	}
	logx.Infof("[db] 开始整理：%d 页中 %d 页空闲（%d%%），可回收 %s",
		st.PageCount, st.FreeCount, st.FreeRatio(), HumanBytes(st.ReclaimableBytes()))
	start := time.Now()
	// 只走完整 VACUUM。试过 auto_vacuum=INCREMENTAL + PRAGMA incremental_vacuum 这条轻量路子，
	// 实测（15 万行删到 37% 空闲页）：freelist 数字看着降了，但文件字节数一个没少 —— 不缩盘，
	// 等于白跑。INCREMENTAL 还会给每个库页加 ptrmap 记录、把文件略微撑大，所以干脆不用。
	// 完整 VACUUM 实测 16.4MB → 4.1MB（-75%），效果确定。
	if _, err := d.Exec(`VACUUM`); err != nil {
		return 0, err
	}
	// 顺手把 WAL 收掉，否则整理完库文件小了、-wal 还挂着几百 MB
	if _, err := d.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		logx.Debugf("[db] checkpoint 失败: %v", err)
	}

	logx.Infof("[db] 整理完成，耗时 %v", time.Since(start).Round(time.Second))
	return st.ReclaimableBytes(), nil
}

// HumanBytes 人类可读的字节数，用于日志与接口提示。
func HumanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// StartAutoVacuum 后台定期检查空闲页并在服务空闲时整理。
// thresholdPct 是触发阈值（空闲页占比），<=0 表示关闭自动整理。
// busy 返回 true 时跳过本次 —— 正在采集或下载时不能被 VACUUM 的写锁卡住。
func StartAutoVacuum(d *sql.DB, thresholdPct int, delay, every time.Duration, busy func() bool) {
	if thresholdPct <= 0 {
		return
	}
	if delay < 0 {
		delay = 0
	}
	if every <= 0 {
		every = 6 * time.Hour
	}
	go func() {
		time.Sleep(delay)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			CheckVacuum(d, thresholdPct, busy)
			<-t.C
		}
	}()
}

// CheckVacuum 检查一次，超过阈值且服务空闲就整理。
func CheckVacuum(d *sql.DB, thresholdPct int, busy func() bool) {
	st, err := FreelistStatsOf(d)
	if err != nil {
		return
	}
	if st.FreeRatio() < thresholdPct {
		return
	}
	if busy != nil && busy() {
		logx.Infof("[db] 空闲页已达 %d%%（可回收 %s），但当前有采集/下载在跑，稍后再整理",
			st.FreeRatio(), HumanBytes(st.ReclaimableBytes()))
		return
	}
	if _, err := Vacuum(d); err != nil {
		logx.Warnf("[db] 自动整理失败: %v", err)
	}
}

// 播放地址表是全库最大的表（实测 226 万行），COUNT(*) 要走完整棵唯一索引，
// 弱 CPU 上要 1s+。这个数字只是展示用，没必要每次都真查。
var (
	playsMu       sync.Mutex
	playsCount    int
	playsCachedAt time.Time
)

const playsCountTTL = 5 * time.Minute

// PlaysCount 返回播放地址总数（带 5 分钟缓存）。
func PlaysCount(d *sql.DB) int {
	playsMu.Lock()
	defer playsMu.Unlock()
	if !playsCachedAt.IsZero() && time.Since(playsCachedAt) < playsCountTTL {
		return playsCount
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM vod_play`).Scan(&n); err == nil {
		playsCount, playsCachedAt = n, time.Now()
	}
	return playsCount
}

// InvalidatePlaysCount 数据变动后让缓存失效（删接口、采集结束等）。
func InvalidatePlaysCount() {
	playsMu.Lock()
	playsCachedAt = time.Time{}
	playsMu.Unlock()
}

// WarmupCounts 启动时后台预热计数缓存，避免第一个访问者撞上全表扫描。
func WarmupCounts(d *sql.DB) {
	go func() {
		PlaysCount(d)
	}()
}

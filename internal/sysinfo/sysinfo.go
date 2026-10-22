// Package sysinfo 采集「网站信息」页需要的系统级数据：
// 数据目录占用、磁盘容量、物理内存、Go 运行时内存。
//
// 目录体积需要遍历文件，放在这里统一做缓存 + 后台重扫：
//  1. 请求路径只读缓存，遍历再慢也不会拖住页面；
//  2. 遍历不做时间截断（旧实现给整个统计 3 秒预算，封面几十万小文件把预算吃光后，
//     排在后面的 videos 直接按 0 处理，几十 GB 视频在页面上只剩几百 MB）；
//  3. 结果落盘，服务重启后先用上次的值顶上，再后台刷新。
package sysinfo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DirUsage 数据目录各部分的体积（字节）
type DirUsage struct {
	DBBytes     int64 `json:"db_bytes"`
	ImgBytes    int64 `json:"img_bytes"`
	ImgCount    int   `json:"img_count"`
	VideoBytes  int64 `json:"video_bytes"`
	BackupBytes int64 `json:"backup_bytes"`
	BinBytes    int64 `json:"bin_bytes"`
	// OtherBytes 是根目录零散文件（ui.yml 等）与未知子目录（如自更新的 update/ 源码缓存），
	// 之前没统计导致 total 偏小、和 du 对不上
	OtherBytes int64 `json:"other_bytes"`
	TotalBytes int64 `json:"total_bytes"`
	// Partial 保留字段以兼容前端：现在遍历不再截断，恒为 false
	Partial bool `json:"partial"`
	// At 本轮扫描开始的 UnixNano，前端用它展示数据新鲜度
	At int64 `json:"at"`
}

const (
	// dirUsageTTL 缓存新鲜期。过期后由访问触发后台重扫，请求侧仍拿上一次的完整结果，
	// 不再为了「几秒内返回」牺牲准确性
	dirUsageTTL = 2 * time.Minute
	// dirUsageCacheFile 统计结果落盘文件（放数据目录里，重启后立即可用）
	dirUsageCacheFile = ".dirusage.json"
)

var (
	dirMu       sync.Mutex
	dirCond     = sync.NewCond(&dirMu)
	dirCache    DirUsage
	dirKey      string
	dirScanning bool // 同一时刻最多跑一次遍历，避免并发访问把磁盘 IO 打满
)

// InvalidateDirUsage 让缓存立即失效（VACUUM 之后库文件会明显变小）。
// 只有 db_bytes 会变，这里就地修正主库/WAL 大小（几次 stat，毫秒级），
// 再触发一次后台全量重扫，整理完刷新页面就能看到变小后的库文件。
func InvalidateDirUsage() {
	dirMu.Lock()
	if dirKey == "" {
		dirMu.Unlock()
		return
	}
	dir := dirKey
	u := dirCache
	dirMu.Unlock()

	u.DBBytes = dbBytesOf(dir)
	u.TotalBytes = u.DBBytes + u.ImgBytes + u.VideoBytes + u.BackupBytes + u.BinBytes + u.OtherBytes
	dirMu.Lock()
	dirCache = u
	dirMu.Unlock()
	saveDirUsage(dir, u)
	refreshAsync(dir)
}

// DirUsageOf 返回数据目录体积。
//
// 缓存命中直接返回；过期时先返回旧值并触发后台重扫；
// 只有完全没有历史结果（首次运行且没有落盘缓存）时才同步扫一次。
func DirUsageOf(dir string) DirUsage {
	dirMu.Lock()
	if dirKey == dir && dirCache.At != 0 {
		u := dirCache
		expired := time.Since(time.Unix(0, u.At)) >= dirUsageTTL
		dirMu.Unlock()
		if expired {
			refreshAsync(dir)
		}
		return u
	}
	dirMu.Unlock()

	// 刚启动 / 刚换数据目录：先用落盘结果顶上，再后台刷新
	if u, ok := loadDirUsage(dir); ok {
		dirMu.Lock()
		if dirKey != dir || dirCache.At == 0 {
			dirCache, dirKey = u, dir
		}
		dirMu.Unlock()
		refreshAsync(dir)
		return u
	}

	// 一点历史结果都没有：同步扫一遍，否则页面只能显示 0
	return scanSync(dir)
}

// scanSync 同步统计一次；并发请求等它结束后直接拿结果，不重复遍历
func scanSync(dir string) DirUsage {
	dirMu.Lock()
	for dirScanning {
		dirCond.Wait()
		if dirKey == dir && dirCache.At != 0 {
			u := dirCache
			dirMu.Unlock()
			return u
		}
	}
	dirScanning = true
	dirMu.Unlock()

	u := calcDirUsage(dir)
	saveDirUsage(dir, u)

	dirMu.Lock()
	dirCache, dirKey = u, dir
	dirScanning = false
	dirCond.Broadcast()
	dirMu.Unlock()
	return u
}

// refreshAsync 后台重扫；已有扫描在跑就直接复用，不排队
func refreshAsync(dir string) {
	dirMu.Lock()
	if dirScanning {
		dirMu.Unlock()
		return
	}
	dirScanning = true
	dirMu.Unlock()

	go func() {
		u := calcDirUsage(dir)
		saveDirUsage(dir, u)

		dirMu.Lock()
		dirCache, dirKey = u, dir
		dirScanning = false
		dirCond.Broadcast()
		dirMu.Unlock()
	}()
}

// calcDirUsage 完整遍历数据目录。故意不设时间上限：
// 一旦中途放弃，还没扫到的目录（videos 首当其冲）就会被当成 0，总占用严重偏小。
func calcDirUsage(dir string) DirUsage {
	u := DirUsage{At: time.Now().UnixNano()}
	walk := func(sub string, dst *int64, count *int) {
		var total int64
		n := 0
		_ = filepath.WalkDir(filepath.Join(dir, sub), func(_ string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if fi, e := d.Info(); e == nil {
				total += fi.Size()
				n++
			}
			return nil
		})
		*dst = total
		if count != nil {
			*count = n
		}
	}

	u.DBBytes = dbBytesOf(dir)
	walk("img", &u.ImgBytes, &u.ImgCount)
	walk("videos", &u.VideoBytes, nil)
	walk("backup", &u.BackupBytes, nil)
	walk("bin", &u.BinBytes, nil)

	// 根目录零散文件 + 未知子目录：只有 img/videos/backup/bin 四个是认识的名字，
	// 其余（update/、tmp、ui.yml…）都算进 OtherBytes，保证 total 和实际磁盘占用对得上
	isKnown := func(name string) bool {
		switch name {
		case "img", "videos", "backup", "bin":
			return true
		}
		return false
	}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "videodl.db") || isKnown(e.Name()) {
				continue
			}
			if e.IsDir() {
				walk(e.Name(), &u.OtherBytes, nil)
			} else if fi, err := e.Info(); err == nil {
				u.OtherBytes += fi.Size()
			}
		}
	}
	u.TotalBytes = u.DBBytes + u.ImgBytes + u.VideoBytes + u.BackupBytes + u.BinBytes + u.OtherBytes
	return u
}

// dbBytesOf 主库 + WAL/SHM（WAL 可能有几 MB，不算进去会显得库里数据太小）
func dbBytesOf(dir string) int64 {
	var total int64
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "videodl.db") {
				continue
			}
			if fi, err := e.Info(); err == nil {
				total += fi.Size()
			}
		}
	}
	return total
}

func dirUsageCachePath(dir string) string { return filepath.Join(dir, dirUsageCacheFile) }

// saveDirUsage 原子写（先写临时文件再改名），避免读到写了一半的 JSON
func saveDirUsage(dir string, u DirUsage) {
	if u.At == 0 {
		return
	}
	b, err := json.Marshal(u)
	if err != nil {
		return
	}
	tmp := dirUsageCachePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, dirUsageCachePath(dir))
}

// loadDirUsage 读上次统计结果；总占用为 0 的旧结果没有意义，直接忽略
func loadDirUsage(dir string) (DirUsage, bool) {
	b, err := os.ReadFile(dirUsageCachePath(dir))
	if err != nil {
		return DirUsage{}, false
	}
	var u DirUsage
	if json.Unmarshal(b, &u) != nil || u.At == 0 || u.TotalBytes <= 0 {
		return DirUsage{}, false
	}
	return u, true
}

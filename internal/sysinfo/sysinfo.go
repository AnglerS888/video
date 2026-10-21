// Package sysinfo 采集「网站信息」页需要的系统级数据：
// 数据目录占用、磁盘容量、物理内存、Go 运行时内存。
//
// 目录体积需要遍历文件，放在这里统一做 60 秒缓存，避免每次刷新页面都扫几万个文件。
package sysinfo

import (
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
	// Partial 表示遍历超时被截断（超大目录时只统计了一部分）
	Partial bool  `json:"partial"`
	At      int64 `json:"at"`
}

var (
	dirMu    sync.Mutex
	dirCache DirUsage
	dirKey   string
)

// InvalidateDirUsage 丢弃目录占用缓存。
// VACUUM 之后库文件会明显变小，不清缓存的话「网站信息」页 60 秒内还显示旧体积。
func InvalidateDirUsage() {
	dirMu.Lock()
	dirKey = ""
	dirMu.Unlock()
}

// DirUsageOf 统计数据目录体积（60 秒缓存）。
// 目录里可能有几十万个封面小文件，遍历最多花 3 秒，超时就返回已统计的部分。
func DirUsageOf(dir string) DirUsage {
	dirMu.Lock()
	if dirKey == dir && time.Since(time.Unix(0, dirCache.At)) < 60*time.Second {
		u := dirCache
		dirMu.Unlock()
		return u
	}
	dirMu.Unlock()

	u := calcDirUsage(dir, 3*time.Second)
	dirMu.Lock()
	dirCache, dirKey = u, dir
	dirMu.Unlock()
	return u
}

func calcDirUsage(dir string, budget time.Duration) DirUsage {
	u := DirUsage{At: time.Now().UnixNano()}
	deadline := time.Now().Add(budget)
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
			if time.Now().After(deadline) {
				u.Partial = true
				return filepath.SkipAll
			}
			return nil
		})
		*dst = total
		if count != nil {
			*count = n
		}
	}

	// 主库 + WAL/SHM（WAL 可能有几 MB，不算进去会显得库里数据太小）
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "videodl.db") {
				continue
			}
			if fi, err := e.Info(); err == nil {
				u.DBBytes += fi.Size()
			}
		}
	}
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

//go:build !windows && !linux

package sysinfo

// DiskOf 其它平台暂不支持，返回 0（前端显示“未知”）
func DiskOf(path string) (total, free uint64) { return 0, 0 }

// MemOf 其它平台暂不支持
func MemOf() (total, available uint64) { return 0, 0 }

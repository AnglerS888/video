//go:build !windows

package main

import (
	"os"
	"syscall"
)

// relaunch 用新二进制替换当前进程（Unix 下 rename 覆盖正在运行的文件是安全的，
// 旧 inode 会等进程退出后释放）。os.Args 原样传下去，端口与数据目录参数保持一致。
func relaunch(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}

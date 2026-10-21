//go:build !windows

package updater

import (
	"os"
	"syscall"
)

// requestRestart 给自身发 SIGTERM，复用 main 里的优雅退出路径
// （停止接收新请求、等在飞下载收尾、关数据库），退出码 0 由 docker 的
// restart: unless-stopped（或 deploy.sh 的 --restart）拉起替换后的新二进制。
func requestRestart() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

//go:build !windows

package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
)

// restartPending 标记本次退出是「更新后重启」：main 完成优雅收尾后会调用 Relaunch。
var restartPending atomic.Bool

// requestRestart 请求重启：先标记，再给自身发 SIGTERM，复用 main 的优雅退出路径
// （停止接收新请求、等在飞下载收尾、关数据库）。
//
// 收尾完成后由 main 调 Relaunch：用新二进制原地 exec 替换进程 —— PID 不变，
// docker 看到的是「同一个容器里换了个程序」，容器不会退出。这样不再依赖
// restart: unless-stopped / --restart（手动 docker run 没配策略时也能更新成功）。
func requestRestart() {
	restartPending.Store(true)
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

// RestartRequested 本次退出是否由更新触发（main 据此决定收尾后是否 exec）。
func RestartRequested() bool {
	return restartPending.Load()
}

// Relaunch 用当前可执行文件（此时已被新二进制原子替换）原地替换进程。
//
// execve 成功不会返回：新进程沿用同一个 PID、同一个容器的 stdout/stderr，
// 重新加载配置、数据库和监听端口（旧 fd 都带 CLOEXEC，会自动关闭）。
// 返回错误只发生在 exec 失败时（文件被删/权限损坏等），调用方应正常退出，
// 让容器的 restart 策略兜底。
func Relaunch() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取程序路径失败: %w", err)
	}
	if r, e := filepath.EvalSymlinks(exe); e == nil {
		exe = r
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("exec %s 失败: %w", exe, err)
	}
	return nil
}

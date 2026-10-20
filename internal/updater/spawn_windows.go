//go:build windows

package updater

import (
	"os/exec"
	"syscall"
)

// launchUpdaterScript 启动替换脚本。
//
// 必须让脚本脱离当前进程的 Job：从服务/任务计划/IDE 里启动时，
// 父进程退出往往会连带杀掉整棵进程树，脚本还没拷贝完就被清理了
// （实测在自动化环境里就是这样：bat 残留、exe 没换、服务没起来）。
// 这里用 DETACHED_PROCESS + CREATE_NEW_PROCESS_GROUP + CREATE_BREAKAWAY_FROM_JOB；
// 某些 Job 不允许 breakaway，失败时退回普通启动。
func launchUpdaterScript(bat, dir string) error {
	const (
		createBreakawayFromJob = 0x01000000
		createNewProcessGroup  = 0x00000200
		detachedProcess        = 0x00000008
	)
	c := exec.Command("cmd", "/c", "start", "", "/min", bat)
	c.Dir = dir
	c.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup | createBreakawayFromJob,
	}
	if err := c.Start(); err == nil {
		return nil
	}
	fallback := exec.Command("cmd", "/c", "start", "", "/min", bat)
	fallback.Dir = dir
	return fallback.Start()
}

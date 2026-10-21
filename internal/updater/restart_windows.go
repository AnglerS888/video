//go:build windows

package updater

import (
	"errors"
	"os"
)

// requestRestart Windows 上直接退出：本平台不参与容器内自更新
// （非容器模式 apply 接口直接拒绝），这里只是为了能编译。
func requestRestart() {
	os.Exit(0)
}

// RestartRequested Windows 不会走「更新后原地重启」路径。
func RestartRequested() bool {
	return false
}

// Relaunch Windows 无法用新文件替换运行中的进程，只能正常退出。
func Relaunch() error {
	return errors.New("Windows 不支持进程内重启")
}

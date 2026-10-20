//go:build !windows

package updater

import "errors"

// launchUpdaterScript 只有 Windows 需要「退出后替换正在运行的 exe」，
// Unix 走 rename + exec，不会调用到这里。
func launchUpdaterScript(bat, dir string) error {
	return errors.New("当前平台不需要替换脚本")
}

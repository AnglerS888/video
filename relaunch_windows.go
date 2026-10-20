//go:build windows

package main

import "errors"

// relaunch Windows 下无法用 exec 替换自身（运行中的 exe 被文件锁占用），
// 更新走 data/update/apply-update.bat：本进程退出后由脚本拷贝并重新启动。
func relaunch(exe string) error {
	return errors.New("Windows 下应通过 update/apply-update.bat 重启，不会走到这里")
}

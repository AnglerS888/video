//go:build windows

package updater

import "os"

// requestRestart Windows 上直接退出：本平台不参与容器内自更新
// （非容器模式 apply 接口直接拒绝），这里只是为了能编译。
func requestRestart() {
	os.Exit(0)
}

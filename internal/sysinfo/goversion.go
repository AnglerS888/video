package sysinfo

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	goToolOnce sync.Once
	goToolVer  string // 运行环境 go 工具链版本，如 "go1.24.1"；拿不到为空
)

// GoToolchainVersion 探测运行环境里的 go 工具链版本（执行 go version）。
// runtime.Version() 只是编译二进制时的编译器版本，和容器/本机实际装的 go 不一定一致
// （例如 Docker 自更新在容器内用容器里的 go 重新编译）。结果进程内只探测一次。
// 探测失败（未装 go / 超时 / 非 go 输出）返回空串，调用方回退到编译期版本。
func GoToolchainVersion() string {
	goToolOnce.Do(func() {
		exe, err := exec.LookPath("go")
		if err != nil {
			// golang 基础镜像里 go 固定在这里，PATH 丢失时兜底
			exe = "/usr/local/go/bin/go"
			if _, err := exec.LookPath(exe); err != nil {
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, exe, "version").Output()
		if err != nil {
			return
		}
		// 输出形如: go version go1.24.1 linux/amd64
		f := strings.Fields(string(out))
		if len(f) >= 3 && strings.HasPrefix(f[2], "go1.") {
			goToolVer = f[2]
		}
	})
	return goToolVer
}

// GoVersionInfo 返回给前端的信息页字段：
// go = 实际工具链版本（探测不到时回退编译期版本并加标记），go_compile = 编译期版本。
func GoVersionInfo() (goVer, goCompile string) {
	goCompile = runtime.Version()
	if v := GoToolchainVersion(); v != "" {
		return v, goCompile
	}
	return goCompile + "（编译期）", goCompile
}

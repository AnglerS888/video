// Package version 集中存放版本信息。
//
// 版本号只有一个来源：仓库根目录的 VERSION 文件。构建脚本（Makefile / Dockerfile）
// 读取它并通过 -ldflags 注入下面的变量，所以二进制里带的是构建时那份 VERSION。
//
//	go build -ldflags "-X videoDownLoad/internal/version.Version=1.3.0 \
//	                   -X videoDownLoad/internal/version.Commit=ced2295 \
//	                   -X videoDownLoad/internal/version.BuildTime=2026-10-20T03:00:00Z"
//
// 不带 ldflags 直接 go build 也能跑：Version 保持默认值，Commit 为空，
// BuildTime 为空时用可执行文件修改时间兜底（用于判断「远端提交比本机新」）。
package version

import (
	"os"
	"strings"
	"time"
)

var (
	// Version 语义化版本号（与仓库根目录 VERSION 一致），构建时由 ldflags 覆盖
	Version = "1.3.0"
	// Commit 构建时的 git 提交短哈希（可能为空：直接 go build 时没有）
	Commit = ""
	// BuildTime 构建时间，RFC3339（UTC），可能为空
	BuildTime = ""
)

const (
	// Repo GitHub 仓库（owner/name），在线更新用它拼接口与下载地址
	Repo = "AnglerS888/video"
	// Branch 更新跟踪的分支
	Branch = "main"
)

// Short 用于界面上展示的版本串，如 v1.3.0 (ced2295)
func Short() string {
	v := Version
	if v == "" {
		v = "dev"
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if Commit != "" {
		return v + " (" + shortCommit() + ")"
	}
	return v
}

func shortCommit() string {
	if len(Commit) > 7 {
		return Commit[:7]
	}
	return Commit
}

// Time 构建时间；没注入时退回可执行文件的修改时间（Docker 里就是镜像构建时间）。
// 拿不到则返回零值，调用方需自行判断。
func Time() time.Time {
	if BuildTime != "" {
		if t, err := time.Parse(time.RFC3339, BuildTime); err == nil {
			return t
		}
	}
	if exe, err := os.Executable(); err == nil {
		if fi, err := os.Stat(exe); err == nil {
			return fi.ModTime().UTC()
		}
	}
	return time.Time{}
}

// InDocker 判断是否跑在容器里（/.dockerenv 是 docker 的固定标记；
// 也支持显式用 VDL_DOCKER=1 覆盖，方便在没有该文件的容器/测试环境里验证）。
func InDocker() bool {
	if v := strings.TrimSpace(os.Getenv("VDL_DOCKER")); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	// 部分容器运行时没有 /.dockerenv，退而看 cgroup
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		return strings.Contains(s, "docker") || strings.Contains(s, "kubepods") || strings.Contains(s, "containerd")
	}
	return false
}

// Mode 运行方式：docker / direct
func Mode() string {
	if InDocker() {
		return "docker"
	}
	return "direct"
}

// ModeText 运行方式的中文说明
func ModeText() string {
	if InDocker() {
		return "Docker 容器"
	}
	return "直接运行"
}

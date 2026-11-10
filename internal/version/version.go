// Package version 集中存放版本信息。
//
// 版本号只有一个来源：仓库根目录的 VERSION 文件。构建脚本（Makefile / Dockerfile）
// 读取它并通过 -ldflags 注入下面的变量，所以二进制里带的是构建时那份 VERSION。
//
//	go build -ldflags "-X videoDownLoad/internal/version.Version=1.4.1 \
//	                   -X videoDownLoad/internal/version.Commit=11d5782 \
//	                   -X videoDownLoad/internal/version.BuildTime=2026-10-21T00:10:00Z"
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
	Version = "1.4.1"
	// Commit 构建时的 git 提交短哈希（可能为空：直接 go build 时没有）
	Commit = ""
	// BuildTime 构建时间，RFC3339（UTC），可能为空
	BuildTime = ""
)

const (
	// Repo 默认更新源仓库（Gitee，owner/name）
	Repo = "anglers/video"
	// Branch 默认更新跟踪的分支
	Branch = "master"
	// SiteBase 默认更新源的站点根地址
	SiteBase = "https://gitee.com"
)

// Remote 一个更新源（同一份代码的多仓库镜像）。
//
// Gitee 与 GitHub 的 OpenAPI 形状基本一致（都是 {API}/repos/{repo}/commits/{branch}），
// 差别只在：GitHub 的 API 根是 https://api.github.com、分支叫 main、
// 匿名读文件要用 contents 接口（raw.githubusercontent.com 国内常不可达）。
type Remote struct {
	Name   string // 机器名：gitee / github
	Label  string // 展示名：Gitee / GitHub
	Site   string // 网页站点根
	API    string // OpenAPI 根（Gitee 含 /api/v5）
	Repo   string // owner/name
	Branch string
}

// PageURL 仓库网页地址（给用户在浏览器里打开）
func (r Remote) PageURL() string { return r.Site + "/" + r.Repo }

// RepoLinksText 全部仓库网页地址，用「或」连接（提示文案用）
func RepoLinksText() string {
	parts := make([]string, 0, len(Remotes))
	for _, r := range Remotes {
		parts = append(parts, r.PageURL())
	}
	return strings.Join(parts, " 或 ")
}

// Remotes 全部更新源，按优先级排列：检查更新时会逐个试，
// 取版本号最高（相同则提交最新）的那个；下载源码包失败时按同样顺序回退。
var Remotes = []Remote{
	{
		Name: "gitee", Label: "Gitee",
		Site: "https://gitee.com", API: "https://gitee.com/api/v5",
		Repo: "anglers/video", Branch: "master",
	},
	{
		Name: "github", Label: "GitHub",
		Site: "https://github.com", API: "https://api.github.com",
		Repo: "AnglerS888/video", Branch: "main",
	},
}

// RepoURL 仓库网页地址
func RepoURL(repo string) string {
	return SiteBase + "/" + repo
}

// Short 用于界面上展示的版本串，如 v1.4.1 (11d5782)
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
	// 部分容器运行时没有 /.dockerenv，退而看 cgroup。
	//
	// cgroup v2：/proc/1/cgroup 内容形如 "0::/"（一条统一的层级行），
	// 里面**没有** docker/kubepods/containerd 字样 —— 只按关键字判会误判成"不是容器"，
	// 于是 K8s / containerd 里自更新被静默禁用。
	// 判定：文件里有 v2 的 "0::" 行 → 认为在容器里（v2 统一层级只在容器里出现）。
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		if strings.Contains(s, "docker") || strings.Contains(s, "kubepods") || strings.Contains(s, "containerd") {
			return true
		}
		if strings.Contains(s, "0::") {
			return true
		}
	}
	// 兜底：cgroup v2 的根挂载点在容器里通常是 /sys/fs/cgroup 且只读
	if st, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		if st.Mode().Perm()&0o200 == 0 { // 不可写 = 只读挂载 = 容器特征
			return true
		}
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

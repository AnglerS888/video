package web

import (
	"context"
	"net/http"
	"time"

	"videoDownLoad/internal/version"
)

// versionInfo 版本与更新状态（不联网，用缓存的上次检查结果）
func (s *Server) versionInfo(w http.ResponseWriter, r *http.Request) {
	if s.Up == nil {
		fail(w, "更新模块未启用")
		return
	}
	ok(w, map[string]any{
		"info":   s.Up.Current(false),
		"status": s.Up.GetStatus(),
		"mode":   version.Mode(),
	})
}

// checkUpdate 主动联网检查（Gitee API + raw VERSION）。
// 直接运行模式也允许检查：至少让用户知道有没有新版本，再自己去 Gitee 下载。
func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	if s.Up == nil {
		fail(w, "更新模块未启用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	ok(w, s.Up.Refresh(ctx))
}

// applyUpdate 下载最新源码。**只允许 Docker 容器内使用**：
// 容器里没有 Go 工具链、也不能替换正在运行的镜像，所以这里只落源码，
// 由用户在宿主机执行 docker compose build && up -d。
// 直接运行模式一律拒绝，引导用户去 Gitee 手动更新。
func (s *Server) applyUpdate(w http.ResponseWriter, r *http.Request) {
	if s.Up == nil {
		fail(w, "更新模块未启用")
		return
	}
	if !version.InDocker() {
		fail(w, "当前为直接运行模式，不支持自动更新。请到 "+version.RepoURL(version.Repo)+" 下载最新版本后手动替换")
		return
	}
	if err := s.Up.Start(); err != nil {
		fail(w, err.Error())
		return
	}
	ok(w, s.Up.GetStatus())
}

// updateStatus 轮询更新进度
func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	if s.Up == nil {
		fail(w, "更新模块未启用")
		return
	}
	ok(w, s.Up.GetStatus())
}

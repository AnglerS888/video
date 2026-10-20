package web

import (
	"context"
	"net/http"
	"runtime"
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

// checkUpdate 主动联网检查（GitHub API / raw 都带镜像回退）
func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	if s.Up == nil {
		fail(w, "更新模块未启用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	ok(w, s.Up.Refresh(ctx))
}

// applyUpdate 启动更新任务：下载源码 → 解压 →（直接运行模式）编译
func (s *Server) applyUpdate(w http.ResponseWriter, r *http.Request) {
	if s.Up == nil {
		fail(w, "更新模块未启用")
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

// restartUpdate 应用新版本并重启进程。
// Docker 模式无法替换容器内的二进制，直接拒绝并提示去宿主机重建镜像。
func (s *Server) restartUpdate(w http.ResponseWriter, r *http.Request) {
	if s.Up == nil {
		fail(w, "更新模块未启用")
		return
	}
	if version.InDocker() {
		fail(w, "Docker 模式请在宿主机执行 docker compose build && docker compose up -d")
		return
	}
	relaunch, err := s.Up.PrepareRestart()
	if err != nil {
		fail(w, err.Error())
		return
	}
	msg := "正在重启…"
	if runtime.GOOS == "windows" {
		msg = "已启动替换脚本，程序将在约 3 秒后重启"
	}
	ok(w, map[string]any{"msg": msg, "relaunch": relaunch})

	// 先把响应写回去，再优雅停机（Windows 由脚本接管重启，Unix 在 main 退出前 exec 新文件）
	if s.Shutdown != nil {
		go func() {
			time.Sleep(400 * time.Millisecond)
			s.Shutdown()
		}()
	}
}

package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"videoDownLoad/internal/collector"
)

// pickSources 支持 ?source_id=N 只采某个接口（接口列表「操作」列里的按钮用）；
// 不传则采全部启用的接口。
func (s *Server) pickSources(r *http.Request) []collector.Source {
	if idStr := r.URL.Query().Get("source_id"); idStr != "" {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		for _, x := range s.Col.SourcesByEnabled(false) { // 单独触发时即使接口被禁用也允许
			if x.ID == id {
				return []collector.Source{x}
			}
		}
		return nil
	}
	return s.Col.EnabledSources()
}

func (s *Server) collectFull(w http.ResponseWriter, r *http.Request) {
	srcs := s.pickSources(r)
	if len(srcs) == 0 {
		fail(w, "没有可采集的接口")
		return
	}
	restart := r.URL.Query().Get("restart") == "1"
	started, skipped := s.Col.RunFull(srcs, restart)
	if started == 0 {
		fail(w, "已在采集中："+strings.Join(skipped, "、"))
		return
	}
	msg := fmt.Sprintf("已启动 %d 个接口的全量采集", started)
	if restart {
		msg = fmt.Sprintf("已从头启动 %d 个接口的全量采集", started)
	}
	if len(skipped) > 0 {
		msg += "；跳过：" + strings.Join(skipped, "、")
	}
	ok(w, msg)
}

func (s *Server) collectIncr(w http.ResponseWriter, r *http.Request) {
	srcs := s.pickSources(r)
	if len(srcs) == 0 {
		fail(w, "没有可采集的接口")
		return
	}
	// 手动增量：不限页数（仍按 24 小时过滤）；定时任务才限制前 5 页
	started, skipped := s.Col.RunIncremental(srcs, 0)
	if started == 0 {
		fail(w, "已在采集中："+strings.Join(skipped, "、"))
		return
	}
	msg := fmt.Sprintf("已启动 %d 个接口的增量采集", started)
	if len(skipped) > 0 {
		msg += "；跳过：" + strings.Join(skipped, "、")
	}
	ok(w, msg)
}

func (s *Server) progress(w http.ResponseWriter, r *http.Request) {
	ok(w, collector.GlobalProgress.Snapshot())
}

// stopCollect 停止采集：?source_id=N 只停该接口，不传则停掉全部正在采集的接口。
// 只是发信号，采集协程会在当前这一页处理完后退出（进度里会出现 stopping → stopped）。
func (s *Server) stopCollect(w http.ResponseWriter, r *http.Request) {
	if idStr := r.URL.Query().Get("source_id"); idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			fail(w, "接口 id 不合法")
			return
		}
		if !collector.GlobalProgress.Stop(id) {
			fail(w, "该接口当前没有在采集")
			return
		}
		ok(w, "已停止采集（正在收尾）")
		return
	}
	n := collector.GlobalProgress.StopAll()
	if n == 0 {
		fail(w, "当前没有正在进行的采集")
		return
	}
	ok(w, fmt.Sprintf("已停止 %d 个接口的采集（正在收尾）", n))
}

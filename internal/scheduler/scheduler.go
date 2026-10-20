package scheduler

import (
	"sync/atomic"
	"time"
	"videoDownLoad/internal/logx"
)

// Scheduler 定时任务：每 interval 执行一次增量采集
type Scheduler struct {
	Interval time.Duration
	Run      func() // 由 main 注入：拉取启用源 -> collector.RunIncremental
	stop     chan struct{}
	started  atomic.Bool
}

func New(interval time.Duration, run func()) *Scheduler {
	return &Scheduler{Interval: interval, Run: run, stop: make(chan struct{})}
}

func (s *Scheduler) Start() {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	go func() {
		t := time.NewTicker(s.Interval)
		defer t.Stop()
		logx.Printf("[scheduler] 增量采集定时任务已启动，间隔 %v", s.Interval)
		for {
			select {
			case <-t.C:
				func() {
					defer func() { // 防止 panic 杀掉定时器
						if r := recover(); r != nil {
							logx.Printf("[scheduler] recover: %v", r)
						}
					}()
					s.Run()
				}()
			case <-s.stop:
				return
			}
		}
	}()
}

func (s *Scheduler) Stop() { close(s.stop) }

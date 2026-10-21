package scheduler

import (
	"sync"
	"sync/atomic"
	"time"
	"videoDownLoad/internal/logx"
)

// Scheduler 定时任务：每 interval 执行一次增量采集。
// 间隔支持运行时修改（信息页保存配置后热更新），不用重启。
type Scheduler struct {
	Run func() // 由 main 注入：拉取启用源 -> collector.RunIncremental

	mu       sync.Mutex
	interval time.Duration
	wake     chan struct{} // 间隔变更时唤醒循环，重建 ticker
	stop     chan struct{}
	started  atomic.Bool
}

func New(interval time.Duration, run func()) *Scheduler {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Scheduler{
		Run:      run,
		interval: interval,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
}

// Interval 当前间隔
func (s *Scheduler) Interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interval
}

// SetInterval 运行时改间隔：立即按新间隔重新计时。
// 正在跑的那次不会被打断，只是下一次触发的时间点变了。
func (s *Scheduler) SetInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	if s.interval == d {
		s.mu.Unlock()
		return
	}
	s.interval = d
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default: // 已经有一次待处理的变更，合并掉
	}
}

func (s *Scheduler) Start() {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	go func() {
		iv := s.Interval()
		t := time.NewTicker(iv)
		defer t.Stop()
		logx.Printf("[scheduler] 增量采集定时任务已启动，间隔 %v", iv)
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
			case <-s.wake:
				// 间隔变了：旧 ticker 停掉再建新的，否则会泄漏
				t.Stop()
				iv = s.Interval()
				t = time.NewTicker(iv)
				logx.Printf("[scheduler] 定时增量间隔已更新为 %v", iv)
			case <-s.stop:
				return
			}
		}
	}()
}

func (s *Scheduler) Stop() { close(s.stop) }

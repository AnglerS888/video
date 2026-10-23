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
	stopOnce sync.Once     // Stop 幂等：重复调用会 panic（close of closed channel）
	done     chan struct{} // 循环退出时关闭，Stop 可等待
	started  atomic.Bool
	// next 下次触发时间（unix 纳秒）：接口页据此显示「下次自动采集倒计时」
	next atomic.Int64
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
		done:     make(chan struct{}),
	}
}

// Interval 当前间隔
func (s *Scheduler) Interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interval
}

// Next 下次自动增量触发的时间；未启动时返回零值
func (s *Scheduler) Next() time.Time {
	n := s.next.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
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
		defer close(s.done)
		iv := s.Interval()
		t := time.NewTicker(iv)
		defer t.Stop()
		s.next.Store(time.Now().Add(iv).UnixNano())
		logx.Printf("[scheduler] 增量采集定时任务已启动，间隔 %v", iv)
		for {
			select {
			case <-t.C:
				// 先记下次触发时间，再执行任务：页面倒计时不会在任务期间停住
				s.next.Store(time.Now().Add(s.Interval()).UnixNano())
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
				s.next.Store(time.Now().Add(iv).UnixNano())
				logx.Printf("[scheduler] 定时增量间隔已更新为 %v", iv)
			case <-s.stop:
				return
			}
		}
	}()
}

// Stop 停止定时任务。幂等（重复调用不会 panic），并等待循环真正退出 ——
// 之前只 close(stop) 不等待，主流程接着就关数据库 → 正在跑的增量任务访问已关闭的连接。
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	if s.started.Load() {
		<-s.done
	}
}

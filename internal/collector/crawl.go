package collector

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	db "videoDownLoad/internal/db"
	"videoDownLoad/internal/logx"
)

const (
	defaultPageWorkers   = 6 // 每个接口的分页并发数
	defaultSourceWorkers = 4 // 同时采集的接口数

	// 全量同步时源站偶发超时/限流很常见，不要一报错就把整个接口判死：
	// 失败后等 180 秒再重试同一页，连续 3 次仍失败才终止该接口的采集。
	maxConsecutiveFail = 3
)

// failRetryDelay 失败后的等待时间，默认 180 秒。
// VDL_COLLECT_RETRY_DELAY（秒）可覆盖，便于本地验证或按源站情况调整。
var failRetryDelay = func() time.Duration {
	if v := os.Getenv("VDL_COLLECT_RETRY_DELAY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 180 * time.Second
}()

// 全量同步时每抓一页之间的间隔（每个分页线程各自计时）。
// 源站对短时间大量请求很敏感（实测 Cloudflare 会直接 520），默认 3~5 秒随机，
// 避免 12 个线程同一节奏撞上去。
// VDL_COLLECT_PAGE_DELAY 可覆盖：写成 "秒" 或 "最小-最大" 秒，例如 "3-5"、"1"、"0"。
var (
	pageDelayMin = 3 * time.Second
	pageDelayMax = 5 * time.Second
)

func init() {
	v := strings.TrimSpace(os.Getenv("VDL_COLLECT_PAGE_DELAY"))
	if v == "" {
		return
	}
	parse := func(s string) (time.Duration, bool) {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 {
			return time.Duration(n) * time.Second, true
		}
		return 0, false
	}
	if i := strings.IndexByte(v, '-'); i > 0 {
		if lo, ok1 := parse(v[:i]); ok1 {
			if hi, ok2 := parse(v[i+1:]); ok2 {
				pageDelayMin, pageDelayMax = lo, hi
				if pageDelayMax < pageDelayMin {
					pageDelayMax = pageDelayMin
				}
			}
		}
		return
	}
	if d, ok := parse(v); ok {
		pageDelayMin, pageDelayMax = d, d
	}
}

// pageGap 一页与下一页之间的等待时长（3~5 秒随机）
func pageGap() time.Duration {
	if pageDelayMax <= 0 {
		return 0
	}
	if pageDelayMax <= pageDelayMin {
		return pageDelayMin
	}
	return pageDelayMin + time.Duration(randInt(int((pageDelayMax-pageDelayMin)/time.Millisecond)))*time.Millisecond
}

// failGate 记录「连续失败轮次」。任一次成功都会清零，
// 所以偶发抖动只会触发等待，连续 3 轮才说明源站真的挂了。
//
// 为什么按「轮」而不是按「次」：分页是 12 个 worker 并发的，源站一挂，
// 同一瞬间会有一批请求一起失败。如果按次数算，3 个并发失败立刻就到 3，
// 根本等不到 180 秒就把整个接口判死。按轮计算的话，同一批并发失败只算一轮，
// 等完 180 秒后重试仍然失败才算第二轮。
type failGate struct {
	mu      sync.Mutex
	streak  int // 连续失败轮次
	round   int // 当前轮次编号（每次等待结束进入新一轮）
	counted int // 已经为哪一轮记过失败
}

// newFailGate 新建闸门。counted 初始为 -1：第 0 轮的第一批失败必须计入。
func newFailGate() *failGate { return &failGate{counted: -1} }

// fail 记一次失败，返回当前连续失败轮次与是否已达上限
func (g *failGate) fail() (int, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.counted != g.round {
		g.streak++
		g.counted = g.round
	}
	return g.streak, g.streak >= maxConsecutiveFail
}

// ok 记一次成功，返回此前是否存在连续失败（用于清掉进度里的重试提示）
func (g *failGate) ok() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	had := g.streak > 0
	g.streak = 0
	return had
}

// beginRound 等待结束后调用：同一批并发失败不会重复计数
func (g *failGate) beginRound() {
	g.mu.Lock()
	g.round++
	g.mu.Unlock()
}

// randInt 返回 [0,n) 的随机数（用于请求间隔抖动）
func randInt(n int) int {
	if n <= 0 {
		return 0
	}
	return rand.Intn(n)
}

// crawlStats 分页并发时的统计（多 goroutine 累加，需加锁）
type crawlStats struct {
	mu       sync.Mutex
	fetched  int
	added    int
	updated  int
	details  int
	pagesOK  int
	pagesErr int
	maxPage  int
	errMsg   string
}

func (s *crawlStats) add(items, added, updated, details, page int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetched += items
	s.added += added
	s.updated += updated
	s.details += details
	s.pagesOK++
	if page > s.maxPage {
		s.maxPage = page
	}
}

func (s *crawlStats) fail(page int, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pagesErr++
	if s.errMsg == "" {
		s.errMsg = fmt.Sprintf("第 %d 页: %s", page, msg)
	}
	if page > s.maxPage {
		s.maxPage = page
	}
}

// crawl 单个接口的采集流程：
//  1. 先取探路页（拿 pagecount，并检测源站是否支持 pg 分页）
//  2. 剩余页交由 worker 池并发抓取，每页内含 list + 批量 detail + 入库
//  3. limitPages > 0 时只采前 limitPages 页（定时增量任务用）
//
// 采集过程中随时可能收到「停止」请求（见 Progress.Stop），每个页边界都会检查一次，
// 收到后立刻收尾：保存断点、标记 phase=stopped。
func (c *Collector) crawl(src Source, hours, limitPages int) {
	mode := "full"
	if hours > 0 {
		mode = "incr"
	}
	stop := GlobalProgress.stopChan(src.ID)
	// 停止信号 → context：这样正在飞的 HTTP 请求也能立刻中断，
	// 而不是等它自然超时（单页请求最长 40s，等起来像卡死）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if stop != nil {
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	startPage := 1
	if hours == 0 {
		var last, completed int
		// 上次已经跑完（completed=1）时，断点就是最后一页，再"续采"等于什么都不做；
		// 这种情况下重新从头采，否则用户点「全量采集」会瞬间结束、看着像没反应。
		if err := c.DB.QueryRow(`SELECT last_page, COALESCE(completed,0) FROM vod_sync_state WHERE source_id=? AND mode='full'`, src.ID).
			Scan(&last, &completed); err == nil && last > 0 {
			if completed == 1 {
				startPage = 1
				logx.Printf("[full] %s 上次已采到最后一页，本轮从第 1 页重新开始", src.Name)
			} else {
				startPage = last + 1
				logx.Printf("[full] %s 从第 %d 页续采", src.Name, startPage)
			}
		}
	}
	GlobalProgress.update(src.ID, func(sp *SourceProgress) { sp.Phase = "running" })

	// 失败闸门：同一接口内共享「连续失败次数」
	gate := newFailGate()

	// 探路页：默认用 ac=detail 拿全字段；若源站不支持 detail 分页则回退 ac=list
	useAC := acDetail
	first, err := c.fetchPageRetry(ctx, &src, startPage, hours, useAC, gate)
	if err == nil && len(first.Items) == 0 {
		if alt, e2 := c.fetchPage(ctx, &src, startPage, hours, acList); e2 == nil && len(alt.Items) > 0 {
			logx.Printf("[collect] %s ac=detail 无数据，回退 ac=list 模式", src.Name)
			useAC, first = acList, alt
		}
	}
	if err != nil {
		if ctx.Err() != nil { // 停止导致的请求失败不算错误
			c.finishStopped(src, mode, 0, 0)
			return
		}
		GlobalProgress.update(src.ID, func(sp *SourceProgress) {
			sp.Phase, sp.Error = "failed", err.Error()
		})
		return
	}
	if ctx.Err() != nil {
		c.finishStopped(src, mode, 0, 0)
		return
	}
	pageCount := first.PageCount
	if pageCount <= 0 && first.Total > 0 {
		effLimit := first.Limit
		if effLimit <= 0 {
			effLimit = len(first.Items)
		}
		if effLimit > 0 {
			pageCount = (first.Total + effLimit - 1) / effLimit
		}
	}
	if pageCount <= 0 {
		pageCount = startPage
	}
	if pageCount > maxPages {
		pageCount = maxPages
	}
	// 定时增量只同步前 N 页（源站按更新时间倒序，前几页即最新数据）
	if limitPages > 0 && pageCount > limitPages {
		pageCount = limitPages
	}
	GlobalProgress.update(src.ID, func(sp *SourceProgress) { sp.PageCount = pageCount })

	if len(first.Items) == 0 {
		logx.Printf("[collect] %s 第 %d 页无数据，采集结束", src.Name, startPage)
		GlobalProgress.update(src.ID, func(sp *SourceProgress) { sp.Phase = "done" })
		return
	}

	// 分页有效性检测：部分源站（如 uuzy.me）忽略 pg 参数，每页都返回同一批数据。
	// 并发抓取前必须先探测一次，否则会白白并发请求上万次重复页。
	if ctx.Err() != nil {
		c.finishStopped(src, mode, 0, pageCount)
		return
	}
	if pageCount > startPage {
		if probe, perr := c.fetchPage(ctx, &src, startPage+1, hours, useAC); perr == nil {
			if pageSignature(probe.Items) == pageSignature(first.Items) {
				msg := "源站忽略 pg 参数（第 2 页与第 1 页内容完全相同），已停止分页。" +
					"该接口不支持分页全量采集，只能采到单页数据；增量采集不受影响。"
				logx.Printf("[collect] %s %s", src.Name, msg)
				added, updated, details := c.savePage(ctx, &src, first.Items)
				logx.Printf("[collect] %s page %d 本页 %d 条 新增 %d 更新 %d 详情 %d",
					src.Name, startPage, len(first.Items), added, updated, details)
				c.saveState(src.ID, mode, startPage, pageCount, false)
				GlobalProgress.update(src.ID, func(sp *SourceProgress) {
					sp.Page, sp.Fetched = startPage, len(first.Items)
					sp.Added, sp.Updated, sp.Details = added, updated, details
					sp.Phase, sp.Error = "warning", msg
				})
				return
			}
		}
	}

	workers := c.PageWorkers
	if workers <= 0 {
		workers = defaultPageWorkers
	}
	remain := pageCount - startPage
	if remain < workers {
		workers = remain
	}
	if workers < 1 {
		workers = 1
	}
	logx.Printf("[collect] %s 开始并发采集：页 %d~%d，并发 %d", src.Name, startPage, pageCount, workers)

	stats := &crawlStats{}
	if added, updated, details := c.savePage(ctx, &src, first.Items); true {
		stats.add(len(first.Items), added, updated, details, startPage)
		c.reportProgress(src.ID, startPage-1, stats)
	}

	jobs := make(chan int, workers*2)
	var wg sync.WaitGroup
	// 连续失败达到上限时终止整个接口：记下原因（只留第一条），并取消 ctx 让其他 worker 收手
	var fatalMu sync.Mutex
	var fatalMsg string
	setFatal := func(msg string) {
		fatalMu.Lock()
		if fatalMsg == "" {
			fatalMsg = msg
		}
		fatalMu.Unlock()
		cancel()
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				// 已请求停止：把队列里剩下的页码快速消费掉（不抓取），让投递方不会阻塞
				if ctx.Err() != nil {
					continue
				}
				// 全量同步：每页之间等 3~5 秒，避免把源站打崩（会直接 520/掐连接）
				if hours == 0 {
					sleepCtx(ctx, pageGap())
				} else {
					// 增量只采前几页，保留原来的轻微抖动即可
					sleepCtx(ctx, time.Duration(10+randInt(40))*time.Millisecond)
				}
				pr, ferr := c.fetchPageRetry(ctx, &src, p, hours, useAC, gate)
				if ferr != nil {
					if ctx.Err() != nil { // 停止/终止导致的失败不再统计
						continue
					}
					stats.fail(p, ferr.Error())
					c.reportProgress(src.ID, startPage-1, stats)
					setFatal(ferr.Error())
					continue
				}
				if len(pr.Items) == 0 { // 越界页（pagecount 不准时常见）
					stats.add(0, 0, 0, 0, p)
					c.reportProgress(src.ID, startPage-1, stats)
					continue
				}
				added, updated, details := c.savePage(ctx, &src, pr.Items)
				stats.add(len(pr.Items), added, updated, details, p)
				c.reportProgress(src.ID, startPage-1, stats)
			}
		}()
	}
feed:
	for p := startPage + 1; p <= pageCount; p++ {
		select {
		case <-ctx.Done(): // 停止：不再投递新页码
			break feed
		case jobs <- p:
		}
	}
	close(jobs)
	wg.Wait()

	// 收尾
	stats.mu.Lock()
	errs, okPages, maxPage, errMsg := stats.pagesErr, stats.pagesOK, stats.maxPage, stats.errMsg
	stats.mu.Unlock()
	c.reportProgress(src.ID, startPage-1, stats)

	// 「停止」优先于「失败」：用户点了停止就不再报失败
	if canceled(stop) {
		c.finishStopped(src, mode, maxPage, pageCount)
		return
	}
	fatalMu.Lock()
	fmsg := fatalMsg
	fatalMu.Unlock()
	if fmsg != "" {
		c.saveState(src.ID, mode, maxPage, pageCount, false)
		logx.Printf("[collect] %s 连续 %d 轮抓取失败，已终止本接口的采集：%s", src.Name, maxConsecutiveFail, fmsg)
		GlobalProgress.update(src.ID, func(sp *SourceProgress) {
			sp.Phase = "failed"
			sp.Error = fmt.Sprintf("连续 %d 轮抓取失败，已终止（已完成到第 %d 页，可再点全量按断点续采）：%s",
				maxConsecutiveFail, maxPage, fmsg)
		})
		return
	}
	if ctx.Err() != nil {
		c.finishStopped(src, mode, maxPage, pageCount)
		return
	}

	completed := okPages+errs >= pageCount-startPage+1
	c.saveState(src.ID, mode, maxPage, pageCount, completed && errs == 0)

	if errs > 0 {
		msg := fmt.Sprintf("%d 页抓取失败（已跳过），可再点一次「全量同步」按断点补齐。首个失败：%s", errs, errMsg)
		logx.Printf("[collect] %s %s", src.Name, msg)
		GlobalProgress.update(src.ID, func(sp *SourceProgress) { sp.Phase, sp.Error = "warning", msg })
	} else {
		GlobalProgress.update(src.ID, func(sp *SourceProgress) { sp.Phase = "done" })
	}
	logx.Printf("[collect] %s 采集结束：成功 %d 页，失败 %d 页", src.Name, okPages, errs)
	// 全量是几十万行级别的大改动，结束后刷新一次统计信息：
	// 否则 SQLite 还按旧的数据分布挑索引，列表/搜索会莫名其妙变慢。
	if mode == "full" {
		db.OptimizeStats(c.DB)
	}
}

// fetchPage 拉取一页。ac=detail 时源站一次返回全字段（封面/演员/地区/年份/简介/播放地址），
// 无需再逐条补 detail，采集效率更高、数据更完整。
func (c *Collector) fetchPage(ctx context.Context, src *Source, page, hours int, ac string) (*pageResult, error) {
	q := url.Values{"ac": {ac}, "pg": {strconv.Itoa(page)}, "limit": {strconv.Itoa(pageSize)}}
	if hours > 0 {
		q.Set("h", strconv.Itoa(hours))
		q.Set("by", "time")
		q.Set("sort", "desc")
	}
	return c.fetch(ctx, src, q)
}

// fetchPageRetry 拉取一页，失败时等 180 秒重试；连续 3 次失败才返回错误（由调用方终止采集）。
// 只有「连续」失败才计数，成功一次就清零 —— 源站偶发超时不会把整个接口的同步打断。
func (c *Collector) fetchPageRetry(ctx context.Context, src *Source, page, hours int, ac string, gate *failGate) (*pageResult, error) {
	for {
		pr, err := c.fetchPage(ctx, src, page, hours, ac)
		if err == nil {
			if gate.ok() {
				// 之前是抖动，已经恢复：清掉进度里的「稍后重试」提示
				GlobalProgress.update(src.ID, func(sp *SourceProgress) { sp.Error = "" })
			}
			return pr, nil
		}
		if ctx.Err() != nil { // 用户停止：直接把取消原因抛回去
			return nil, err
		}
		n, fatal := gate.fail()
		if fatal {
			return nil, fmt.Errorf("连续 %d 轮抓取失败（每轮间隔 %d 秒）：%w",
				maxConsecutiveFail, int(failRetryDelay.Seconds()), err)
		}
		logx.Printf("[collect] %s 第 %d 页抓取失败（连续第 %d/%d 轮），%d 秒后重试：%v",
			src.Name, page, n, maxConsecutiveFail, int(failRetryDelay.Seconds()), err)
		GlobalProgress.update(src.ID, func(sp *SourceProgress) {
			sp.Error = fmt.Sprintf("第 %d 页抓取失败，%d 秒后重试（连续 %d/%d 轮）",
				page, int(failRetryDelay.Seconds()), n, maxConsecutiveFail)
		})
		sleepCtx(ctx, failRetryDelay)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// 等待结束，进入下一轮：下一批失败才继续累计
		gate.beginRound()
	}
}

// canceled 非阻塞检查是否已收到「停止采集」请求（stop 为 nil 表示没有取消通道）
func canceled(stop <-chan struct{}) bool {
	if stop == nil {
		return false
	}
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// finishStopped 用户主动停止后的收尾：已入库的数据保留，记下断点供下次续采。
// maxPage > 0 时把全量断点推进到已完成的最大页，下次点「全量采集」从下一页继续。
func (c *Collector) finishStopped(src Source, mode string, maxPage, pageCount int) {
	if maxPage > 0 {
		c.saveState(src.ID, mode, maxPage, pageCount, false)
	}
	logx.Printf("[collect] %s 已按请求停止采集（已完成到第 %d 页）", src.Name, maxPage)
	GlobalProgress.update(src.ID, func(sp *SourceProgress) {
		sp.Phase, sp.Error = "stopped", ""
	})
}

// reportProgress 把统计结果同步到全局进度（加锁）
func (c *Collector) reportProgress(sourceID int64, pageOffset int, st *crawlStats) {
	st.mu.Lock()
	fetched, added, updated, details, donePages, maxPage := st.fetched, st.added, st.updated, st.details, st.pagesOK, st.maxPage
	st.mu.Unlock()
	GlobalProgress.update(sourceID, func(sp *SourceProgress) {
		sp.Page = maxPage
		sp.Fetched, sp.Added, sp.Updated, sp.Details = fetched, added, updated, details
		if pageOffset+donePages > sp.Page {
			sp.Page = pageOffset + donePages
		}
	})
}

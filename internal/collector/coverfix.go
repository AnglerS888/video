package collector

// 封面补充（信息页手动任务）
//
// 背景：采集时部分源站的封面（vod_pic）拉不到（防盗链/图床挂/429），
// 而同一部剧往往在多个接口源里都有记录 —— 只要有一条记录的同名封面
// 已经在本地缓存里，就可以直接拿它的图片把「拉不到的那条」补上。
//
// 本任务做三件事：
//  1. 扫描 vod_info 里全部封面地址，逐个确认本地缓存文件是否存在（只 stat，不联网）
//  2. 对缺失的封面，先找同名（同剧）其它源已缓存的封面，直接复制过去
//  3. 找不到可复制的，再尝试重新下载；仍不行就计入失败
//
// 之所以「复制文件」而不是改库里的 vod_pic：封面路由 /api/img?u=<url> 是按 URL 哈希
// 找本地文件的，把字节写到目标 URL 对应的哈希路径上，前端、缓存、DB 全都不用动。
// 复制过去的图仍是 480px 压缩图，和正常缓存的封面一致。

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"videoDownLoad/internal/imgproc"
	"videoDownLoad/internal/logx"
)

// CoverFixStatus 封面补充任务的进度快照（给前端轮询）
type CoverFixStatus struct {
	Running bool   `json:"running"`
	Phase   string `json:"phase"` // idle / scanning / processing / stopping / done / stopped / failed
	// Videos/Covers 扫描阶段：影片记录数、去重后的封面地址数
	Videos int `json:"videos"`
	Covers int `json:"covers"`
	// Checked 已检查本地缓存的封面数（扫描阶段进度）
	Checked int `json:"checked"`
	// Healthy 本地已有缓存的封面数；Missing 需要处理的封面数（= 本次任务总量）
	Healthy int `json:"healthy"`
	Missing int `json:"missing"`
	// Processed 已处理数；Fetched 重新下载成功；Filled 用其它源封面补上；Proxied 走图片代理补上；Failed 仍失败
	Processed int `json:"processed"`
	Fetched   int `json:"fetched"`
	Filled    int `json:"filled"`
	Proxied   int `json:"proxied"`
	Failed    int `json:"failed"`

	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	ElapsedSec int64  `json:"elapsed_sec"`
	LastError  string `json:"last_error"`
	Message    string `json:"message"`
}

var coverFix = struct {
	mu        sync.Mutex
	running   bool
	stopCh    chan struct{}
	startedAt time.Time
	st        CoverFixStatus
}{
	st: CoverFixStatus{Phase: "idle"},
}

// coverFixTarget 一个需要处理的封面地址
type coverFixTarget struct {
	URL  string
	Name string
}

// StartCoverSupplement 启动封面补充任务；已在运行时返回错误。
func (c *Collector) StartCoverSupplement() error {
	if c.CoverDir == "" {
		return fmt.Errorf("封面目录未配置")
	}
	coverFix.mu.Lock()
	if coverFix.running {
		coverFix.mu.Unlock()
		return fmt.Errorf("封面补充正在进行中")
	}
	ch := make(chan struct{})
	coverFix.running = true
	coverFix.stopCh = ch
	coverFix.startedAt = time.Now()
	coverFix.st = CoverFixStatus{
		Phase:     "scanning",
		StartedAt: time.Now().Format("2006-01-02 15:04:05"),
	}
	coverFix.mu.Unlock()

	go c.runCoverSupplement(ch)
	return nil
}

// StopCoverSupplement 请求停止（当前这一张图下完后退出）。
func StopCoverSupplement() bool {
	coverFix.mu.Lock()
	defer coverFix.mu.Unlock()
	if !coverFix.running || coverFix.stopCh == nil {
		return false
	}
	select {
	case <-coverFix.stopCh:
	default:
		close(coverFix.stopCh)
	}
	coverFix.st.Phase = "stopping"
	coverFix.st.Message = "正在停止…"
	return true
}

// CoverSupplementStatus 读取当前进度（elapsed 实时算）
func CoverSupplementStatus() CoverFixStatus {
	coverFix.mu.Lock()
	defer coverFix.mu.Unlock()
	st := coverFix.st
	st.Running = coverFix.running
	if coverFix.running && !coverFix.startedAt.IsZero() {
		st.ElapsedSec = int64(time.Since(coverFix.startedAt).Seconds())
	}
	return st
}

func coverFixUpdate(fn func(st *CoverFixStatus)) {
	coverFix.mu.Lock()
	fn(&coverFix.st)
	coverFix.mu.Unlock()
}

func coverFixStopped(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (c *Collector) runCoverSupplement(stop <-chan struct{}) {
	defer func() {
		coverFix.mu.Lock()
		coverFix.running = false
		coverFix.stopCh = nil
		switch coverFix.st.Phase {
		case "stopping":
			coverFix.st.Phase = "stopped"
		case "failed":
			// 保留 failed
		default:
			coverFix.st.Phase = "done"
		}
		if coverFix.st.Phase == "done" && coverFix.st.Message == "" {
			coverFix.st.Message = coverFixSummary(coverFix.st)
		}
		coverFix.st.ElapsedSec = int64(time.Since(coverFix.startedAt).Seconds())
		coverFix.st.FinishedAt = time.Now().Format("2006-01-02 15:04:05")
		coverFix.mu.Unlock()
	}()

	imgDir := c.CoverDir
	logx.Infof("[coverfix] 开始检查封面缓存；直连失败时走图片代理 %s（间隔 %v~%v）",
		coverProxyBase(), coverProxyGapMin, coverProxyGapMax)

	// ---- 1. 扫描全库封面地址 ----
	// DB 只有 1 个连接：必须把结果集读完并关闭，之后才能做别的查询/耗时操作
	rows, err := c.DB.Query(`SELECT vod_name, vod_pic FROM vod_info WHERE vod_pic<>''`)
	if err != nil {
		coverFixUpdate(func(st *CoverFixStatus) {
			st.Phase = "failed"
			st.LastError = "读取影片表失败: " + err.Error()
			st.Message = "读取影片表失败"
		})
		return
	}
	byName := map[string][]string{} // 同剧（同名）→ 所有封面地址
	urlName := map[string]string{}  // 封面地址 → 剧名（去重）
	videos := 0
	for rows.Next() {
		var name, pic string
		if err := rows.Scan(&name, &pic); err != nil {
			continue
		}
		videos++
		pic = strings.TrimSpace(pic)
		if pic == "" || (!strings.HasPrefix(pic, "http://") && !strings.HasPrefix(pic, "https://")) {
			continue
		}
		if _, dup := urlName[pic]; dup {
			continue
		}
		urlName[pic] = name
		byName[name] = append(byName[name], pic)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		logx.Warnf("[coverfix] 扫描影片表出错: %v", err)
	}

	coverFixUpdate(func(st *CoverFixStatus) {
		st.Videos = videos
		st.Covers = len(urlName)
	})

	// 逐个确认本地缓存文件是否存在（分片目录 + 旧扁平目录）
	targets := make([]coverFixTarget, 0, len(urlName))
	healthy, checked := 0, 0
	for u, name := range urlName {
		if coverFixStopped(stop) {
			coverFixUpdate(func(st *CoverFixStatus) {
				st.Checked = checked
				st.Healthy = healthy
				st.Message = "已停止"
			})
			return
		}
		checked++
		if localCoverFile(imgDir, u) != "" {
			healthy++
			continue
		}
		targets = append(targets, coverFixTarget{URL: u, Name: name})
		// 扫描 10 万条 stat 也要几秒～几十秒，每 200 条回写一次进度
		if checked%200 == 0 {
			coverFixUpdate(func(st *CoverFixStatus) {
				st.Checked = checked
				st.Healthy = healthy
				st.Missing = len(targets)
			})
		}
	}

	coverFixUpdate(func(st *CoverFixStatus) {
		st.Checked = checked
		st.Healthy = healthy
		st.Missing = len(targets)
		st.Phase = "processing"
	})

	// ---- 2. 逐张补封面 ----
	// 同一图床连续失败 coverFixHostFailMax 次就整轮跳过它的地址：
	// 图床整体挂掉时（本机实测过 TLS 直接 EOF 的 CDN），逐张等超时能把任务拖成几十小时。
	fixCtx := &coverFixCtx{imgDir: imgDir, hostFail: map[string]int{}, hostSkip: map[string]bool{}}
	for i, t := range targets {
		if coverFixStopped(stop) {
			coverFixUpdate(func(st *CoverFixStatus) {
				st.Message = fmt.Sprintf("已停止（已处理 %d / %d）", st.Processed, st.Missing)
			})
			return
		}

		status := fixCtx.fixOne(t, byName[t.Name])
		coverFixUpdate(func(st *CoverFixStatus) {
			st.Processed = i + 1
			switch status {
			case coverFixFetched:
				st.Fetched++
			case coverFixFilled:
				st.Filled++
			case coverFixProxied:
				st.Proxied++
			default:
				st.Failed++
			}
			if i == len(targets)-1 {
				st.Message = coverFixSummary(*st)
			}
		})
		if status == coverFixFailed {
			logx.Debugf("[coverfix] 封面补充失败: %s（%s）", t.URL, t.Name)
		}
		if (i+1)%20 == 0 {
			st := CoverSupplementStatus()
			logx.Infof("[coverfix] 进度 %d/%d（重新下载 %d，其它源补充 %d，代理补充 %d，失败 %d）",
				i+1, len(targets), st.Fetched, st.Filled, st.Proxied, st.Failed)
		}
	}
	coverFixUpdate(func(st *CoverFixStatus) { st.Message = coverFixSummary(*st) })
}

const (
	coverFixFetched = iota // 重新下载成功
	coverFixFilled         // 用其它源的封面补上
	coverFixProxied        // 走图片代理下载成功
	coverFixFailed         // 其它源也没有可用封面
)

// ---- 图片代理兜底 ----
//
// 直连和「同剧其它源」都失败时，最后走图片代理再试一次（默认 https://wsrv.nl/?url=）：
// 防盗链、源站被墙、证书不兼容的图，代理那边通常还能抓到。
// 免费代理对频率很敏感（连打会直接 429），所以两次请求之间固定等 2~3 秒。
//
//	VDL_COVER_PROXY          代理前缀（必须带 url 参数，如 https://wsrv.nl/?url=）
//	VDL_COVER_PROXY_GAP_MS   间隔毫秒，写 "2000-3000" / "2500" / "0"（不限速）
var (
	coverProxyMu     sync.Mutex
	coverProxyNextAt time.Time
	coverProxyGapMin = 2 * time.Second
	coverProxyGapMax = 3 * time.Second
)

// coverProxyBase 图片代理前缀（默认 wsrv.nl）。每次现读环境变量，方便运行时切换/测试。
func coverProxyBase() string {
	if v := strings.TrimSpace(os.Getenv("VDL_COVER_PROXY")); v != "" {
		return v
	}
	return "https://wsrv.nl/?url="
}

func init() {
	v := strings.TrimSpace(os.Getenv("VDL_COVER_PROXY_GAP_MS"))
	if v == "" {
		return
	}
	parse := func(s string) (time.Duration, bool) {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 {
			return time.Duration(n) * time.Millisecond, true
		}
		return 0, false
	}
	if i := strings.IndexByte(v, '-'); i > 0 {
		if lo, ok := parse(v[:i]); ok {
			if hi, ok2 := parse(v[i+1:]); ok2 {
				coverProxyGapMin, coverProxyGapMax = lo, hi
			}
		}
	} else if d, ok := parse(v); ok {
		coverProxyGapMin, coverProxyGapMax = d, d
	}
	if coverProxyGapMax < coverProxyGapMin {
		coverProxyGapMax = coverProxyGapMin
	}
}

// coverProxyPace 代理请求节流：全局串行，默认每 2~3 秒才发下一次
func coverProxyPace() {
	if coverProxyGapMax <= 0 {
		return
	}
	coverProxyMu.Lock()
	now := time.Now()
	if coverProxyNextAt.Before(now) {
		coverProxyNextAt = now
	}
	at := coverProxyNextAt
	gap := coverProxyGapMin
	if coverProxyGapMax > coverProxyGapMin {
		gap += time.Duration(randInt(int((coverProxyGapMax-coverProxyGapMin)/time.Millisecond))) * time.Millisecond
	}
	coverProxyNextAt = at.Add(gap)
	coverProxyMu.Unlock()

	if d := time.Until(at); d > 0 {
		time.Sleep(d)
	}
}

// fetchCoverViaProxy 通过图片代理取图（调用前已按节流等待）
func fetchCoverViaProxy(raw string) ([]byte, error) {
	coverProxyPace()
	req, err := http.NewRequest("GET", coverProxyBase()+url.QueryEscape(raw), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0 Safari/537.36")
	req.Header.Set("Accept", "image/jpeg,image/png,image/gif,image/webp;q=0.9,*/*;q=0.5")
	resp, err := coverHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("图片代理: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("图片代理: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || len(b) == 0 {
		return nil, fmt.Errorf("图片代理: 读取失败 %v（%d 字节）", err, len(b))
	}
	return b, nil
}

// coverFixHostFailMax 同一图床连续失败多少次就整轮跳过（见 runCoverSupplement 里的说明）
const coverFixHostFailMax = 5

// coverFixCtx 单张封面的补充逻辑 + 图床熔断状态（任务内串行执行，无需加锁）
type coverFixCtx struct {
	imgDir   string
	hostFail map[string]int  // 图床 → 连续失败次数
	hostSkip map[string]bool // 图床 → 本轮跳过

	proxyFail int  // 代理连续失败次数
	proxyOff  bool // 代理连续失败太多，本轮不再用
}

// hostOf 取 URL 的 host（解析失败就整串当 key）
func hostOf(u string) string {
	if i := strings.Index(u, "://"); i > 0 {
		rest := u[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j > 0 {
			return rest[:j]
		}
		return rest
	}
	return u
}

// fetch 带图床熔断的取图：连续失败的图床直接跳过，不再等超时
func (x *coverFixCtx) fetch(u string) ([]byte, error) {
	host := hostOf(u)
	if x.hostSkip[host] {
		return nil, fmt.Errorf("图床 %s 连续失败，本轮跳过", host)
	}
	raw, err := fetchCoverBytes(u)
	if err != nil {
		x.hostFail[host]++
		if x.hostFail[host] >= coverFixHostFailMax {
			x.hostSkip[host] = true
			logx.Warnf("[coverfix] 图床 %s 连续失败 %d 次，本轮跳过该图床的封面", host, x.hostFail[host])
		}
		return nil, err
	}
	delete(x.hostFail, host)
	return raw, nil
}

// fetchProxy 走图片代理取图；连续失败 coverFixHostFailMax 次就整轮关掉代理，
// 免得代理被限流后还要对几万张图各等 2~3 秒。
func (x *coverFixCtx) fetchProxy(u string) ([]byte, error) {
	if x.proxyOff {
		return nil, fmt.Errorf("图片代理连续失败，本轮跳过")
	}
	raw, err := fetchCoverViaProxy(u)
	if err != nil {
		x.proxyFail++
		if x.proxyFail >= coverFixHostFailMax {
			x.proxyOff = true
			logx.Warnf("[coverfix] 图片代理连续失败 %d 次，本轮不再使用代理（%v）", x.proxyFail, err)
		}
		return nil, err
	}
	x.proxyFail = 0
	return raw, nil
}

// fixOne 处理一张缺失封面：先找同剧其它源已缓存的图，再试重新下载，最后试下载同剧其它源的图。
func (x *coverFixCtx) fixOne(t coverFixTarget, alts []string) int {
	targetPath := CoverPath(x.imgDir, t.URL)

	// a) 同剧其它源本地已有缓存：直接复制（不联网，最快）
	for _, alt := range alts {
		if alt == t.URL {
			continue
		}
		if src := localCoverFile(x.imgDir, alt); src != "" {
			if err := copyCoverFile(src, targetPath); err == nil {
				return coverFixFilled
			}
		}
	}

	// b) 重新下载本源的封面（可能只是采集时被限流/临时失败）
	var lastErr error
	if raw, err := x.fetch(t.URL); err == nil {
		if err := storeCoverBytes(x.imgDir, t.URL, raw); err == nil {
			return coverFixFetched
		} else {
			lastErr = err
		}
	} else {
		lastErr = err
	}

	// c) 下载同剧其它源的封面（最多 2 个，避免一个剧有十几个源时打太久）
	tried := 0
	for _, alt := range alts {
		if alt == t.URL || tried >= 2 {
			continue
		}
		tried++
		altPath := CoverPath(x.imgDir, alt)
		if src := localCoverFile(x.imgDir, alt); src != "" {
			if err := copyCoverFile(src, targetPath); err == nil {
				return coverFixFilled
			}
			continue
		}
		raw, err := x.fetch(alt)
		if err != nil {
			lastErr = err
			continue
		}
		if err := storeCoverBytes(x.imgDir, alt, raw); err != nil {
			lastErr = err
			continue
		}
		if err := copyCoverFile(altPath, targetPath); err == nil {
			return coverFixFilled
		}
		lastErr = fmt.Errorf("复制其它源封面失败")
	}

	// d) 直连全失败：走图片代理（默认 wsrv.nl，全局 2~3 秒一次）
	if raw, err := x.fetchProxy(t.URL); err == nil {
		if err := storeCoverBytes(x.imgDir, t.URL, raw); err == nil {
			return coverFixProxied
		} else {
			lastErr = err
		}
	} else {
		lastErr = err
	}

	// e) 代理再试一个同剧其它源的地址（只试一个，代理请求本身就很慢）
	for _, alt := range alts {
		if alt == t.URL {
			continue
		}
		if x.proxyOff {
			break
		}
		raw, err := x.fetchProxy(alt)
		if err != nil {
			lastErr = err
			break
		}
		if err := storeCoverBytes(x.imgDir, alt, raw); err != nil {
			lastErr = err
			break
		}
		if err := copyCoverFile(CoverPath(x.imgDir, alt), targetPath); err == nil {
			return coverFixProxied
		}
		lastErr = fmt.Errorf("复制代理下载的封面失败")
		break
	}

	if lastErr != nil {
		coverFixUpdate(func(st *CoverFixStatus) {
			st.LastError = fmt.Sprintf("%s：%v", t.Name, lastErr)
		})
	}
	return coverFixFailed
}

// localCoverFile 返回已存在且非空的封面文件路径（新分片路径优先，其次旧扁平路径）；没有则返回空
func localCoverFile(dir, u string) string {
	for _, p := range []string{CoverPath(dir, u), CoverPathLegacy(dir, u)} {
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			return p
		}
	}
	return ""
}

// copyCoverFile 把一张已缓存的封面复制到目标 URL 的缓存路径
func copyCoverFile(from, to string) error {
	if from == to {
		return nil
	}
	b, err := os.ReadFile(from)
	if err != nil || len(b) == 0 {
		return fmt.Errorf("读取源封面失败")
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	tmp := to + ".part"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, to)
}

// storeCoverBytes 压缩并落盘到封面缓存路径（与 fetchAndStoreCover 的压缩参数一致）
func storeCoverBytes(dir, u string, raw []byte) error {
	out, _, err := imgproc.CompressImage(raw, 480, 80)
	if err != nil {
		return fmt.Errorf("压缩失败: %v", err)
	}
	if len(out) == 0 {
		return fmt.Errorf("压缩结果为空")
	}
	p := CoverPath(dir, u)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".part"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// coverFixSummary 任务结束时的结果文案
func coverFixSummary(st CoverFixStatus) string {
	if st.Missing == 0 {
		return fmt.Sprintf("封面缓存全部正常（检查 %d 个封面）", st.Covers)
	}
	return fmt.Sprintf("处理完成：缺失 %d 张，重新下载 %d 张，其它源补上 %d 张，代理补上 %d 张，仍失败 %d 张",
		st.Missing, st.Fetched, st.Filled, st.Proxied, st.Failed)
}

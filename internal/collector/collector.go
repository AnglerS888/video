package collector

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	db "videoDownLoad/internal/db"
	"videoDownLoad/internal/imgproc"
	"videoDownLoad/internal/logx"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// ============================================================================
// 一、接口返回结构（依据《采集接口文档.docx》）
//
// 实测 5 个源站后的三条重要结论（决定了下列实现）：
//  1. 字段类型不统一：vod_id 可能是字符串、page/limit 可能是字符串、type_id 可能是 false、
//     vod_year/vod_score 可能是数字 → 必须柔性解析，否则整页数据解析失败（旧版"全量同步报错"主因）。
//  2. 部分源站在 ac=list 时不返回 vod_play_url（只有 vod_time/vod_remarks 等基础字段），
//     播放地址必须用 ac=detail&ids=xxx 批量补齐。
//  3. 异常状态码不代表失败：uuzy.me 正常返回数据但 HTTP 状态码是 404；
//     同时源站普遍忽略 limit 参数（固定 20 条/页）→ 分页结束不能用"本页不足 limit 条"判断。
// ============================================================================

type VodItem struct {
	VodID       int
	VodName     string
	TypeName    string
	VodPic      string
	VodActor    string
	VodDirector string
	VodYear     string
	VodArea     string
	VodLang     string
	VodRemarks  string
	VodScore    string
	VodContent  string
	VodPlayFrom string
	VodPlayURL  string
	SrcTime     string // 源站更新时间 vod_time
}

// ============================================================================
// 柔性类型 + 强类型解析
//
// 源站字段类型不统一（vod_id 可能是字符串、type_id 可能是 false、vod_score 可能是数字），
// 所以用带 UnmarshalJSON 的柔性类型兜底；但列表本体用**结构体**而不是 map[string]any：
// 一条数据从"十几个键的 map + interface 装箱 + 多次字符串拷贝"变成一段连续内存，
// 全量采集（几十万条）时的堆分配和 GC 压力能降一个量级 —— NAS 只有 4G 内存，这点很关键。
// ============================================================================

// flexString 兼容 string / number / bool / null / 对象数组
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			*f = "" // 单个字段坏值不要毁掉整页数据
			return nil
		}
		*f = flexString(strings.TrimSpace(s))
	case 't', 'f':
		if bytes.Equal(b, []byte("true")) {
			*f = "1"
		} else {
			*f = "0"
		}
	case '{', '[':
		// 少数源站把播放地址等返回成对象/数组：保留原始 JSON 文本（与旧实现一致）
		*f = flexString(string(b))
	default:
		// 数字：12.0 -> "12"（与旧的 float64 分支输出保持一致）
		if bytes.IndexByte(b, '.') >= 0 || bytes.IndexAny(b, "eE") >= 0 {
			if v, err := strconv.ParseFloat(string(b), 64); err == nil {
				*f = flexString(strconv.FormatFloat(v, 'f', -1, 64))
				return nil
			}
		}
		*f = flexString(string(b))
	}
	return nil
}

// flexInt 兼容 number / "12" / true / false / null
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	var s flexString
	_ = s.UnmarshalJSON(b)
	n, _ := strconv.Atoi(strings.TrimSpace(string(s)))
	*f = flexInt(n)
	return nil
}

type rawResp struct {
	Code      flexInt    `json:"code"`
	Msg       flexString `json:"msg"`
	Page      flexInt    `json:"page"`
	Pagecount flexInt    `json:"pagecount"`
	Limit     flexInt    `json:"limit"`
	Total     flexInt    `json:"total"`
	List      []vodJSON  `json:"list"`
}

// vodJSON 采集接口返回的单条影片（字段名与苹果CMS v10 一致）
type vodJSON struct {
	VodID       flexInt    `json:"vod_id"`
	VodName     flexString `json:"vod_name"`
	TypeName    flexString `json:"type_name"`
	VodClass    flexString `json:"vod_class"`
	VodPic      flexString `json:"vod_pic"`
	VodActor    flexString `json:"vod_actor"`
	VodDirector flexString `json:"vod_director"`
	VodYear     flexString `json:"vod_year"`
	VodArea     flexString `json:"vod_area"`
	VodLang     flexString `json:"vod_lang"`
	VodLanguage flexString `json:"vod_language"`
	VodRemarks  flexString `json:"vod_remarks"`
	VodNote     flexString `json:"vod_note"`
	VodState    flexString `json:"vod_state"`
	VodScore    flexString `json:"vod_score"`
	VodContent  flexString `json:"vod_content"`
	VodBlurb    flexString `json:"vod_blurb"`
	VodDes      flexString `json:"vod_des"`
	VodPlayFrom flexString `json:"vod_play_from"`
	VodPlayURL  flexString `json:"vod_play_url"`
	VodTime     flexString `json:"vod_time"`
}

// item 转成内部结构（清洗简介、兼容字段别名）
func (v *vodJSON) item() VodItem {
	return VodItem{
		VodID:       int(v.VodID),
		VodName:     string(v.VodName),
		TypeName:    firstNonEmpty(string(v.TypeName), string(v.VodClass)),
		VodPic:      string(v.VodPic),
		VodActor:    string(v.VodActor),
		VodDirector: string(v.VodDirector),
		VodYear:     string(v.VodYear),
		VodArea:     string(v.VodArea),
		VodLang:     firstNonEmpty(string(v.VodLang), string(v.VodLanguage)),
		VodRemarks:  firstNonEmpty(string(v.VodRemarks), string(v.VodNote), string(v.VodState)),
		VodScore:    string(v.VodScore),
		VodContent: firstNonEmpty(cleanText(string(v.VodContent)),
			cleanText(string(v.VodBlurb)), cleanText(string(v.VodDes))),
		VodPlayFrom: string(v.VodPlayFrom),
		VodPlayURL:  string(v.VodPlayURL),
		SrcTime:     string(v.VodTime),
	}
}

// 采集接口 ac 参数：detail 一次给全字段（封面/演员/地区/年份/简介/播放地址），优先使用
const (
	acDetail = "detail"
	acList   = "list"
)

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

var htmlUnescaper = strings.NewReplacer(
	"&nbsp;", " ", "&amp;", "&", "&quot;", `"`, "&#39;", "'",
	"&lt;", "<", "&gt;", ">", "&ldquo;", "“", "&rdquo;", "”", "&mdash;", "—",
)

// cleanText 清洗简介：去 HTML 标签、还原实体、压缩空白（源站简介常带 <p>&nbsp; 等）
func cleanText(s string) string {
	if s == "" {
		return ""
	}
	s = htmlUnescaper.Replace(s)
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ============================================================================
// 二、进度上报（所有写入都走 update() 加锁，避免与 /api/progress 读取竞争）
// ============================================================================

// SourceProgress 单个接口的采集进度。
// 每个接口独立维护自己的状态，因此可以同时点多个接口的「全量 / 增量」。
type SourceProgress struct {
	SourceID  int64  `json:"source_id"`
	Name      string `json:"name"`
	Mode      string `json:"mode"`  // full / incr
	Phase     string `json:"phase"` // pending/running/done/failed/warning
	Page      int    `json:"page"`
	PageCount int    `json:"page_count"`
	Fetched   int    `json:"fetched"`
	Added     int    `json:"added"`
	Updated   int    `json:"updated"`
	Details   int    `json:"details"`
	Running   bool   `json:"running"`
	StartedAt string `json:"started_at,omitempty"`
	Finished  string `json:"finished_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

type ProgressSnapshot struct {
	Mode         string           `json:"mode"` // 兼容旧字段：最近一次触发的模式
	Running      bool             `json:"running"`
	RunningCount int              `json:"running_count"`
	Finished     string           `json:"finished_at,omitempty"`
	Sources      []SourceProgress `json:"sources"`
}

// Progress 按接口记录进度（旧实现是全局单任务，只能同时采一个接口）
type Progress struct {
	mu      sync.Mutex
	sources map[int64]*SourceProgress
	cancels map[int64]chan struct{} // 每个接口的停止信号：close 即请求停止采集
	order   []int64
	lastMod string
}

func (p *Progress) Snapshot() ProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := ProgressSnapshot{Mode: p.lastMod, Finished: ""}
	for _, id := range p.order {
		sp, ok := p.sources[id]
		if !ok {
			continue
		}
		cp := *sp
		snap.Sources = append(snap.Sources, cp)
		if cp.Running {
			snap.RunningCount++
			snap.Finished = ""
		}
	}
	snap.Running = snap.RunningCount > 0
	return snap
}

// update 更新指定接口的进度；不存在时自动创建（便于并发启动时先上报）
func (p *Progress) update(id int64, fn func(*SourceProgress)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sp, ok := p.sources[id]
	if !ok {
		sp = &SourceProgress{SourceID: id, Phase: "pending"}
		p.sources[id] = sp
		p.order = append(p.order, id)
	}
	fn(sp)
}

// start 标记某接口开始采集；该接口已在采集则返回 false（不同接口互不影响）
func (p *Progress) start(src Source, mode string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sp, ok := p.sources[src.ID]; ok && sp.Running {
		return false
	}
	sp := &SourceProgress{
		SourceID: src.ID, Name: src.Name, Mode: mode, Phase: "pending",
		Running: true, StartedAt: time.Now().Format(timeLayout),
	}
	if _, ok := p.sources[src.ID]; !ok {
		p.order = append(p.order, src.ID)
	}
	p.sources[src.ID] = sp
	if p.cancels == nil {
		p.cancels = map[int64]chan struct{}{}
	}
	p.cancels[src.ID] = make(chan struct{})
	p.lastMod = mode
	return true
}

func (p *Progress) end(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sp, ok := p.sources[id]; ok {
		sp.Running = false
		sp.Finished = time.Now().Format(timeLayout)
		if sp.Phase == "pending" || sp.Phase == "running" || sp.Phase == "stopping" {
			sp.Phase = "done"
		}
	}
	delete(p.cancels, id)
}

// Stop 请求停止某个接口的采集：向该接口的取消通道发信号并标记为「停止中」。
// 真正退出由采集协程在下一个检查点完成（可能还需要等当前这一页抓完）。
// 返回 false 表示该接口本来就没在采集。
func (p *Progress) Stop(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	sp, ok := p.sources[id]
	if !ok || !sp.Running {
		return false
	}
	if ch, ok := p.cancels[id]; ok {
		select {
		case <-ch: // 已经发过停止信号
		default:
			close(ch)
		}
	}
	sp.Phase = "stopping"
	return true
}

// StopAll 停止全部正在采集的接口，返回实际发出停止信号的接口数。
func (p *Progress) StopAll() int {
	ids := p.RunningIDs()
	n := 0
	for _, id := range ids {
		if p.Stop(id) {
			n++
		}
	}
	return n
}

// stopChan 取该接口的停止信号通道（nil 表示该接口不受停止控制）
func (p *Progress) stopChan(id int64) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancels[id]
}

// IsRunning 该接口是否正在采集
func (p *Progress) IsRunning(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	sp, ok := p.sources[id]
	return ok && sp.Running
}

// RunningIDs 当前正在采集的接口 id
func (p *Progress) RunningIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []int64
	for _, id := range p.order {
		if sp, ok := p.sources[id]; ok && sp.Running {
			out = append(out, id)
		}
	}
	return out
}

var GlobalProgress = &Progress{sources: map[int64]*SourceProgress{}, lastMod: "-"}

// ============================================================================
// 三、Collector
// ============================================================================

type Source struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	APIURL   string `json:"api_url"`
	Enabled  int    `json:"enabled"`   // 是否启用（关掉后不参与任何采集）
	AutoIncr int    `json:"auto_incr"` // 是否参与定时自动增量（0=只能手动点增量）
	Remark   string `json:"remark"`
}

// distinctNames 取本条数据的剧名（去重，供聚合表增量刷新）
func distinctNames(items []VodItem) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for i := range items {
		n := items[i].VodName
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// dbtx 抽象 *sql.DB 与 *sql.Tx，便于把一页数据的写入合并进单个事务
type dbtx interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

type Collector struct {
	DB   *sql.DB
	HTTP *http.Client
	// CoverDir 封面压缩图的存储目录（data/img），同步时预取，页面访问直接读本地
	CoverDir string
	// PageWorkers 每个接口的分页并发数（默认 6）
	PageWorkers int
	// SourceWorkers 同时采集的接口数（默认 4）
	SourceWorkers int

	// 播放地址主机字典缓存：源站就那么几个域名，不必每行都去查一次 play_host。
	// 分页采集是多线程的，所以带锁。
	dictMu  sync.Mutex
	hostIDs map[string]int64
}

func New(db *sql.DB) *Collector {
	return &Collector{
		DB:            db,
		PageWorkers:   defaultPageWorkers,
		SourceWorkers: defaultSourceWorkers,
		hostIDs:       map[string]int64{},
		HTTP: &http.Client{
			Timeout: 40 * time.Second,
			Transport: &http.Transport{
				// 空闲连接池按并发量收敛：默认 MaxIdleConns=100 在 NAS 上没意义，
				// 只会让一批连接长期挂着
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     60 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
	}
}

// buildURL 拼接标准接口地址；scheme 非空时替换源站协议（https 不通时回落 http）
func buildURL(apiURL string, query url.Values, scheme string) string {
	u := strings.TrimRight(apiURL, "/?")
	if scheme != "" {
		if idx := strings.Index(u, "://"); idx > 0 {
			u = scheme + u[idx:]
		}
	}
	return u + "/?" + query.Encode()
}

// reqRaw 执行一次请求：网络错误才会真正失败；HTTP 非 200 也把 body 带回去，
// 由上层判断 body 是否为合法数据（实测部分源站 404 仍返回正常数据）。
// ctx 被取消（用户点了「停止采集」）时立刻中断请求，不再重试。
func (c *Collector) reqRaw(ctx context.Context, apiURL string, q url.Values) (body []byte, usedURL string, status int, err error) {
	var lastErr error
	schemes := []string{""}
	if strings.HasPrefix(strings.TrimSpace(apiURL), "https://") {
		schemes = append(schemes, "http") // https 握手/证书异常时回落 http
	}
	for _, scheme := range schemes {
		u := buildURL(apiURL, q, scheme)
		for attempt := 1; attempt <= 3; attempt++ {
			if cerr := ctx.Err(); cerr != nil {
				return nil, u, 0, cerr
			}
			req, rerr := http.NewRequestWithContext(ctx, "GET", u, nil)
			if rerr != nil {
				return nil, u, 0, rerr
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0 Safari/537.36")
			req.Header.Set("Accept", "application/json,text/plain,*/*")
			resp, derr := c.HTTP.Do(req)
			if derr != nil {
				if ctx.Err() != nil {
					return nil, u, 0, ctx.Err()
				}
				lastErr = fmt.Errorf("网络请求失败: %w", derr)
				sleepCtx(ctx, time.Duration(attempt)*700*time.Millisecond)
				continue
			}
			b, rerr2 := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
			resp.Body.Close()
			if rerr2 != nil {
				lastErr = fmt.Errorf("读取响应失败: %w", rerr2)
				continue
			}
			if len(b) == 0 {
				lastErr = fmt.Errorf("响应内容为空（HTTP %d）", resp.StatusCode)
				sleepCtx(ctx, time.Duration(attempt)*700*time.Millisecond)
				continue
			}
			// 200 / 404 直接返回（404 在部分源站是正常数据）；其余状态码重试一次
			if resp.StatusCode == 200 || resp.StatusCode == 404 {
				return b, u, resp.StatusCode, nil
			}
			lastErr = fmt.Errorf("HTTP %d，响应片段: %s", resp.StatusCode, snippet(b))
			if resp.StatusCode < 500 { // 4xx（非 404）多为拦截/参数问题，重试无意义
				return b, u, resp.StatusCode, nil
			}
			sleepCtx(ctx, time.Duration(attempt)*700*time.Millisecond)
		}
	}
	return nil, "", 0, lastErr
}

// sleepCtx 可被取消的等待：ctx 取消时立即返回，避免「停止采集」后还要等重试间隔。
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func snippet(b []byte) string {
	t := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(strings.TrimSpace(string(b)))
	if len(t) > 200 {
		t = t[:200] + "..."
	}
	return t
}

type pageResult struct {
	Items     []VodItem
	Page      int
	PageCount int
	Total     int
	Limit     int
	HTTPCode  int
	Format    string // json / xml（测试接口时展示，便于确认源站实际返回的格式）
}

// fetch 拉取一页列表数据（自动识别 JSON / XML，容忍非 200 状态码）
func (c *Collector) fetch(ctx context.Context, src *Source, q url.Values) (*pageResult, error) {
	body, u, status, err := c.reqRaw(ctx, src.APIURL, q)
	if err != nil {
		return nil, fmt.Errorf("%v（请求地址 %s）", err, u)
	}
	// 苹果CMS 有两种接口路径：/api.php/provide/vod/… 与 /api/provide/vod/…（去掉了 .php）。
	// 用户只填域名时我们默认补 .php 版本；若站点只有另一种写法，首包会 404。
	// 这里自动换一次，成功后把正确地址回写 vod_source —— 下次直接命中，不再多一次 404。
	if status == 404 {
		if alt, ok := alternateAPIURL(src.APIURL); ok {
			if b2, u2, s2, e2 := c.reqRaw(ctx, alt, q); e2 == nil && s2 == 200 {
				body, u, status = b2, u2, s2
				c.persistAPIURL(src, alt)
			}
		}
	}
	pr := &pageResult{HTTPCode: status, Format: "json"}

	head := strings.ToLower(strings.TrimLeft(string(body), " \t\r\n\ufeff"))
	if strings.HasPrefix(head, "<?xml") || strings.HasPrefix(head, "<rss") ||
		strings.HasPrefix(head, "<xml") || strings.HasPrefix(head, "<list") {
		items, page, pagecount, total, xerr := parseXML(toUTF8XML(body))
		if xerr != nil {
			return nil, xerr
		}
		pr.Items, pr.Page, pr.PageCount, pr.Total, pr.Format = items, page, pagecount, total, "xml"
		return pr, nil
	}

	var r rawResp
	if e := json.Unmarshal(body, &r); e != nil {
		return nil, fmt.Errorf("返回内容不是 JSON（HTTP %d）: %v，响应片段: %s", status, e, snippet(body))
	}
	pr.Page = int(r.Page)
	pr.PageCount = int(r.Pagecount)
	pr.Total = int(r.Total)
	pr.Limit = int(r.Limit)
	if int(r.Code) != 1 && len(r.List) == 0 {
		return pr, nil // code=0 且无数据：该页为空（翻到末尾也如此）
	}
	for i := range r.List {
		it := r.List[i].item()
		if it.VodID == 0 { // 个别源不返回 vod_id，用名称+地址哈希兜底
			h := sha256.Sum256([]byte(it.VodName + it.VodPlayURL))
			it.VodID = int(uint32(h[0])<<24|uint32(h[1])<<16|uint32(h[2])<<8|uint32(h[3])) & 0x7fffffff
		}
		if it.VodName == "" {
			continue
		}
		pr.Items = append(pr.Items, it)
	}
	return pr, nil
}

// alternateAPIURL 在两种苹果CMS 路径写法之间互换（子路径原样保留）。
func alternateAPIURL(u string) (string, bool) {
	switch {
	case strings.Contains(u, "/api.php/provide/vod"):
		return strings.Replace(u, "/api.php/provide/vod", "/api/provide/vod", 1), true
	case strings.Contains(u, "/api/provide/vod"):
		return strings.Replace(u, "/api/provide/vod", "/api.php/provide/vod", 1), true
	}
	return "", false
}

// apiURLFixed 记录本进程内已经纠正过的接口（并发分页线程只纠正/写库一次）
var apiURLFixed sync.Map

// persistAPIURL 把自动纠正后的地址写回 vod_source，下次采集直接用对的地址。
func (c *Collector) persistAPIURL(src *Source, fixed string) {
	if fixed == "" || fixed == src.APIURL {
		return
	}
	src.APIURL = fixed // 同一次采集里后续分页立刻用新地址
	if c.DB == nil || src.ID <= 0 {
		return
	}
	key := strconv.FormatInt(src.ID, 10)
	if _, loaded := apiURLFixed.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	if _, err := c.DB.Exec(`UPDATE vod_source SET api_url=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		fixed, src.ID); err != nil {
		logx.Printf("[collect] %s 接口地址回写失败（不影响本次采集）: %v", src.Name, err)
		return
	}
	logx.Printf("[collect] %s 接口地址自动纠正为 %s", src.Name, fixed)
}

// ---- XML 兼容（at=xml 或个别源默认返回 XML）----
//
// 苹果CMS v10 的 XML 变体（`/api.php/provide/vod/at/xml/`）字段与 JSON 一一对应，
// 但有四处差异，不处理就会「能采到标题、其它全空」：
//  1. 根节点是 <rss version="5.1"><list page="1" pagecount="5742" recordcount="114824">，
//     总条数的属性叫 recordcount，不叫 total；
//  2. ac=list 时只给 <id><name><type><dt><note><last>，封面/演员/播放地址要 ac=detail 才有；
//  3. 播放地址在 <dl><dd flag="线路名">第1集$url#第2集$url</dd></dl>，线路名是属性；
//  4. 文本普遍用 CDATA（encoding/xml 的 chardata 会自动还原，不用特殊处理）。
//
// 注意：分页属性挂在 <list> 上，不在 <rss> 上 —— 早先把 attr 写在根结构体上，
// 结果 page/pagecount/recordcount 永远读成 0（XML 源"共 0 页/0 条"的根因）。
type xmlList struct {
	Page        int        `xml:"page,attr"`
	PageCount   int        `xml:"pagecount,attr"`
	Total       int        `xml:"total,attr"`
	RecordCount int        `xml:"recordcount,attr"`
	PageSize    int        `xml:"pagesize,attr"`
	Videos      []xmlVideo `xml:"video"`
}

// xmlRSS 标准外壳：<rss><list page="1" pagecount="…" recordcount="…"><video>…</video></list></rss>
type xmlRSS struct {
	List xmlList `xml:"list"`
}

type xmlVideo struct {
	ID       string `xml:"id"`
	Name     string `xml:"name"`
	Type     string `xml:"type"`
	Pic      string `xml:"pic"`
	Actor    string `xml:"actor"`
	Director string `xml:"director"`
	Year     string `xml:"year"`
	Area     string `xml:"area"`
	Lang     string `xml:"lang"`
	Note     string `xml:"note"`
	State    string `xml:"state"`
	Score    string `xml:"score"`
	Des      string `xml:"des"`
	Last     string `xml:"last"`
	Time     string `xml:"time"`
	// DT：ac=list 时的线路名列表（如 "hyyun,hym3u8"），detail 才有真正的 <dl>
	DT string `xml:"dt"`
	DL struct {
		DD []struct {
			Flag string `xml:"flag,attr"`
			Text string `xml:",chardata"`
		} `xml:"dd"`
	} `xml:"dl"`
}

func parseXML(body []byte) (items []VodItem, page, pagecount, total int, err error) {
	var r xmlRSS
	if e := xml.Unmarshal(body, &r); e != nil {
		return nil, 0, 0, 0, fmt.Errorf("XML 解析失败: %v，响应片段: %s", e, snippet(body))
	}
	l := r.List
	// 根节点直接是 <list> 的变体（没有 <rss> 外壳）：上面按 <rss><list> 找不到任何条目，再试一次。
	// 注意要接受「空列表页」（f.Page>0 但没有 video），否则扁平结构的源翻到最后一页时
	// page/pagecount 会读成 0，被当成"没有分页"提前结束。
	if len(l.Videos) == 0 && l.Page == 0 {
		var f xmlList
		if e := xml.Unmarshal(body, &f); e == nil && (f.Page > 0 || len(f.Videos) > 0) {
			l = f
		}
	}
	page, pagecount = l.Page, l.PageCount
	total = l.Total
	if total == 0 {
		total = l.RecordCount
	}
	for _, v := range l.Videos {
		id, _ := strconv.Atoi(strings.TrimSpace(v.ID))
		var froms, urls []string
		for _, dd := range v.DL.DD {
			froms = append(froms, strings.TrimSpace(dd.Flag))
			urls = append(urls, strings.TrimSpace(dd.Text))
		}
		playFrom := strings.Join(froms, "$$$")
		if playFrom == "" {
			// 列表页只给 <dt>（逗号分隔的线路名），统一成下游用的 $$$ 分隔
			playFrom = strings.ReplaceAll(strings.TrimSpace(v.DT), ",", "$$$")
		}
		srcTime := firstNonEmpty(strings.TrimSpace(v.Last), strings.TrimSpace(v.Time))
		items = append(items, VodItem{
			VodID: id, VodName: strings.TrimSpace(v.Name), TypeName: strings.TrimSpace(v.Type),
			VodPic: strings.TrimSpace(v.Pic), VodActor: strings.TrimSpace(v.Actor),
			VodDirector: strings.TrimSpace(v.Director), VodYear: strings.TrimSpace(v.Year),
			VodArea: strings.TrimSpace(v.Area), VodLang: strings.TrimSpace(v.Lang),
			VodRemarks:  firstNonEmpty(strings.TrimSpace(v.Note), strings.TrimSpace(v.State)),
			VodScore:    strings.TrimSpace(v.Score),
			VodContent:  cleanText(v.Des),
			VodPlayFrom: playFrom,
			VodPlayURL:  strings.Join(urls, "$$$"),
			SrcTime:     srcTime,
		})
	}
	return
}

// toUTF8XML 把声明为 GBK/GB2312/GB18030 的 XML 转成 UTF-8。
//
// encoding/xml 只按 UTF-8 解码，遇到 GBK 不会报错，只会把中文变成乱码 ——
// 这种「静默乱码」比解析失败更难发现（表现为整库片名是问号/方块）。
// 老一些的苹果CMS 站点仍然输出 gb2312，所以这里按 XML 声明里的 encoding 兜一层。
func toUTF8XML(body []byte) []byte {
	head := body
	if len(head) > 256 {
		head = head[:256]
	}
	low := strings.ToLower(string(head))
	if !strings.Contains(low, "encoding=") {
		return body
	}
	if !strings.Contains(low, "gb2312") && !strings.Contains(low, "gbk") && !strings.Contains(low, "gb18030") {
		return body
	}
	out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), body)
	if err != nil {
		return body // 转换失败就按原样解析，至少不会整页丢掉
	}
	// 声明必须跟着改：字节已经转成 UTF-8，但 encoding="gb2312" 还留着的话，
	// encoding/xml 会直接报 `encoding "gb2312" declared but Decoder.CharsetReader is nil`
	return xmlEncodingRe.ReplaceAll(out, []byte(`encoding="utf-8"`))
}

// xmlEncodingRe 匹配 XML 声明里的 GB 系编码（大小写与引号都可能不同）
var xmlEncodingRe = regexp.MustCompile(`(?i)encoding\s*=\s*["'](?:gb2312|gbk|gb18030)["']`)

// fetchDetails 用 ac=detail&ids= 批量补齐播放地址（每批 50 个 id）
func (c *Collector) fetchDetails(ctx context.Context, src *Source, ids []int) map[int]VodItem {
	out := map[int]VodItem{}
	for i := 0; i < len(ids); i += 50 {
		if ctx.Err() != nil { // 已请求停止：剩下的批次直接放弃
			break
		}
		end := i + 50
		if end > len(ids) {
			end = len(ids)
		}
		strs := make([]string, 0, 50)
		for _, id := range ids[i:end] {
			strs = append(strs, strconv.Itoa(id))
		}
		pr, err := c.fetch(ctx, src, url.Values{"ac": {"detail"}, "ids": {strings.Join(strs, ",")}})
		if err != nil {
			logx.Printf("[detail] %s ids=%s: %v", src.Name, strings.Join(strs, ","), err)
			continue
		}
		for _, it := range pr.Items {
			out[it.VodID] = it
		}
	}
	return out
}

// querySources 读接口列表（所有调用方共用同一份列顺序）
func (c *Collector) querySources(q string) []Source {
	rows, err := c.DB.Query(q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []Source{}
	for rows.Next() {
		var s Source
		if err := rows.Scan(&s.ID, &s.Name, &s.APIURL, &s.Enabled, &s.Remark, &s.AutoIncr); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// SourcesByEnabled 读取接口列表
func (c *Collector) SourcesByEnabled(enabled bool) []Source {
	q := `SELECT id, name, api_url, enabled, remark, auto_incr FROM vod_source`
	if enabled {
		q += ` WHERE enabled=1`
	}
	q += ` ORDER BY id`
	return c.querySources(q)
}

func (c *Collector) EnabledSources() []Source { return c.SourcesByEnabled(true) }

// AutoIncrSources 定时增量任务要采的接口：启用 且 打开「自动增量」。
// 单独关掉自动增量的接口仍可以手动点「增量采集」「全量采集」。
func (c *Collector) AutoIncrSources() []Source {
	return c.querySources(`SELECT id, name, api_url, enabled, remark, auto_incr FROM vod_source
		WHERE enabled=1 AND auto_incr=1 ORDER BY id`)
}

// Test 测试单个接口连通性，返回诊断信息（页面"测试"按钮使用）
func (c *Collector) Test(id int64) map[string]any {
	var src Source
	if err := c.DB.QueryRow(`SELECT id, name, api_url, enabled, remark, auto_incr FROM vod_source WHERE id=?`, id).
		Scan(&src.ID, &src.Name, &src.APIURL, &src.Enabled, &src.Remark, &src.AutoIncr); err != nil {
		return map[string]any{"ok": false, "msg": "接口不存在"}
	}
	res := c.probeOne(context.Background(), &src)
	// fetch 可能已经把地址换成另一种写法（.php / 无 .php），这里回给前端显示
	res["api_url"] = src.APIURL
	return res
}

// Probe 依次尝试多个候选地址（两种 .php 写法 / JSON 与 XML），
// 返回第一个能真正解析出数据的地址。用于「还没入库」的域名预检。
func (c *Collector) Probe(candidates []string) map[string]any {
	// 预检是同步接口（用户点了「测试」在等），必须限总时长：
	// 域名打不开时每个候选都要走重试，不设上限会让按钮转几十秒。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tried := make([]string, 0, len(candidates))
	var last map[string]any
	for _, u := range candidates {
		src := &Source{Name: "接口预检", APIURL: u}
		res := c.probeOne(ctx, src)
		tried = append(tried, u)
		res["tried"] = tried
		res["api_url"] = src.APIURL
		if okv, _ := res["ok"].(bool); okv {
			if n, _ := res["parsed"].(int); n > 0 {
				return res
			}
		}
		last = res
		if ctx.Err() != nil {
			last["msg"] = fmt.Sprintf("%v（预检超时，先跳过剩余候选）", last["msg"])
			break
		}
	}
	if last == nil {
		return map[string]any{"ok": false, "msg": "没有可尝试的接口地址", "tried": tried}
	}
	if okv, _ := last["ok"].(bool); !okv {
		// 全失败：给人话总结，再保留最后一次的具体原因（403/超时/不是采集站）
		last["msg"] = fmt.Sprintf("没找到可用的采集接口（已试 %d 种常见地址）。最后一次：%v",
			len(tried), last["msg"])
		last["api_url"] = "" // 没有可用地址，别让前端显示「将使用」
	}
	return last
}

// probeOne 对一个具体地址做完整诊断：列表 → detail 播放地址 → 分页是否生效。
func (c *Collector) probeOne(ctx context.Context, src *Source) map[string]any {
	q := url.Values{"ac": {"list"}, "pg": {"1"}, "limit": {"2"}}
	start := time.Now()
	pr, err := c.fetch(ctx, src, q)
	cost := time.Since(start).Milliseconds()
	res := map[string]any{"cost_ms": cost}
	if err != nil {
		res["ok"], res["msg"] = false, err.Error()
		return res
	}
	res["ok"] = true
	res["http_code"] = pr.HTTPCode
	res["format"] = pr.Format
	res["parsed"] = len(pr.Items)
	res["page"], res["pagecount"], res["total"] = pr.Page, pr.PageCount, pr.Total
	if len(pr.Items) > 0 {
		it := pr.Items[0]
		res["sample"] = map[string]any{"vod_id": it.VodID, "vod_name": it.VodName,
			"vod_time": it.SrcTime, "vod_play_url": truncate(it.VodPlayURL, 100)}
	}
	// 顺带验证 detail 是否能拿到播放地址（这是决定能否下载的关键）
	if len(pr.Items) > 0 {
		d := c.fetchDetails(ctx, src, []int{pr.Items[0].VodID})
		if it, ok := d[pr.Items[0].VodID]; ok && it.VodPlayURL != "" {
			res["detail_ok"] = true
			res["detail_sample"] = truncate(it.VodPlayURL, 120)
		} else {
			res["detail_ok"] = false
		}
	}
	// 验证分页是否真的生效（部分源站忽略 pg 参数，导致全量只能采到第 1 页）
	if len(pr.Items) > 0 && pr.PageCount > 1 {
		if p2, e := c.fetch(ctx, src, url.Values{"ac": {"list"}, "pg": {"2"}, "limit": {"2"}}); e == nil {
			if pageSignature(p2.Items) == pageSignature(pr.Items) {
				res["pagination"] = false
				res["pagination_msg"] = "源站忽略 pg 参数，第 2 页与第 1 页内容相同，全量采集只能采到单页数据"
			} else {
				res["pagination"] = true
			}
		}
	}
	format := strings.ToUpper(pr.Format)
	if format == "" {
		format = "JSON"
	}
	msg := fmt.Sprintf("接口正常（HTTP %d · %s），本页 %d 条，共 %d 页 / %d 条",
		pr.HTTPCode, format, len(pr.Items), pr.PageCount, pr.Total)
	if pr.HTTPCode != 200 {
		msg += "；源站返回非 200 状态码但数据有效，已自动兼容"
	}
	if v, ok := res["pagination"]; ok && v == false {
		msg += "；⚠️ 该源不支持分页，全量只能采首页"
	}
	if v, ok := res["detail_ok"]; ok && v == false {
		msg += "；⚠️ 该源 detail 拿不到播放地址，采到的视频无法下载"
	}
	res["msg"] = msg
	return res
}

// truncate 按「字符」截断（不是字节）。
// 直接切字节会把中文/emoji 劈成半个字符，前端显示成 �（测试结果的 detail 样例踩过）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// ============================================================================
// 四、采集主流程
// ============================================================================

const (
	pageSize   = 100 // 每页向源站请求 100 条（部分源站会忽略并固定返回 20）
	maxPages   = 30000
	timeLayout = "2006-01-02 15:04:05"

	// maxRespBytes 单次接口响应体的硬上限。旧值 64MB 在弱机上是隐患：
	// 12 个分页线程 × 4 个接口同时命中大响应就会瞬间吃掉几 GB（NAS 只有 4G）。
	maxRespBytes = 16 << 20
)

// RunFull 全量采集。restart=false 时若上次未跑完则从断点续采。
// 每个接口独立起一个 goroutine，因此可以同时启动多个接口的全量采集；
// 已在采集的接口会被跳过（返回在 skipped 里）。
func (c *Collector) RunFull(sources []Source, restart bool) (started int, skipped []string) {
	return c.startJobs(sources, "full", 0, restart)
}

// RunIncremental 增量采集：只取各接口当天（24 小时内）有更新的数据；
// limitPages > 0 时只同步前 N 页（定时任务用，避免每次跑全量）。
func (c *Collector) RunIncremental(sources []Source, limitPages int) (started int, skipped []string) {
	return c.startJobs(sources, "incr", limitPages, false)
}

// maxConcurrentSources 同时采集的接口数硬上限（保护源站，也避免本地连接打满）
const maxConcurrentSources = 16

// sourceLimit 同时采集的接口数上限（可由 -sources 调整）
func (c *Collector) sourceLimit() int {
	n := c.SourceWorkers
	if n <= 0 {
		n = defaultSourceWorkers
	}
	if n > maxConcurrentSources {
		n = maxConcurrentSources
	}
	return n
}

// startJobs 为每个接口起独立采集协程（非阻塞，立即返回）
func (c *Collector) startJobs(sources []Source, mode string, limitPages int, restart bool) (int, []string) {
	var skipped []string
	started := 0
	limit := c.sourceLimit()
	for _, src := range sources {
		if len(GlobalProgress.RunningIDs()) >= limit {
			skipped = append(skipped, src.Name+"（同时采集的接口已达上限 "+strconv.Itoa(limit)+" 个）")
			continue
		}
		if !GlobalProgress.start(src, mode) {
			skipped = append(skipped, src.Name+"（正在采集中）")
			continue
		}
		started++
		if mode == "full" && restart {
			// 只重置该接口的断点，不影响其他接口
			c.DB.Exec(`UPDATE vod_sync_state SET last_page=0, completed=0 WHERE mode='full' AND source_id=?`, src.ID)
		}
		go func(src Source) {
			defer func() {
				if r := recover(); r != nil {
					logx.Printf("[collect] %s panic: %v", src.Name, r)
					GlobalProgress.update(src.ID, func(sp *SourceProgress) {
						sp.Phase, sp.Error = "failed", fmt.Sprintf("内部错误: %v", r)
					})
				}
				GlobalProgress.end(src.ID)
			}()
			if mode == "incr" {
				c.crawl(src, 24, limitPages)
			} else {
				c.crawl(src, 0, 0)
			}
		}(src)
	}
	return started, skipped
}

// pageSignature 用本页所有 vod_id 生成指纹，用于识别"源站忽略 pg 参数"的情况
func pageSignature(items []VodItem) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString(strconv.Itoa(it.VodID))
		b.WriteByte(',')
	}
	return b.String()
}

func (c *Collector) saveState(sourceID int64, mode string, page, totalPages int, completed bool) {
	done := 0
	if completed {
		done = 1
	}
	_, err := c.DB.Exec(`INSERT INTO vod_sync_state(source_id, mode, last_page, total_pages, completed, updated_at)
		VALUES(?,?,?,?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(source_id, mode) DO UPDATE SET last_page=excluded.last_page, total_pages=excluded.total_pages,
		completed=excluded.completed, updated_at=CURRENT_TIMESTAMP`,
		sourceID, mode, page, totalPages, done)
	if err != nil {
		logx.Printf("[state] 保存进度失败: %v", err)
	}
}

// savePage 处理一页数据：先写基础信息，再对"新增/有变化"的条目批量拉 detail 补播放地址
func (c *Collector) savePage(ctx context.Context, src *Source, items []VodItem) (added, updated, details int) {
	type pending struct {
		vid     int64
		idx     int
		oldHash string
	}
	var needDetail []pending

	// 阶段一：基础信息 + 直接带播放地址的条目，全部放进一个事务（大幅减少 fsync 次数）
	tx, terr := c.DB.Begin()
	var q dbtx = c.DB
	if terr == nil {
		q = tx
	} else {
		logx.Printf("[collect] 开启事务失败，退化为逐条写入: %v", terr)
	}
	for i := range items {
		vid, isNew, changed, oldHash, err := c.upsertBasic(q, src, &items[i])
		if err != nil {
			logx.Printf("[collect] %s#%d 基础信息入库失败: %v", src.Name, items[i].VodID, err)
			continue
		}
		if isNew {
			added++
		} else if changed {
			updated++
		}
		if !isNew && !changed {
			continue // 源站更新时间未变，跳过（不重复入库）
		}
		if strings.TrimSpace(items[i].VodPlayURL) != "" {
			c.replacePlays(q, vid, &items[i], oldHash)
		} else {
			needDetail = append(needDetail, pending{vid, i, oldHash})
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			logx.Printf("[collect] %s 提交事务失败: %v", src.Name, err)
		}
	}

	// 刷新剧名聚合表（一次一页涉及的剧名），列表/搜索走预聚合，避免全表 GROUP BY
	if aggNames := distinctNames(items); len(aggNames) > 0 {
		db.RefreshNameAgg(c.DB, aggNames)
	}

	// 同步时就预取封面（压缩后存 data/img），页面访问直接读本地
	picSeen := map[string]bool{}
	for i := range items {
		if p := strings.TrimSpace(items[i].VodPic); p != "" && !picSeen[p] {
			picSeen[p] = true
			c.queueCover(p)
		}
	}

	if len(needDetail) == 0 {
		return
	}
	// 阶段二：详情请求走网络（不占用事务），拿回播放地址
	ids := make([]int, len(needDetail))
	for i, p := range needDetail {
		ids[i] = items[p.idx].VodID
	}
	det := c.fetchDetails(ctx, src, ids)
	details = (len(ids) + 49) / 50

	// 阶段三：把详情里的播放地址写库，同样合并为一个事务
	tx2, terr2 := c.DB.Begin()
	var q2 dbtx = c.DB
	if terr2 == nil {
		q2 = tx2
	}
	for _, p := range needDetail {
		it := items[p.idx]
		if d, ok := det[it.VodID]; ok {
			if d.VodPlayURL != "" {
				it.VodPlayFrom, it.VodPlayURL = d.VodPlayFrom, d.VodPlayURL
			}
			if d.VodContent != "" {
				it.VodContent = d.VodContent
			}
			if d.TypeName != "" {
				it.TypeName = d.TypeName
			}
			if d.VodActor != "" {
				it.VodActor = d.VodActor
			}
			if d.VodDirector != "" {
				it.VodDirector = d.VodDirector
			}
			if d.VodPic != "" {
				it.VodPic = d.VodPic
			}
		}
		if it.VodPlayURL == "" {
			continue // 源站确实没有播放地址，先留基础信息
		}
		c.replacePlays(q2, p.vid, &it, p.oldHash)
		// 补拉回来的简介落 vod_detail（阶段一 ac=list 时拿不到，旧实现这里会丢）
		c.upsertDetail(q2, p.vid, it.VodContent)
	}
	if tx2 != nil {
		if err := tx2.Commit(); err != nil {
			logx.Printf("[collect] %s 提交事务失败: %v", src.Name, err)
		}
	}
	return
}

// upsertBasic 写入/更新视频基础信息，返回 (vid, isNew, changed, 旧播放地址指纹)
func (c *Collector) upsertBasic(q dbtx, src *Source, it *VodItem) (int64, bool, bool, string, error) {
	var vid int64
	var oldSrcTime, oldName, oldRemarks, oldType, oldPlayHash string
	err := q.QueryRow(`SELECT id, src_time, vod_name, vod_remarks, type_name, play_hash FROM vod_info
		WHERE source_id=? AND source_vod_id=?`, src.ID, it.VodID).Scan(&vid, &oldSrcTime, &oldName, &oldRemarks, &oldType, &oldPlayHash)

	if err == sql.ErrNoRows {
		// 列顺序：source_id ... src_time(13 个占位) + play_hash('') + updated_at(CURRENT_TIMESTAMP) = 15 列
		res, ierr := q.Exec(`INSERT INTO vod_info
			(source_id, source_vod_id, vod_name, type_name, vod_pic, vod_actor, vod_director,
			 vod_year, vod_area, vod_lang, vod_remarks, vod_score, src_time, play_hash, updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'',CURRENT_TIMESTAMP)`,
			src.ID, it.VodID, it.VodName, it.TypeName, it.VodPic, it.VodActor, it.VodDirector,
			it.VodYear, it.VodArea, it.VodLang, it.VodRemarks, it.VodScore, it.SrcTime)
		if ierr != nil {
			return 0, false, false, "", ierr
		}
		vid, _ = res.LastInsertId()
		c.upsertDetail(q, vid, it.VodContent)
		return vid, true, true, "", nil
	}
	if err != nil {
		return 0, false, false, "", err
	}

	// 去重判断一：源站更新时间（vod_time）未变 → 视为无变化
	if it.SrcTime != "" && oldSrcTime == it.SrcTime && oldName == it.VodName && oldRemarks == it.VodRemarks {
		return vid, false, false, oldPlayHash, nil
	}
	// 去重判断二：源站不提供 vod_time（如部分自建接口）时，用关键字段比对
	if it.SrcTime == "" && oldSrcTime == "" && oldName == it.VodName &&
		oldRemarks == it.VodRemarks && oldType == it.TypeName {
		return vid, false, false, oldPlayHash, nil
	}
	_, uerr := q.Exec(`UPDATE vod_info SET vod_name=?, type_name=?, vod_pic=?, vod_actor=?, vod_director=?,
		vod_year=?, vod_area=?, vod_lang=?, vod_remarks=?, vod_score=?, src_time=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=?`,
		it.VodName, it.TypeName, it.VodPic, it.VodActor, it.VodDirector,
		it.VodYear, it.VodArea, it.VodLang, it.VodRemarks, it.VodScore, it.SrcTime, vid)
	if uerr != nil {
		return 0, false, false, "", uerr
	}
	c.upsertDetail(q, vid, it.VodContent)
	return vid, false, true, oldPlayHash, nil
}

// upsertDetail 简介单独存 vod_detail 表（列表/聚合都不用它，只详情页读），
// 主表 vod_info 因此变窄 ~400B/行，整棵 B 树、全表扫描、备份体积全部受益。
// 新内容为空时不动旧值（与旧的 CASE WHEN 语义一致）。
func (c *Collector) upsertDetail(q dbtx, vid int64, content string) {
	if content == "" {
		return
	}
	if _, err := q.Exec(`INSERT INTO vod_detail(vod_id, content) VALUES(?,?)
		ON CONFLICT(vod_id) DO UPDATE SET content=excluded.content`, vid, content); err != nil {
		logx.Debugf("[collect] 简介入库失败 vod=%d: %v", vid, err)
	}
}

// replacePlays 重建该视频的播放地址表。
// oldHash 是库里存的旧播放地址指纹：与新指纹相同说明播放地址完全没变
// （典型场景：增量采集时源站只改了"更新至X集"备注），直接跳过
// DELETE+重插 —— 这一步是增量采集写放大的主要来源，449 万行的表
// 每部片重插一次要白写几十行、还要多留几十个空闲页。
func (c *Collector) replacePlays(q dbtx, vid int64, it *VodItem, oldHash string) {
	h := sha256.Sum256([]byte(it.VodPlayURL))
	hash := hex.EncodeToString(h[:8])
	if oldHash != "" && oldHash == hash {
		return
	}
	q.Exec(`DELETE FROM vod_play WHERE vod_id=?`, vid)
	q.Exec(`UPDATE vod_info SET play_hash=? WHERE id=?`, hash, vid)

	// 解析播放地址：$$$ 分源 → # 分集 → $ 分 名称/地址
	froms := strings.Split(it.VodPlayFrom, "$$$")
	groups := strings.Split(it.VodPlayURL, "$$$")
	type ep struct {
		idx  int
		name string
		from string
		url  string
	}
	var eps []ep
	for gi, g := range groups {
		var from string
		if gi < len(froms) {
			from = strings.TrimSpace(froms[gi])
		}
		if strings.TrimSpace(g) == "" {
			continue
		}
		for i, seg := range strings.Split(g, "#") {
			parts := strings.SplitN(seg, "$", 2)
			if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
				continue
			}
			eps = append(eps, ep{i, strings.TrimSpace(parts[0]), from, strings.TrimSpace(parts[1])})
		}
	}
	// 每条线路的首个 m3u8 集标记为优先（下载/播放默认用它）
	prefSet := map[string]bool{}
	// 同一部片、同一条线路里重复出现的地址只留第一条：源站常把同一集列两遍
	// （例如第 45、46 集给同一个 m3u8），写进去就是两条完全一样的播放地址，
	// 详情页会多出一集、播放表也会多一行。
	seenURL := map[string]bool{}
	kept, dropped, dup := 0, 0, 0
	for _, e := range eps {
		// 只入库真正的 m3u8：源站常把「网页分享页」也塞进 vod_play_url
		// （如 https://host/share/<hash>，实测返回 text/html），这类地址既不能播也不能下，
		// 入库只会让详情页多出无用线路、播放表凭空翻倍。
		if !isM3U8URL(e.url) {
			dropped++
			continue
		}
		key := e.from + "\x00" + e.url
		if seenURL[key] {
			dup++
			continue
		}
		seenURL[key] = true
		pref := 0
		if !prefSet[e.from] {
			prefSet[e.from] = true
			pref = 1
		}
		// 主机抽成字典：226 万行里每行省掉二十几个字节的重复域名
		host, path := db.SplitURL(e.url)
		// 集名能由集号推导出来就不存（读取时再推回去，见 db.EpNameExpr）
		name := e.name
		if name == db.EpName(e.idx) {
			name = ""
		}
		if _, err := q.Exec(`INSERT OR REPLACE INTO vod_play
			(vod_id, play_from, ep_index, ep_name, host_id, path, is_preferred)
			VALUES(?,?,?,?,?,?,?)`,
			vid, e.from, e.idx, name, c.hostID(q, host), path, pref); err != nil {
			logx.Printf("[collect] 播放地址入库失败 vod=%d: %v", vid, err)
		}
		kept++
	}
	if dropped > 0 && kept == 0 {
		logx.Debugf("[collect] vod=%d 的播放地址全部不是 m3u8（丢弃 %d 条）", vid, dropped)
	}
	if dup > 0 {
		logx.Debugf("[collect] vod=%d 有 %d 条重复播放地址已跳过（源站同一集列了两遍）", vid, dup)
	}
}

// hostID 取主机字典 id，没有就先插入。
// 注意用的是当前事务的 q：只有 1 个 DB 连接，不能在事务里另开连接。
// 事务回滚时缓存里的 id 可能落空，后果只是那批地址的 URL 拼不回来，
// 重新采集一次即可恢复。
func (c *Collector) hostID(q dbtx, host string) any {
	if host == "" {
		return nil
	}
	c.dictMu.Lock()
	defer c.dictMu.Unlock()
	if id, ok := c.hostIDs[host]; ok {
		return id
	}
	if _, err := q.Exec(`INSERT OR IGNORE INTO play_host(host) VALUES(?)`, host); err != nil {
		return nil
	}
	var id int64
	if err := q.QueryRow(`SELECT id FROM play_host WHERE host=?`, host).Scan(&id); err != nil {
		return nil
	}
	c.hostIDs[host] = id
	return id
}

// isM3U8URL 判断播放地址是不是 m3u8。
// 只认路径/查询里带 m3u8 的地址：源站的 html 分享页（/share/xxx）、mp4/flv 直链都不入库。
func isM3U8URL(u string) bool {
	return strings.Contains(strings.ToLower(u), "m3u8")
}

// ============================================================================
// 六、封面预取：同步采集时下载并压缩封面到 data/img/（页面访问直接读本地文件）
// ============================================================================

var (
	coverQueue   chan string
	coverMu      sync.Mutex
	coverFlight  = map[string]bool{}      // 正在下载的 URL（去重）
	coverFail    = map[string]time.Time{} // 失败的 URL -> 时间（负缓存，避免反复打源站）
	coverErr     = map[string]string{}    // 失败的 URL -> 最近一次原因（排查用）
	coverFailN   atomic.Int64             // 累计下载失败张数
	coverOKN     atomic.Int64             // 累计下载成功张数
	coverLastErr atomic.Value             // 最近一次失败原因
	imgProxy     string                   // 图片代理前缀，空=不用
	// coverSem 同时连接源站下载/解码封面的并发上限。
	// 每张图解码后要占几 MB（1920x2880 的 JPEG 解成 YCbCr 约 8MB），
	// 旧的固定 8 并发在 4G 内存的 NAS 上很容易顶出 OOM，改成可由 main 调整（默认 4）。
	coverSem  = make(chan struct{}, 4)
	coverHTTP = &http.Client{
		Timeout: 12 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     60 * time.Second,
		},
	}
)

const coverFailTTL = 10 * time.Minute

// coverQueueLen 封面预取队列长度（超过则丢弃，页面访问时按需补拉）
const coverQueueLen = 8192

// ---- 封面下载全局限速 ----
//
// 封面和采集走的是不同域名（图床 CDN），但图床对短时间批量抓图更敏感：
// 一页 100 条就是 100 张图，原来 3 个协程一口气全打过去，很容易被 429/403 甚至直接掐连接。
// 这里让所有封面协程共用一个「下一次允许发起」的时间点，全局串行节流，
// 默认每张间隔 500~1000 毫秒（约 1~2 张/秒）。
// 环境变量 VDL_COVER_GAP_MS 可覆盖：写 "500-1000"（毫秒）或 "800" 或 "0"（不限速）。
var (
	coverPaceMu  sync.Mutex
	coverNextAt  time.Time
	coverGapMin  = 500 * time.Millisecond
	coverGapMax  = 1000 * time.Millisecond
	coverCooling atomic.Int64 // 被限流时把队列整体往后推（unix 毫秒）
)

func init() {
	v := strings.TrimSpace(os.Getenv("VDL_COVER_GAP_MS"))
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
				coverGapMin, coverGapMax = lo, hi
			}
		}
	} else if d, ok := parse(v); ok {
		coverGapMin, coverGapMax = d, d
	}
	if coverGapMax < coverGapMin {
		coverGapMax = coverGapMin
	}
}

// coverPace 等到轮到本次下载：全局串行，避免多个协程同时打图床。
// 睡眠期间如果被限流（冷却时间被推后），会重新排队再等，保证冷却是真的生效的。
func coverPace() {
	if coverGapMax <= 0 {
		return
	}
	for {
		gap := coverGapMin
		if coverGapMax > coverGapMin {
			gap += time.Duration(randInt(int((coverGapMax-coverGapMin)/time.Millisecond))) * time.Millisecond
		}
		coverPaceMu.Lock()
		now := time.Now()
		if cool := time.UnixMilli(coverCooling.Load()); cool.After(now) {
			now = cool // 刚被限流过：整体往后推
		}
		if coverNextAt.Before(now) {
			coverNextAt = now
		}
		at := coverNextAt
		coverNextAt = coverNextAt.Add(gap)
		coverPaceMu.Unlock()

		if d := time.Until(at); d > 0 {
			time.Sleep(d)
		}
		if cool := time.UnixMilli(coverCooling.Load()); time.Now().Before(cool) {
			continue // 等的时候又被限流了，重新排队
		}
		return
	}
}

// coverCooldown 被限流（429/403）时暂停整个封面队列一段时间
func coverCooldown(d time.Duration) {
	until := time.Now().Add(d).UnixMilli()
	for {
		cur := coverCooling.Load()
		if cur >= until || coverCooling.CompareAndSwap(cur, until) {
			return
		}
	}
}

// SetCoverConcurrency 调整封面并发（main 按机器规格设置）
func SetCoverConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	if n > 16 {
		n = 16
	}
	coverSem = make(chan struct{}, n)
}

// StartCoverWorkers 启动封面下载压缩协程（独立于采集 worker，不阻塞主流程）
func (c *Collector) StartCoverWorkers(n int) {
	if c.CoverDir == "" {
		return
	}
	os.MkdirAll(c.CoverDir, 0o755)
	if n < 1 {
		n = 2
	}
	if n > 8 {
		n = 8
	}
	coverQueue = make(chan string, coverQueueLen)
	for i := 0; i < n; i++ {
		go func() {
			for u := range coverQueue {
				c.fetchAndStoreCover(u)
			}
		}()
	}
}

// queueCover 把封面加入预取队列（不阻塞，队列满则丢弃）
func (c *Collector) queueCover(u string) {
	if coverQueue == nil || u == "" {
		return
	}
	select {
	case coverQueue <- u:
	default: // 队列满则丢弃，页面访问时会按需补拉
	}
}

// EnsureCover 供 web 层调用：本地已有/正在下载/刚失败过则立即返回，否则排队预取
func EnsureCover(u string) { EnsureCoverRetry(u, false) }

// EnsureCoverRetry 同 EnsureCover；force=true 时忽略"近期失败"的负缓存再试一次。
//
// 用途：页面上某张封面 404 后，前端带 ?r=<时间戳> 重试；单源站点的图床恢复/刷新后，
// 靠它重新抓到新图，而不是干等 10 分钟负缓存过期。
func EnsureCoverRetry(u string, force bool) {
	if coverQueue == nil || u == "" {
		return
	}
	coverMu.Lock()
	if coverFlight[u] {
		coverMu.Unlock()
		return
	}
	if force {
		delete(coverFail, u)
		delete(coverErr, u)
	} else if t, bad := coverFail[u]; bad && time.Since(t) < coverFailTTL {
		coverMu.Unlock()
		return
	}
	coverMu.Unlock()
	select {
	case coverQueue <- u:
	default: // 队列满就交给浏览器稍后重试，绝不让 HTTP 处理器卡在入队上
	}
}

// EnsureCovers 批量预取（列表/详情返回前调用，浏览器请求图片时大概率已就绪）
func EnsureCovers(urls []string) {
	seen := make(map[string]bool, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u != "" && !seen[u] {
			seen[u] = true
			EnsureCover(u)
		}
	}
}

// CoverKnownBad 该 URL 近期下载失败过（负缓存内），页面直接给占位图，不必再等
func CoverKnownBad(u string) bool {
	coverMu.Lock()
	defer coverMu.Unlock()
	t, bad := coverFail[u]
	return bad && time.Since(t) < coverFailTTL
}

// CoverPath 封面本地路径：img/<哈希前2位>/<哈希>.jpg
//
// 分片存放：NAS 上 data/img 单目录堆几十万个小文件时，目录项查找、备份/同步
// （rsync、相册索引）都会明显变慢；按哈希前两位分层后每级目录只有 1/256 的量。
func CoverPath(dir, u string) string {
	k := CoverKey(u)
	return filepath.Join(dir, k[:2], k+".jpg")
}

// CoverPathLegacy 旧版扁平路径，仅用于读取已缓存的封面（写盘一律走 CoverPath）
func CoverPathLegacy(dir, u string) string {
	return filepath.Join(dir, CoverKey(u)+".jpg")
}

// CoverKey 封面文件名（按 URL 哈希，天然去重）
func CoverKey(u string) string {
	h := sha1.Sum([]byte(u))
	return hex.EncodeToString(h[:])
}

// fetchAndStoreCover 下载封面 -> 压缩 -> 存 data/img/<xx>/<key>.jpg
func (c *Collector) fetchAndStoreCover(u string) {
	path := CoverPath(c.CoverDir, u)
	if _, err := os.Stat(path); err == nil {
		return // 已存在
	}
	if _, err := os.Stat(CoverPathLegacy(c.CoverDir, u)); err == nil {
		return // 旧扁平路径已有，不必重下
	}
	coverMu.Lock()
	if coverFlight[u] {
		coverMu.Unlock()
		return
	}
	if t, bad := coverFail[u]; bad && time.Since(t) < coverFailTTL {
		coverMu.Unlock()
		return
	}
	coverFlight[u] = true
	coverMu.Unlock()

	var lastErr error
	defer func() {
		coverMu.Lock()
		delete(coverFlight, u)
		if lastErr == nil {
			delete(coverFail, u)
			delete(coverErr, u)
		} else {
			// 负缓存也要限容：长期运行下失败的封面 URL 会持续累积
			if len(coverFail) > 50000 {
				coverFail = map[string]time.Time{}
				coverErr = map[string]string{}
			}
			coverFail[u] = time.Now()
			coverErr[u] = lastErr.Error()
		}
		coverMu.Unlock()
		if lastErr != nil {
			n := coverFailN.Add(1)
			coverLastErr.Store(lastErr.Error())
			// 每 25 张失败报一次，避免刷屏；第一张也报，方便定位"是不是全都拉不到"
			if n == 1 || n%25 == 0 {
				logx.Warnf("[cover] 封面下载失败（累计 %d 张，最近一次: %v）%s", n, lastErr, coverHint())
			}
		} else {
			coverOKN.Add(1)
		}
	}()

	// 全局限速：排到自己那一格再发起下载（图床对批量抓图很敏感）
	coverPace()

	coverSem <- struct{}{}
	defer func() { <-coverSem }()

	raw, err := fetchCoverBytes(u)
	if err != nil {
		lastErr = err
		return
	}
	out, _, err := imgproc.CompressImage(raw, 480, 80)
	if err != nil || len(out) == 0 {
		lastErr = fmt.Errorf("压缩失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		lastErr = err
		return
	}
	tmp := path + ".part"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		lastErr = err
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		lastErr = err
		return
	}
}

// coverHint 首次失败时给出排查提示（本地/内网环境最常见的是源站被墙、防盗链、DNS 不通）
func coverHint() string {
	if imgProxy != "" {
		return "" // 已经配了图片代理，别再提示
	}
	return "；若大量封面都失败，检查源站是否可访问，或配置 -img-proxy（如 https://wsrv.nl/?url=）"
}

// fetchCoverBytes 依次尝试若干种取图方式：
//  1. 配置了 -img-proxy 时先走代理
//  2. 原始地址 + Referer（多数源站防盗链只校验 Referer）
//  3. 原始地址但不带 Referer
//  4. https 失败时回落 http（部分站的证书/443 被拦）
func fetchCoverBytes(rawURL string) ([]byte, error) {
	type attempt struct {
		url       string
		withRefer bool
		desc      string
	}
	var list []attempt
	if imgProxy != "" {
		list = append(list, attempt{imgProxy + url.QueryEscape(rawURL), false, "图片代理"})
	}
	list = append(list, attempt{rawURL, true, "原文+Referer"}, attempt{rawURL, false, "原文"})
	if strings.HasPrefix(rawURL, "https://") {
		list = append(list, attempt{"http://" + strings.TrimPrefix(rawURL, "https://"), true, "http 回退"})
	}

	var lastErr error
	for i, a := range list {
		if i > 0 {
			// 换下一种取图方式前歇 300ms：命中防盗链的站被连打几次会直接封 IP
			time.Sleep(300 * time.Millisecond)
		}
		req, err := http.NewRequest("GET", a.url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0 Safari/537.36")
		req.Header.Set("Accept", "image/jpeg,image/png,image/gif,image/webp;q=0.9,*/*;q=0.5")
		if a.withRefer {
			if i := strings.Index(rawURL, "://"); i > 0 {
				if j := strings.Index(rawURL[i+3:], "/"); j > 0 {
					req.Header.Set("Referer", rawURL[:i+3+j]+"/") // 防盗链
				}
			}
		}
		resp, derr := coverHTTP.Do(req)
		if derr != nil {
			lastErr = fmt.Errorf("%s: %v", a.desc, derr)
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			// 429/403 基本都是限流或防盗链：把整个封面队列整体放慢一会儿，
			// 否则队列里剩下几百张会继续往上撞，把 IP 撞进黑名单
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
				wait := 30 * time.Second
				if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
					if n, e := strconv.Atoi(ra); e == nil && n > 0 && n <= 600 {
						wait = time.Duration(n) * time.Second
					}
				}
				logx.Debugf("[cover] 被限流（HTTP %d），封面队列暂停 %v", resp.StatusCode, wait)
				coverCooldown(wait)
				lastErr = fmt.Errorf("%s: HTTP %d（已限流，暂停 %v）", a.desc, resp.StatusCode, wait)
				break // 已经被限流了，别再换着花样连打同一个站
			}
			lastErr = fmt.Errorf("%s: HTTP %d", a.desc, resp.StatusCode)
			continue
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if rerr != nil || len(raw) == 0 {
			lastErr = fmt.Errorf("%s: 读取失败 %v（%d 字节）", a.desc, rerr, len(raw))
			continue
		}
		return raw, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("无可用地址")
	}
	return nil, lastErr
}

// SetImageProxy 设置图片代理前缀（形如 https://wsrv.nl/?url=），为空则不用代理
func SetImageProxy(p string) { imgProxy = strings.TrimSpace(p) }

// CoverStats 封面下载统计（给 /api/system 展示，便于判断"没图"是拉不到还是没触发）
func CoverStats() (okN, failN int64, lastErr string) {
	if v := coverLastErr.Load(); v != nil {
		lastErr, _ = v.(string)
	}
	return coverOKN.Load(), coverFailN.Load(), lastErr
}

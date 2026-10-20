package web

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"videoDownLoad/internal/collector"
	db "videoDownLoad/internal/db"
	"videoDownLoad/internal/logx"
)

// invalidateSourceStats 接口增删改后清缓存，避免统计数字滞后
func invalidateSourceStats() {
	srcStatMu.Lock()
	srcStatCache, srcStatExp = nil, time.Time{}
	srcStatMu.Unlock()
}

// sourceStats 统计每个接口的数据量。
// 影片/剧名/最后更新走 vod_info 的 (source_id, ...) 索引，很快；
// 播放地址数要遍历 vod_play（449 万行，走 vod_id 主键前缀，无 source_id 冗余列），
// 弱 CPU 上要秒级 —— 所以整体缓存 10 分钟，且接口增删改时主动失效。
func (s *Server) sourceStats() map[int64]srcStat {
	srcStatMu.Lock()
	if srcStatCache != nil && time.Now().Before(srcStatExp) {
		c := srcStatCache
		srcStatMu.Unlock()
		return c
	}
	srcStatMu.Unlock()

	out := map[int64]srcStat{}
	if rows, err := s.DB.Query(`SELECT source_id, COUNT(*), COUNT(DISTINCT vod_name),
		COALESCE(MAX(updated_at),'') FROM vod_info GROUP BY source_id`); err == nil {
		for rows.Next() {
			var id int64
			var st srcStat
			if rows.Scan(&id, &st.videos, &st.names, &st.last) == nil {
				out[id] = st
			}
		}
		rows.Close()
	}
	if rows, err := s.DB.Query(`SELECT v.source_id, COUNT(*)
		FROM vod_info v JOIN vod_play p ON p.vod_id=v.id GROUP BY v.source_id`); err == nil {
		for rows.Next() {
			var id, n int64
			if rows.Scan(&id, &n) == nil {
				st := out[id]
				st.plays = n
				out[id] = st
			}
		}
		rows.Close()
	}
	srcStatMu.Lock()
	srcStatCache, srcStatExp = out, time.Now().Add(10*time.Minute)
	srcStatMu.Unlock()
	return out
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	// 必须先算统计再开 rows：连接池只给 1 个连接（SetMaxOpenConns(1)），
	// rows 没遍历完就发新查询会永远等不到连接（表现为接口卡死、页面打不开）。
	// 统计结果有 30 秒缓存，放前面不增加开销。
	stats := s.sourceStats()
	rows, err := s.DB.Query(`SELECT id, name, api_url, enabled, auto_incr, remark, created_at, updated_at
		FROM vod_source ORDER BY id`)
	if err != nil {
		fail(w, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, en, ai int64
		var name, url, remark, ca, ua string
		rows.Scan(&id, &name, &url, &en, &ai, &remark, &ca, &ua)
		st := stats[id]
		out = append(out, map[string]any{"id": id, "name": name, "api_url": url, "enabled": en,
			"auto_incr": ai,
			"videos":    st.videos, "names": st.names, "plays": st.plays, "last_update": st.last,
			"remark": remark, "created_at": ca, "updated_at": ua})
	}
	ok(w, out)
}

type sourceInput struct {
	Name    string `json:"name"`
	APIURL  string `json:"api_url"`
	Remark  string `json:"remark"`
	Enabled int    `json:"enabled"`
}

func (s *Server) addSource(w http.ResponseWriter, r *http.Request) {
	var in sourceInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, "请求体解析失败: "+err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || strings.TrimSpace(in.APIURL) == "" {
		fail(w, "名称和接口地址必填")
		return
	}
	apiURL, err := normalizeAPIURL(in.APIURL)
	if err != nil {
		fail(w, err.Error())
		return
	}
	if in.Enabled == 0 {
		in.Enabled = 1
	}
	// 已存在就给出明确提示（含是哪一条），而不是丢一句 SQLite 的 UNIQUE 报错
	var dupID int64
	var dupName string
	if err := s.DB.QueryRow(`SELECT id, name FROM vod_source WHERE api_url=?`, apiURL).
		Scan(&dupID, &dupName); err == nil {
		fail(w, fmt.Sprintf("该接口已存在：#%d %s", dupID, dupName))
		return
	}
	res, err := s.DB.Exec(`INSERT INTO vod_source(name, api_url, enabled, remark) VALUES(?,?,?,?)`,
		in.Name, apiURL, in.Enabled, strings.TrimSpace(in.Remark))
	if err != nil {
		fail(w, "添加失败: "+err.Error())
		return
	}
	id, _ := res.LastInsertId()
	s.srcMu.Lock()
	s.srcNameCache = nil
	s.srcMu.Unlock()
	logx.Infof("[source] 新增接口 #%d %s -> %s", id, in.Name, apiURL)
	ok(w, map[string]any{"id": id, "api_url": apiURL})
}

// normalizeAPIURL 把用户输入的地址统一成苹果CMS v10 接口地址。
//
// 用户只需要填前端站点（如 https://www.iqiyizyapi.com），这里自动补成
// https://www.iqiyizyapi.com/api.php/provide/vod/ ；已经填了完整接口地址的也兼容。
func normalizeAPIURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	// 顺手去掉粘贴时带进来的中文标点/多余斜杠
	s = strings.Trim(s, " \t\r\n，,。；;")
	if s == "" {
		return "", fmt.Errorf("接口地址不能为空")
	}
	hadScheme := strings.Contains(s, "://")
	probe := s
	if !hadScheme {
		probe = "http://" + s // 先借 http 解析，最后再决定协议
	}
	u, err := neturl.Parse(probe)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("接口地址格式不正确：%s", raw)
	}
	// 挡掉「不是网址」这类中文/带空格的输入：它们会在探测时白等到超时
	if !validHost(u.Hostname()) {
		return "", fmt.Errorf("接口地址格式不正确（只填域名即可，如 www.example.com）：%s", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if hadScheme && scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("只支持 http/https 地址：%s", raw)
	}
	if !hadScheme {
		// 没写协议时：带非 443 端口或内网地址按 http 处理，其余默认 https
		if (u.Port() != "" && u.Port() != "443") || isPrivateHost(u.Hostname()) {
			scheme = "http"
		} else {
			scheme = "https"
		}
	} else if scheme != "http" && scheme != "https" {
		scheme = "https"
	}

	path := u.Path
	// 苹果CMS 有两种路径写法：
	//   /api.php/provide/vod/…（PHP 站点，最常见）
	//   /api/provide/vod/…    （部分站点去掉了 .php，如 api.juliang.live）
	// 命中哪种就保留哪种，并保留其后的子路径（at/xml/ 等格式变体）。
	// 不能一截到底：截掉 at/xml 等于把用户填的 XML 接口悄悄改回 JSON。
	apiBases := []string{"/api.php/provide/vod", "/api/provide/vod"}
	path = ""
	for _, base := range apiBases {
		i := strings.Index(u.Path, base)
		if i < 0 {
			continue
		}
		// 折叠多余斜杠，避免 /vod//at//xml 这类粘贴痕迹
		parts := []string{}
		for _, seg := range strings.Split(strings.Trim(u.Path[i+len(base):], "/"), "/") {
			if seg != "" {
				parts = append(parts, seg)
			}
		}
		path = base
		if len(parts) > 0 {
			path += "/" + strings.Join(parts, "/")
		}
		break
	}
	if path == "" {
		path = apiBases[0] // 只填域名：按最常见的 .php 版本补全
	}
	return scheme + "://" + u.Host + path + "/", nil
}

// apiCandidates 把用户输入展开成「值得一试」的接口地址列表。
//
// 用户现在只需要填域名，但苹果CMS 有四种常见写法：
//
//	/api.php/provide/vod/        （PHP 站点，JSON）
//	/api/provide/vod/            （去掉了 .php，如 api.juliang.live）
//	/api.php/provide/vod/at/xml/ （XML 变体）
//	/api/provide/vod/at/xml/     （无 .php 的 XML 变体）
//
// 按「JSON 优先、.php 优先」的顺序返回，由 collector.Probe 逐个试。
// 如果用户自己贴了完整路径，就只在两种 .php 写法之间互换，不去猜别的子路径。
func apiCandidates(raw string) ([]string, error) {
	first, err := normalizeAPIURL(raw)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	if strings.Contains(strings.ToLower(raw), "/api") {
		add(first)
		add(swapAPIBase(first))
		return out, nil
	}
	u, perr := neturl.Parse(first)
	if perr != nil || u.Host == "" {
		return nil, fmt.Errorf("接口地址格式不正确：%s", raw)
	}
	origin := u.Scheme + "://" + u.Host
	add(origin + "/api.php/provide/vod/")
	add(origin + "/api/provide/vod/")
	add(origin + "/api.php/provide/vod/at/xml/")
	add(origin + "/api/provide/vod/at/xml/")
	return out, nil
}

// swapAPIBase 在 .php 与无 .php 两种写法之间互换（子路径保持不变）
func swapAPIBase(u string) string {
	switch {
	case strings.Contains(u, "/api.php/provide/vod"):
		return strings.Replace(u, "/api.php/provide/vod", "/api/provide/vod", 1)
	case strings.Contains(u, "/api/provide/vod"):
		return strings.Replace(u, "/api/provide/vod", "/api.php/provide/vod", 1)
	}
	return ""
}

// probeSource 预检一个还没入库的地址：只探测、不写库，供添加表单的「测试」按钮使用。
func (s *Server) probeSource(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, "参数错误: "+err.Error())
		return
	}
	raw := strings.TrimSpace(in.URL)
	if raw == "" {
		fail(w, "请填写接口地址（填域名即可）")
		return
	}
	cands, err := apiCandidates(raw)
	if err != nil {
		fail(w, err.Error())
		return
	}
	logx.Infof("[source] 预检 %s，候选 %d 个", raw, len(cands))
	ok(w, s.Col.Probe(cands))
}

// validHost 拒绝中文/空格等明显不是域名的输入（IDN 中文域名在采集站里不存在）
func validHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, c := range h {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '-' || c == '_' || c == ':'
		if !ok {
			return false
		}
	}
	return true
}

// isPrivateHost 内网/本机地址（补协议时优先按 http）
func isPrivateHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

func (s *Server) updateSource(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in sourceInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, "请求体解析失败: "+err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		fail(w, "名称必填")
		return
	}
	apiURL, err := normalizeAPIURL(in.APIURL)
	if err != nil {
		fail(w, err.Error())
		return
	}
	var dupID int64
	if err := s.DB.QueryRow(`SELECT id FROM vod_source WHERE api_url=? AND id<>?`, apiURL, id).
		Scan(&dupID); err == nil {
		fail(w, fmt.Sprintf("接口地址已被 #%d 占用", dupID))
		return
	}
	if _, err := s.DB.Exec(`UPDATE vod_source SET name=?, api_url=?, remark=?, enabled=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		in.Name, apiURL, strings.TrimSpace(in.Remark), in.Enabled, id); err != nil {
		fail(w, err.Error())
		return
	}
	s.srcMu.Lock()
	s.srcNameCache = nil
	s.srcMu.Unlock()
	ok(w, map[string]any{"api_url": apiURL})
}

// setAutoIncr 接口级「自动增量」开关：只影响定时任务采哪些接口，
// 手动点「增量采集 / 全量采集」不受影响（关掉后仍可手动采）。
func (s *Server) setAutoIncr(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		On *int `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.On == nil {
		fail(w, "参数错误")
		return
	}
	on := 0
	if *in.On != 0 {
		on = 1
	}
	res, err := s.DB.Exec(`UPDATE vod_source SET auto_incr=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, on, id)
	if err != nil {
		fail(w, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, "接口不存在")
		return
	}
	ok(w, map[string]any{"auto_incr": on})
}

// deleteSource 删除接口，并清理该接口在全站的数据：
// 影片 / 播放地址 / 下载记录 / 下载的本地文件 / 封面缓存 / 采集断点，最后重建聚合表。
func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id <= 0 {
		fail(w, "参数错误")
		return
	}
	var name string
	if err := s.DB.QueryRow(`SELECT name FROM vod_source WHERE id=?`, id).Scan(&name); err != nil {
		fail(w, "接口不存在（可能已被删除）")
		return
	}
	// 采集中的接口不能删：爬虫还会继续写库，删完又冒出来
	if collector.GlobalProgress.IsRunning(id) {
		fail(w, "该接口正在采集中，请等采集结束或先停用后再删除")
		return
	}

	start := time.Now()
	stats := map[string]int{}
	phase := func(name string, t0 time.Time) time.Duration {
		d := time.Since(t0)
		logx.Debugf("[source] 删除 #%d 阶段 %s 耗时 %v", id, name, d.Round(time.Millisecond))
		return d
	}

	// 1) 下载任务：走统一的删除逻辑（停 worker、删 mp4/封面/分片目录/数据库记录）
	drows, derr := s.DB.Query(`SELECT id FROM vod_download WHERE source_id=?`, id)
	var dlIDs []int64
	if derr == nil {
		for drows.Next() {
			var did int64
			if drows.Scan(&did) == nil {
				dlIDs = append(dlIDs, did)
			}
		}
		drows.Close()
	}
	for _, did := range dlIDs {
		if err := collector.DeleteDownload(did); err == nil {
			stats["downloads"]++
		} else {
			logx.Warnf("[source] 删除下载任务 #%d 失败: %v", did, err)
		}
	}

	tCoverList := time.Now()
	// 2) 封面缓存：先把该接口用到的封面地址收集起来，删库后再删文件
	//    （封面是纯缓存，删多了最多下次重新拉，绝不至于删掉别的接口还在用的"数据"）
	var pics []string
	if prows, perr := s.DB.Query(`SELECT DISTINCT vod_pic FROM vod_info WHERE source_id=? AND vod_pic<>''`, id); perr == nil {
		for prows.Next() {
			var p string
			if prows.Scan(&p) == nil && p != "" {
				pics = append(pics, p)
			}
		}
		prows.Close()
	}

	phase(fmt.Sprintf("收集封面地址 %d 条", len(pics)), tCoverList)

	// 3) 一个事务里删干净：播放地址 -> 下载记录 -> 影片 -> 接口 -> 断点
	tTx := time.Now()
	tx, terr := s.DB.Begin()
	if terr != nil {
		fail(w, "开启事务失败: "+terr.Error())
		return
	}
	steps := []struct {
		key string
		q   string
	}{
		{"plays", `DELETE FROM vod_play WHERE vod_id IN (SELECT id FROM vod_info WHERE source_id=?)`},
		{"downloads", `DELETE FROM vod_download WHERE source_id=?`},
		{"videos", `DELETE FROM vod_info WHERE source_id=?`},
		{"sync", `DELETE FROM vod_sync_state WHERE source_id=?`},
	}
	for _, st := range steps {
		res, err := tx.Exec(st.q, id)
		if err != nil {
			tx.Rollback()
			fail(w, fmt.Sprintf("清理数据失败(%s): %v", st.key, err))
			return
		}
		n, _ := res.RowsAffected()
		stats[st.key] += int(n)
	}
	if _, err := tx.Exec(`DELETE FROM vod_source WHERE id=?`, id); err != nil {
		tx.Rollback()
		fail(w, "删除接口失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, "提交失败: "+err.Error())
		return
	}

	phase("删除数据库记录", tTx)

	s.invalidateListCache()
	invalidateSourceStats()
	s.srcMu.Lock()
	s.srcNameCache = nil
	s.srcMu.Unlock()
	// 数据量变了，清掉 /api/system 的统计缓存，避免前端 15 秒内还显示旧数字
	s.statMu.Lock()
	s.sysCache, s.sysExp = nil, time.Time{}
	s.doneExp = time.Time{}
	s.statMu.Unlock()

	// 4) 封面文件 + 聚合表：都放后台做。
	//    封面逐个 os.Remove 在小文件很多的目录/杀软开着的情况下很慢（实测 2.5 万个不存在的路径
	//    在 Windows 上要 40 秒），不能卡住删除请求；删多少条封面不影响"数据已清空"的语义。
	//    数量少时同步做，好让接口能返回准确张数。
	const syncCoverLimit = 500
	cleanCovers := func() int {
		imgDir := filepath.Join(s.DataDir, "img")
		n := 0
		for _, p := range pics {
			removed := false
			for _, f := range []string{collector.CoverPath(imgDir, p), collector.CoverPathLegacy(imgDir, p)} {
				if _, err := os.Stat(f); err == nil {
					if os.Remove(f) == nil {
						removed = true
					}
				}
			}
			if removed {
				n++
			}
		}
		return n
	}
	coversAsync := len(pics) > syncCoverLimit
	if coversAsync {
		stats["covers"] = 0
		go func() {
			t0 := time.Now()
			n := cleanCovers()
			logx.Infof("[source] 删除接口 #%d 的封面缓存完成：%d 张（候选 %d），耗时 %v",
				id, n, len(pics), time.Since(t0).Round(time.Millisecond))
		}()
	} else {
		tCover := time.Now()
		stats["covers"] = cleanCovers()
		phase(fmt.Sprintf("删除封面文件 %d 个", stats["covers"]), tCover)
	}

	// 5) 聚合表重建（大库要几秒，放后台，不阻塞本次请求）
	go func() {
		t0 := time.Now()
		if err := db.RebuildNameAgg(s.DB); err != nil {
			logx.Warnf("[source] 删除接口后重建聚合表失败: %v", err)
			return
		}
		s.invalidateListCache()
		// 删掉的接口可能贡献了几十万条播放地址，计数缓存必须失效
		db.InvalidatePlaysCount()
		logx.Infof("[source] 删除接口后重建聚合表完成，耗时 %v", time.Since(t0).Round(time.Millisecond))
	}()

	logx.Infof("[source] 已删除接口 #%d %s：影片 %d、播放地址 %d、下载 %d、封面 %d，耗时 %v",
		id, name, stats["videos"], stats["plays"], stats["downloads"], stats["covers"],
		time.Since(start).Round(time.Millisecond))
	ok(w, map[string]any{
		"name":      name,
		"videos":    stats["videos"],
		"plays":     stats["plays"],
		"downloads": stats["downloads"],
		"covers":    stats["covers"],
		"cost_ms":   time.Since(start).Milliseconds(),
		"msg":       fmt.Sprintf("已删除「%s」，清理影片 %d 部、播放地址 %d 条、下载任务 %d 个、封面 %d 张", name, stats["videos"], stats["plays"], stats["downloads"], stats["covers"]),
	})
}

func (s *Server) testSource(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ok(w, s.Col.Test(id))
}

// sourceExportItem 导出/导入的接口条目（与 sourceInput 分开：导入要带 auto_incr）
type sourceExportItem struct {
	Name     string `json:"name"`
	APIURL   string `json:"api_url"`
	Enabled  int    `json:"enabled"`
	AutoIncr int    `json:"auto_incr"`
	Remark   string `json:"remark"`
}

// exportSources 导出全部接口为 JSON 文件：换机器/重装后导入即可恢复，不用逐个重加。
func (s *Server) exportSources(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(`SELECT name, api_url, enabled, auto_incr, remark FROM vod_source ORDER BY id`)
	if err != nil {
		fail(w, err.Error())
		return
	}
	defer rows.Close()
	out := []sourceExportItem{}
	for rows.Next() {
		var it sourceExportItem
		if rows.Scan(&it.Name, &it.APIURL, &it.Enabled, &it.AutoIncr, &it.Remark) == nil {
			out = append(out, it)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=vod-sources.json")
	_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "sources": out})
}

// importSources 导入接口列表：地址规范化后按 api_url 去重，已存在的跳过。
// 不做连通性探测（几十个接口各试 4 种写法太慢），导入后用「测试全部」逐个验证。
func (s *Server) importSources(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Sources []sourceExportItem `json:"sources"`
	}
	// 限制 1MB：正常导出文件几十 KB，防止误传大文件撑内存
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		fail(w, "导入文件解析失败（请选择本系统导出的 JSON）: "+err.Error())
		return
	}
	if len(in.Sources) == 0 {
		fail(w, "文件里没有可导入的接口")
		return
	}
	// 现有地址集合：规范化后去重
	existing := map[string]bool{}
	if rows, err := s.DB.Query(`SELECT api_url FROM vod_source`); err == nil {
		for rows.Next() {
			var u string
			if rows.Scan(&u) == nil {
				existing[u] = true
			}
		}
		rows.Close()
	}
	added, skipped := 0, 0
	errs := []string{}
	for _, it := range in.Sources {
		name := strings.TrimSpace(it.Name)
		apiURL, err := normalizeAPIURL(it.APIURL)
		if err != nil || name == "" {
			skipped++
			if len(errs) < 3 && err != nil {
				errs = append(errs, err.Error())
			}
			continue
		}
		if existing[apiURL] {
			skipped++
			continue
		}
		en, ai := it.Enabled, it.AutoIncr
		if en != 0 {
			en = 1
		}
		if ai != 0 {
			ai = 1
		}
		if _, err := s.DB.Exec(`INSERT INTO vod_source(name, api_url, enabled, auto_incr, remark)
			VALUES(?,?,?,?,?)`, name, apiURL, en, ai, strings.TrimSpace(it.Remark)); err != nil {
			skipped++
			if len(errs) < 3 {
				errs = append(errs, name+": "+err.Error())
			}
			continue
		}
		existing[apiURL] = true // 文件内部也可能重复
		added++
	}
	if added > 0 {
		invalidateSourceStats()
		s.srcMu.Lock()
		s.srcNameCache = nil
		s.srcMu.Unlock()
	}
	msg := fmt.Sprintf("导入完成：新增 %d 个，跳过 %d 个（已存在或格式不对）", added, skipped)
	if len(errs) > 0 {
		msg += "；" + strings.Join(errs, "；")
	}
	logx.Infof("[source] 导入接口：新增 %d，跳过 %d", added, skipped)
	ok(w, map[string]any{"added": added, "skipped": skipped, "msg": msg})
}

package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"videoDownLoad/internal/collector"
	db "videoDownLoad/internal/db"
)

// ---- 筛选项（年份 / 国家地区 / 类型）----
//
// 只返回片量最多的前 filterTopN 个取值，剩下的全部收进「其他」桶。
//
// 为什么：来源站点的取值极其分散。实测 15 万部影片里
//   - 地区有 1808 种写法，其中 1403 种是 "英国,美国"、"美国,英国" 这种逗号合成值
//     （同一组合顺序颠倒就算另一个值），还有 "中国大陆"/"大陆"/"内地"/"中国" 四种同义写法；
//   - 年份有 290 种，混着 "22025"（脏数据）、"1988~"、"1990~1995" 这类区间值；
//   - 类型有 72 种。
// 全列出来筛选行会长到几千像素（2026-10-20 用户反馈）。
// 前 20 名已能覆盖绝大多数影片，剩下的用「其他」兜底：
// 点它 = 「该列的值不在前 20 名里」，所以规范的取值一个都不会被漏掉。
const (
	filterTopN = 20
	// 「其他」在 URL 里的哨兵值。不能直接用"其他"两个字：
	// 库里真的有叫"其它"/"其他"的地区取值，会和正常取值撞车。
	filterOtherSentinel = "__other__"
)

// filterItem 是一个筛选取值 + 它覆盖的影片数。
type filterItem struct {
	Value string `json:"v"`
	Count int64  `json:"c"`
}

// filterTable 选出用于筛选统计的表：优先聚合表 vod_name_agg（行数少、有索引），
// 老库还没建好聚合表时退回明细表 vod_info。
// 统计和取值必须用同一张表，否则「其他」的条数会和实际筛选结果对不上。
func (s *Server) filterTable() string {
	var n int64
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM vod_name_agg`).Scan(&n); err == nil && n > 0 {
		return "vod_name_agg"
	}
	return "vod_info"
}

// filterTopValues 取某列片量最多的前 N 个取值。
// 排序「片量倒序 + 名称升序」保证结果稳定（否则同样的数据每次刷新顺序可能不同）。
func (s *Server) filterTopValues(col string) []filterItem {
	q := `SELECT TRIM(` + col + `) AS v, COUNT(*) AS c FROM %s
		WHERE TRIM(` + col + `) <> '' GROUP BY v ORDER BY c DESC, v ASC LIMIT ?`
	for _, table := range []string{"vod_name_agg", "vod_info"} {
		rows, err := s.DB.Query(fmt.Sprintf(q, table), filterTopN)
		if err != nil {
			continue
		}
		out := make([]filterItem, 0, filterTopN)
		for rows.Next() {
			var it filterItem
			if rows.Scan(&it.Value, &it.Count) == nil && it.Value != "" {
				out = append(out, it)
			}
		}
		rows.Close() // 单连接库：必须关闭后再查下一张，否则死锁
		if len(out) > 0 {
			return out
		}
	}
	return []filterItem{}
}

// filterOtherCount = 该表总行数 - 前 N 名覆盖的影片数 = 「其他」桶的大小。
//
// 注意用的是「全表行数」而不是「非空行数」：空值（比如没填地区的影片）
// 同样不在前 20 名里，查询侧用的是 NOT IN(...)，空值天然会被算进来 ——
// 两边口径必须一致，否则前端显示的条数和实际筛选结果对不上。
// total 由调用方传入：三个维度共用同一次 COUNT(*)，不用每维都扫一遍表。
// 结果 ≤ 0 说明取值本来就不超过 20 个，不该出现「其他」。
func filterOtherCount(top []filterItem, total int64) int64 {
	var sum int64
	for _, it := range top {
		sum += it.Count
	}
	if rest := total - sum; rest > 0 {
		return rest
	}
	return 0
}

// sortYearsDesc 把年份按数值倒序（2026 → 2025 → …），让用户找新片更顺手。
// "1988~"、"1990~1995" 这类区间值取前 4 位数字参与比较；
// 完全取不到数字的排最后，避免脏数据（如 "22025"）混在最前面。
func sortYearsDesc(in []filterItem) []filterItem {
	out := make([]filterItem, len(in))
	copy(out, in)
	yearOf := func(v string) int {
		n := 0
		for i := 0; i < len(v) && i < 4; i++ {
			c := v[i]
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := yearOf(out[i].Value), yearOf(out[j].Value)
		if a != b {
			return a > b
		}
		return out[i].Value > out[j].Value
	})
	return out
}

func (s *Server) listFilters(w http.ResponseWriter, r *http.Request) {
	// 不做缓存：这几条查询走索引只需几毫秒，缓存反而会让刚采集到的新年份/地区延迟可见。
	// 全部取值都来自数据库，不做任何硬编码。
	years := s.filterTopValues("vod_year")
	areas := s.filterTopValues("vod_area")
	types := s.filterTopValues("type_name")

	// 三个维度共用同一次 COUNT(*)：COUNT(*) 在没有覆盖索引时是全表扫描，
	// 分三次跑等于白扫三遍 15 万行。
	var total int64
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM ` + s.filterTable()).Scan(&total)

	data := map[string]any{
		// 年份：按片量取前 20 名后，再按数值倒序展示
		"years": sortYearsDesc(years),
		// 地区 / 类型：按片量从多到少（后端已排好，前端直接渲染）
		"areas": areas,
		"types": types,
		// 「其他」桶的影片数：点它 = 该列的值不在前 20 名里
		"other": map[string]int64{
			"year": filterOtherCount(years, total),
			"area": filterOtherCount(areas, total),
			"type": filterOtherCount(types, total),
		},
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "data": data})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(b)
}

// ---- 视频列表：同名合并 ----
func (s *Server) listVideos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	// 每页条数：前端按"每行数 × 8 行"传入，保证末行满行不空缺；
	// 不传时用 48（2/3/4/6 列都能整除，避免桌面端最后一行少几格）
	size := 48
	if sv := q.Get("size"); sv != "" {
		if n, err := strconv.Atoi(sv); err == nil && n >= 8 && n <= 120 {
			size = n
		}
	}
	wd := strings.TrimSpace(q.Get("wd"))
	// 搜索范围：name=只搜片名 / actor=只搜演员 / all=片名+演员+类型（默认）
	searchField := q.Get("field")
	if searchField != "name" && searchField != "actor" {
		searchField = "all"
	}
	srcID := q.Get("source_id")
	onlyDone := q.Get("filter") == "downloaded"
	fYear := strings.TrimSpace(q.Get("year"))
	fArea := strings.TrimSpace(q.Get("area"))
	fType := strings.TrimSpace(q.Get("type"))

	cacheKey := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%d|%d",
		searchField, wd, srcID, q.Get("filter"), fYear, fArea, fType, page, size)
	if b, hit := s.cacheGet(cacheKey); hit {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(b)
		return
	}

	// 列表走预聚合表 vod_name_agg（同名合并 + 索引排序），
	// 不再对 vod_info 全表 GROUP BY + ORDER BY（30 万条时单次 300ms 起步，百万级 1s+）
	where := " WHERE 1=1 "
	var args []any
	if wd != "" {
		like := "%" + wd + "%"
		switch searchField {
		case "name":
			where += " AND vod_name LIKE ? "
			args = append(args, like)
		case "actor":
			where += " AND vod_actor LIKE ? "
			args = append(args, like)
		default:
			where += " AND (vod_name LIKE ? OR vod_actor LIKE ? OR type_name LIKE ?) "
			args = append(args, like, like, like)
		}
	}
	if srcID != "" {
		if id, err := strconv.ParseInt(srcID, 10, 64); err == nil && id > 0 {
			where += " AND src_ids LIKE ? "
			args = append(args, fmt.Sprintf("%%,%d,%%", id))
		}
	}
	if onlyDone {
		where += ` AND vod_name IN (SELECT y.vod_name FROM vod_download d
			JOIN vod_info y ON y.id = d.vod_id WHERE d.status='done') `
	}
	// 筛选：年份 / 国家地区 / 类型（值来自 /api/filters，直接精确匹配走索引）
	//
	// 值为 filterOtherSentinel（"__other__"）时表示「不在前 20 名里」：
	// 展开成 NOT IN (前 20 个取值)。这是「其他」桶唯一需要特殊处理的地方 ——
	// 普通取值仍走精确匹配，索引照常命中。
	applyFilter := func(col, val string) {
		if val == "" {
			return
		}
		if val == filterOtherSentinel {
			top := s.filterTopValues(col)
			if len(top) == 0 {
				return
			}
			where += " AND " + col + " NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(top)), ",") + ") "
			for _, it := range top {
				args = append(args, it.Value)
			}
			return
		}
		where += " AND " + col + " = ? "
		args = append(args, val)
	}
	applyFilter("vod_year", fYear)
	applyFilter("vod_area", fArea)
	applyFilter("type_name", fType)

	var total int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM vod_name_agg`+where, args...).Scan(&total); err != nil {
		fail(w, err.Error())
		return
	}

	rows, err := s.DB.Query(`SELECT vod_name, cnt, vod_pic, type_name, vod_remarks, vod_year,
		vod_area, rid, src_ids, updated_at FROM vod_name_agg`+where+
		` ORDER BY updated_at DESC LIMIT ? OFFSET ?`, append(args, size, (page-1)*size)...)
	if err != nil {
		fail(w, err.Error())
		return
	}
	defer rows.Close()

	type rowItem struct {
		item   map[string]any
		srcIDs string
	}
	list := []map[string]any{}
	items := make([]rowItem, 0, size)
	var pics []string
	for rows.Next() {
		var name, pic, typ, remarks, year, area, srcids, ua sql.NullString
		var cnt, rid int64
		if err := rows.Scan(&name, &cnt, &pic, &typ, &remarks, &year, &area, &rid, &srcids, &ua); err != nil {
			continue
		}
		if pic.String != "" {
			pics = append(pics, pic.String)
		}
		items = append(items, rowItem{srcIDs: srcids.String, item: map[string]any{
			"id": rid, "vod_name": name.String, "source_count": cnt,
			"vod_pic": pic.String, "type_name": typ.String, "vod_remarks": remarks.String,
			"vod_year": year.String, "vod_area": area.String,
			"source_names": []string{}, "downloaded_count": int64(0),
			"updated_at": ua.String,
		}})
	}
	rows.Close() // 必须先关闭：连接池只有 1 个连接，未关闭时再查询会死锁

	// 接口名解析 + 已下载数量回填（此时 rows 已关闭）
	for _, it := range items {
		it.item["source_names"] = s.sourceNamesOf(it.srcIDs)
		list = append(list, it.item)
	}

	// 已下载数量：下载表通常不大，但每次列表请求都做一次 JOIN + GROUP BY
	// 在弱 CPU 上很浪费，这里加 15 秒缓存
	doneCnt := s.doneCounts()
	for _, it := range list {
		it["downloaded_count"] = doneCnt[it["vod_name"].(string)]
	}
	// 同名多源时给前端准备备选封面：某个源的图床被墙/防盗链时，可以换另一个源的图显示，
	// 前端 onerror 会依次尝试（否则一张图挂了整块就是白的）
	altPics := s.altCovers(list)
	for _, it := range list {
		name, _ := it["vod_name"].(string)
		primary, _ := it["vod_pic"].(string)
		it["pics"] = altCoversFor(name, primary, altPics[name], 3)
	}

	// 提前排队预取封面：浏览器请求图片时多半已落盘
	collector.EnsureCovers(pics)

	data, _ := json.Marshal(map[string]any{"ok": true, "data": map[string]any{
		"total": total, "page": page, "size": size, "list": list,
	}})
	s.cachePut(cacheKey, data)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(data)
}

// altCovers 批量取同一批剧名的所有封面地址（同名多源 → 多张候选图）
func (s *Server) altCovers(list []map[string]any) map[string][]string {
	out := map[string][]string{}
	if len(list) == 0 {
		return out
	}
	names := make([]any, 0, len(list))
	for _, it := range list {
		if n, ok := it["vod_name"].(string); ok && n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return out
	}
	in := strings.TrimRight(strings.Repeat("?,", len(names)), ",")
	rows, err := s.DB.Query(`SELECT vod_name, vod_pic FROM vod_info
		WHERE vod_name IN (`+in+`) AND vod_pic<>'' ORDER BY vod_name`, names...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var n, p string
		if rows.Scan(&n, &p) == nil {
			out[n] = append(out[n], p)
		}
	}
	return out
}

// altCoversFor 去重、排除主封面、限量
func altCoversFor(name, primary string, all []string, limit int) []string {
	out := make([]string, 0, limit)
	seen := map[string]bool{primary: true}
	for _, p := range all {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// doneCounts 已下载数量（按剧名），带 15 秒缓存。前端翻页/筛选时会连续命中。
func (s *Server) doneCounts() map[string]int64 {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	if s.doneCnt != nil && time.Now().Before(s.doneExp) {
		return s.doneCnt
	}
	out := map[string]int64{}
	if drows, derr := s.DB.Query(`SELECT y.vod_name, COUNT(*) FROM vod_download d
		JOIN vod_info y ON y.id = d.vod_id WHERE d.status='done' GROUP BY y.vod_name`); derr == nil {
		for drows.Next() {
			var n string
			var c int64
			if drows.Scan(&n, &c) == nil {
				out[n] = c
			}
		}
		drows.Close()
	}
	s.doneCnt, s.doneExp = out, time.Now().Add(15*time.Second)
	return out
}

// sourceNamesOf 把 ",1,5,9," 转成接口名称列表（小表，带 60 秒缓存）
func (s *Server) sourceNamesOf(csv string) []string {
	s.srcMu.Lock()
	names := s.srcNameCache
	if names == nil || time.Now().After(s.srcNameExp) {
		names = map[int64]string{}
		if rows, err := s.DB.Query(`SELECT id, name FROM vod_source`); err == nil {
			for rows.Next() {
				var id int64
				var n string
				if rows.Scan(&id, &n) == nil {
					names[id] = n
				}
			}
			rows.Close()
		}
		s.srcNameCache = names
		s.srcNameExp = time.Now().Add(60 * time.Second)
	}
	s.srcMu.Unlock()

	out := []string{}
	seen := map[string]bool{}
	for _, p := range strings.Split(csv, ",") {
		id, _ := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		n := names[id]
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func (s *Server) videoDetail(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var vodName string
	if err := s.DB.QueryRow(`SELECT vod_name FROM vod_info WHERE id=?`, id).Scan(&vodName); err != nil {
		fail(w, "视频不存在")
		return
	}

	type row struct {
		id, sourceID          int64
		srcName, remarks      string
		pic, typ, year        string
		area, score, intro    string
		actor, director, lang string
	}
	rows, err := s.DB.Query(`SELECT v.id, v.source_id, COALESCE(s.name,'未知接口'), v.vod_remarks,
		v.vod_pic, v.type_name, v.vod_year, v.vod_area, v.vod_score, COALESCE(dd.content,''),
		v.vod_actor, v.vod_director, v.vod_lang
		FROM vod_info v
		LEFT JOIN vod_source s ON s.id=v.source_id
		LEFT JOIN vod_detail dd ON dd.vod_id=v.id
		WHERE v.vod_name=? ORDER BY v.source_id`, vodName)
	if err != nil {
		fail(w, err.Error())
		return
	}
	var rs []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.id, &x.sourceID, &x.srcName, &x.remarks, &x.pic, &x.typ,
			&x.year, &x.area, &x.score, &x.intro, &x.actor, &x.director, &x.lang); err == nil {
			rs = append(rs, x)
		}
	}
	rows.Close()
	if len(rs) == 0 {
		fail(w, "视频不存在")
		return
	}
	// 详情页封面大图：提前预取，避免打开详情时卡在源站拉图
	dpics := make([]string, 0, len(rs))
	for _, x := range rs {
		if x.pic != "" {
			dpics = append(dpics, x.pic)
		}
	}
	collector.EnsureCovers(dpics)

	// 同名多源时挑信息最全的一条做详情展示
	best := rs[0]
	for _, x := range rs {
		if len(x.intro) > len(best.intro) {
			best = x
		}
		if x.pic != "" && best.pic == "" {
			best.pic = x.pic
		}
	}
	detail := map[string]any{
		"vod_name": vodName, "type_name": best.typ, "vod_pic": best.pic,
		"vod_content": best.intro, "vod_year": best.year, "vod_area": best.area,
		"vod_score": best.score, "vod_remarks": best.remarks,
		"vod_actor": best.actor, "vod_director": best.director, "vod_lang": best.lang,
	}
	// 该片所有下载记录（整条线路 ep_index=-1，单集 ep_index>=0）
	type dlRec struct {
		id     int64
		status string
		pct    int
		file   string
		size   int64
		eps    int
		err    string
	}
	dlMap := map[string]dlRec{}
	if drows, derr := s.DB.Query(`SELECT id, play_from, ep_index, status, progress, file_path, file_size, episodes, error
		FROM vod_download d WHERE d.vod_id IN (SELECT id FROM vod_info WHERE vod_name=?)`, vodName); derr == nil {
		for drows.Next() {
			var rec dlRec
			var from string
			var epIdx int
			if err := drows.Scan(&rec.id, &from, &epIdx, &rec.status, &rec.pct, &rec.file, &rec.size, &rec.eps, &rec.err); err == nil {
				dlMap[from+"#"+strconv.Itoa(epIdx)] = rec
			}
		}
		drows.Close()
	}
	dlObj := func(from string, epIdx int) any {
		rec, ok := dlMap[from+"#"+strconv.Itoa(epIdx)]
		if !ok {
			return nil // 每集一条记录（勾选批量下载时逐集创建）
		}
		return map[string]any{"id": rec.id, "status": rec.status, "percent": rec.pct,
			"file": rec.file, "size_mb": float64(rec.size) / 1024 / 1024,
			"episodes": rec.eps, "error": rec.err, "ep_index": epIdx}
	}

	sources := []map[string]any{}
	for _, x := range rs {
		// 该源下的线路（play_from）与剧集。
		// 播放地址是拆成 host_id + path 存的，这里拼回完整 URL（见 db.PlayURLExpr）
		prows, perr := s.DB.Query(`SELECT p.play_from, p.ep_index, `+db.EpNameExpr+`, `+
			db.PlayURLExpr+`, p.is_preferred FROM vod_play p
			LEFT JOIN play_host h ON h.id=p.host_id
			WHERE p.vod_id=? ORDER BY p.is_preferred DESC, p.play_from, p.ep_index`, x.id)
		lineOrder := []string{}
		lineEps := map[string][]map[string]any{}
		linePref := map[string]int{}
		if perr == nil {
			for prows.Next() {
				var from, epName, u string
				var epIdx, pref int
				if err := prows.Scan(&from, &epIdx, &epName, &u, &pref); err != nil {
					continue
				}
				if _, exist := lineEps[from]; !exist {
					lineOrder = append(lineOrder, from)
					linePref[from] = pref
				}
				lineEps[from] = append(lineEps[from], map[string]any{
					"ep_index": epIdx, "ep_name": epName, "url": u,
					"download": dlObj(from, epIdx), // 单集下载状态
				})
			}
			prows.Close()
		}
		lines := []map[string]any{}
		for _, from := range lineOrder {
			lines = append(lines, map[string]any{
				"play_from": from, "episode_count": len(lineEps[from]), "episodes": lineEps[from],
			})
		}
		sources = append(sources, map[string]any{
			"vod_id": x.id, "source_id": x.sourceID, "source_name": x.srcName,
			"remarks": x.remarks, "pic": x.pic, "lines": lines,
		})
	}
	detail["sources"] = sources
	ok(w, detail)
}

func (s *Server) proxyImage(w http.ResponseWriter, r *http.Request) {
	u := strings.TrimSpace(r.URL.Query().Get("u"))
	if u == "" || (!strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://")) {
		http.NotFound(w, r)
		return
	}
	imgDir := filepath.Join(s.DataDir, "img")
	local := collector.CoverPath(imgDir, u)

	serve := func(data []byte) {
		// 按真实格式返回（历史缓存里可能存过 webp/png 原图，标成 jpeg 会让浏览器当坏图）
		w.Header().Set("Content-Type", imageContentType(data))
		w.Header().Set("Cache-Control", "public, max-age=2592000")
		w.Write(data)
	}

	// 1) 本地已有压缩图：直接读文件返回（毫秒级，不访问源站）
	if data, err := os.ReadFile(local); err == nil && len(data) > 0 {
		serve(data)
		return
	}

	// 2) 兼容旧版扁平目录：直接返回，顺手搬到分片目录
	if legacy := collector.CoverPathLegacy(imgDir, u); legacy != local {
		if data, err := os.ReadFile(legacy); err == nil && len(data) > 0 {
			serve(data)
			go func(from, to string) {
				if err := os.MkdirAll(filepath.Dir(to), 0o755); err == nil {
					_ = os.Rename(from, to)
				}
			}(legacy, local)
			return
		}
	}

	// 3) 本地还没有：排队抓取，然后告诉前端"暂时没有"
	//    force=1（前端带 ?r= 时间戳重试）会忽略失败负缓存再试一次 —— 单源时靠这个
	//    在源站刷新后重新拿到新图。
	force := r.URL.Query().Get("r") != ""
	collector.EnsureCoverRetry(u, force)
	// 用 204 而不是 404：语义上都是"当前没有内容"，但 204 不会在浏览器控制台
	// 刷一排红字（每张没来得及缓存的封面都算一次失败请求）。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

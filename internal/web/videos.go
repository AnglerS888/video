package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"videoDownLoad/internal/collector"
	db "videoDownLoad/internal/db"
)

// ---- 筛选项（年份 / 国家地区 / 类型）----
// 直接取聚合表的存量值（结果集小、可走索引），并缓存 5 分钟。
func (s *Server) listFilters(w http.ResponseWriter, r *http.Request) {
	// 不做缓存：这三条查询走索引只需几毫秒，缓存反而会让刚采集到的新年份/地区延迟可见

	// 全部取值都来自数据库，不做任何硬编码。
	// 优先查预聚合表 vod_name_agg（行数少、有索引）；若聚合表还没建好（老库刚升级），
	// 退回到 vod_info 实时 DISTINCT，保证下拉框不会空着。
	distinct := func(col string, orderBy string, limit int) []string {
		q := `SELECT TRIM(` + col + `) AS v, COUNT(*) AS c FROM %s
			WHERE TRIM(` + col + `) <> '' GROUP BY v ORDER BY ` + orderBy + ` LIMIT ` + strconv.Itoa(limit)
		for _, table := range []string{"vod_name_agg", "vod_info"} {
			rows, err := s.DB.Query(fmt.Sprintf(q, table))
			if err != nil {
				continue
			}
			out := []string{}
			for rows.Next() {
				var v string
				var c int64
				if rows.Scan(&v, &c) == nil && v != "" {
					out = append(out, v)
				}
			}
			rows.Close() // 单连接库：必须关闭后再查下一张，否则死锁
			if len(out) > 0 {
				return out
			}
		}
		return []string{}
	}

	data := map[string]any{
		// 年份：按数值倒序（2026 → 2025 → …），不按出现次数排
		"years": distinct("vod_year", "CAST(v AS INTEGER) DESC, v DESC", 200),
		// 地区 / 类型：按片量从多到少，数量相同按名称排序
		"areas": distinct("vod_area", "c DESC, v ASC", 300),
		"types": distinct("type_name", "c DESC, v ASC", 300),
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
	srcID := q.Get("source_id")
	onlyDone := q.Get("filter") == "downloaded"
	fYear := strings.TrimSpace(q.Get("year"))
	fArea := strings.TrimSpace(q.Get("area"))
	fType := strings.TrimSpace(q.Get("type"))

	cacheKey := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%d|%d",
		wd, srcID, q.Get("filter"), fYear, fArea, fType, page, size)
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
		where += " AND (vod_name LIKE ? OR vod_actor LIKE ? OR type_name LIKE ?) "
		like := "%" + wd + "%"
		args = append(args, like, like, like)
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
	if fYear != "" {
		where += " AND vod_year = ? "
		args = append(args, fYear)
	}
	if fArea != "" {
		where += " AND vod_area = ? "
		args = append(args, fArea)
	}
	if fType != "" {
		where += " AND type_name = ? "
		args = append(args, fType)
	}

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

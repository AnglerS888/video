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
)

func (s *Server) enqueueDownload(w http.ResponseWriter, r *http.Request) {
	var in struct {
		VodID     int64  `json:"vod_id"`
		PlayFrom  string `json:"play_from"`
		EpIndices []int  `json:"ep_indices"` // 勾选的集号（空 = 全部集）；每集一个独立 mp4
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, "参数错误: "+err.Error())
		return
	}
	if in.VodID == 0 || in.PlayFrom == "" {
		fail(w, "vod_id 与 play_from 必填")
		return
	}
	created, skipped, err := collector.EnqueueDownloadBatch(in.VodID, in.PlayFrom, in.EpIndices)
	if err != nil {
		fail(w, err.Error())
		return
	}
	if created == 0 {
		fail(w, fmt.Sprintf("没有可新建的任务：跳过 %d 集（已下载或下载中）", skipped))
		return
	}
	ok(w, map[string]any{"created": created, "skipped": skipped,
		"msg": fmt.Sprintf("已加入 %d 个下载任务（每集一个 mp4）", created)})
}

func (s *Server) listDownloads(w http.ResponseWriter, r *http.Request) {
	all := collector.SnapshotDownloads()
	done, active := []collector.DownloadTask{}, []collector.DownloadTask{}
	for _, t := range all {
		switch t.Status {
		case "done":
			done = append(done, t)
		case "downloading", "queued", "paused":
			active = append(active, t)
		}
	}
	ok(w, map[string]any{"done": done, "tasks": all, "active": active})
}

func (s *Server) pauseDownload(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := collector.PauseDownload(id); err != nil {
		fail(w, err.Error())
		return
	}
	ok(w, "已暂停")
}

func (s *Server) resumeDownload(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := collector.ResumeDownload(id); err != nil {
		fail(w, err.Error())
		return
	}
	ok(w, "已继续下载")
}

// priorityDownload 插队：当前正在下的那集结束后，优先下载这一集
func (s *Server) priorityDownload(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := collector.SetDownloadPriority(id); err != nil {
		fail(w, err.Error())
		return
	}
	ok(w, "已设为优先下载，当前这一集完成后立刻轮到它")
}

func (s *Server) deleteDownload(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := collector.DeleteDownload(id); err != nil {
		fail(w, err.Error())
		return
	}
	ok(w, "已删除")
}

// retryDownload 重试失败的任务（failed → queued，已下好的分片会复用）
func (s *Server) retryDownload(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := collector.RetryDownload(id); err != nil {
		fail(w, err.Error())
		return
	}
	ok(w, "已重新加入下载队列")
}

// batchDownload 批量操作下载任务：pause / resume / retry / delete
func (s *Server) batchDownload(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string  `json:"action"`
		IDs    []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, "参数错误: "+err.Error())
		return
	}
	if len(in.IDs) == 0 {
		fail(w, "请先选择任务")
		return
	}
	var fn func(int64) error
	var label string
	switch in.Action {
	case "pause":
		fn, label = collector.PauseDownload, "暂停"
	case "resume":
		fn, label = collector.ResumeDownload, "继续"
	case "retry":
		fn, label = collector.RetryDownload, "重试"
	case "delete":
		fn, label = collector.DeleteDownload, "删除"
	default:
		fail(w, "不支持的操作: "+in.Action)
		return
	}

	okCnt, errs := 0, []string{}
	for _, id := range in.IDs {
		if err := fn(id); err != nil {
			if len(errs) < 3 {
				errs = append(errs, fmt.Sprintf("#%d %v", id, err))
			}
			continue
		}
		okCnt++
	}
	msg := fmt.Sprintf("已%s %d 个任务", label, okCnt)
	if len(errs) > 0 {
		msg += fmt.Sprintf("，失败 %d 个（%s）", len(in.IDs)-okCnt, strings.Join(errs, "；"))
	}
	if okCnt == 0 {
		fail(w, msg)
		return
	}
	ok(w, msg)
}

// playVideo 仅允许播放已下载完成的本地 mp4（参数为 vod_download.id）
func (s *Server) playVideo(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var file, status string
	if err := s.DB.QueryRow(`SELECT file_path, status FROM vod_download WHERE id=?`, id).Scan(&file, &status); err != nil {
		http.Error(w, "下载记录不存在", http.StatusNotFound)
		return
	}
	if status != "done" || file == "" {
		http.Error(w, "视频尚未下载完成，无法播放", http.StatusForbidden)
		return
	}
	file = resolveStoredPath(file)
	if _, err := os.Stat(file); err != nil {
		http.Error(w, "本地文件已丢失，请重新下载", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Accept-Ranges", "bytes")
	if r.URL.Query().Get("dl") == "1" {
		// 分享链接加 ?dl=1：浏览器直接当文件下载，而不是内嵌播放
		name := filepath.Base(file)
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+neturl.PathEscape(name))
	}
	http.ServeFile(w, r, file)
}

// shareDownload 生成一条局域网可访问的外链（分享页），前端据此展示二维码与链接。
// 不引入 token：这是内网自用的媒体服务，链接本身只在本机所在局域网可达。
//
// 注意：对外只给「分享页」地址 /s/{id}。play_url / file_url 仍随接口返回，
// 但前端分享弹窗不再展示——直接把视频文件地址发给别人既不好看，
// 也没法在页面里提示「这一集还没下完」「同剧还有哪几集可看」。
func (s *Server) shareDownload(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var file, status, name string
	var epIdx int
	if err := s.DB.QueryRow(`SELECT COALESCE(file_path,''), status, vod_name, ep_index
		FROM vod_download WHERE id=?`, id).Scan(&file, &status, &name, &epIdx); err != nil {
		fail(w, "下载记录不存在")
		return
	}
	if status != "done" || file == "" {
		fail(w, "这一集还没下载完成，无法分享")
		return
	}
	// 有效期：?hours=24 / 168（7 天）/ 0（永久）。每次打开分享弹窗都会按选择重写，
	// 选「永久」等于把之前设过的失效时间清掉。
	hours, _ := strconv.ParseFloat(r.URL.Query().Get("hours"), 64)
	expires := ""
	if hours > 0 {
		if hours > 24*365 { // 上限一年，防止手滑填出 100 年
			hours = 24 * 365
		}
		expires = time.Now().UTC().Add(time.Duration(hours * float64(time.Hour))).Format("2006-01-02 15:04:05")
	}
	s.DB.Exec(`UPDATE vod_download SET share_expires_at=? WHERE id=?`, expires, id)
	base := publicBaseURL(r)
	ep := "整条"
	if epIdx >= 0 {
		ep = fmt.Sprintf("第%d集", epIdx+1)
	}
	ok(w, map[string]any{
		"id": id, "name": name, "episode": ep,
		// 分享给别人用这个：打开是一个页面，可播放/下载，不会直接拿到视频文件地址
		"page_url":   base + "/s/" + strconv.FormatInt(id, 10),
		"play_url":   base + "/play/" + strconv.FormatInt(id, 10),
		"file_url":   base + "/play/" + strconv.FormatInt(id, 10) + "?dl=1",
		"expires_at": expires, // UTC，空 = 永久有效
	})
}

// sharePage 分享页数据（/s/{id}）：标题、封面、集数、同剧已下载的其他集。
// 播放/下载仍走 /play/{id}，由页面上的按钮触发，页面本身不直接嵌入视频直链。
func (s *Server) sharePage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id <= 0 {
		fail(w, "分享链接无效")
		return
	}
	var (
		vodID                  int64
		name, status, src, pic string
		playFrom               string
		epIdx                  int
		size                   int64
		shareExpires           string
	)
	err := s.DB.QueryRow(`SELECT d.vod_id, d.vod_name, d.ep_index, COALESCE(d.source_name,''),
		d.status, d.play_from, COALESCE(d.file_size,0), COALESCE(v.vod_pic,''),
		COALESCE(d.share_expires_at,'')
		FROM vod_download d LEFT JOIN vod_info v ON v.id = d.vod_id
		WHERE d.id=?`, id).
		Scan(&vodID, &name, &epIdx, &src, &status, &playFrom, &size, &pic, &shareExpires)
	if err != nil {
		fail(w, "分享的内容不存在（可能已被删除）")
		return
	}
	if status != "done" {
		fail(w, "这一集还没下载完成，暂时无法分享")
		return
	}
	// 失效时间：分享者选过有效期后，过期的链接不再展示内容
	if shareExpires != "" {
		if t, perr := time.Parse("2006-01-02 15:04:05", shareExpires); perr == nil && time.Now().UTC().After(t) {
			fail(w, "分享链接已过期")
			return
		}
	}
	ep := "整条"
	if epIdx >= 0 {
		ep = fmt.Sprintf("第%d集", epIdx+1)
	}

	// 同剧其它已下载的集（同名合并，多源同集只留第一条）——
	// DB 只有 1 个连接：先把结果读成切片，关掉 rows 再继续（否则下一步会永久阻塞）
	type epItem struct {
		ID  int64 `json:"id"`
		Idx int   `json:"ep_index"`
	}
	eps := []epItem{}
	rows, qerr := s.DB.Query(`SELECT id, ep_index FROM vod_download
		WHERE vod_name=? AND status='done' ORDER BY ep_index, id LIMIT 300`, name)
	if qerr == nil {
		for rows.Next() {
			var e epItem
			if rows.Scan(&e.ID, &e.Idx) == nil {
				eps = append(eps, e)
			}
		}
		rows.Close()
	}
	seen := map[int]bool{}
	list := make([]epItem, 0, len(eps))
	for _, e := range eps {
		if seen[e.Idx] {
			continue
		}
		seen[e.Idx] = true
		list = append(list, e)
	}

	ok(w, map[string]any{
		"id": id, "vod_id": vodID, "name": name, "episode": ep,
		"source_name": src, "vod_pic": pic, "play_from": playFrom,
		"size_mb":    float64(size) / 1024 / 1024,
		"play_url":   "/play/" + strconv.FormatInt(id, 10),
		"file_url":   "/play/" + strconv.FormatInt(id, 10) + "?dl=1",
		"episodes":   list,
		"expires_at": shareExpires, // UTC，空 = 永久有效
	})
}

// shareBase 返回外部可访问的站点根地址（首页/详情页的二维码分享用）。
// 本机 localhost 访问时会换成内网 IP，否则手机扫码打不开。
func (s *Server) shareBase(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"base_url": publicBaseURL(r)})
}

// publicBaseURL 推断外部可访问的地址：
// 优先用请求的 Host（局域网访问时就是 192.168.x.x:5200），
// 本机 localhost 访问时换成网卡上的第一个内网 IPv4，这样二维码扫出来别人才打得开。
func publicBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// 保留请求本来的端口：以前 Host 不带端口时硬补 5200，
	// 通过 80 端口/反向代理访问时二维码会扫到 :5200 打不开。
	h, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		h, port = r.Host, ""
	}
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "" {
		if ip := lanIPv4(); ip != "" {
			h = ip
		}
	}
	if port == "" {
		return scheme + "://" + h
	}
	return scheme + "://" + net.JoinHostPort(h, port)
}

// lanIPv4 取本机第一个私有网卡地址
func lanIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, aerr := iface.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			if ip.IsPrivate() {
				return ip.String()
			}
		}
	}
	return ""
}

// resolveStoredPath 旧版本可能把相对路径存进数据库，这里按当前工作目录还原
func resolveStoredPath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, p)
	}
	return p
}

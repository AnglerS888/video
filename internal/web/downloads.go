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
	// 集号数组上限：一部剧正常也就几百集，超出说明是异常输入
	if len(in.EpIndices) > maxArrayItems {
		fail(w, fmt.Sprintf("一次最多提交 %d 集（当前 %d 集）", maxArrayItems, len(in.EpIndices)))
		return
	}
	created, skipped, err := collector.EnqueueDownloadBatch(in.VodID, in.PlayFrom, in.EpIndices)
	if err != nil {
		fail(w, publicErr(err))
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
		fail(w, publicErr(err))
		return
	}
	ok(w, "已暂停")
}

func (s *Server) resumeDownload(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := collector.ResumeDownload(id); err != nil {
		fail(w, publicErr(err))
		return
	}
	ok(w, "已继续下载")
}

// priorityDownload 插队：当前正在下的那集结束后，优先下载这一集
func (s *Server) priorityDownload(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := collector.SetDownloadPriority(id); err != nil {
		fail(w, publicErr(err))
		return
	}
	ok(w, "已设为优先下载，当前这一集完成后立刻轮到它")
}

func (s *Server) deleteDownload(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := collector.DeleteDownload(id); err != nil {
		fail(w, publicErr(err))
		return
	}
	ok(w, "已删除")
}

// retryDownload 重试失败的任务（failed → queued，已下好的分片会复用）
func (s *Server) retryDownload(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := collector.RetryDownload(id); err != nil {
		fail(w, publicErr(err))
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
	// 批量操作条数上限：ids 可达百万级，逐条 DB 写会长时间占着连接
	if len(in.IDs) > maxArrayItems {
		fail(w, fmt.Sprintf("一次最多操作 %d 个任务（当前 %d 个），请分批操作", maxArrayItems, len(in.IDs)))
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

// playVideo 播放已下载完成的本地 mp4。
//
// 路径参数有两种：
//   - 数字 id：本地播放，必须带播放令牌（?t=...）或管理口令。
//     没有令牌就放行的话，id 是自增整数 → /play/1、/play/2 能被直接枚举，
//     同一局域网的任何人��能把整个下载库拖走。
//   - 分享令牌：分享链接专用，长期有效（删除下载记录时一并失效）。
func (s *Server) playVideo(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("id")
	var id int64
	if n, err := strconv.ParseInt(key, 10, 64); err == nil {
		id = n
		if !validPlayToken(r.URL.Query().Get("t")) && !adminAuthorized(r) {
			http.Error(w, "需要播放令牌（请刷新页面后重试）", http.StatusForbidden)
			return
		}
	} else {
		// 分享令牌
		did, valid := db.ShareTokenLookup(s.DB, key)
		if !valid {
			http.Error(w, "分享链接无效", http.StatusForbidden)
			return
		}
		id = did
		// 分享页切到同剧其它集时带 ?ep=N：令牌授权的是「这部剧」
		// （分享页本来就列出同剧所有已下载的集），这里换成该集的下载记录
		if v := strings.TrimSpace(r.URL.Query().Get("ep")); v != "" {
			if ep, e := strconv.Atoi(v); e == nil {
				var vodName string
				var curEp int
				if err := s.DB.QueryRow(`SELECT vod_name, ep_index FROM vod_download WHERE id=?`, did).
					Scan(&vodName, &curEp); err == nil && ep != curEp {
					var nid int64
					if err := s.DB.QueryRow(`SELECT id FROM vod_download
						WHERE vod_name=? AND ep_index=? AND status='done' ORDER BY id LIMIT 1`,
						vodName, ep).Scan(&nid); err == nil {
						id = nid
					}
				}
			}
		}
	}
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
//
// 分享链接用**随机令牌**而不是下载 id：
//   - id 是自增整数，/play/{id} 能被直接枚举（整个下载库都能拖走）；
//   - 令牌是独立的随机凭据，删除下载记录时一并失效。
//
// 分享链接永久有效，不再提供有效期选项。
//
// 对外给的是「分享页」地址 /s/{token}。play_url / file_url 也随接口返回，
// 但前端分享弹窗不展示——直接把视频文件地址发给别人既不好看，
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
	// 令牌入库（永久有效）
	token, terr := db.NewShareToken(s.DB, id)
	if terr != nil {
		fail(w, "生成分享链接失败: "+publicErr(terr))
		return
	}
	base := publicBaseURL(r)
	ep := "整条"
	if epIdx >= 0 {
		ep = fmt.Sprintf("第%d集", epIdx+1)
	}
	ok(w, map[string]any{
		"id": id, "name": name, "episode": ep, "token": token,
		// 分享给别人用这个：打开是一个页面，可播放/下载，不会直接拿到视频文件地址
		"page_url": base + "/s/" + token,
		"play_url": base + "/play/" + token,
		"file_url": base + "/play/" + token + "?dl=1",
	})
}

// sharePage 分享页数据（/s/{token}）：标题、封面、集数、同剧已下载的其他集。
// 播放/下载走 /play/{token}（同一个令牌），由页面上的按钮触发。
//
// 路径参数既支持新令牌，也兼容旧版数字 id（老的分享链接）：
//   - 令牌：永久有效，删除下载记录时一并失效；
//   - 数字 id：没有分享凭据，只允许分享者本人（播放令牌/管理口令）查看。
func (s *Server) sharePage(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("id")
	var id int64
	isToken := false
	if n, err := strconv.ParseInt(key, 10, 64); err == nil {
		// 旧版数字链接没有分享凭据：只允许分享者本人查看
		if !validPlayToken(r.URL.Query().Get("t")) && !adminAuthorized(r) {
			fail(w, "分享链接无效")
			return
		}
		id = n
	} else {
		did, valid := db.ShareTokenLookup(s.DB, key)
		if !valid {
			fail(w, "分享链接无效")
			return
		}
		id, isToken = did, true
	}
	if id <= 0 {
		fail(w, "分享链接无效")
		return
	}

	// 取一条下载记录（分享页展示用）。抽成函数是因为下面切集时可能要重新查一次
	type sharePageRow struct {
		vodID    int64
		name     string
		epIdx    int
		src      string
		status   string
		playFrom string
		size     int64
		pic      string
	}
	load := func(did int64) (sharePageRow, bool) {
		var row sharePageRow
		err := s.DB.QueryRow(`SELECT d.vod_id, d.vod_name, d.ep_index, COALESCE(d.source_name,''),
			d.status, d.play_from, COALESCE(d.file_size,0), COALESCE(v.vod_pic,'')
			FROM vod_download d LEFT JOIN vod_info v ON v.id = d.vod_id
			WHERE d.id=?`, did).
			Scan(&row.vodID, &row.name, &row.epIdx, &row.src, &row.status, &row.playFrom, &row.size, &row.pic)
		return row, err == nil
	}
	row, found := load(id)
	if !found {
		fail(w, "分享的内容不存在（可能已被删除）")
		return
	}
	// 令牌分享页允许用 ?ep=N 切到同剧其它已下载的集：
	// 直接跳到 /s/{数字id} 会被数字 id 的凭据校验挡住（页面提示「分享链接无效」），
	// 令牌本来是这部剧的凭据（下面也列出了同剧所有集），用集号换记录即可。
	if isToken {
		if v := strings.TrimSpace(r.URL.Query().Get("ep")); v != "" {
			if ep, e := strconv.Atoi(v); e == nil && ep != row.epIdx {
				var nid int64
				if e2 := s.DB.QueryRow(`SELECT id FROM vod_download
					WHERE vod_name=? AND ep_index=? AND status='done' ORDER BY id LIMIT 1`,
					row.name, ep).Scan(&nid); e2 == nil {
					if r2, ok2 := load(nid); ok2 {
						id, row = nid, r2
					}
				}
			}
		}
	}
	if row.status != "done" {
		fail(w, "这一集还没下载完成，暂时无法分享")
		return
	}
	ep := "整条"
	if row.epIdx >= 0 {
		ep = fmt.Sprintf("第%d集", row.epIdx+1)
	}

	// 同剧其它已下载的集（同名合并，多源同集只留第一条）——
	// 先把结果读成切片，关掉 rows 再继续：
	// 持有 rows 时同一条连接上再发查询会阻塞（连接池默认 4 个连接，但这是同一条）
	// 只给集号，不给下载 id：页面切集只带 ?ep=N，不需要暴露内部自增 id。
	type epItem struct {
		Idx int `json:"ep_index"`
	}
	eps := []epItem{}
	rows, qerr := s.DB.Query(`SELECT ep_index FROM vod_download
		WHERE vod_name=? AND status='done' ORDER BY ep_index, id LIMIT 300`, row.name)
	if qerr == nil {
		for rows.Next() {
			var e epItem
			if rows.Scan(&e.Idx) == nil {
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

	// 播放/下载地址用令牌（分享页访问者没有播放令牌，只能用分享令牌）。
	// 带上当前集号：切到同剧其它集后播放/下载的是那一集，而不是令牌绑定的那一集。
	playKey := ""
	if isToken {
		playKey = key
	}
	resp := map[string]any{
		"id": id, "vod_id": row.vodID, "name": row.name, "episode": ep, "ep_index": row.epIdx,
		"source_name": row.src, "vod_pic": row.pic, "play_from": row.playFrom,
		"size_mb":  float64(row.size) / 1024 / 1024,
		"episodes": list,
	}
	if playKey != "" {
		q := "?ep=" + strconv.Itoa(row.epIdx)
		resp["play_url"] = "/play/" + playKey + q
		resp["file_url"] = "/play/" + playKey + q + "&dl=1"
	}
	ok(w, resp)
}

// shareBase 返回外部可访问的站点根地址（首页/详情页的二维码分享用）。
// 本机 localhost 访问时会换成内网 IP，否则手机扫码打不开。
func (s *Server) shareBase(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"base_url": publicBaseURL(r)})
}

// publicBaseURL 推断外部可访问的地址：
// 优先用请求的 Host（局域网访问时就是 192.168.x.x:5200），
// 本机 localhost 访问时换成真实网卡的局域网 IPv4（避开虚拟机/VPN 虚拟网卡），
// 这样二维码扫出来别人才打得开。
//
// 安全：Host 头是客户端可控的，攻击者可以伪造 Host: evil.com，
// 于是分享链接 / 二维码就指向攻击者的域名（钓鱼）。
// 所以只有当 Host 指向**本机**（IP 字面量 / localhost / 本机名）时才采信，
// 其余一律换成内网 IP；反向代理场景用环境变量 VDL_PUBLIC_URL 显式指定。
func publicBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// 反向代理 / 域名访问：由运维显式指定，避免猜错
	if v := strings.TrimSpace(os.Getenv("VDL_PUBLIC_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	// 保留请求本来的端口：以前 Host 不带端口时硬补 5200，
	// 通过 80 端口/反向代理访问时二维码会扫到 :5200 打不开。
	h, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		h, port = r.Host, ""
	}
	if !hostIsLocal(h) {
		// Host 不可信：换成内网 IP（后面统一处理）
		h = ""
	}
	if h == "" || h == "localhost" || h == "127.0.0.1" || h == "::1" {
		if ip := lanIPv4(); ip != "" {
			h = ip
		} else if h == "" {
			h = "127.0.0.1"
		}
	}
	if port == "" {
		return scheme + "://" + h
	}
	return scheme + "://" + net.JoinHostPort(h, port)
}

// hostIsLocal 判断 Host 的主机部分是否指向本机：
// IP 字面量、localhost、本机 hostname 都算；其它（外部域名）一律不信。
func hostIsLocal(h string) bool {
	h = strings.TrimSpace(strings.Trim(h, "[]"))
	if h == "" {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	lower := strings.ToLower(h)
	if lower == "localhost" {
		return true
	}
	// 本机 hostname（NAS 上常见 "MyNAS" / "docker"）
	if name, err := os.Hostname(); err == nil && name != "" {
		if strings.EqualFold(lower, strings.ToLower(name)) {
			return true
		}
	}
	return false
}

// lanIPv4 推断本机的局域网 IPv4。
//
// 不能直接取「第一个私有地址」：装了虚拟机 / VPN / 手机互联软件后，
// 网卡顺序可能是 NodeBabyLink(10.x)、VMnet、Hyper-V 这些虚拟设备，
// 分享链接/二维码就会指向别人连不上的地址（表现为「分享打开失效」）。
//
// 选路顺序：
//  1. 默认路由的出口地址（最接近「别人能访问到的那个地址」）；
//  2. 枚举网卡，跳过虚拟设备，优先名字像无线/以太网的；
//  3. 全部被判为虚拟时，退回第一个私有地址（保证总比空着好）。
func lanIPv4() string {
	if ip := outboundIPv4(); ip != "" {
		return ip
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	first, preferred, fallback := "", "", ""
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		ip := ifaceIPv4(iface)
		if ip == "" {
			continue
		}
		if isVirtualIface(iface.Name) {
			if fallback == "" {
				fallback = ip
			}
			continue
		}
		if first == "" {
			first = ip
		}
		if isRealIface(iface.Name) {
			preferred = ip
			break
		}
	}
	switch {
	case preferred != "":
		return preferred
	case first != "":
		return first
	}
	return fallback
}

// outboundIPv4 用默认路由问出本机出口 IPv4。
// UDP 不握手，这次 dial 只让系统选一次路由、不会真的发包；
// 出口不是内网地址（代理 TUN / 公网）或正好落在虚拟网卡上时返回空，交给网卡枚举。
func outboundIPv4() string {
	c, err := net.DialTimeout("udp4", "8.8.8.8:80", 300*time.Millisecond)
	if err != nil {
		return ""
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || ua.IP == nil || ua.IP.To4() == nil || !ua.IP.IsPrivate() {
		return ""
	}
	if ipOnVirtualIface(ua.IP) {
		return ""
	}
	return ua.IP.String()
}

// ifaceIPv4 取网卡上的私有 IPv4（169.254 链路本地地址不算）
func ifaceIPv4(iface net.Interface) string {
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() || ip.To4() == nil || !ip.IsPrivate() {
			continue
		}
		return ip.String()
	}
	return ""
}

// ipOnVirtualIface 判断地址是否落在虚拟网卡上
func ipOnVirtualIface(ip net.IP) bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		if !isVirtualIface(iface.Name) {
			continue
		}
		addrs, aerr := iface.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// isVirtualIface 判断网卡是否是虚拟机 / VPN / 容器 / 手机互联等虚拟设备：
// 这些地址只在对应软件内部有意义，发给手机/电视打不开。
func isVirtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, kw := range []string{
		"vmware", "vmnet", "virtualbox", "vbox", "hyper-v", "vethernet", "wsl",
		"docker", "tailscale", "zerotier", "openvpn", "tap-", "tun", "radmin",
		"virtual", "npcap", "nodebaby",
	} {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}

// isRealIface 名字像无线/以太网的网卡优先：
// Windows 的 WLAN / 以太网，Linux 的 wlan0 / eth0 / enp3s0 / wlp2s0
func isRealIface(name string) bool {
	n := strings.ToLower(name)
	for _, kw := range []string{"wlan", "wi-fi", "wifi", "wireless", "无线", "ethernet", "以太网", "eth", "enp", "wlp"} {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
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

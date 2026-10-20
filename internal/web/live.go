package web

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"videoDownLoad/internal/logx"
)

// ============================================================================
// HLS 在线播放代理
// ============================================================================
//
// 为什么必须代理：源站的 m3u8 基本都不带 CORS 头，浏览器里 hls.js 直接请求会被
// 同源策略拦掉；而且播放列表里的分片地址常常是相对路径。
// 统一走本服务转发：
//
//	GET /api/live?u=<encodeURIComponent(绝对地址)>
//
//   - 播放列表（m3u8，含 master playlist）：读成文本，把每一行 URI 以及
//     URI="..."（EXT-X-KEY / EXT-X-MAP / EXT-X-MEDIA / EXT-X-I-FRAME-STREAM-INF）
//     全部改写成代理地址，再返回
//   - 分片 / 密钥文件：流式透传（转发 Range，回写 206、Content-Type、Content-Length）
//
// 安全：只允许 http/https，拒绝本机与私网地址，避免被当成内网探测代理。

const liveUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/122.0 Safari/537.36"

// liveClient 流式转发不设总超时（分片大小不定），只限制响应头等待时间
var liveClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// liveURIPat 匹配 m3u8 标签里的 URI="..."
var liveURIPat = regexp.MustCompile(`URI="([^"]*)"`)

// liveProxy GET /api/live?u=<绝对地址>
func (s *Server) liveProxy(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("u"))
	if raw == "" {
		http.Error(w, "missing u", http.StatusBadRequest)
		return
	}
	target, err := url.Parse(raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	if !liveHostAllowed(target.Hostname()) {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}

	// 客户端断开（关掉播放器）就取消上游请求，避免源站连接挂着。
	// 注意不能加总超时：分片大小和源站速度都不定，慢源拖过超时会被中途掐断，
	// 表现为"播几十秒必卡死"。
	req, rerr := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if rerr != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Header.Set("User-Agent", liveUA)
	// 部分源站校验 Referer：用 m3u8 自己的站点根地址最不容易被拒
	req.Header.Set("Referer", target.Scheme+"://"+target.Host+"/")
	req.Header.Set("Accept", "*/*")
	if rh := r.Header.Get("Range"); rh != "" {
		req.Header.Set("Range", rh)
	}

	resp, derr := liveClient.Do(req)
	if derr != nil {
		if r.Context().Err() != nil {
			return // 自己这边断了，不用再写响应
		}
		logx.Warnf("[live] 拉取失败 %s: %v", target.Redacted(), derr)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		logx.Warnf("[live] 上游 HTTP %d %s", resp.StatusCode, target.Redacted())
		http.Error(w, "upstream "+resp.Status, http.StatusBadGateway)
		return
	}

	// ---- 播放列表：读成文本改写 ----
	if isM3U8Response(resp, target) {
		body, berr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if berr != nil {
			http.Error(w, "read upstream error", http.StatusBadGateway)
			return
		}
		body = trimBOM(body)
		if strings.HasPrefix(strings.TrimSpace(string(body)), "#EXTM3U") {
			out := rewriteM3U8(body, resp.Request.URL)
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)
			return
		}
		// 内容其实不是播放列表（源站返回了错误页/HTML）：按二进制原样回，方便排查
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}

	// ---- 分片 / 密钥：流式透传；伪装分片先剥头 ----
	// 部分源（如 play.ly166.com）把 TS 分片伪装成 .jpeg 防盗链：文件头是真实
	// JPEG/PNG，往后几十~几千字节才是 TS 数据。下载器合并前会剥掉这层伪装头，
	// 浏览器直拉拿到图片头必然解不了 —— 这就是"下载正常、在线播放失败"的根因，
	// 代理转发时必须做同样的剥离。
	head := make([]byte, 512)
	hn, herr := io.ReadFull(resp.Body, head)
	head = head[:hn]
	if herr != nil && herr != io.EOF && herr != io.ErrUnexpectedEOF {
		if r.Context().Err() == nil {
			logx.Warnf("[live] 读分片头失败 %s: %v", target.Redacted(), herr)
		}
		return
	}
	if hn == 0 { // 空分片，原样回状态码
		w.WriteHeader(resp.StatusCode)
		return
	}

	// 剥伪装头。找不到 TS 同步头就原样透传（密钥文件、未知格式），不替上游做判断。
	// 带了 Range 也照常剥：此时直接忽略 Range 回 200 全量（HTTP 允许服务端忽略
	// Range；Safari 原生播 HLS 会带 Range 探针，不剥它拿到 JPEG 头就直接判死）。
	// BYTERANGE 播放列表的媒体文件都是裸 TS，走不到这个分支，不受影响。
	strip := 0
	buf := head
	if !livePlainSegment(buf) {
		for len(buf) < livePeekMax {
			if off := liveTSSyncOffset(buf); off > 0 {
				strip = off
				break
			}
			chunk := make([]byte, 32<<10)
			n, rerr2 := resp.Body.Read(chunk)
			if n > 0 {
				buf = append(buf, chunk[:n]...)
			}
			if rerr2 != nil {
				break
			}
		}
	}

	if strip > 0 {
		// 剥了头，长度变了：Content-Length / Content-Range 一律不回写
		logx.Debugf("[live] 剥离伪装头 %d 字节 %s", strip, target.Redacted())
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf[strip:])
		_, _ = io.Copy(w, resp.Body)
		return
	}

	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Content-Encoding"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	// 上游谎报 Content-Type 的（TS 分片标成 image/jpeg）顺手纠正，
	// 浏览器按图片处理会直接弃掉响应
	if len(buf) > 0 && buf[0] == 0x47 && strings.HasPrefix(w.Header().Get("Content-Type"), "image/") {
		w.Header().Set("Content-Type", "video/mp2t")
	}
	if w.Header().Get("Accept-Ranges") == "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	// 分片内容不可变，允许浏览器短缓存，回拖时不用重复下载
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(resp.StatusCode)
	if _, werr := w.Write(buf); werr == nil {
		if _, cerr := io.Copy(w, resp.Body); cerr != nil && r.Context().Err() == nil {
			logx.Debugf("[live] 转发中断 %s: %v", target.Redacted(), cerr)
		}
	}
}

// livePeekMax 伪装头扫描上限：伪装头一般几十到几千字节，超过 256KB 还没找到
// TS 同步头就放弃剥离、原样透传（避免为异常内容占太多内存）
const livePeekMax = 256 << 10

// livePlainSegment 判断分片是不是"裸格式"（TS / fMP4 / ADTS AAC），
// 与下载器 looksPlainSegment 同一套规则，两处要保持一致。
func livePlainSegment(b []byte) bool {
	if len(b) >= 1 && b[0] == 0x47 && (len(b) <= 188 || b[188] == 0x47) {
		return true // 标准 TS：首字节同步头，且第二包也同步（GIF 魔数 'G' 也是 0x47，必须验第二包）
	}
	if len(b) >= 8 {
		switch string(b[4:8]) {
		case "ftyp", "styp", "moof", "sidx", "emsg": // fMP4 box
			return true
		}
	}
	if len(b) >= 2 && b[0] == 0xFF && b[1]&0xF6 == 0xF0 {
		return true // ADTS 裸 AAC
	}
	return false
}

// liveTSSyncOffset 在 b 里找 188 字节对齐的 TS 同步头（连续 3 包验证防误判），
// 返回伪装头长度；找不到返回 -1。调用方保证 b[0] 不是合法 TS 头。
func liveTSSyncOffset(b []byte) int {
	for i := 1; i+188*2 < len(b); i++ {
		if b[i] == 0x47 && b[i+188] == 0x47 && b[i+376] == 0x47 {
			return i
		}
	}
	return -1
}

func isM3U8Response(resp *http.Response, target *url.URL) bool {
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "mpegurl") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(target.Path), ".m3u8")
}

// trimBOM 去掉 UTF-8 BOM：个别源站的播放列表开头带 BOM，
// 不处理的话既认不出 #EXTM3U，还会把这一行当成相对路径改写掉。
func trimBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
}

// rewriteM3U8 把播放列表里的所有 URI 换成代理地址。
// base 用上游「最终」URL（跟随重定向之后），相对路径才能解析正确。
func rewriteM3U8(body []byte, base *url.URL) []byte {
	lines := strings.Split(string(body), "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if strings.Contains(line, `URI="`) {
				lines[i] = liveURIPat.ReplaceAllStringFunc(line, func(m string) string {
					sub := m[len(`URI="`) : len(m)-1]
					return `URI="` + liveProxyURL(base, sub) + `"`
				})
			}
			continue
		}
		lines[i] = liveProxyURL(base, line)
	}
	return []byte(strings.Join(lines, "\n"))
}

// liveProxyURL 相对/绝对地址 → /api/live?u=...
func liveProxyURL(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "/api/live?") {
		return ref
	}
	u, err := base.Parse(ref)
	if err != nil {
		return ref
	}
	return "/api/live?u=" + url.QueryEscape(u.String())
}

// liveHostAllowed 只挡本机/私网 IP 字面量与内网域名。
// 域名不做 DNS 解析：既省掉每个分片一次解析，也避免 NAS 没配 DNS 时整站播不了。
func liveHostAllowed(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "" {
		return false
	}
	switch h {
	case "localhost", "localhost.localdomain", "ip6-localhost", "ip6-loopback":
		return false
	}
	for _, suf := range []string{".local", ".internal", ".localhost", ".lan", ".home"} {
		if strings.HasSuffix(h, suf) {
			return false
		}
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	return true
}

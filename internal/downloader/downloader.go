// Package downloader 负责 m3u8 视频的并发分片下载与本地合并。
//
// 产出统一为 mp4（需求：不允许输出 ts）。合并策略：
//  1. 每个 m3u8 的分片并发下载到固定目录 <dir>/segs（已存在则跳过 → 支持暂停续传）
//  2. 多集时把所有分片按顺序汇总成一份 concat 清单，由内置 ffmpeg 一次性转封装成 mp4
//     （不能用 mp4 拼 mp4：ffmpeg 的 concat 对多份 mp4 会报 duplicated MOOV 并产出空文件）
//  3. 直链 mp4 先落地再转 ts，与其它集一起合并
package downloader

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"videoDownLoad/internal/logx"
	"videoDownLoad/internal/sysinfo"
)

var (
	httpClient = &http.Client{
		Timeout: 60 * time.Second,
		// 共享连接池：默认 Transport 的 MaxIdleConnsPerHost=2，16 并发分片下载时
		// 绝大多数连接无法复用，每个分片都要重新 TCP+TLS 握手 —— J4125 上握手
		// 的 AES 计算正是下载期 CPU 的大头。这里显式放大每主机空闲连接数。
		Transport: &http.Transport{
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
	ffmpegBin string

	// 转码参数：弱 CPU 的 NAS 上非常重要（详见 SetTranscodeOptions）
	ffThreads         = 0    // ffmpeg -threads，0=用默认（通常是全部核心）
	allowFullReencode = true // 允许"全量重编码视频"兜底（libx264 会把弱 CPU 打满数小时）
	enableSyncVerify  = true // 合并后是否跑音画同步自检（每次多起 2 个 ffmpeg 进程）

	// segBudget 全局分片下载闸门：多个下载任务共享，避免"2 任务 × 16 并发"瞬间打满内存
	segBudget chan struct{}
)

// SetTranscodeOptions 由 main 按机器规格设置 ffmpeg 行为。
//
//	threads          给 ffmpeg 传 -threads（NAS 上建议 1~2，避免转码把整机占满）
//	allowFullReencode 是否允许音画仍不同步时把视频全量重编码为 libx264
//	enableVerify     是否在合并后跑音画同步自检
func SetTranscodeOptions(threads int, allowFullReencodeV, enableVerify bool) {
	if threads > 0 {
		ffThreads = threads
	}
	allowFullReencode = allowFullReencodeV
	enableSyncVerify = enableVerify
}

// SetSegmentBudget 设置全局同时下载的分片数上限（跨任务共享）
func SetSegmentBudget(n int) {
	if n < 1 {
		n = 1
	}
	segBudget = make(chan struct{}, n)
}

func acquireSeg() func() {
	if segBudget == nil {
		return func() {}
	}
	segBudget <- struct{}{}
	return func() { <-segBudget }
}

// tailBuf 只保留最后 lim 字节的 io.Writer，用来承接 ffmpeg stderr。
// 旧的 bytes.Buffer 无上限：长片合并时 ffmpeg 会刷大量 "Non-monotonous DTS" 警告，
// 累积到几十 MB 很常见（NAS 4G 内存下这就是致命的一击）。这里只保留尾部，够报错即可。
type tailBuf struct {
	buf []byte
	lim int
}

func newTailBuf(lim int) *tailBuf { return &tailBuf{lim: lim} }

func (t *tailBuf) Write(p []byte) (int, error) {
	n := len(p)
	if t.lim <= 0 {
		return n, nil
	}
	if n >= t.lim {
		t.buf = append(t.buf[:0], p[n-t.lim:]...)
		return n, nil
	}
	if len(t.buf)+n > t.lim {
		drop := len(t.buf) + n - t.lim
		copy(t.buf, t.buf[drop:])
		t.buf = t.buf[:len(t.buf)-drop]
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuf) String() string { return string(t.buf) }

// ErrCanceled 用户暂停/删除时返回，调用方据此保留已下载部分以便续传
var ErrCanceled = errors.New("下载已取消")

// Init 注入 ffmpeg 可执行文件路径（内置解压后的路径或系统路径）
func Init(ffmpegPath string) { ffmpegBin = ffmpegPath }

// FFmpeg 返回当前可用的 ffmpeg 路径
func FFmpeg() string { return ffmpegBin }

// threadArgs 给 ffmpeg 传 -threads（弱 CPU 的 NAS 上限制在 1~2，
// 否则一次合并/重编码就把整机核心占满，页面和其它任务全部卡住）
func threadArgs() []string {
	if ffThreads > 0 {
		return []string{"-threads", strconv.Itoa(ffThreads)}
	}
	return nil
}

const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0 Safari/537.36"

// ---------------------------------------------------------------- m3u8 解析

// keyInfo 一条 EXT-X-KEY。同一个分片可能同时挂多条 KEY（不同 KEYFORMAT），
// 下载时优先取 identity 那条；只有 DRM 密钥时明确报错，绝不放行乱码分片。
type keyInfo struct {
	Method    string
	URI       string
	IV        string
	KeyFormat string
	Inline    []byte // 密钥直接以内联 base64 写在 URI 位置
}

// segInfo 一个分片：地址 + 该分片的密钥集合 + 可选字节范围（EXT-X-BYTERANGE）
type segInfo struct {
	URL   string
	Keys  []*keyInfo
	Start int64 // -1 表示整文件
	Len   int64 // >0 时按字节范围请求
	DRM   bool  // 只有 SAMPLE-AES / 非 identity 密钥，无法解密
}

// variant 主播放列表里的一个码率变体
type variant struct {
	URL       string
	Bandwidth int
	Width     int
	Height    int
	AudioGrp  string
}

type playlist struct {
	Segments   []segInfo
	MediaSeq   uint64 // EXT-X-MEDIA-SEQUENCE：未显式给 IV 时分片的 IV 基准
	MapURL     string
	MapStart   int64 // EXT-X-MAP 的字节范围（fMP4 单文件分片场景）
	MapLen     int64
	IsMaster   bool
	Variants   []variant
	AudioURL   string  // EXT-X-MEDIA 独立音频轨（音视频分离的源，不下载会得到无声视频）
	HasDiscont bool    // 存在 EXT-X-DISCONTINUITY（片头/广告插入点，两侧时间戳不连续）
	Duration   float64 // 所有 EXTINF 之和，下载前估算体积用
	Bandwidth  int     // 选中变体的码率（bps）
	Base       string  // 播放列表自身地址，用于推导 Referer
}

// pickKey 从某个分片挂的多条 KEY 里挑出能解的那条：
// 只认 KEYFORMAT 为空/identity 且不是 SAMPLE-AES（那是 DRM，整段解密会产出乱码）。
func pickKey(keys []*keyInfo) *keyInfo {
	for _, k := range keys {
		if k == nil || k.Method == "" {
			continue
		}
		if k.KeyFormat != "" && !strings.EqualFold(k.KeyFormat, "identity") {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(k.Method), "SAMPLE-AES") {
			continue
		}
		return k
	}
	return nil
}

// pickVariant 多码率时选最高画质（下载场景优先画质，不看第一个）：
// 先比 BANDWIDTH，再比分辨率像素数。
func pickVariant(vs []variant) variant {
	best := vs[0]
	for _, v := range vs[1:] {
		if v.Bandwidth > best.Bandwidth ||
			(v.Bandwidth == best.Bandwidth && v.Width*v.Height > best.Width*best.Height) {
			best = v
		}
	}
	return best
}

var attrRe = regexp.MustCompile(`([A-Z0-9\-]+)=("[^"]*"|[^,]*)`)

func parseAttrs(line string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(line, -1) {
		out[m[1]] = strings.Trim(m[2], `"`)
	}
	return out
}

func resolveURL(base, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func parseM3U8(base, body string) *playlist {
	pl := &playlist{MapStart: -1, Base: base}
	var curKeys []*keyInfo
	audioByGroup := map[string]string{}
	var pendingVariant *variant
	pendingStart, pendingLen := int64(-1), int64(0)
	lastEnd := int64(-1)

	// EXT-X-BYTERANGE 语法：<长度>[@<偏移>]，省略偏移时紧接上一段末尾
	setRange := func(v string) {
		v = strings.TrimSpace(v)
		idx := strings.Index(v, "@")
		head := v
		if idx >= 0 {
			head = strings.TrimSpace(v[:idx])
		}
		n, err := strconv.ParseInt(head, 10, 64)
		if err != nil || n <= 0 {
			return
		}
		pendingLen = n
		if idx >= 0 {
			if o, e2 := strconv.ParseInt(strings.TrimSpace(v[idx+1:]), 10, 64); e2 == nil && o >= 0 {
				pendingStart = o
			}
		} else if lastEnd >= 0 {
			pendingStart = lastEnd
		} else {
			pendingStart = 0
		}
	}

	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "#EXT-X-STREAM-INF"):
			at := parseAttrs(line)
			v := variant{AudioGrp: at["AUDIO"]}
			v.Bandwidth, _ = strconv.Atoi(strings.TrimSpace(at["BANDWIDTH"]))
			if r := at["RESOLUTION"]; r != "" {
				if k := strings.IndexAny(r, "xX"); k > 0 {
					v.Width, _ = strconv.Atoi(strings.TrimSpace(r[:k]))
					v.Height, _ = strconv.Atoi(strings.TrimSpace(r[k+1:]))
				}
			}
			pendingVariant = &v
			pl.IsMaster = true
		case strings.HasPrefix(up, "#EXT-X-MEDIA-SEQUENCE"): // 必须排在 "#EXT-X-MEDIA" 之前（前缀包含关系）
			if n := strings.Index(line, ":"); n >= 0 {
				if v, err := strconv.ParseUint(strings.TrimSpace(line[n+1:]), 10, 64); err == nil {
					pl.MediaSeq = v
				}
			}
		case strings.HasPrefix(up, "#EXT-X-MEDIA"):
			// 音视频分离的源：音频在独立 m3u8 里，不处理会得到无声视频
			at := parseAttrs(line)
			if strings.EqualFold(at["TYPE"], "AUDIO") && at["URI"] != "" {
				u := resolveURL(base, at["URI"])
				if g := at["GROUP-ID"]; g != "" {
					if _, ok := audioByGroup[g]; !ok {
						audioByGroup[g] = u
					}
				}
				if pl.AudioURL == "" {
					pl.AudioURL = u
				}
			}
		case strings.HasPrefix(up, "#EXT-X-KEY"):
			at := parseAttrs(line)
			m := strings.ToUpper(strings.TrimSpace(at["METHOD"]))
			if m == "" || m == "NONE" {
				curKeys = nil // 密钥段结束，其后分片为明文
				continue
			}
			k := &keyInfo{Method: m, IV: at["IV"], KeyFormat: at["KEYFORMAT"]}
			if raw := strings.TrimSpace(at["URI"]); raw != "" {
				// URI 位置也可能是 base64 内联密钥（部分源这么写）
				if b, err := base64.StdEncoding.DecodeString(raw); err == nil &&
					(len(b) == 16 || len(b) == 24 || len(b) == 32) {
					k.Inline = b
				} else {
					k.URI = resolveURL(base, raw)
				}
			}
			curKeys = append(curKeys, k)
		case strings.HasPrefix(up, "#EXT-X-DISCONTINUITY"):
			pl.HasDiscont = true
		case strings.HasPrefix(up, "#EXT-X-MAP"):
			at := parseAttrs(line)
			if at["URI"] != "" {
				pl.MapURL = resolveURL(base, at["URI"])
			}
			if br := at["BYTERANGE"]; br != "" {
				saveS, saveL := pendingStart, pendingLen
				setRange(br)
				pl.MapStart, pl.MapLen = pendingStart, pendingLen
				if pl.MapStart >= 0 && pl.MapLen > 0 {
					lastEnd = pl.MapStart + pl.MapLen
				}
				pendingStart, pendingLen = saveS, saveL
			}
		case strings.HasPrefix(up, "#EXTINF"):
			if n := strings.Index(line, ":"); n >= 0 {
				d := line[n+1:]
				if c := strings.Index(d, ","); c >= 0 {
					d = d[:c]
				}
				if f, err := strconv.ParseFloat(strings.TrimSpace(d), 64); err == nil && f > 0 {
					pl.Duration += f
				}
			}
		case strings.HasPrefix(up, "#EXT-X-BYTERANGE"):
			if n := strings.Index(line, ":"); n >= 0 {
				setRange(line[n+1:])
			}
		case strings.HasPrefix(line, "#"):
			// 其余标签（VERSION/TARGETDURATION/ENDLIST 等）不影响下载
		default:
			u := resolveURL(base, line)
			if pendingVariant != nil {
				pendingVariant.URL = u
				pl.Variants = append(pl.Variants, *pendingVariant)
				pendingVariant = nil
				continue
			}
			sg := segInfo{URL: u, Keys: curKeys, Start: pendingStart, Len: pendingLen}
			if sg.Start >= 0 && sg.Len > 0 {
				lastEnd = sg.Start + sg.Len
			}
			sg.DRM = len(sg.Keys) > 0 && pickKey(sg.Keys) == nil
			pl.Segments = append(pl.Segments, sg)
			pendingStart, pendingLen = -1, 0
		}
	}

	// 主列表：选最高码率，并把对应音频组带出来
	if pl.IsMaster && len(pl.Variants) > 0 {
		best := pickVariant(pl.Variants)
		pl.Bandwidth = best.Bandwidth
		if u, ok := audioByGroup[best.AudioGrp]; ok {
			pl.AudioURL = u
		}
	}
	return pl
}

// ---------------------------------------------------------------- 对外接口

// FetchResult 一次分片下载的结果
type FetchResult struct {
	Segments   []string // 有序分片（视频或音视频混合流）
	AudioFile  string   // EXT-X-MEDIA 独立音频轨合并成的 ts（音视频未分离时为空）
	ForceReenc bool     // 播放列表存在 DISCONTINUITY / 双轨等风险，合并时应跳过纯 copy 直接校正时间轴
	Duration   float64  // 正片总时长（秒），合并阶段用来把 ffmpeg 的进度换算成百分比
}

// FetchSegments 下载某个播放地址的全部分片到 dir/segs，返回有序分片路径。
// 已存在的分片会跳过（暂停后续传）；mp4 直链会先落地再转封装为 ts；
// 源为音视频分离（EXT-X-MEDIA）时会额外下载音频轨并合并成单个 ts。
func FetchSegments(playURL, dir string, concurrency int, onProgress func(int), onSpeed func(string), isCanceled func() bool) (*FetchResult, error) {
	if concurrency <= 0 {
		concurrency = 8
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	check := func() bool { return isCanceled != nil && isCanceled() }
	if check() {
		return nil, ErrCanceled
	}
	if onProgress != nil {
		onProgress(1)
	}

	// 非 m3u8（mp4/flv 直链）：下到本地后转成 ts，便于与其它集一起合并
	if !strings.Contains(playURL, ".m3u8") {
		raw := filepath.Join(dir, "raw.mp4")
		if _, err := os.Stat(raw); err != nil {
			if err := downloadFile(playURL, raw, onProgress, onSpeed, check); err != nil {
				return nil, err
			}
		}
		ts := filepath.Join(dir, "raw.ts")
		if _, err := os.Stat(ts); err != nil {
			if ffmpegBin == "" {
				return nil, fmt.Errorf("内置 ffmpeg 不可用，无法处理直链视频")
			}
			cmd := exec.Command(ffmpegBin, "-y", "-i", raw, "-c", "copy", "-bsf:a", "aac_adtstoasc", ts)
			eb := newTailBuf(32 << 10)
			cmd.Stderr = eb
			if err := cmd.Run(); err != nil {
				return nil, fmt.Errorf("转封装失败: %v | %s", err, tail(eb.String(), 200))
			}
		}
		os.Remove(raw)
		if onProgress != nil {
			onProgress(100)
		}
		return &FetchResult{Segments: []string{ts}}, nil
	}

	body := fetchTextRef(playURL, "")
	if body == "" {
		return nil, fmt.Errorf("获取 m3u8 失败: %s", playURL)
	}
	pl := parseM3U8(playURL, body)
	// 主播放列表（多码率）：选最高画质那个变体再解析。
	// 旧实现取第一个变体，而绝大多数源把最低码率放在第一个，下载下来画质最差。
	if pl.IsMaster {
		if len(pl.Variants) == 0 {
			return nil, fmt.Errorf("主播放列表中没有可用的码率变体")
		}
		best := pickVariant(pl.Variants)
		masterAudio := pl.AudioURL
		sub := fetchTextRef(best.URL, originOf(playURL))
		if sub == "" {
			return nil, fmt.Errorf("获取子 m3u8 失败: %s", best.URL)
		}
		pl = parseM3U8(best.URL, sub)
		pl.Bandwidth = best.Bandwidth
		if pl.AudioURL == "" {
			pl.AudioURL = masterAudio
		}
		logx.Printf("[downloader] 多码率源，选用 %dx%d / %dkbps 变体",
			best.Width, best.Height, best.Bandwidth/1000)
	}
	if len(pl.Segments) == 0 {
		return nil, fmt.Errorf("m3u8 中未解析到分片")
	}
	if drm := countDRM(pl); drm > 0 {
		return nil, fmt.Errorf("该视频有 %d/%d 个分片使用 SAMPLE-AES 等 DRM 加密，无法解密下载", drm, len(pl.Segments))
	}
	// 下载前空间预检：知道总时长与码率时能较准确估出体积，磁盘不够就立即报错，
	// 避免下了几百个分片才因为 ENOSPC 失败（分片文件还占着空间）
	if err := precheckSpace(dir, pl); err != nil {
		return nil, err
	}
	segs, err := downloadSegments(pl, dir, concurrency, onProgress, onSpeed, check)
	if err != nil {
		return nil, err
	}
	res := &FetchResult{Segments: segs, ForceReenc: pl.HasDiscont, Duration: pl.Duration}
	// 音视频分离的源：再下载独立音频轨。双轨合并必须对齐时间轴，强制走音频重编码校正
	if pl.AudioURL != "" {
		audioTS, aerr := fetchAudioTrack(pl.AudioURL, dir, concurrency, check)
		if aerr != nil {
			logx.Printf("[downloader] 独立音频轨下载失败（输出将只有画面）: %v", aerr)
		} else {
			res.AudioFile = audioTS
			res.ForceReenc = true
		}
	}
	return res, nil
}

// fetchAudioTrack 下载 EXT-X-MEDIA 独立音频轨的全部分片并合并成单个 ts。
// 直接重编码为 AAC：一方面归一化各分片时间轴（消除分片间戳不连续），
// 另一方面兼容 .aac/.ts/fMP4 等分片封装差异，避免 concat demuxer 混格式失败。
func fetchAudioTrack(audioURL, dir string, concurrency int, check func() bool) (string, error) {
	if ffmpegBin == "" {
		return "", fmt.Errorf("内置 ffmpeg 不可用，无法合并独立音频轨")
	}
	body := fetchTextRef(audioURL, "")
	if body == "" {
		return "", fmt.Errorf("获取音频 m3u8 失败: %s", audioURL)
	}
	pl := parseM3U8(audioURL, body)
	if pl.IsMaster && len(pl.Variants) > 0 {
		best := pickVariant(pl.Variants)
		sub := fetchTextRef(best.URL, originOf(audioURL))
		if sub == "" {
			return "", fmt.Errorf("获取音频子 m3u8 失败: %s", best.URL)
		}
		pl = parseM3U8(best.URL, sub)
	}
	if len(pl.Segments) == 0 {
		return "", fmt.Errorf("音频 m3u8 中未解析到分片")
	}
	segs, err := downloadSegments(pl, filepath.Join(dir, "audio"), concurrency, nil, nil, check)
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "audio.ts")
	if fi, err := os.Stat(out); err == nil && fi.Size() > 1024 {
		return out, nil // 续传：已合并过
	}
	listFile, err := writeConcatList(segs, filepath.Join(dir, "audio"))
	if err != nil {
		return "", err
	}
	defer os.Remove(listFile)
	audioArgs := []string{"-y", "-fflags", "+genpts",
		"-f", "concat", "-safe", "0", "-i", listFile, "-vn"}
	audioArgs = append(audioArgs, threadArgs()...)
	audioArgs = append(audioArgs,
		"-c:a", "aac", "-b:a", "192k", "-af", "aresample=async=1:first_pts=0", out)
	cmd := exec.Command(ffmpegBin, audioArgs...)
	eb := newTailBuf(32 << 10)
	cmd.Stderr = eb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("音频轨合并失败: %v | %s", err, tail(eb.String(), 200))
	}
	return out, nil
}

// writeConcatList 生成 ffmpeg concat 清单（绝对路径 + 正斜杠）
func writeConcatList(segments []string, dir string) (string, error) {
	absSegs := make([]string, 0, len(segments))
	for _, p := range segments {
		ap, aerr := filepath.Abs(p)
		if aerr != nil {
			ap = p
		}
		absSegs = append(absSegs, ap)
	}
	listFile := filepath.Join(dir,
		fmt.Sprintf(".concat_%d_%d.txt", os.Getpid(), time.Now().UnixNano()))
	var b strings.Builder
	for _, p := range absSegs {
		// 必须正斜杠：Windows 下反斜杠路径会让 ffmpeg concat 静默产出空文件
		b.WriteString(fmt.Sprintf("file '%s'\n", strings.ReplaceAll(filepath.ToSlash(p), "'", `'\''`)))
	}
	if err := os.WriteFile(listFile, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return listFile, nil
}

// DownloadToMP4 下载单个播放地址（m3u8 或 mp4 直链）并合并为一个 mp4。
// 分片目录固定为 <out>.segs：暂停后再次下载复用已下好的分片（断点续传）。
// Options 一次下载的进度回调集合。
//
// 下载与合并是两个阶段，各自从 0 走到 100：OnProgress 会被两个阶段复用，
// OnPhase 用来告诉上层"现在进入合并了"（前端据此把状态从"下载中"切成"合并中"）。
type Options struct {
	Concurrency int
	OnProgress  func(int)    // 0-100
	OnSpeed     func(string) // 下载速度，合并阶段不再上报
	OnPhase     func(string) // "download" / "merge"
	IsCanceled  func() bool
}

// DownloadToMP4 下载单个播放地址（m3u8 或 mp4 直链）并合并为一个 mp4。
// 分片目录固定为 <out>.segs：暂停后再次下载复用已下好的分片（断点续传）。
func DownloadToMP4(playURL, outPath string, opt Options) error {
	conc := opt.Concurrency
	if conc <= 0 {
		conc = 8
	}
	if opt.OnPhase != nil {
		opt.OnPhase("download")
	}
	res, err := FetchSegments(playURL, outPath+".segs", conc, opt.OnProgress, opt.OnSpeed, opt.IsCanceled)
	if err != nil {
		return err // ErrCanceled 时保留分片目录，续传复用
	}
	// 下载完 → 合并。ffmpeg 合并一个 2GB 的片子要几十秒，
	// 这段时间旧实现里进度停在 100%、状态还是"下载中"，看起来像卡死。
	if opt.OnPhase != nil {
		opt.OnPhase("merge")
	}
	if err := ConcatToMP4(res.Segments, outPath, res.AudioFile, res.ForceReenc, res.Duration, opt.OnProgress); err != nil {
		return err
	}
	os.RemoveAll(outPath + ".segs")
	return nil
}

// ConcatToMP4 把所有 ts 分片按顺序合并成一个 mp4（一次 ffmpeg 转封装）。
// audioFile 非空表示 EXT-X-MEDIA 独立音频轨，与视频流混流并对齐时间轴。
// forceReenc 为 true（DISCONTINUITY / 双轨）时跳过纯 copy，直接做音频重编码校正。
//
// 音画不同步的根因与对策（分级处理，能 copy 就 copy，只对确有风险的流付出重编码成本）：
//  1. TS 分片 AAC 带编码器 priming、各片 DTS 不连续 → copy 到 mp4 会累积固定/渐进错位
//     → 音频重编码 + aresample=async=1:first_pts=0 校正
//  2. ffmpeg 未报警告的隐性错位 → 合并后用 verifySync 实测音画首帧 PTS 差，超阈值自动重做
//  3. 视频时间轴本身损坏（DISCONTINUITY 两侧跳变）→ 最后兜底全量重编码
func ConcatToMP4(segments []string, out, audioFile string, forceReenc bool, totalDur float64, onPct func(int)) error {
	if len(segments) == 0 {
		return fmt.Errorf("没有可合并的分片")
	}
	if ffmpegBin == "" {
		return fmt.Errorf("内置 ffmpeg 不可用，无法合并为 mp4")
	}
	if abs, aerr := filepath.Abs(out); aerr == nil {
		out = abs // ffmpeg 的输出也用绝对路径，避免受调用方 CWD 影响
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	listDir, _ := filepath.Abs(filepath.Dir(segments[0]))
	listFile, err := writeConcatList(segments, listDir)
	if err != nil {
		return err
	}
	defer os.Remove(listFile)

	run := func(codecArgs []string, extra ...string) (string, error) {
		args := []string{"-y", "-fflags", "+genpts", "-f", "concat", "-safe", "0", "-i", listFile}
		if audioFile != "" {
			args = append(args, "-i", audioFile, "-map", "0:v:0", "-map", "1:a:0")
		}
		args = append(args, threadArgs()...)
		args = append(args, codecArgs...)
		args = append(args, extra...)
		args = append(args, "-progress", "pipe:1", "-nostats", out)
		if onPct != nil {
			onPct(0) // 每一轮尝试都从 0 开始（copy 失败会退到音频重编码，进度要重置）
		}
		stderr, runErr := runFFmpegProgress(args, totalDur, onPct)
		if runErr != nil {
			return stderr, fmt.Errorf("%v | %s", runErr, tail(stderr, 400))
		}
		if fi, serr := os.Stat(out); serr != nil || fi.Size() < 1024 {
			return stderr, fmt.Errorf("ffmpeg 产出为空: %s", tail(stderr, 400))
		}
		if onPct != nil {
			onPct(99) // 后面还有音画自检，先停在 99
		}
		return stderr, nil
	}

	reencArgs := []string{"-c:v", "copy", "-c:a", "aac", "-b:a", "192k",
		"-af", "aresample=async=1:first_pts=0"}
	reencExtra := []string{"-avoid_negative_ts", "make_zero", "-movflags", "+faststart"}
	if audioFile != "" {
		reencExtra = append(reencExtra, "-shortest")
	}

	// 无风险标记时先试纯 copy（最快、无损），但合并后必须过音画自检
	if audioFile == "" && !forceReenc {
		copyArgs := []string{"-c", "copy", "-bsf:a", "aac_adtstoasc"}
		stderr1, err := run(copyArgs, "-avoid_negative_ts", "make_zero", "-movflags", "+faststart")
		if err == nil && !hasDTSIssue(stderr1) {
			if !enableSyncVerify || verifySync(out) == nil {
				return nil
			}
			logx.Printf("[downloader] 合并产物音画自检未通过，改用音频重编码校正: %s", filepath.Base(out))
		}
	}

	// 音频重编码校正（视频照旧 copy，速度快）：修复 priming/DTS 不连续/双轨时间轴错位
	if _, err2 := run(reencArgs, reencExtra...); err2 == nil {
		if !enableSyncVerify || verifySync(out) == nil {
			if forceReenc || audioFile != "" {
				logx.Printf("[downloader] 时间轴风险源（discontinuity=%v, 双轨=%v），已重编码音频校正: %s",
					forceReenc, audioFile != "", filepath.Base(out))
			}
			return nil
		}
		// 音频校正后仍不同步 → 视频时间轴本身损坏，全量重编码兜底。
		// NAS/弱 CPU 上这一步可能要跑掉几个小时的全部核心，所以可关闭（保留音频校正版本）。
		if !allowFullReencode {
			logx.Printf("[downloader] 音画仍有偏差，但已禁用全量重编码，保留音频校正版本: %s", filepath.Base(out))
			return nil
		}
		logx.Printf("[downloader] 音频校正后仍有偏差，全量重编码视频: %s", filepath.Base(out))
		full := []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "20",
			"-c:a", "aac", "-b:a", "192k", "-af", "aresample=async=1:first_pts=0"}
		if _, err4 := run(full, reencExtra...); err4 == nil {
			return nil
		}
		logx.Printf("[downloader] 全量重编码失败，保留音频校正版本（可播，可能有轻微错位）")
		return nil
	}

	// 没有音轨或重编码不可用：退回纯 copy（至少保证能播）
	if _, err3 := run([]string{"-c", "copy", "-bsf:a", "aac_adtstoasc"}, "-movflags", "+faststart"); err3 != nil {
		os.Remove(out)
		return fmt.Errorf("合并失败（重编码: %v）", err3)
	}
	return nil
}

// runFFmpegProgress 执行 ffmpeg 并解析 -progress 输出，换算成 0-100 的百分比。
//
//   - out_time_us / out_time_ms 都是微秒（ffmpeg 历史原因，ms 实际也是 us）
//   - totalDur<=0（直链 mp4、列表没写 EXTINF）时无法换算，只上报 0/99 两个点
//
// stdout 被 -progress 占用，ffmpeg 的日志仍在 stderr，所以不会互相干扰。
func runFFmpegProgress(args []string, totalDur float64, onPct func(int)) (string, error) {
	cmd := exec.Command(ffmpegBin, args...)
	stdout, perr := cmd.StdoutPipe()
	if perr != nil {
		return "", perr
	}
	ebuf := newTailBuf(128 << 10)
	cmd.Stderr = ebuf
	if err := cmd.Start(); err != nil {
		return ebuf.String(), err
	}
	// 防卡死：ffmpeg 正常合并会每 0.5 秒左右吐一行 progress，
	// 连续 90 秒没有任何输出 = 进程卡死（NAS 上 IO 挂起、坏分片、网络盘掉线都可能），
	// 直接 kill，让任务落到 failed，用户点「继续」即可重来，不会永远挂在"合并中"。
	const stallLimit = 90 * time.Second
	// 总耗时上限：正常合并（copy）约为片长的 1/10，音频重编码更慢，
	// 给到「片长 × 2 + 3 小时」足够宽松，只用来兜底死循环。
	maxTotal := 3*time.Hour + time.Duration(totalDur)*time.Second

	var lastAct atomic.Int64
	lastAct.Store(time.Now().UnixNano())
	var killMsg atomic.Value
	watchDone := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		start := time.Now()
		for {
			select {
			case <-watchDone:
				return
			case <-t.C:
				if time.Since(start) > maxTotal {
					killMsg.Store("合并超过 " + maxTotal.Round(time.Minute).String() + "，已强制结束")
					_ = cmd.Process.Kill()
					return
				}
				if time.Since(time.Unix(0, lastAct.Load())) > stallLimit {
					killMsg.Store("合并过程 90 秒无任何进展，判定卡死并已强制结束")
					_ = cmd.Process.Kill()
					return
				}
			}
		}
	}()
	defer close(watchDone)

	last := -1
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lastAct.Store(time.Now().UnixNano())
		line := sc.Text()
		if onPct == nil || totalDur <= 0 {
			continue
		}
		if !strings.HasPrefix(line, "out_time_us=") && !strings.HasPrefix(line, "out_time_ms=") {
			continue
		}
		i := strings.Index(line, "=")
		if i < 0 {
			continue
		}
		us, err := strconv.ParseInt(strings.TrimSpace(line[i+1:]), 10, 64)
		if err != nil || us <= 0 {
			continue
		}
		pct := int(float64(us) / (totalDur * 1e6) * 100)
		if pct > 99 {
			pct = 99
		}
		if pct > last {
			last = pct
			onPct(pct)
		}
	}
	waitErr := cmd.Wait()
	if v := killMsg.Load(); v != nil {
		return ebuf.String(), fmt.Errorf("%v", v)
	}
	return ebuf.String(), waitErr
}

// verifySync 实测合并产物的音画起点偏差（内置 ffmpeg 无 ffprobe，
// 用 showinfo/ashowinfo 打印前 3 秒首帧 pts_time 对比）。
// 无音轨时视为通过；|视频首帧PTS - 音频首帧PTS| > 0.15s 判定为可感知错位。
func verifySync(out string) error {
	firstPTS := func(mapSel, filterOpt, filter, tag string) (float64, bool) {
		vargs := append([]string{"-hide_banner", "-t", "3", "-i", out, "-map", mapSel, filterOpt, filter},
			threadArgs()...)
		vargs = append(vargs, "-f", "null", "-")
		cmd := exec.Command(ffmpegBin, vargs...)
		ebuf := newTailBuf(64 << 10)
		cmd.Stderr = ebuf
		_ = cmd.Run() // -t 截断导致非零退出属正常，只解析输出
		for _, line := range strings.Split(ebuf.String(), "\n") {
			if !strings.Contains(line, tag) {
				continue
			}
			if m := ptsTimeRe.FindStringSubmatch(line); m != nil {
				if v, perr := strconv.ParseFloat(m[1], 64); perr == nil {
					return v, true
				}
			}
		}
		return 0, false
	}
	vPts, hasV := firstPTS("0:v:0", "-vf", "showinfo", "Parsed_showinfo")
	aPts, hasA := firstPTS("0:a:0", "-af", "ashowinfo", "Parsed_ashowinfo")
	if !hasV || !hasA {
		return nil // 无音轨（或纯音频）无法对比，不视为错位
	}
	if d := math.Abs(vPts - aPts); d > 0.15 {
		return fmt.Errorf("音画起点偏差 %.3fs（视频 %.3fs / 音频 %.3fs）", d, vPts, aPts)
	}
	return nil
}

var ptsTimeRe = regexp.MustCompile(`pts_time:(-?[\d.]+)`)

// hasDTSIssue 判断 ffmpeg 输出里是否出现时间戳不连续（音画会因此错位）
func hasDTSIssue(s string) bool {
	for _, k := range []string{
		"Non-monotonous DTS", "Non-monotonic DTS", "non monotonically increasing dts",
		"Queue input is backward in time", "Audio timestamp",
		"timestamp discontinuity", "Past duration", "invalid, dropping",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 分片下载

func downloadSegments(pl *playlist, dir string, concurrency int, onProgress func(int), onSpeed func(string), check func() bool) ([]string, error) {
	segDir := filepath.Join(dir, "segs")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return nil, err
	}
	referer := originOf(pl.Base)

	keyCache := map[string][]byte{}
	var keyMu sync.Mutex
	getKey := func(k *keyInfo) ([]byte, error) {
		if len(k.Inline) > 0 {
			return k.Inline, nil // 密钥直接内联在播放列表里
		}
		if k.URI == "" {
			return nil, fmt.Errorf("EXT-X-KEY 缺少可用密钥")
		}
		keyMu.Lock()
		if kb, ok := keyCache[k.URI]; ok {
			keyMu.Unlock()
			return kb, nil
		}
		keyMu.Unlock()
		b := fetchBytesRef(k.URI, referer)
		if b == nil {
			return nil, fmt.Errorf("密钥下载失败: %s", k.URI)
		}
		kb, kerr := normalizeKey(b)
		if kerr != nil {
			return nil, fmt.Errorf("密钥无效（%s）: %v", k.URI, kerr)
		}
		keyMu.Lock()
		keyCache[k.URI] = kb
		keyMu.Unlock()
		return kb, nil
	}

	total := int64(len(pl.Segments))
	var done, failed, canceled, downloadedBytes atomic.Int64
	stopSpeed := make(chan struct{})
	if onSpeed != nil {
		go func() {
			last := int64(0)
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-stopSpeed:
					return
				case <-t.C:
					cur := downloadedBytes.Load()
					onSpeed(humanSize(cur-last) + "/s")
					last = cur
				}
			}
		}()
	}

	// 固定大小的 worker 池（旧实现给每个分片都起一个 goroutine，
	// 一部 2000 分片的电影就是 2000 个阻塞在信号量上的协程，白占栈内存与调度开销）
	type segJob struct{ idx int }
	jobs := make(chan segJob, concurrency)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var failedIdx []int
	firstErr := ""

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if check() {
					canceled.Add(1)
					done.Add(1)
					continue
				}
				dst := filepath.Join(segDir, fmt.Sprintf("%06d.ts", j.idx+1))
				n, err := fetchSegment(pl.Segments[j.idx], dst, pl, j.idx, getKey, referer)
				if err != nil {
					failed.Add(1)
					os.Remove(dst)
					errMu.Lock()
					failedIdx = append(failedIdx, j.idx)
					if firstErr == "" {
						firstErr = fmt.Sprintf("%s -> %v", pl.Segments[j.idx].URL, err)
					}
					errMu.Unlock()
				} else {
					downloadedBytes.Add(n)
				}
				d := done.Add(1)
				if onProgress != nil {
					onProgress(int(d * 100 / total))
				}
			}
		}()
	}
	go func() {
		for i := range pl.Segments {
			jobs <- segJob{i}
		}
		close(jobs)
	}()
	wg.Wait()
	if onSpeed != nil {
		close(stopSpeed)
	}
	if canceled.Load() > 0 || check() {
		return nil, ErrCanceled // 保留已下好的分片，续传时复用
	}

	// 失败分片串行补扫：并发下载时偶发超时/断连很常见，
	// 因为个别分片让整条线路前功尽弃代价太大（已下好的分片也白下）
	if n := failed.Load(); n > 0 {
		errMu.Lock()
		list := append([]int(nil), failedIdx...)
		errMu.Unlock()
		logx.Printf("[downloader] %d/%d 个分片失败，开始串行补下", n, total)
		retried := 0
		for _, idx := range list {
			if check() {
				break
			}
			dst := filepath.Join(segDir, fmt.Sprintf("%06d.ts", idx+1))
			if _, err := fetchSegment(pl.Segments[idx], dst, pl, idx, getKey, referer); err == nil {
				failed.Add(-1)
				retried++
			}
		}
		if retried > 0 {
			logx.Printf("[downloader] 补下成功 %d/%d 个分片", retried, len(list))
		}
	}
	if f := failed.Load(); f > 0 {
		return nil, fmt.Errorf("%d/%d 个分片下载失败（首个错误: %s）", f, total, firstErr)
	}

	// EXT-X-MAP 初始化段（fMP4）放最前
	var segs []string
	if pl.MapURL != "" {
		init := filepath.Join(segDir, "000000_init.mp4")
		if fi, err := os.Stat(init); err != nil || fi.Size() == 0 {
			if err := downloadFileRange(pl.MapURL, init, referer, pl.MapStart, pl.MapLen); err != nil {
				return nil, fmt.Errorf("初始化段下载失败: %w", err)
			}
		}
		segs = append(segs, init)
	}
	for i := 1; i <= len(pl.Segments); i++ {
		segs = append(segs, filepath.Join(segDir, fmt.Sprintf("%06d.ts", i)))
	}
	return segs, nil
}

func fetchSegment(sg segInfo, dst string, pl *playlist, idx int, getKey func(*keyInfo) ([]byte, error), referer string) (int64, error) {
	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		return 0, nil // 断点续传：已下好的分片直接跳过
	}
	if sg.DRM {
		return 0, fmt.Errorf("分片使用 SAMPLE-AES / 非 identity 密钥加密（DRM），无法解密")
	}
	key := pickKey(sg.Keys)

	release := acquireSeg()
	defer release()

	// 无加密且无字节范围：边下边写盘，不把整片读进内存。
	// 旧实现每片都 io.ReadAll（10MB 分片 × 16 并发 = 160MB 常驻 + 一次解密拷贝），
	// 4G 内存的 NAS 上就是 OOM 的直接来源。
	if key == nil && sg.Len <= 0 {
		if n, done, err := fetchSegmentStream(sg.URL, dst, referer); done {
			return n, err
		}
		// 落到内存路径（伪装头/异常格式/字节范围），极少数
	}

	data, err := fetchSegmentBytes(sg, referer)
	if err != nil {
		return 0, err
	}
	if key != nil {
		if !strings.HasPrefix(strings.ToUpper(key.Method), "AES-") {
			return 0, fmt.Errorf("不支持的加密方式 %s", key.Method)
		}
		kb, kerr := getKey(key)
		if kerr != nil {
			return 0, kerr
		}
		var iv []byte
		if key.IV != "" {
			iv = parseHexIV(key.IV)
		} else {
			// HLS 规定：未显式给 IV 时用该分片的媒体序号（EXT-X-MEDIA-SEQUENCE + 下标）。
			// 之前误用纯下标 idx：MEDIA-SEQUENCE 非 0 的源会解出乱码分片，
			// 混进合并后 ffmpeg 丢包 → 音画错位
			iv = make([]byte, 16)
			binaryPutUint64(iv[8:], pl.MediaSeq+uint64(idx))
		}
		dec, derr := aesCBCDecrypt(kb, iv, data)
		if derr != nil {
			return 0, derr
		}
		data = dec
	}
	// 伪装格式还原：部分站把 TS 分片套上 PNG/JPG/BMP 等图片头防盗链，
	// 剥掉前缀、按 188 字节同步头对齐；fMP4 / ADTS AAC 分片原样放行。
	// 找不到同步头直接报错，不让坏分片混进合并（解码丢包 → 音画错位）。
	nd, nerr := normalizeSegment(data)
	if nerr != nil {
		return 0, nerr
	}
	if len(nd) != len(data) && idx == 0 {
		logx.Printf("[downloader] 检测到伪装分片（%s），已剥离 %d 字节伪装头",
			magicName(data), len(data)-len(nd))
	}
	data = nd
	if err := os.WriteFile(dst+".part", data, 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(dst+".part", dst); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

// fetchSegmentBytes 取一个分片的内容（含字节范围），带退避重试。
// EXT-X-BYTERANGE 的分片必须带 Range 头，否则会把整个文件当一段下回来。
func fetchSegmentBytes(sg segInfo, referer string) ([]byte, error) {
	const attempts = 4
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, rerr := newReq(sg.URL, referer, sg.Start, sg.Len)
		if rerr != nil {
			return nil, rerr
		}
		resp, derr := httpClient.Do(req)
		if derr != nil {
			lastErr = derr
			sleepBackoff(attempt, 0)
			continue
		}
		status := resp.StatusCode
		var b []byte
		var rerr2 error
		if status == http.StatusOK || status == http.StatusPartialContent {
			b, rerr2 = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		}
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		resp.Body.Close()

		switch {
		case status == http.StatusOK || status == http.StatusPartialContent:
			if rerr2 != nil || len(b) == 0 {
				lastErr = fmt.Errorf("读取分片失败: %v", rerr2)
				sleepBackoff(attempt, retryAfter)
				continue
			}
			// 服务器忽略 Range 直接返回整文件：自己按偏移切
			if sg.Len > 0 && status == http.StatusOK {
				if int64(len(b)) < sg.Start+sg.Len {
					return nil, fmt.Errorf("字节范围越界（返回 %d 字节，需要 %d-%d）",
						len(b), sg.Start, sg.Start+sg.Len-1)
				}
				b = b[sg.Start : sg.Start+sg.Len]
			}
			return b, nil
		case status == 404 || status == 403 || status == 410 || status == 416:
			return nil, fmt.Errorf("http %d（该分片不可用）", status)
		default:
			lastErr = fmt.Errorf("http %d", status)
			sleepBackoff(attempt, retryAfter)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("下载失败")
	}
	return nil, lastErr
}

// sleepBackoff 指数退避 + 抖动。抖动很重要：N 个协程同时失败会一起重试，
// 没有抖动就是自己对自己发起 DDoS，CDN 会继续限流。
func sleepBackoff(attempt int, retryAfter time.Duration) {
	d := time.Duration(400*(1<<uint(attempt-1))) * time.Millisecond // 400/800/1600/3200ms
	if d > 4*time.Second {
		d = 4 * time.Second
	}
	d += time.Duration(time.Now().UnixNano()%250) * time.Millisecond
	if retryAfter > 0 && retryAfter <= 10*time.Second {
		d = retryAfter // 429/503 明确要求等多久，优先听它的
	}
	time.Sleep(d)
}

// parseRetryAfter 解析 Retry-After（秒数形式；HTTP-date 形式忽略）
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 60 {
		return time.Duration(n) * time.Second
	}
	return 0
}

// fetchSegmentStream 无加密分片的流式落盘。
// done=true 表示已处理完毕（n 为写入字节数，err 为结果）；done=false 时调用方需回退到内存路径。
func fetchSegmentStream(u, dst, referer string) (n int64, done bool, err error) {
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		req, rerr := newReq(u, referer, -1, 0)
		if rerr != nil {
			return 0, true, rerr
		}
		resp, derr := httpClient.Do(req)
		if derr != nil {
			lastErr = derr
			sleepBackoff(attempt, 0)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			resp.Body.Close()
			lastErr = fmt.Errorf("http %d", resp.StatusCode)
			if resp.StatusCode == 404 || resp.StatusCode == 403 || resp.StatusCode == 410 {
				return 0, true, lastErr // 权限/不存在，重试无意义
			}
			sleepBackoff(attempt, retryAfter)
			continue
		}

		head := make([]byte, 512)
		hn, herr := io.ReadFull(resp.Body, head)
		if herr != nil && herr != io.ErrUnexpectedEOF && herr != io.EOF {
			resp.Body.Close()
			lastErr = fmt.Errorf("读取分片失败: %v", herr)
			sleepBackoff(attempt, 0)
			continue
		}
		head = head[:hn]
		if hn == 0 {
			resp.Body.Close()
			lastErr = fmt.Errorf("分片内容为空")
			sleepBackoff(attempt, 0)
			continue
		}
		// 头部不合法 → 交给内存路径做伪装头剥离，不要在这里猜
		if !looksPlainSegment(head) {
			resp.Body.Close()
			return 0, false, nil
		}

		f, ferr := os.Create(dst + ".part")
		if ferr != nil {
			resp.Body.Close()
			return 0, true, ferr
		}
		written := int64(0)
		if _, werr := f.Write(head); werr != nil {
			f.Close()
			resp.Body.Close()
			os.Remove(dst + ".part")
			return 0, true, werr
		}
		written += int64(len(head))
		// 128KB 拷贝缓冲：整条链路只有这一块内存，与分片大小无关
		cn, cerr := io.CopyBuffer(f, resp.Body, make([]byte, 128<<10))
		written += cn
		resp.Body.Close()
		if cerr == nil {
			cerr = f.Close()
		} else {
			f.Close()
		}
		if cerr != nil {
			os.Remove(dst + ".part")
			lastErr = fmt.Errorf("写入分片失败: %v", cerr)
			sleepBackoff(attempt, 0)
			continue
		}
		if err := os.Rename(dst+".part", dst); err != nil {
			return 0, true, err
		}
		return written, true, nil
	}
	return 0, true, lastErr
}

// looksPlainSegment 头部是否是可以直接落盘的合法分片（标准 TS / fMP4 / ADTS AAC）
func looksPlainSegment(b []byte) bool {
	if len(b) >= 2 && b[0] == 0x47 {
		// 第二包也是 0x47 才算真 TS（GIF 魔数 "G" 也是 0x47）
		if len(b) <= 188 || b[188] == 0x47 {
			return true
		}
	}
	if len(b) >= 8 {
		switch string(b[4:8]) {
		case "ftyp", "styp", "moof", "sidx", "emsg":
			return true
		}
	}
	if len(b) >= 2 && b[0] == 0xFF && b[1]&0xF6 == 0xF0 {
		return true // ADTS 裸 AAC
	}
	return false
}

func binaryPutUint64(b []byte, v uint64) {
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
}

func parseHexIV(s string) []byte {
	b := make([]byte, 16)
	s = strings.ToUpper(strings.TrimPrefix(strings.ToUpper(s), "0X"))
	if len(s) > 32 {
		s = s[:32]
	}
	for i := 0; i+1 < len(s); i += 2 {
		v, err := strconv.ParseUint(s[i:i+2], 16, 8)
		if err != nil {
			return b
		}
		b[i/2] = byte(v)
	}
	return b
}

// normalizeKey 归一化 AES 密钥：支持 16/24/32 字节原始密钥（AES-128/192/256-CBC，
// Go 的 aes.NewCipher 按长度自动匹配），以及站点常用的 hex 文本形式（32/48/64 字符）。
// 32 字节有歧义（可能是原始 AES-256 密钥，也可能是 AES-128 的 hex 文本）：
// 能整体按 hex 解码的按 hex 处理（原始密钥恰好全是 hex 字符的概率可忽略）。
func normalizeKey(b []byte) ([]byte, error) {
	b = bytes.TrimSpace(b)
	switch len(b) {
	case 16, 24:
		return b, nil
	case 32, 48, 64:
		if d, err := hex.DecodeString(string(b)); err == nil {
			return d, nil
		}
		if len(b) == 32 {
			return b, nil // 原始 AES-256 密钥
		}
	}
	return nil, fmt.Errorf("密钥长度 %d 无效（支持 16/24/32 字节或对应 hex 文本）", len(b))
}

// normalizeSegment 识别并剥掉伪装头（PNG/JPG/BMP/GIF 等图片或垃圾前缀），还原 TS 流。
// fMP4 分片（EXT-X-MAP 体系）与 ADTS 裸 AAC 分片原样放行；
// 找不到 TS 同步头则报错——坏分片混进合并会导致解码丢包、音画错位。
func normalizeSegment(data []byte) ([]byte, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("分片过小（%d 字节）", len(data))
	}
	// 标准 TS：0x47 同步字节打头，且第二个包（+188）也是 0x47。
	// 必须验证第二个同步头：GIF 魔数 "G" 恰好也是 0x47，单看首字节会误判
	if data[0] == 0x47 && (len(data) <= 188 || data[188] == 0x47) {
		return data, nil
	}
	// fMP4 分片：box 类型在第 5~8 字节
	if len(data) >= 8 {
		switch string(data[4:8]) {
		case "ftyp", "styp", "moof", "sidx", "emsg":
			return data, nil
		}
	}
	// ADTS 裸 AAC 音频分片（独立音频轨常见）
	if data[0] == 0xFF && data[1]&0xF6 == 0xF0 {
		return data, nil
	}
	// 伪装格式：在头部扫描 TS 同步头（188 字节对齐，连续 3 个包验证防误判）
	limit := len(data)
	if limit > 256<<10 {
		limit = 256 << 10
	}
	for i := 1; i+188*2 < len(data) && i < limit; i++ {
		if data[i] == 0x47 && data[i+188] == 0x47 && data[i+376] == 0x47 {
			return data[i:], nil
		}
	}
	return nil, fmt.Errorf("无法识别的分片格式（%s，%d 字节，无 TS 同步头）", magicName(data), len(data))
}

// magicName 按魔数识别伪装格式（用于日志/报错）
func magicName(b []byte) string {
	n := len(b)
	switch {
	case n >= 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "PNG 伪装"
	case n >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "JPEG 伪装"
	case n >= 2 && b[0] == 'B' && b[1] == 'M':
		return "BMP 伪装"
	case n >= 4 && string(b[:4]) == "GIF8":
		return "GIF 伪装"
	case n >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "WebP 伪装"
	}
	head := b
	if n > 8 {
		head = b[:8]
	}
	return fmt.Sprintf("未知头 % x", head)
}

func aesCBCDecrypt(key, iv, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	// 旧行为：长度不对时把密文原样当成功返回 → 坏分片混入合并，ffmpeg 解码丢包导致音画错位。
	// 现在严格报错，让上层走失败重试/整体失败，绝不放行坏数据。
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("AES-CBC 密文长度 %d 不是块大小整数倍，分片不完整或已损坏", len(data))
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	// 校验并去除 PKCS7 padding：padding 非法说明密钥/IV 不匹配，同样不能放行
	if n := len(out); n > 0 {
		p := int(out[n-1])
		if p == 0 || p > aes.BlockSize || p > n {
			return nil, fmt.Errorf("AES-CBC 解密结果 padding 非法，密钥或 IV 不匹配")
		}
		for i := n - p; i < n; i++ {
			if int(out[i]) != p {
				return nil, fmt.Errorf("AES-CBC 解密 padding 校验失败，密钥或 IV 不匹配")
			}
		}
		out = out[:n-p]
	}
	return out, nil
}

// ---------------------------------------------------------------- 基础工具

// extraHeaders 额外的全局请求头（如 Cookie），由 main 通过 SetHTTPHeaders 注入
var extraHeaders = map[string]string{}

// SetHTTPHeaders 设置 m3u8 / 分片 / 密钥请求都会带上的额外请求头。
// 部分源校验 Referer、Cookie，缺了直接 403；Referer 默认按播放列表域名自动补。
func SetHTTPHeaders(h map[string]string) {
	extraHeaders = map[string]string{}
	for k, v := range h {
		extraHeaders[k] = v
	}
}

// originOf 取 URL 的「协议 + 主机」作为默认 Referer（防盗链最常见的要求）
func originOf(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return ""
	}
	return p.Scheme + "://" + p.Host + "/"
}

// newReq 统一构造请求：UA / Referer / 自定义头 / 可选字节范围。
func newReq(u, referer string, start, length int64) (*http.Request, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	// 按字节范围切分片时必须禁用压缩，否则服务端压过的偏移量全错
	req.Header.Set("Accept-Encoding", "identity")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	if start >= 0 && length > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+length-1))
	}
	return req, nil
}

func fetchText(u string) string { return fetchTextRef(u, "") }

func fetchTextRef(u, referer string) string { return string(fetchBytesRef(u, referer)) }

func fetchBytes(u string) []byte { return fetchBytesRef(u, "") }

func fetchBytesRef(u, referer string) []byte {
	req, err := newReq(u, referer, -1, 0)
	if err != nil {
		return nil
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return b
}

// downloadFileRange 下载单文件（可选字节范围），用于 EXT-X-MAP 初始化段
func downloadFileRange(u, dst, referer string, start, length int64) error {
	req, err := newReq(u, referer, start, length)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("http %d for %s", resp.StatusCode, u)
	}
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if rerr != nil || len(b) == 0 {
		return fmt.Errorf("读取失败: %v", rerr)
	}
	// 服务器忽略 Range 时自己切
	if length > 0 && resp.StatusCode == http.StatusOK {
		if int64(len(b)) < start+length {
			return fmt.Errorf("初始化段字节范围越界（返回 %d 字节）", len(b))
		}
		b = b[start : start+length]
	}
	tmp := dst + ".part"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func downloadFileRaw(u, dst string) error { return downloadFile(u, dst, nil, nil, nil) }

// downloadFile 下载单个文件，支持 Range 断点续传（中断后 .part 保留，下次接着下）。
func downloadFile(u, dst string, onProgress func(int), onSpeed func(string), check func() bool) error {
	if check != nil && check() {
		return ErrCanceled
	}
	part := dst + ".part"
	var offset int64
	if fi, err := os.Stat(part); err == nil && fi.Size() > 0 {
		offset = fi.Size() // 上次中断：从断点续传
	}
	req, rerr := newReq(u, originOf(u), -1, 0)
	if rerr != nil {
		return rerr
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case offset > 0 && resp.StatusCode == http.StatusPartialContent:
		// 正常续传
	case offset > 0 && resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		// 已经下完了
		resp.Body.Close()
		return os.Rename(part, dst)
	case offset > 0 && resp.StatusCode == http.StatusOK:
		offset = 0 // 服务器不支持 Range，从头来
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("http %d for %s", resp.StatusCode, u)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	total := resp.ContentLength
	if total > 0 {
		total += offset
	}
	written := offset
	var last int64
	lastTick := time.Now()
	buf := make([]byte, 128<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			written += int64(n)
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return werr
			}
			if onProgress != nil && total > 0 {
				onProgress(int(written * 100 / total))
			}
			if onSpeed != nil && time.Since(lastTick) > time.Second {
				onSpeed(humanSize((written-last)/int64(time.Since(lastTick).Seconds()+1)) + "/s")
				last, lastTick = written, time.Now()
			}
			if check != nil && check() {
				f.Close()
				return ErrCanceled // 保留 .part，下次续传
			}
		}
		if rerr != nil {
			break
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(part, dst)
}

// precheckSpace 下载前按「总时长 × 码率」估算体积，磁盘不够就立即失败，
// 避免下了几百个分片才因为 ENOSPC 报错（已下的分片还占着空间）。
// 估不出来（没有 BANDWIDTH 或 EXTINF）就跳过，绝不误报。
func precheckSpace(dir string, pl *playlist) error {
	if pl.Duration <= 0 || pl.Bandwidth <= 0 {
		return nil
	}
	need := int64(pl.Duration*float64(pl.Bandwidth)/8) * 11 / 10
	if need <= 0 {
		return nil
	}
	_, free := sysinfo.DiskOf(dir)
	if free == 0 {
		return nil
	}
	if int64(free) < need {
		return fmt.Errorf("磁盘空间不足：预计需要 %s，可用仅 %s（%s）",
			humanSize(need), humanSize(int64(free)), dir)
	}
	return nil
}

// countDRM 统计无法解密的分片数（SAMPLE-AES / 非 identity 密钥）
func countDRM(pl *playlist) int {
	n := 0
	for _, sg := range pl.Segments {
		if sg.DRM {
			n++
		}
	}
	return n
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

var sanitizeReplacer = strings.NewReplacer(`\`, "_", "/", "_", ":", "_", "*", "_", "?", "_",
	`"`, "_", "<", "_", ">", "_", "|", "_", " ", "_")

// Sanitize 文件名安全化
func Sanitize(name string) string {
	s := sanitizeReplacer.Replace(strings.TrimSpace(name))
	if s == "" {
		s = "video"
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

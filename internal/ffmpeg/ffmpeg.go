// Package ffmpeg 负责定位/安装可用的 ffmpeg 可执行文件。
//
// 解析优先级（从高到低）：
//  1. 环境变量 VDL_FFMPEG 指定的路径（用户显式覆盖，文件存在就用）；
//  2. 数据目录缓存 <data>/bin/ffmpeg(.exe)——本程序下载/安装留下的那份；
//  3. 系统 PATH 中的 ffmpeg，以及几个常见安装目录；
//  4. 都没有 → 状态 missing，由用户在「信息」页点按钮按需下载（npmmirror 静态包），
//     下载后解压到 <data>/bin/ 并 chmod 0755，之后走第 2 条直接复用。
//
// 自 2026-10-19 起不再在编译期 go:embed 二进制：
// 单文件体积从约 40MB 降到约 12MB，常驻内存也少一份只读段；
// 代价是首次使用要联网下载一次（约 29MB），已下载过的实例无感。
package ffmpeg

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// State 是信息页展示的 ffmpeg 状态机
type State string

const (
	StateReady       State = "ready"       // 已就绪（系统装了 / 已下载过）
	StateMissing     State = "missing"     // 未安装，可点按钮下载
	StateDownloading State = "downloading" // 下载中
	StateFailed      State = "failed"      // 下载/校验失败，可重试
)

// Progress 下载进度（字节数按压缩包大小计）
type Progress struct {
	Downloaded int64 `json:"downloaded"`
	Total      int64 `json:"total"`
	Percent    int   `json:"percent"`
}

// Snapshot 给 /api/ffmpeg/status 与 /api/sysinfo 用
type Snapshot struct {
	State       State     `json:"state"`
	Available   bool      `json:"available"`
	Source      string    `json:"source,omitempty"` // env / cache / system / download
	Path        string    `json:"path,omitempty"`
	Version     string    `json:"version,omitempty"`
	Error       string    `json:"error,omitempty"`
	Platform    string    `json:"platform"`     // 例：linux/amd64
	DownloadURL string    `json:"download_url"` // 当前平台的下载地址（空=不支持）
	Supported   bool      `json:"supported"`
	Progress    *Progress `json:"progress,omitempty"`
}

// packURLs 各平台的静态包地址（ffmpeg-static b6.1.1，npmmirror 国内镜像）。
// 只列真正存在的平台；windows/arm64 用 x64 包（x64 模拟）并不合适，故不列。
var packURLs = map[string]string{
	"windows/amd64": "https://registry.npmmirror.com/-/binary/ffmpeg-static/b6.1.1/ffmpeg-win32-x64.gz",
	"linux/amd64":   "https://registry.npmmirror.com/-/binary/ffmpeg-static/b6.1.1/ffmpeg-linux-x64.gz",
	"linux/arm64":   "https://registry.npmmirror.com/-/binary/ffmpeg-static/b6.1.1/ffmpeg-linux-arm64.gz",
	"linux/arm":     "https://registry.npmmirror.com/-/binary/ffmpeg-static/b6.1.1/ffmpeg-linux-arm.gz",
	"darwin/amd64":  "https://registry.npmmirror.com/-/binary/ffmpeg-static/b6.1.1/ffmpeg-darwin-x64.gz",
	"darwin/arm64":  "https://registry.npmmirror.com/-/binary/ffmpeg-static/b6.1.1/ffmpeg-darwin-arm64.gz",
}

// fallbackURLs 主地址 404/超时时的备份镜像（b6.0 目录下同名文件）。
// 只在主地址彻底失败后依次尝试，避免一次下载打多个源。
var fallbackURLs = map[string][]string{
	"windows/amd64": {"https://registry.npmmirror.com/-/binary/ffmpeg-static/b6.0/ffmpeg-win32-x64.gz"},
	"linux/amd64":   {"https://registry.npmmirror.com/-/binary/ffmpeg-static/b6.0/ffmpeg-linux-x64.gz"},
	"linux/arm64":   {"https://registry.npmmirror.com/-/binary/ffmpeg-static/b6.0/ffmpeg-linux-arm64.gz"},
	"linux/arm":     {"https://registry.npmmirror.com/-/binary/ffmpeg-static/b6.0/ffmpeg-linux-arm.gz"},
	"darwin/amd64":  {"https://registry.npmmirror.com/-/binary/ffmpeg-static/b6.0/ffmpeg-darwin-x64.gz"},
	"darwin/arm64":  {"https://registry.npmmirror.com/-/binary/ffmpeg-static/b6.0/ffmpeg-darwin-arm64.gz"},
}

// 低于 1MB 的文件一律当作坏包（真的 ffmpeg 静态二进制约 50~80MB）
const minBinSize = 1 << 20

var (
	mu          sync.Mutex
	installing  bool
	lastErr     string
	progBytes   int64
	progTotal   int64
	installedBy string // 本进程刚下载安装的路径（Status 里把 cache 区分成 download）
	onReady     func(string)
	versionMu   sync.Mutex
	versionByOK = map[string]string{}
)

// SetOnReady 注册下载成功后的回调：main 里用它重新把路径交给下载器与图片处理器
// （启动时 ffmpeg 缺失，这两个模块拿到的是空路径）。
func SetOnReady(fn func(path string)) {
	mu.Lock()
	onReady = fn
	mu.Unlock()
}

func platformKey() string { return runtime.GOOS + "/" + runtime.GOARCH }

func exeName() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

// DownloadURL 返回当前平台的下载地址（空表示不支持自动下载）
func DownloadURL() string { return packURLs[platformKey()] }

// Resolve 返回当前可用的 ffmpeg 路径；没有则返回错误（不会再自动联网）。
// dataDir 为空时只看环境变量与系统 PATH。
func Resolve(dataDir string) (string, string, error) {
	// 1) 显式环境变量
	if p := strings.TrimSpace(os.Getenv("VDL_FFMPEG")); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Size() > 0 {
			return p, "env", nil
		}
	}

	// 2) 数据目录里下载/安装过的那份
	if dataDir != "" {
		p := filepath.Join(dataDir, "bin", exeName())
		if fi, err := os.Stat(p); err == nil && fi.Size() > minBinSize {
			return p, "cache", nil
		}
	}

	// 3) 系统 PATH（以及常见安装目录）
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, "system", nil
	}
	for _, p := range systemCandidates() {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Size() > minBinSize {
			return p, "system", nil
		}
	}

	return "", "", fmt.Errorf("未检测到 ffmpeg，可在此页点击下载（%s）", platformKey())
}

// Ensure 兼容旧调用：只要路径，失败给 error
func Ensure(dataDir string) (string, error) {
	p, _, err := Resolve(dataDir)
	return p, err
}

// SourceOf 兼容旧调用：env / cache / system / none
func SourceOf(dataDir string) string {
	_, src, err := Resolve(dataDir)
	if err != nil {
		return "none"
	}
	return src
}

func systemCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		var out []string
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), `C:\`} {
			if root != "" {
				out = append(out, filepath.Join(root, "ffmpeg", "bin", "ffmpeg.exe"))
			}
		}
		return out
	case "darwin":
		// GUI 启动的进程拿不到 Homebrew 的 PATH，这里补上两个标准位置
		return []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg"}
	default:
		// Alpine/Debian 装的 ffmpeg 通常在 /usr/bin；/usr/local/bin 是手动编译的
		return []string{"/usr/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/bin/ffmpeg", "/opt/bin/ffmpeg"}
	}
}

// Status 汇总当前状态供前端展示。会做一次本地探测（不联网）。
func Status(dataDir string) Snapshot {
	mu.Lock()
	busy := installing
	errMsg := lastErr
	p := &Progress{Downloaded: progBytes, Total: progTotal}
	if progTotal > 0 {
		p.Percent = int(progBytes * 100 / progTotal)
		if p.Percent > 100 {
			p.Percent = 100
		}
	}
	mu.Unlock()

	st := Snapshot{
		Platform:    platformKey(),
		DownloadURL: DownloadURL(),
		Supported:   DownloadURL() != "",
	}

	if busy {
		st.State, st.Error, st.Progress = StateDownloading, "", p
		return st
	}

	path, src, err := Resolve(dataDir)
	if err == nil {
		// 本次运行刚下载的报 download，方便前端区分「页面下载」与「以前留下的缓存」
		if src == "cache" {
			mu.Lock()
			if installedBy != "" && installedBy == path {
				src = "download"
			}
			mu.Unlock()
		}
		st.State, st.Available, st.Source, st.Path = StateReady, true, src, path
		st.Version = Version(path)
		return st
	}
	st.Error = errMsg
	if errMsg != "" {
		st.State = StateFailed
	} else {
		st.State = StateMissing
	}
	return st
}

// StartInstall 异步下载并安装当前平台的 ffmpeg。重复调用会返回错误，
// 前端据此把按钮切成「下载中」。
func StartInstall(dataDir string) error {
	if dataDir == "" {
		return errors.New("数据目录未初始化，无法安装 ffmpeg")
	}
	if DownloadURL() == "" {
		return fmt.Errorf("当前平台 %s 暂不支持自动下载，请自行安装 ffmpeg 或设置 VDL_FFMPEG", platformKey())
	}
	if p, _, err := Resolve(dataDir); err == nil {
		return fmt.Errorf("ffmpeg 已可用（%s），无需重复下载", p)
	}

	mu.Lock()
	if installing {
		mu.Unlock()
		return errors.New("正在下载中，请稍候")
	}
	installing = true
	lastErr = ""
	progBytes, progTotal = 0, 0
	mu.Unlock()

	go runInstall(dataDir)
	return nil
}

func runInstall(dataDir string) {
	path, err := downloadAndExtract(dataDir)

	mu.Lock()
	installing = false
	if err != nil {
		lastErr = err.Error()
	} else {
		installedBy = path
		lastErr = ""
	}
	cb := onReady
	mu.Unlock()

	if err != nil {
		return
	}
	// 下载成功：清掉版本缓存，重新读一次真实版本
	versionMu.Lock()
	delete(versionByOK, path)
	versionMu.Unlock()

	if cb != nil {
		cb(path)
	}
}

func setProgress(read, total int64) {
	mu.Lock()
	progBytes, progTotal = read, total
	mu.Unlock()
}

func downloadAndExtract(dataDir string) (string, error) {
	primary := DownloadURL()
	urls := append([]string{primary}, fallbackURLs[platformKey()]...)

	binDir := filepath.Join(dataDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("创建 %s 失败: %w", binDir, err)
	}
	dst := filepath.Join(binDir, exeName())

	var lastErr error
	for _, u := range urls {
		p, err := fetchOne(u, binDir, dst)
		if err == nil {
			return p, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的下载地址")
	}
	// 失败时把进度归零，前端不要停在半截的进度条上
	setProgress(0, 0)
	return "", lastErr
}

// fetchOne 下载单个 .gz，流式解压写入临时文件，校验后原子改名。
func fetchOne(url, binDir, dst string) (string, error) {
	cli := &http.Client{
		Timeout: 20 * time.Minute,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ResponseHeaderTimeout: 30 * time.Second,
			// 下载大文件走 keep-alive，不要每次重连
			ForceAttemptHTTP2: true,
		},
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "videodl/ffmpeg-installer")
	resp, err := cli.Do(req)
	if err != nil {
		return "", friendlyNetErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("镜像返回 HTTP %d（%s）", resp.StatusCode, url)
	}

	total := resp.ContentLength
	if total < 0 {
		total = 0
	}
	setProgress(0, total)

	tmp := filepath.Join(binDir, exeName()+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return "", fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	// gzip 解压 + 进度统计（进度按压缩包已读字节算，和 Content-Length 对得上）
	gr, err := gzip.NewReader(&progressReader{r: resp.Body, total: total})
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("下载内容不是有效的 gzip 包: %w", err)
	}
	n, err := io.Copy(f, gr)
	gr.Close()
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", friendlyNetErr(err)
	}
	if n < minBinSize {
		os.Remove(tmp)
		return "", fmt.Errorf("下载内容异常（解压后仅 %d 字节）", n)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmp, 0o755); err != nil {
			os.Remove(tmp)
			return "", fmt.Errorf("设置可执行权限失败: %w", err)
		}
	}

	// 先删旧的（Windows 上 rename 到已存在文件会失败）
	_ = os.Remove(dst)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("安装到 %s 失败: %w", dst, err)
	}

	// 真跑一次校验，避免把 HTML 错误页/半截文件当成 ffmpeg
	if err := verify(dst); err != nil {
		os.Remove(dst)
		return "", fmt.Errorf("下载的 ffmpeg 无法运行: %w", err)
	}
	setProgress(total, total)
	return dst, nil
}

func friendlyNetErr(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "timeout") || os.IsTimeout(err):
		return fmt.Errorf("下载超时，请检查网络后重试: %w", err)
	case strings.Contains(msg, "no such host"):
		return fmt.Errorf("域名解析失败，请检查 DNS/代理: %w", err)
	case strings.Contains(msg, "connection reset"), strings.Contains(msg, "EOF"):
		return fmt.Errorf("连接被中断，请重试: %w", err)
	}
	return err
}

func verify(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-version").Output()
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(string(out)), "ffmpeg version") {
		return errors.New("版本输出异常")
	}
	return nil
}

// progressReader 把"已下载字节"实时喂给 Status
type progressReader struct {
	r     io.Reader
	total int64
	read  int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.read += int64(n)
		setProgress(p.read, p.total)
	}
	return n, err
}

// Version 返回 ffmpeg 版本首行（用于页面展示）。
// 按路径缓存：/api/sysinfo 会被前端反复轮询，每次都起一个 ffmpeg 进程在弱 CPU 上没必要。
func Version(bin string) string {
	if bin == "" {
		return "未检测到"
	}
	versionMu.Lock()
	if v, ok := versionByOK[bin]; ok {
		versionMu.Unlock()
		return v
	}
	versionMu.Unlock()

	v := readVersion(bin)
	versionMu.Lock()
	versionByOK[bin] = v
	versionMu.Unlock()
	return v
}

func readVersion(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-version").Output()
	if err != nil {
		return "调用失败: " + err.Error()
	}
	line := strings.SplitN(string(out), "\n", 2)[0]
	return strings.TrimSpace(line)
}

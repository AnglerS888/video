// Package updater 实现在线检查与更新。
//
// 检查：GitHub 上没有 release / tag，所以以「分支最新提交 + 仓库根目录 VERSION 文件」为准，
// 三个信号任一命中即视为有更新：
//  1. 远端 VERSION 比本地版本号大；
//  2. 本地记录了构建提交，且与远端最新提交不同；
//  3. 本地没有提交哈希时，远端提交时间晚于构建时间（+2 分钟容差）。
//
// 更新：直接下载 https://github.com/<repo>/archive/refs/heads/main.zip（源码包），
// 直连失败自动切国内镜像前缀（ghproxy.net / gh-proxy.com，可用 -update-proxy 覆盖）。
//
// 落地的两种运行方式：
//   - 直接运行：用本机 Go 工具链编译出新二进制，Windows 生成 apply-update.bat
//     （退出后拷贝替换并重新启动），Linux/macOS 直接 rename + exec 重启；
//   - Docker：容器内没有 Go 工具链，也没有宿主机 docker 权限，所以只把源码落到
//     数据目录 update/src，返回「到宿主机执行 docker compose build 后 up -d」的指令。
package updater

import (
	"archive/zip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"videoDownLoad/internal/logx"
	"videoDownLoad/internal/version"
)

// defaultMirrors 国内可用的 GitHub 代理前缀（透传原始 URL）。
// 2026-10-20 实测：raw.githubusercontent.com 直连不通，这两个镜像可用；
// ghfast.top 已失效，不要加回来。
var defaultMirrors = []string{
	"https://ghproxy.net/",
	"https://gh-proxy.com/",
}

const (
	maxZipBytes  = 200 << 20 // 源码包硬上限
	maxFileBytes = 60 << 20  // 单个文件解压上限
)

// Info 版本与更新检查结果
type Info struct {
	CurrentVersion  string   `json:"current_version"`
	CurrentCommit   string   `json:"current_commit"`
	BuildTime       string   `json:"build_time"`
	Mode            string   `json:"mode"` // docker / direct
	Repo            string   `json:"repo"`
	Branch          string   `json:"branch"`
	LatestVersion   string   `json:"latest_version"`
	LatestCommit    string   `json:"latest_commit"`
	LatestCommitAt  string   `json:"latest_commit_at"`
	LatestCommitMsg string   `json:"latest_commit_msg"`
	Available       bool     `json:"available"`
	Reasons         []string `json:"reasons,omitempty"`
	CheckedAt       string   `json:"checked_at"`
	Via             string   `json:"via,omitempty"` // 本次检查实际走通的通道：direct / 镜像地址
	Error           string   `json:"error,omitempty"`
}

// Status 更新任务进度
type Status struct {
	Phase        string   `json:"phase"` // idle/checking/downloading/extracting/building/staged/ready/applying/failed
	Percent      int      `json:"percent"`
	Message      string   `json:"message"`
	Bytes        int64    `json:"bytes"`
	Total        int64    `json:"total"`
	SourceDir    string   `json:"source_dir,omitempty"`
	NewBinary    string   `json:"new_binary,omitempty"`
	Ready        bool     `json:"ready"`        // 是否已就绪，可点「重启并应用」
	NeedGo       bool     `json:"need_go"`      // 源码已下载，但本机缺 Go 工具链
	Instructions []string `json:"instructions"` // Docker 模式的重建步骤 / 缺工具链时的提示
	Error        string   `json:"error,omitempty"`
	UpdatedAt    string   `json:"updated_at"`
}

// Updater 更新器。同一进程只用一个实例。
type Updater struct {
	dataDir string
	repo    string
	branch  string
	mirrors []string
	client  *http.Client

	mu      sync.Mutex
	info    Info
	infoAt  time.Time
	status  Status
	running bool

	// 重启相关的状态（PrepareRestart 后由 main 在退出前读取）
	relaunch bool
	exePath  string
}

// New 创建更新器。mirrors 为空时用内置镜像列表。
func New(dataDir, repo, branch string, mirrors []string) *Updater {
	if repo == "" {
		repo = version.Repo
	}
	if branch == "" {
		branch = version.Branch
	}
	ms := make([]string, 0, len(mirrors))
	for _, m := range mirrors {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if !strings.HasSuffix(m, "/") {
			m += "/"
		}
		ms = append(ms, m)
	}
	if len(ms) == 0 {
		ms = append(ms, defaultMirrors...)
	}
	return &Updater{
		dataDir: dataDir,
		repo:    repo,
		branch:  branch,
		mirrors: ms,
		client: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		status: Status{Phase: "idle", Message: "尚未开始", Instructions: []string{}},
	}
}

func (u *Updater) ua() string {
	return "videoDownload-updater/" + version.Version + " (+https://github.com/" + u.repo + ")"
}

func (u *Updater) base() Info {
	buildTime := ""
	if t := version.Time(); !t.IsZero() {
		buildTime = t.UTC().Format(time.RFC3339)
	}
	return Info{
		CurrentVersion: version.Version,
		CurrentCommit:  version.Commit,
		BuildTime:      buildTime,
		Mode:           version.Mode(),
		Repo:           u.repo,
		Branch:         u.branch,
	}
}

// Current 只返回本地信息 + 上次检查的缓存（不联网），force=false 时调用
func (u *Updater) Current(force bool) Info {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !force && u.infoAt.IsZero() {
		return u.base()
	}
	if !force && time.Since(u.infoAt) < 10*time.Minute {
		return u.info
	}
	return u.info
}

// Refresh 主动联网检查；失败时把错误写进返回值的 Error 字段（不抛错，前端好展示）
func (u *Updater) Refresh(ctx context.Context) Info {
	info := u.base()

	commit, commitAt, commitMsg, via1, err1 := u.fetchCommit(ctx)
	remoteVer, via2, err2 := u.fetchRemoteVersion(ctx)
	if via1 != "" {
		info.Via = via1
	} else if via2 != "" {
		info.Via = via2
	}
	info.LatestCommit = commit
	info.LatestCommitMsg = commitMsg
	if !commitAt.IsZero() {
		info.LatestCommitAt = commitAt.UTC().Format(time.RFC3339)
	}
	info.LatestVersion = remoteVer

	// 判定是否有更新
	if remoteVer != "" && compareSemver(remoteVer, version.Version) > 0 {
		info.Available = true
		info.Reasons = append(info.Reasons, "远端版本号 "+remoteVer+" 高于本机 "+version.Version)
	}
	if commit != "" {
		switch {
		case version.Commit != "":
			// 本地提交可能是短哈希（7 位），远端是完整 40 位：互相前缀匹配即视为同一个提交
			same := strings.HasPrefix(commit, version.Commit) || strings.HasPrefix(version.Commit, commit)
			if !same {
				info.Available = true
				info.Reasons = append(info.Reasons, "远端最新提交与本机构建提交不同")
			}
		case !commitAt.IsZero():
			bt := version.Time()
			if !bt.IsZero() && commitAt.After(bt.Add(2*time.Minute)) {
				info.Available = true
				info.Reasons = append(info.Reasons, "远端提交时间晚于本机构建时间")
			}
		}
	}
	if err1 != nil && err2 != nil {
		info.Error = fmt.Sprintf("检查失败：%v；%v", err1, err2)
	} else if err1 != nil && remoteVer == "" {
		info.Error = "读取提交信息失败：" + err1.Error()
	}
	info.CheckedAt = time.Now().Format("2006-01-02 15:04:05")

	u.mu.Lock()
	u.info, u.infoAt = info, time.Now()
	u.mu.Unlock()
	logx.Infof("[update] 检查完成：本机 %s（%s），远端 %s（%s），有更新=%v",
		info.CurrentVersion, shortSHA(info.CurrentCommit), info.LatestVersion, shortSHA(info.LatestCommit), info.Available)
	return info
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	if s == "" {
		return "-"
	}
	return s
}

// fetchCommit 读分支最新提交（直连 GitHub API，失败依次试镜像前缀）
func (u *Updater) fetchCommit(ctx context.Context) (sha string, at time.Time, msg, via string, err error) {
	api := fmt.Sprintf("https://api.github.com/repos/%s/commits/%s", u.repo, u.branch)
	var lastErr error
	for _, c := range u.urlCandidates(api) {
		var body struct {
			SHA    string `json:"sha"`
			Commit struct {
				Author struct {
					Date string `json:"date"`
				} `json:"author"`
				Message string `json:"message"`
			} `json:"commit"`
		}
		if err := u.getJSON(ctx, c.url, &body); err != nil {
			lastErr = err
			continue
		}
		if body.SHA == "" {
			lastErr = errors.New("响应里没有 sha")
			continue
		}
		t, _ := time.Parse(time.RFC3339, body.Commit.Author.Date)
		return body.SHA, t, firstLine(body.Commit.Message), c.via, nil
	}
	if lastErr == nil {
		lastErr = errors.New("未知错误")
	}
	return "", time.Time{}, "", "", lastErr
}

// fetchRemoteVersion 读仓库根目录 VERSION 文件（raw 直连在国内基本不通，优先镜像）
func (u *Updater) fetchRemoteVersion(ctx context.Context) (ver, via string, err error) {
	raw := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/VERSION", u.repo, u.branch)
	var lastErr error
	for _, c := range u.urlCandidates(raw) {
		s, err := u.getText(ctx, c.url, 4<<10)
		if err != nil {
			lastErr = err
			continue
		}
		return strings.TrimSpace(s), c.via, nil
	}
	if lastErr == nil {
		lastErr = errors.New("未知错误")
	}
	return "", "", lastErr
}

type candidate struct {
	url string
	via string
}

// urlCandidates 直连优先，其次逐个镜像前缀；镜像会把原始 URL 直接拼在后面
func (u *Updater) urlCandidates(raw string) []candidate {
	out := []candidate{{url: raw, via: "direct"}}
	for _, m := range u.mirrors {
		out = append(out, candidate{url: m + raw, via: m})
	}
	return out
}

func (u *Updater) getJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", u.ua())
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst)
}

func (u *Updater) getText(ctx context.Context, url string, limit int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", u.ua())
	resp, err := u.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// compareSemver 比较 a、b 的语义化版本（忽略前导 v 与 -rc 之类的后缀），a>b 返回 1
func compareSemver(a, b string) int {
	pa, pb := parseSemver(a), parseSemver(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

func parseSemver(s string) [3]int {
	var out [3]int
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	for i, part := range strings.Split(s, ".") {
		if i > 2 {
			break
		}
		n := 0
		for _, c := range part {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}

// ---------------- 更新任务 ----------------

// GetStatus 当前任务状态（前端轮询）
func (u *Updater) GetStatus() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.status
	s.Instructions = append([]string{}, u.status.Instructions...)
	return s
}

func (u *Updater) setStatus(fn func(*Status)) {
	u.mu.Lock()
	fn(&u.status)
	u.status.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	u.mu.Unlock()
}

// Start 启动一次更新任务（异步）。已在运行时返回错误。
func (u *Updater) Start() error {
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		return errors.New("更新任务正在进行中")
	}
	u.running = true
	u.status = Status{Phase: "downloading", Message: "准备下载最新源码…", Instructions: []string{}}
	u.status.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	u.mu.Unlock()

	go func() {
		defer func() {
			u.mu.Lock()
			u.running = false
			u.mu.Unlock()
		}()
		if err := u.run(context.Background()); err != nil {
			logx.Warnf("[update] 更新失败: %v", err)
			u.setStatus(func(s *Status) {
				s.Phase = "failed"
				s.Error = err.Error()
				s.Message = "更新失败：" + err.Error()
			})
		}
	}()
	return nil
}

func (u *Updater) run(ctx context.Context) error {
	dir := filepath.Join(u.dataDir, "update")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建更新目录失败: %w", err)
	}

	// 1) 下载源码包
	zipPath := filepath.Join(dir, "main.zip")
	if err := u.download(ctx, zipPath); err != nil {
		return err
	}

	// 2) 解压
	u.setStatus(func(s *Status) { s.Phase = "extracting"; s.Message = "正在解压源码…"; s.Percent = 70 })
	srcDir, err := u.extract(zipPath, dir)
	if err != nil {
		return err
	}
	_ = os.Remove(zipPath)

	srcOK := fileExists(filepath.Join(srcDir, "go.mod")) &&
		fileExists(filepath.Join(srcDir, "internal", "web", "static", "index.html"))
	if !srcOK {
		return errors.New("下载的源码包不完整（缺少 go.mod 或前端产物 internal/web/static）")
	}
	u.setStatus(func(s *Status) { s.SourceDir = srcDir })

	// 3) Docker：容器里没有 Go，也没有宿主机 docker 权限 —— 只落源码 + 指令
	if version.InDocker() {
		u.setStatus(func(s *Status) {
			s.Phase = "staged"
			s.Percent = 100
			s.Message = "源码已下载到数据目录，请在宿主机重新构建镜像"
			s.Instructions = dockerInstructions(srcDir)
		})
		return nil
	}

	// 4) 直接运行：用本机 Go 编译
	goBin, lookErr := exec.LookPath("go")
	if lookErr != nil {
		u.setStatus(func(s *Status) {
			s.Phase = "staged"
			s.Percent = 100
			s.NeedGo = true
			s.Message = "源码已下载，但本机没有 Go 工具链，无法自动编译"
			s.Instructions = []string{
				"安装 Go 1.26+（https://go.dev/dl/ ，国内可用 https://golang.google.cn/dl/）",
				"在源码目录执行：go build -trimpath -ldflags \"-s -w\" -o videodl .",
				"或者改用 Docker 方式运行，镜像内会自行构建",
			}
		})
		return nil
	}

	u.setStatus(func(s *Status) {
		s.Phase = "building"
		s.Percent = 75
		s.Message = "正在编译新的可执行文件（首次可能需要 1~2 分钟）…"
	})
	binName := "videodl-new"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	newBin := filepath.Join(dir, binName)
	if err := u.build(ctx, goBin, srcDir, newBin); err != nil {
		u.setStatus(func(s *Status) { s.NewBinary = "" })
		return fmt.Errorf("编译失败: %w", err)
	}

	u.setStatus(func(s *Status) {
		s.Phase = "ready"
		s.Percent = 100
		s.Ready = true
		s.NewBinary = newBin
		s.Message = "新版本已编译完成，点「重启并应用」生效"
		if runtime.GOOS == "windows" {
			s.Instructions = []string{"会先退出当前进程，再由脚本替换可执行文件并重新启动（约 3 秒）"}
		}
	})
	return nil
}

// download 依次尝试直连与镜像，写文件并更新进度
func (u *Updater) download(ctx context.Context, dst string) error {
	zipURL := fmt.Sprintf("https://github.com/%s/archive/refs/heads/%s.zip", u.repo, u.branch)
	var lastErr error
	for _, c := range u.urlCandidates(zipURL) {
		u.setStatus(func(s *Status) { s.Message = "正在下载源码包（" + c.via + "）…" })
		err := u.downloadOne(ctx, c.url, dst)
		if err == nil {
			return nil
		}
		lastErr = err
		logx.Warnf("[update] 下载失败（%s）：%v", c.via, err)
	}
	if lastErr == nil {
		lastErr = errors.New("未知错误")
	}
	return fmt.Errorf("下载源码包失败: %w", lastErr)
}

func (u *Updater) downloadOne(ctx context.Context, url, dst string) error {
	// 下载可能超过 client 的 20s 超时，这里用独立的长超时请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", u.ua())
	dl := &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
	resp, err := dl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	total := resp.ContentLength
	if total > maxZipBytes {
		return fmt.Errorf("源码包过大（%s）", humanBytes(total))
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(tmp)
	}()

	buf := make([]byte, 64<<10)
	var done int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if done > maxZipBytes {
				return errors.New("源码包超过大小上限")
			}
			pct := 5
			if total > 0 {
				pct = 5 + int(float64(done)/float64(total)*65)
			}
			u.setStatus(func(s *Status) { s.Bytes = done; s.Total = total; s.Percent = pct })
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if done < 32<<10 {
		return fmt.Errorf("下载内容过小（%d 字节），可能被代理拦截", done)
	}
	return os.Rename(tmp, dst)
}

// extract 解压 zip 到 <dir>/src（覆盖旧的），自动去掉压缩包里那一层仓库名目录
func (u *Updater) extract(zipPath, dir string) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("源码包不是有效的 zip: %w", err)
	}
	defer zr.Close()

	tmpDir := filepath.Join(dir, "src.new")
	if err := os.RemoveAll(tmpDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}

	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		// 去掉第一层目录（GitHub 的 zip 一定是 video-main/xxx）
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		if name == "" {
			continue
		}
		// 防 zip-slip：绝对路径、..、盘符一律拒绝
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || strings.Contains(clean, ":") {
			return "", fmt.Errorf("源码包里有非法路径: %s", f.Name)
		}
		target := filepath.Join(tmpDir, clean)
		if !strings.HasPrefix(target, tmpDir+string(os.PathSeparator)) {
			return "", fmt.Errorf("源码包里有非法路径: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if f.UncompressedSize64 > maxFileBytes {
			return "", fmt.Errorf("源码包里有超大文件: %s", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err := extractFile(f, target); err != nil {
			return "", fmt.Errorf("解压 %s 失败: %w", f.Name, err)
		}
	}

	final := filepath.Join(dir, "src")
	_ = os.RemoveAll(final)
	if err := os.Rename(tmpDir, final); err != nil {
		return "", err
	}
	return final, nil
}

func extractFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, io.LimitReader(rc, maxFileBytes+1))
	return err
}

// build 在源码目录里编译新二进制
func (u *Updater) build(ctx context.Context, goBin, srcDir, out string) error {
	ldflags := fmt.Sprintf(`-s -w -X videoDownLoad/internal/version.Version=%s -X videoDownLoad/internal/version.Commit=%s -X videoDownLoad/internal/version.BuildTime=%s`,
		u.remoteVersionOrDefault(), u.remoteCommitOrEmpty(), time.Now().UTC().Format(time.RFC3339))

	args := []string{"build", "-trimpath", "-ldflags", ldflags, "-o", out, "."}
	cmd := exec.CommandContext(ctx, goBin, args...)
	cmd.Dir = srcDir
	// 不要强制 GOTOOLCHAIN=local：go.mod 往往要求比本机更高的 Go 版本，
	// 交给 Go 自动切换/下载 toolchain 比直接失败好（用户自己设了 GOTOOLCHAIN 就尊重）。
	env := os.Environ()
	if os.Getenv("GOPROXY") == "" {
		env = append(env, "GOPROXY=https://goproxy.cn,direct")
	}
	if os.Getenv("GOTOOLCHAIN") == "" {
		env = append(env, "GOTOOLCHAIN=auto")
	}
	cmd.Env = env

	var tail strings.Builder
	cmd.Stdout = &tail
	cmd.Stderr = &tail
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(tail.String())
		if len(msg) > 800 {
			msg = msg[len(msg)-800:]
		}
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(msg, "requires go >=") || strings.Contains(msg, "GOTOOLCHAIN") {
			msg += "（本机 Go 版本低于源码要求：升级 Go，或把 GOTOOLCHAIN 设为 auto 让 Go 自动下载工具链）"
		}
		return errors.New(msg)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Size() < 3<<20 {
		return errors.New("编译产物异常（文件不存在或过小）")
	}
	return nil
}

func (u *Updater) remoteVersionOrDefault() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.info.LatestVersion != "" {
		return u.info.LatestVersion
	}
	if version.Version != "" {
		return version.Version
	}
	return "dev"
}

func (u *Updater) remoteCommitOrEmpty() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.info.LatestCommit
}

// PrepareRestart 应用已编译好的新二进制。
// 返回值 relaunch=true 表示调用方在优雅退出后应执行 ExecNew（Unix）；
// Windows 无法替换正在运行的 exe，改为生成并启动替换脚本，relaunch 返回 false。
func (u *Updater) PrepareRestart() (relaunch bool, err error) {
	u.mu.Lock()
	st := u.status
	u.mu.Unlock()
	if !st.Ready || st.NewBinary == "" {
		return false, errors.New("还没有可应用的新版本，请先下载更新")
	}
	exe, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("定位当前可执行文件失败: %w", err)
	}
	if real, e2 := filepath.EvalSymlinks(exe); e2 == nil {
		exe = real
	}
	if _, err := os.Stat(st.NewBinary); err != nil {
		return false, errors.New("新版本文件不存在，请重新下载更新")
	}

	if runtime.GOOS == "windows" {
		bat := filepath.Join(u.dataDir, "update", "apply-update.bat")
		// 带上原本的启动参数（-addr / -data / -lowmem…），否则重启后会退回默认端口与数据目录
		if err := writeWindowsScript(bat, st.NewBinary, exe, os.Args[1:]); err != nil {
			return false, err
		}
		// 独立进程跑脚本：本进程退出后它才拷贝，避免 "文件被占用"
		if err := launchUpdaterScript(bat, filepath.Join(u.dataDir, "update")); err != nil {
			return false, fmt.Errorf("启动替换脚本失败: %w", err)
		}
		logx.Infof("[update] 已启动替换脚本 %s（新文件 %s -> %s）", bat, st.NewBinary, exe)
		u.setStatus(func(s *Status) {
			s.Phase = "applying"
			s.Message = "正在替换可执行文件并重启…"
		})
		return false, nil
	}

	// Unix：先备份再原子替换，进程退出后 exec 新文件即可
	if err := copyFile(exe, exe+".bak"); err != nil {
		logx.Warnf("[update] 备份旧文件失败（继续替换）: %v", err)
	}
	if err := os.Rename(st.NewBinary, exe); err != nil {
		return false, fmt.Errorf("替换可执行文件失败: %w", err)
	}
	if err := os.Chmod(exe, 0o755); err != nil {
		logx.Warnf("[update] 设置可执行权限失败: %v", err)
	}
	u.mu.Lock()
	u.relaunch, u.exePath = true, exe
	u.mu.Unlock()
	u.setStatus(func(s *Status) {
		s.Phase = "applying"
		s.Message = "已替换可执行文件，正在重启…"
	})
	logx.Infof("[update] 已替换 %s，退出后重新启动", exe)
	return true, nil
}

// RelaunchPending Unix 下退出前调用，返回 true 表示需要用新二进制重新启动
func (u *Updater) RelaunchPending() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.relaunch
}

// ExePath 需要重启时可执行文件的路径
func (u *Updater) ExePath() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.exePath
}

func writeWindowsScript(bat, newBin, exe string, args []string) error {
	// 等待旧进程释放文件锁；最多重试 15 次；成功后按原参数重新启动
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, quoteWinArg(a))
	}
	script := "@echo off\r\n" +
		"chcp 65001 >nul\r\n" +
		"setlocal\r\n" +
		"set NEW=\"" + newBin + "\"\r\n" +
		"set EXE=\"" + exe + "\"\r\n" +
		"for /l %%i in (1,1,15) do (\r\n" +
		"  copy /y %NEW% %EXE% >nul 2>&1 && goto started\r\n" +
		"  timeout /t 1 /nobreak >nul\r\n" +
		")\r\n" +
		"echo 替换失败：目标文件仍被占用。请手动把 %NEW% 覆盖到 %EXE%\r\n" +
		"pause\r\n" +
		"exit /b 1\r\n" +
		":started\r\n" +
		"start \"\" /min %EXE% " + strings.Join(quoted, " ") + "\r\n" +
		"del \"%~f0\" >nul 2>&1\r\n"
	return os.WriteFile(bat, []byte(script), 0o755)
}

// quoteWinArg 给 cmd 启动参数加引号（含空格或引号时才需要）
func quoteWinArg(s string) string {
	if s == "" {
		return "\"\""
	}
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func dockerInstructions(srcDir string) []string {
	return []string{
		"源码已下载到：" + srcDir,
		"在宿主机项目目录执行（二选一）：",
		"  1) docker compose build && docker compose up -d",
		"  2) docker build -t videodl:latest . && docker compose up -d",
		"提示：容器内没有 Go 工具链，所以无法在容器里自动编译；镜像重建后数据目录（volume）不受影响。",
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 0, 64) + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

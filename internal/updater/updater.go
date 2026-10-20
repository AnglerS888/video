// Package updater 实现在线检查与更新。
//
// 更新源只支持 Gitee（GitHub 仓库已删除，不再做镜像回退）。
// 没有 release / tag 依赖，所以以「分支最新提交 + 仓库根目录 VERSION 文件」为准，
// 三个信号任一命中即视为有更新：
//  1. 远端 VERSION 比本地版本号大；
//  2. 本地记录了构建提交，且与远端最新提交不同；
//  3. 本地没有提交哈希时，远端提交时间晚于构建时间（+2 分钟容差）。
//
// 更新：直接下载 Gitee 的源码包 /repository/archive/<branch>.zip。
//
// 只支持 Docker 容器内更新：容器里没有 Go 工具链，也不能替换正在运行的镜像，
// 所以流程是「下载最新源码到数据目录 → 校验 → 给出在宿主机重建镜像的指令」。
// 直接运行（单个二进制 / NAS 裸跑）**不提供自动更新**，页面上只给 Gitee 仓库链接，
// 由用户自己下载最新版本替换。
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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"videoDownLoad/internal/logx"
	"videoDownLoad/internal/version"
)

// cliUserAgent 匿名下载源码包时必须用命令行风格 UA：
// Gitee 对浏览器 UA 会返回「下载仓库」登录/验证页（实测 Chrome UA 拿到 46KB HTML）。
const cliUserAgent = "curl/8.10.1"

const (
	maxZipBytes  = 200 << 20 // 源码包硬上限
	maxFileBytes = 60 << 20  // 单个文件解压上限
)

// Info 版本与更新检查结果
type Info struct {
	CurrentVersion  string   `json:"current_version"`
	CurrentCommit   string   `json:"current_commit"`
	BuildTime       string   `json:"build_time"`
	Mode            string   `json:"mode"`     // docker / direct
	SiteURL         string   `json:"site_url"` // 仓库网页地址（Gitee）
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
	Phase        string   `json:"phase"` // idle/downloading/extracting/staged/failed
	Percent      int      `json:"percent"`
	Message      string   `json:"message"`
	Bytes        int64    `json:"bytes"`
	Total        int64    `json:"total"`
	SourceDir    string   `json:"source_dir,omitempty"`
	Instructions []string `json:"instructions"` // 在宿主机重建镜像的步骤
	Error        string   `json:"error,omitempty"`
	UpdatedAt    string   `json:"updated_at"`
}

// Updater 更新器。同一进程只用一个实例。
type Updater struct {
	dataDir string
	repo    string
	branch  string
	token   string // 可选的 Gitee 私人令牌（配置后走官方 zipball 接口，不依赖匿名下载）
	client  *http.Client

	mu      sync.Mutex
	info    Info
	infoAt  time.Time
	status  Status
	running bool
}

// New 创建更新器。repo/branch 为空时用 version 包里的默认值（anglers/video@master）；
// token 是可选的 Gitee 私人令牌，配了就优先用官方 zipball 接口。
func New(dataDir, repo, branch, token string) *Updater {
	if repo == "" {
		repo = version.Repo
	}
	if branch == "" {
		branch = version.Branch
	}
	return &Updater{
		dataDir: dataDir,
		repo:    repo,
		branch:  branch,
		token:   strings.TrimSpace(token),
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
	return "videoDownload-updater/" + version.Version + " (+" + version.RepoURL(u.repo) + ")"
}

// apiCommitURL 分支最新提交的 API 地址（Gitee OpenAPI v5）
func (u *Updater) apiCommitURL() string {
	return fmt.Sprintf("%s/api/v5/repos/%s/commits/%s", version.SiteBase, u.repo, u.branch)
}

// rawFileURL 仓库内原始文件（Gitee: /<owner>/<repo>/raw/<branch>/<file>）
func (u *Updater) rawFileURL(name string) string {
	return fmt.Sprintf("%s/%s/raw/%s/%s", version.SiteBase, u.repo, u.branch, name)
}

// archiveURL 分支源码包（匿名下载地址）
func (u *Updater) archiveURL() string {
	return fmt.Sprintf("%s/%s/repository/archive/%s.zip", version.SiteBase, u.repo, u.branch)
}

// zipballURL 官方 OpenAPI 源码包（需要 access_token）
func (u *Updater) zipballURL() string {
	return fmt.Sprintf("%s/api/v5/repos/%s/zipball?ref=%s&access_token=%s",
		version.SiteBase, u.repo, u.branch, u.token)
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
		SiteURL:        version.RepoURL(u.repo),
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
	logx.Infof("[update] 检查完成（gitee:%s@%s）：本机 %s（%s），远端 %s（%s），有更新=%v",
		u.repo, u.branch, info.CurrentVersion, shortSHA(info.CurrentCommit),
		info.LatestVersion, shortSHA(info.LatestCommit), info.Available)
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

// fetchCommit 读分支最新提交（Gitee OpenAPI）
func (u *Updater) fetchCommit(ctx context.Context) (sha string, at time.Time, msg, via string, err error) {
	var body struct {
		SHA    string `json:"sha"`
		Commit struct {
			Author struct {
				Date string `json:"date"`
			} `json:"author"`
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := u.getJSON(ctx, u.apiCommitURL(), &body); err != nil {
		return "", time.Time{}, "", "", err
	}
	if body.SHA == "" {
		return "", time.Time{}, "", "", errors.New("接口响应里没有 sha（仓库可能是空的）")
	}
	t, _ := time.Parse(time.RFC3339, body.Commit.Author.Date)
	return body.SHA, t, firstLine(body.Commit.Message), "gitee", nil
}

// fetchRemoteVersion 读仓库根目录 VERSION 文件
func (u *Updater) fetchRemoteVersion(ctx context.Context) (ver, via string, err error) {
	s, err := u.getText(ctx, u.rawFileURL("VERSION"), 4<<10)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(s), "gitee", nil
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
	// 非容器模式不提供自动更新（页面会直接给 Gitee 链接）
	if !version.InDocker() {
		return errors.New("非容器模式不支持自动更新，请到 Gitee 仓库手动下载最新版本")
	}

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

	// 3) 容器里没有 Go，也不该替换运行中的镜像：落源码 + 让用户在宿主机重建
	u.setStatus(func(s *Status) {
		s.Phase = "staged"
		s.Percent = 100
		s.SourceDir = srcDir
		s.Message = "最新源码已下载到数据目录，请在宿主机重建镜像"
		s.Instructions = dockerInstructions(srcDir)
	})
	return nil
}

// download 下载源码包。
//
// 注意：Gitee 的 /repository/archive/<branch>.zip 对「浏览器 UA」会返回登录/验证网页，
// 只有命令行 UA 才直接给 zip（2026-10-20 实测）。所以这里分两条路：
//  1. 配了 Gitee 令牌：走官方 /api/v5/repos/<repo>/zipball（最稳）；
//  2. 没配令牌：匿名 archive 地址 + CLI 风格 UA（拿到 HTML 时给出明确错误提示）。
func (u *Updater) download(ctx context.Context, dst string) error {
	type candidate struct{ url, ua, note string }
	var cands []candidate
	if u.token != "" {
		cands = append(cands, candidate{u.zipballURL(), u.ua(), "官方接口"})
	}
	cands = append(cands, candidate{u.archiveURL(), cliUserAgent, "匿名下载"})

	var lastErr error
	for _, c := range cands {
		u.setStatus(func(s *Status) { s.Message = "正在下载源码包（Gitee " + c.note + "）…" })
		err := u.downloadOne(ctx, c.url, c.ua, dst)
		if err == nil {
			return nil
		}
		lastErr = err
		logx.Warnf("[update] 下载源码包失败（%s）：%v", c.note, err)
	}
	if lastErr == nil {
		lastErr = errors.New("未知错误")
	}
	return fmt.Errorf("下载源码包失败: %w（可在启动参数加 -update-token <Gitee令牌> 走官方接口）", lastErr)
}

func (u *Updater) downloadOne(ctx context.Context, url, ua, dst string) error {
	// 下载可能超过 client 的 20s 超时，这里用独立的长超时请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
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
	// Gitee 在要求登录/验证时回的是 HTML（状态码仍是 200），提前识别，别把网页当 zip 存下来
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		return errors.New("Gitee 返回了网页而不是源码包（可能需要登录/验证）")
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

func dockerInstructions(srcDir string) []string {
	return []string{
		"最新源码已下载到：" + srcDir + "（容器内是 /app/videodata/update/src，对应宿主机数据目录）",
		"在宿主机项目目录更新并重建镜像：",
		"  1) git pull                              # 或把上面的源码同步到项目目录",
		"  2) docker compose build && docker compose up -d",
		"说明：容器里没有 Go 工具链，无法在容器内编译；镜像重建后数据目录（volume）不受影响。",
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

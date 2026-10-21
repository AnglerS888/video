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
// 只支持 Docker 容器内更新。2026-10-21 起运行镜像自带 Go 工具链（golang 基座），
// 流程是「下载源码 → 容器内 go build → 原子替换二进制 → 原地 exec 重启（PID 不变，
// 容器不退出）」；旧的精简镜像（容器里找不到 go 命令）自动回退为
// 「下载源码到数据目录 → 给出在宿主机重建镜像的指令」。
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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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
	maxZipBytes   = 200 << 20 // 源码包硬上限
	maxFileBytes  = 60 << 20  // 单个文件解压上限
	maxTotalBytes = 500 << 20 // 解压总字节上限（防 zip 炸弹：单文件不超但总量可以很大）
	maxFileCount  = 20000     // 解压文件数上限（防大量小文件耗尽磁盘/inode）
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
	Phase        string   `json:"phase"` // idle/downloading/extracting/building/restarting/done/staged/failed
	Percent      int      `json:"percent"`
	Message      string   `json:"message"`
	Bytes        int64    `json:"bytes"`
	Total        int64    `json:"total"`
	SourceDir    string   `json:"source_dir,omitempty"`
	Instructions []string `json:"instructions"` // 无 Go 的旧镜像：在宿主机重建镜像的步骤
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
	u := &Updater{
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
	// 容器内自更新会用新二进制原地 exec（PID 不变），新进程的内存态是空的；
	// 旧进程退出前会写 done.json，这里读出来恢复成「更新完成」状态给页面展示（只报一次）
	if d, ok := readDoneFile(dataDir); ok {
		u.status = Status{
			Phase:        "done",
			Percent:      100,
			Message:      fmt.Sprintf("已更新到 v%s 并自动重启（原版本 v%s）", d.NewVersion, d.OldVersion),
			Instructions: []string{},
			UpdatedAt:    d.At,
		}
		_ = os.Remove(donePath(dataDir))
		// 新进程能读到 done.json 就说明新二进制真的跑起来了（没崩），
		// 写一份 ok.json 作为「这次更新确实生效」的凭据，方便排障时对照。
		_ = writeOKFile(dataDir, d.NewVersion, d.NewCommit)
	}
	// 回滚检查：替换二进制前会写「崩溃哨兵」。
	// 新进程启动时认领它；跑起来（监听成功）后再由 MarkHealthy 清掉。
	// 如果哨兵已被认领却没被清掉 → 上次那轮启动崩了 → 自动换回 .bak。
	checkCrashAndMaybeRollback(dataDir)
	return u
}

// crashSentinel 回滚哨兵的内容
type crashSentinel struct {
	NewVersion string `json:"new_version"`
	Claimed    bool   `json:"claimed"`
	ClaimedAt  string `json:"claimed_at"`
}

// crashSentinelPath 回滚哨兵文件
func crashSentinelPath(dataDir string) string {
	return filepath.Join(dataDir, "update", "crash.sentinel")
}

// writeCrashSentinel 替换二进制前写下哨兵（未认领状态）
func writeCrashSentinel(dataDir string, newVer string) error {
	p := crashSentinelPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(crashSentinel{NewVersion: newVer, Claimed: false})
	return os.WriteFile(p, b, 0o644)
}

// MarkHealthy 新进程成功跑起来（HTTP 已监听）后调用：清掉哨兵 = 这次更新成功。
// 由 main 在 ListenAndServe 之后调用。
func MarkHealthy(dataDir string) {
	clearCrashSentinel(dataDir)
}

// clearCrashSentinel 清掉回滚哨兵
func clearCrashSentinel(dataDir string) {
	_ = os.Remove(crashSentinelPath(dataDir))
}

// checkCrashAndMaybeRollback 处理回滚哨兵：
//
//	哨兵不存在        → 没有发生过更新（或上次更新成功），无事发生
//	哨警未认领        → 这是新版本的**首次**启动：认领它，等跑起来后由 MarkHealthy 清掉
//	哨兵已认领但还在  → 上一次启动认领后**没能跑起来**（崩了）→ 自动换回 .bak
//
// 为什么需要"认领"这一步：光有哨兵区分不了"新版本第一次启动"和"崩溃后重启"，
// 会在新版本明明正常时也把它换回去。
func checkCrashAndMaybeRollback(dataDir string) {
	defer func() {
		if r := recover(); r != nil {
			logx.Errorf("[update] 回滚检查异常: %v", r)
		}
	}()
	b, err := os.ReadFile(crashSentinelPath(dataDir))
	if err != nil {
		return // 没有哨兵 = 上次更新后新版本正常启动了
	}
	var s crashSentinel
	if json.Unmarshal(b, &s) != nil {
		return
	}
	if !s.Claimed {
		// 首次启动：认领哨兵。如果这轮启动崩了，哨兵会留在"已认领"状态，
		// 下一轮就能识别出"上次崩了"并回滚。
		s.Claimed = true
		s.ClaimedAt = time.Now().Format(time.RFC3339)
		nb, _ := json.Marshal(s)
		if err := os.WriteFile(crashSentinelPath(dataDir), nb, 0o644); err != nil {
			logx.Warnf("[update] 写入启动认领标记失败: %v", err)
		}
		logx.Infof("[update] 已认领更新启动哨兵（版本 %s），服务跑起来后自动清除", s.NewVersion)
		return
	}
	// 已认领却走到这里 = 上次启动崩了
	if runtime.GOOS == "windows" {
		logx.Warnf("[update] 检测到上次更新后启动失败，但 Windows 下无法替换运行中的程序文件，"+
			"请手动用 %s.exe.bak 回滚", "videodl")
		_ = os.Remove(crashSentinelPath(dataDir))
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	bak := exe + ".bak"
	if _, err := os.Stat(bak); err != nil {
		logx.Warnf("[update] 检测到上次更新后启动失败，但没有回滚副本 %s", bak)
		_ = os.Remove(crashSentinelPath(dataDir))
		return
	}
	// 把 .bak 复制回来（不能直接 rename .bak → exe：那会把唯一的回滚副本搬走）
	if err := copyFile(bak, exe); err != nil {
		logx.Errorf("[update] 自动回滚失败（可手动执行 cp %s %s）: %v", bak, exe, err)
		return
	}
	_ = os.Remove(crashSentinelPath(dataDir))
	logx.Errorf("[update] 检测到新版本（%s）启动失败，已自动回滚到旧版本（备份 %s）", s.NewVersion, bak)
	// 原地 exec 刚恢复的旧版本：容器不会退出，也不依赖 restart 策略；
	// exec 失败才退出进程，交给容器重启策略兜底
	time.Sleep(500 * time.Millisecond)
	if err := Relaunch(); err != nil {
		logx.Warnf("[update] 回滚后原地重启失败（%v），退出进程交给容器重启策略", err)
		os.Exit(0)
	}
}

// copyFile 复制文件（src → dst），保持可执行权限
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
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
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

// zipballURL 官方 OpenAPI 源码包地址（**URL 里不带令牌**）。
//
// 令牌改用 Authorization 请求头传递：Gitee 的 OpenAPI 与 GitHub 一致，
// 支持 "Authorization: token <令牌>" 形式（swagger 里虽然只登记了
// access_token 查询参数，但请求头同样被服务端接受）。
// 令牌不能拼在 URL 上——Go 的 *url.Error 会把完整 URL 带进错误文本，
// 那段文本随后会进容器 stdout 和对外的更新状态接口，等于把令牌公开。
func (u *Updater) zipballURL() string {
	return fmt.Sprintf("%s/api/v5/repos/%s/zipball?ref=%s",
		version.SiteBase, u.repo, url.QueryEscape(u.branch))
}

// zipballURLWithToken 官方 OpenAPI 源码包地址（令牌放查询串，老接口形态）。
// 只在请求头方式不被接受时作为回退使用；它生成的错误文本必须经过 redact() 脱敏。
func (u *Updater) zipballURLWithToken() string {
	return fmt.Sprintf("%s/api/v5/repos/%s/zipball?ref=%s&access_token=%s",
		version.SiteBase, u.repo, url.QueryEscape(u.branch), url.QueryEscape(u.token))
}

// accessTokenRe 匹配错误文本里的 access_token 查询参数（含 URL 编码后的值）
var accessTokenRe = regexp.MustCompile(`access_token=[^&\s"']+`)

// authTokenRe 匹配错误文本里的 Authorization 头令牌（万一被回显）
var authTokenRe = regexp.MustCompile(`(?i)(authorization:\s*(?:token|bearer)\s+)[^\s"']+`)

// redact 脱敏：错误文本里可能混进私有令牌（Gitee 的 access_token / Authorization 头）。
// 这些文本会写进容器 stdout，也会经 /api/update/status 返回给未鉴权的调用方，
// 所以所有对外/日志的错误文本都要先过一遍 redact。
func redact(s string) string {
	if s == "" {
		return s
	}
	s = accessTokenRe.ReplaceAllString(s, "access_token=***")
	s = authTokenRe.ReplaceAllString(s, "${1}***")
	return s
}

// redactTok 在 redact 之上再抹掉令牌原文本身。
// 正则只能覆盖已知形态；万一令牌以别的形式出现在错误文本里（比如被编码过、
// 或者被服务端原样回显），这里兜底：只要知道令牌值，就能把它替换掉。
func (u *Updater) redactTok(s string) string {
	s = redact(s)
	if u.token != "" {
		s = strings.ReplaceAll(s, u.token, "***")
	}
	return s
}

// redactErr 对错误做脱敏（保留错误信息本身，只抹掉令牌），便于统一处理
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(redact(err.Error()))
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

// Current 返回本地信息 + 上次检查的缓存（不联网）。
//
// force=true 只表示「忽略 10 分钟缓存窗口」，不代表要联网——联网由 Refresh 负责。
// 关键点：从未刷新过时（u.infoAt 为零值）必须返回 u.base()，
// 否则会返回零值 Info{}（版本号/模式全空），页面看着像没装版本信息。
func (u *Updater) Current(force bool) Info {
	u.mu.Lock()
	defer u.mu.Unlock()
	// 还没检查过：本地基础信息永远有效，先返回它
	if u.infoAt.IsZero() {
		return u.base()
	}
	// 缓存窗口内且不强制：用上次结果
	if !force && time.Since(u.infoAt) < 10*time.Minute {
		return copyInfo(u.info)
	}
	// 缓存过期（或强制）：仍返回上次结果，由前端触发 Refresh 拿最新数据。
	// 这里不联网，避免持锁时做网络 IO。
	return copyInfo(u.info)
}

// copyInfo 拷贝一份 Info（含 Reasons slice 的底层数组），
// 避免调用方拿到与内部状态共享的 slice。
func copyInfo(in Info) Info {
	out := in
	if len(in.Reasons) > 0 {
		out.Reasons = append([]string(nil), in.Reasons...)
	}
	return out
}

// snapshot 持锁拷贝一份当前缓存的 Info。
// 读侧必须加锁：Refresh 会整体替换 u.info（含 Reasons slice），
// 无锁读会与写侧形成 data race，slice header 撕裂会直接 panic。
func (u *Updater) snapshot() Info {
	u.mu.Lock()
	defer u.mu.Unlock()
	return copyInfo(u.info)
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
		info.Error = fmt.Sprintf("检查失败：%s；%s", u.redactTok(err1.Error()), u.redactTok(err2.Error()))
	} else if err1 != nil && remoteVer == "" {
		info.Error = "读取提交信息失败：" + u.redactTok(err1.Error())
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
		return redactErr(err)
	}
	req.Header.Set("User-Agent", u.ua())
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.client.Do(req)
	if err != nil {
		return redactErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// Gitee 要求登录时回的是 HTML：直接丢给 JSON 解码只会得到一句含糊的
	// "invalid character '<'"，这里提前识别，给出能看懂的提示。
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		return errors.New("Gitee 返回了网页而不是接口数据（可能需要登录/验证）")
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst)
}

func (u *Updater) getText(ctx context.Context, url string, limit int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", redactErr(err)
	}
	req.Header.Set("User-Agent", u.ua())
	resp, err := u.client.Do(req)
	if err != nil {
		return "", redactErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// Gitee 在要求登录/验证时回的是 HTML（状态码仍是 200）。
	// 不拦的话，读 VERSION 会把一整段登录页当成版本号，界面上显示成乱码。
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		return "", errors.New("Gitee 返回了网页而不是文本内容（可能需要登录/验证）")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return "", err
	}
	// Content-Type 有时不可靠（代理会改写），再按内容嗅探一次兜底
	if looksLikeHTML(b) {
		return "", errors.New("Gitee 返回了网页而不是文本内容（可能需要登录/验证）")
	}
	return string(b), nil
}

// looksLikeHTML 判断内容开头是不是 HTML 文档
func looksLikeHTML(b []byte) bool {
	s := strings.ToLower(strings.TrimSpace(string(b)))
	return strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html")
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
		// 更新流程里的任何 panic 都不能带崩整个服务（会连带拖垮采集/下载）。
		// 这里兜住 panic，把状态置为失败并记日志，页面能显示原因。
		defer func() {
			if r := recover(); r != nil {
				logx.Errorf("[update] 更新流程 panic: %v", r)
				u.setStatus(func(s *Status) {
					s.Phase = "failed"
					s.Message = "更新失败：内部错误（已捕获 panic）"
					s.Error = fmt.Sprintf("panic: %v", r)
				})
			}
		}()
		if err := u.run(context.Background()); err != nil {
			// 错误文本会进容器日志，也会经 /api/update/status 返回给未鉴权调用方：
			// 必须脱敏，否则私有令牌会泄漏出去。
			msg := u.redactTok(err.Error())
			logx.Warnf("[update] 更新失败: %s", msg)
			u.setStatus(func(s *Status) {
				s.Phase = "failed"
				s.Error = msg
				s.Message = "更新失败：" + msg
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

	// 先看有没有 Go 工具链（2026-10-21 起运行基座是 golang）：
	//   有 → 容器内编译：源码与构建缓存都放容器内部路径，宿主机碰不到（防提权）；
	//   没有 → 旧镜像：源码必须放数据目录，宿主机重建镜像时要用。
	goBin, lookErr := exec.LookPath("go")
	work := dir
	if lookErr == nil {
		work = u.buildWorkspace()
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return fmt.Errorf("创建更新工作目录失败: %w", err)
	}

	// 1) 下载源码包
	zipPath := filepath.Join(work, "main.zip")
	if err := u.download(ctx, zipPath); err != nil {
		return err
	}

	// 2) 解压
	u.setStatus(func(s *Status) { s.Phase = "extracting"; s.Message = "正在解压源码…"; s.Percent = 70 })
	srcDir, commitHint, err := u.extract(zipPath, work)
	_ = os.Remove(zipPath)
	if err != nil {
		return err
	}

	srcOK := fileExists(filepath.Join(srcDir, "go.mod")) &&
		fileExists(filepath.Join(srcDir, "internal", "web", "static", "index.html"))
	if !srcOK {
		return errors.New("下载的源码包不完整（缺少 go.mod 或前端产物 internal/web/static）")
	}

	// 3) 镜像自带 Go 工具链就在容器内编译：
	//    go build → 校验 → 原子替换 → 原地 exec 新进程（PID 不变，容器不退出）。
	//    旧的精简镜像（找不到 go 命令）回退为「落源码 + 提示宿主机重建」。
	if lookErr != nil {
		u.setStatus(func(s *Status) {
			s.Phase = "staged"
			s.Percent = 100
			s.SourceDir = srcDir
			s.Message = "最新源码已下载到数据目录，请在宿主机重建镜像（当前镜像不含 Go 工具链）"
			s.Instructions = dockerInstructions(srcDir, u.dataDir)
		})
		return nil
	}
	return u.buildAndRestart(ctx, srcDir, work, commitHint, goBin)
}

// buildWorkspace 返回本次更新使用的构建工作目录。
//
// 为什么不能放在数据目录里：docker-compose 把 ./videodata 挂进容器，
// 宿主侧有写权限的账号可以替换源码，再让容器以 root 编译它 —— 这是提权路径。
// 所以「编译用的源码 + go 模块缓存 + go 构建缓存」都放容器内部路径 /tmp/vdl-update，
// 宿主侧碰不到。代价：容器重建后模块缓存要重新下载（安全优先）。
//
// Windows 宿主直跑时 /tmp 不可用（或容器只读 rootfs），回退到数据目录并打警告。
func (u *Updater) buildWorkspace() string {
	const preferred = "/tmp/vdl-update"
	if runtime.GOOS != "windows" {
		if err := os.MkdirAll(preferred, 0o755); err == nil {
			// 真写一次确认可写（只读 rootfs / 只挂载了部分目录都会失败）
			probe := filepath.Join(preferred, ".probe")
			if err := os.WriteFile(probe, []byte("1"), 0o644); err == nil {
				_ = os.Remove(probe)
				return preferred
			}
		}
	}
	fallback := filepath.Join(u.dataDir, "update", "build")
	if err := os.MkdirAll(fallback, 0o755); err != nil {
		return filepath.Join(u.dataDir, "update")
	}
	logx.Warnf("[update] 容器内临时目录不可用，构建工作目录回退到数据目录 %s（宿主侧可写，存在源码被替换的风险）", fallback)
	return fallback
}

// buildAndRestart 在容器内编译新二进制、校验、原子替换当前程序文件，
// 然后原地 exec 新二进制重启（PID 不变，容器不会退出，不依赖 restart 策略）。
//
// work 是容器内的构建工作目录（源码 + go 缓存都在里面，宿主机碰不到）。
// commitHint 是从归档首层目录名解析出的真实提交号（可能为空）。
func (u *Updater) buildAndRestart(ctx context.Context, srcDir, work, commitHint, goBin string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取当前程序路径失败: %w", err)
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}

	// 注入版本信息：版本号以源码包里的 VERSION 为准
	newVer := ""
	if b, err := os.ReadFile(filepath.Join(srcDir, "VERSION")); err == nil {
		newVer = strings.TrimSpace(string(b))
	}
	// commit 优先用归档首层目录名里解析出来的真实 sha；
	// 用户没先点「检查更新」直接点「立即更新」时 u.info 是零值，
	// 那时只能用 zip 目录名（Gitee 归档形如 video-<sha>/）兜底，
	// 实在拿不到才留空（比注入一个错的 sha 好）。
	info := u.snapshot()
	if newVer == "" {
		newVer = info.LatestVersion
	}
	commit := commitHint
	if commit == "" {
		commit = info.LatestCommit
	}
	buildTime := time.Now().UTC().Format(time.RFC3339)

	u.setStatus(func(s *Status) {
		s.Phase = "building"
		s.Percent = 85
		s.Message = "正在容器内编译新版本（弱机上可能要几分钟）…"
	})

	newBin := exe + ".new"
	_ = os.Remove(newBin)

	buildCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	ldflags := fmt.Sprintf("-s -w -X videoDownLoad/internal/version.Version=%s "+
		"-X videoDownLoad/internal/version.Commit=%s "+
		"-X videoDownLoad/internal/version.BuildTime=%s", newVer, commit, buildTime)
	// -p 2 + GOGC=50：压低编译并发与内存，避免弱机容器被 OOM（modernc/sqlite 编译很吃内存）
	cmd := exec.CommandContext(buildCtx, goBin, "build", "-trimpath", "-p", "2",
		"-ldflags", ldflags, "-o", newBin, ".")
	cmd.Dir = srcDir
	// 模块与构建缓存放容器内非共享路径：数据目录是宿主机可写卷，
	// 宿主侧能往缓存里塞东西污染编译结果。放在 work 目录里宿主机碰不到。
	env := append(os.Environ(),
		"CGO_ENABLED=0",
		"GOGC=50",
		"GOMODCACHE="+filepath.Join(work, "gomodcache"),
		"GOCACHE="+filepath.Join(work, "gocache"),
	)
	// 国内网络：proxy.golang.org / sum.golang.org 都连不上，
	// 竖线分隔 = 前一个代理出错（超时/DNS 抖动）也立刻换下一个；
	// GOSUMDB=off 后 go 仍按仓库里的 go.sum 校验哈希。
	if os.Getenv("GOPROXY") == "" {
		env = append(env, "GOPROXY=https://goproxy.cn|https://mirrors.aliyun.com/goproxy/|https://goproxy.io|direct")
	}
	if os.Getenv("GOSUMDB") == "" {
		env = append(env, "GOSUMDB=off")
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(newBin)
		return fmt.Errorf("容器内编译失败: %w（%s）", err, tailString(string(out), 1500))
	}

	// 自检：新二进制跑得起来、版本号对得上才替换
	vout, verr := exec.CommandContext(ctx, newBin, "-version").CombinedOutput()
	if verr != nil || !strings.Contains(string(vout), newVer) {
		_ = os.Remove(newBin)
		return fmt.Errorf("新二进制自检失败: %v %s", verr, tailString(string(vout), 500))
	}

	// 替换前先把旧二进制留一份 .bak：自更新没有回滚余地的话，
	// 新版本一启动就崩（容器 crash-loop）就只能重新拉镜像救。
	// 失败不阻断更新（只是没有回滚手段而已）。
	bakPath := exe + ".bak"
	if err := backupBinary(exe, bakPath); err != nil {
		logx.Warnf("[update] 备份旧二进制失败（更新继续，但没有回滚副本）: %v", err)
	} else {
		logx.Infof("[update] 旧二进制已备份到 %s", bakPath)
	}

	// 写回滚哨兵：新进程启动时认领它，跑起来后清除；
	// 如果它认领了却没跑起来（崩了），下一轮启动会自动换回 .bak。
	if err := writeCrashSentinel(u.dataDir, newVer); err != nil {
		logx.Warnf("[update] 写回滚哨兵失败（更新继续，但崩溃后不会自动回滚）: %v", err)
	}

	// rename 是原子替换，Linux 下覆盖正在运行的可执行文件也没问题
	// （旧进程继续用旧 inode，新进程拿到的就是新文件）
	if err := os.Rename(newBin, exe); err != nil {
		_ = os.Remove(newBin)
		return fmt.Errorf("替换程序文件失败: %w", err)
	}

	oldVer := version.Version
	u.setStatus(func(s *Status) {
		s.Phase = "restarting"
		s.Percent = 100
		s.Message = "编译完成，正在原地重启到新版本…（如新版本启动异常，可用容器内 " + bakPath + " 回滚）"
		s.Instructions = []string{}
	})
	// 落一份结果：新进程启动时在 New() 里读出来恢复「更新完成」状态
	_ = writeDoneFile(u.dataDir, oldVer, newVer, commit)

	logx.Infof("[update] 已替换二进制（v%s → v%s），请求原地重启（PID 不变，容器不会退出）", oldVer, newVer)
	time.Sleep(2 * time.Second) // 给前端最后一轮轮询留点窗口
	requestRestart()            // SIGTERM：走 main 的优雅退出，收尾后 exec 新二进制
	return nil                  // 不会执行到这里
}

// backupBinary 把当前二进制复制一份到 dst，作为回滚副本。
//
// 为什么用复制而不是硬链接：硬链接与原文件共享 inode，
// 任何「就地覆写」原路径的操作（比如 os.WriteFile 打开 O_TRUNC）
// 会把 .bak 一起改掉，回滚副本就悄悄失效了。
// 复制出的 .bak 是独立文件，任何后续操作都动不到它。
func backupBinary(src, dst string) error {
	_ = os.Remove(dst)
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
	if _, err := io.Copy(out, in); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// tailString 取字符串末尾 n 字节（编译报错要看的是最后几行）
func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// updateDone 自更新结果：旧进程退出前写，新进程 New() 读
type updateDone struct {
	OldVersion string `json:"old_version"`
	NewVersion string `json:"new_version"`
	NewCommit  string `json:"new_commit"`
	At         string `json:"at"`
}

func donePath(dataDir string) string {
	return filepath.Join(dataDir, "update", "done.json")
}

func writeDoneFile(dataDir, oldVer, newVer, commit string) error {
	b, err := json.Marshal(updateDone{
		OldVersion: oldVer, NewVersion: newVer, NewCommit: commit,
		At: time.Now().Format("2006-01-02 15:04:05"),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(donePath(dataDir), b, 0o644)
}

func readDoneFile(dataDir string) (updateDone, bool) {
	var d updateDone
	b, err := os.ReadFile(donePath(dataDir))
	if err != nil || json.Unmarshal(b, &d) != nil || d.NewVersion == "" {
		return updateDone{}, false
	}
	return d, true
}

// okPath 新版本成功启动的标记文件
func okPath(dataDir string) string {
	return filepath.Join(dataDir, "update", "ok.json")
}

// writeOKFile 写「新版本已成功启动」的标记
func writeOKFile(dataDir, newVer, commit string) error {
	b, err := json.Marshal(updateDone{
		OldVersion: version.Version, NewVersion: newVer, NewCommit: commit,
		At: time.Now().Format("2006-01-02 15:04:05"),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(okPath(dataDir), b, 0o644)
}

// download 下载源码包。
//
// 注意：Gitee 的 /repository/archive/<branch>.zip 对「浏览器 UA」会返回登录/验证网页，
// 只有命令行 UA 才直接给 zip（2026-10-20 实测）。所以这里分两条路：
//  1. 配了 Gitee 令牌：走官方 /api/v5/repos/<repo>/zipball（最稳）；
//     令牌优先用 Authorization 头传（URL 里不带令牌，避免令牌随错误文本外泄），
//     请求头方式不被接受时再回退到 URL 查询串形式（该形式的错误文本一律脱敏）。
//  2. 没配令牌：匿名 archive 地址 + CLI 风格 UA（拿到 HTML 时给出明确错误提示）。
func (u *Updater) download(ctx context.Context, dst string) error {
	type candidate struct {
		url, ua, note string
		useTokenQuery bool // 令牌放 URL 查询串（兼容回退；错误文本必须脱敏）
	}
	var cands []candidate
	if u.token != "" {
		// 主路径：Authorization 头带令牌，URL 干净
		cands = append(cands, candidate{u.zipballURL(), u.ua(), "官方接口", false})
		// 兼容回退：令牌放查询串（老接口形态）
		cands = append(cands, candidate{u.zipballURLWithToken(), u.ua(), "官方接口(兼容)", true})
	}
	cands = append(cands, candidate{u.archiveURL(), cliUserAgent, "匿名下载", false})

	var lastErr error
	for _, c := range cands {
		u.setStatus(func(s *Status) { s.Message = "正在下载源码包（Gitee " + c.note + "）…" })
		err := u.downloadOne(ctx, c.url, c.ua, dst, c.useTokenQuery)
		if err == nil {
			return nil
		}
		lastErr = err
		// 脱敏后再写日志：出错时错误文本里可能带 URL（含令牌）
		logx.Warnf("[update] 下载源码包失败（%s）：%s", c.note, u.redactTok(err.Error()))
	}
	if lastErr == nil {
		lastErr = errors.New("未知错误")
	}
	return fmt.Errorf("下载源码包失败: %w（可用环境变量 VDL_UPDATE_TOKEN 提供 Gitee 令牌走官方接口）", errors.New(u.redactTok(lastErr.Error())))
}

func (u *Updater) downloadOne(ctx context.Context, url, ua, dst string, tokenInQuery bool) error {
	// 下载可能超过 client 的 20s 超时，这里用独立的长超时请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	// 令牌优先走请求头：URL 一旦带上令牌，Go 的 *url.Error 会把它带进错误文本，
	// 那段文本会进容器日志和对外接口，属于泄漏。
	if u.token != "" && !tokenInQuery {
		req.Header.Set("Authorization", "token "+u.token)
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
		// 这里必须脱敏：*url.Error 里含完整 URL
		return redactErr(err)
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

// extract 解压 zip 到 <dir>/src（覆盖旧的），自动去掉压缩包里那一层仓库名目录。
//
// 返回解压后的目录，以及从归档首层目录名里解析出的提交号（可能为空）。
//
// 安全限制（防 zip 炸弹 / 伪造头部）：
//   - 单文件：头部声明的大小可能被伪造，所以按**实际写入字节数**判定，写满了上限就报错；
//   - 总量：所有文件解压字节数累加，超过 maxTotalBytes 立刻中止；
//   - 文件数：超过 maxFileCount 立刻中止（防大量小文件耗尽磁盘/inode）。
func (u *Updater) extract(zipPath, dir string) (string, string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", "", fmt.Errorf("源码包不是有效的 zip: %w", err)
	}
	defer zr.Close()

	tmpDir := filepath.Join(dir, "src.new")
	if err := os.RemoveAll(tmpDir); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", "", err
	}

	var totalWritten int64
	var fileCount int
	commitHint := ""

	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		// 去掉第一层目录（GitHub 的 zip 一定是 video-main/xxx）
		if i := strings.IndexByte(name, '/'); i >= 0 {
			top := name[:i]
			// 首层目录名形如 video-<sha>：从中解析真实提交号。
			// 用户没先点「检查更新」直接点「立即更新」时 u.info 是零值，
			// 只有靠这个才能把正确的 sha 注入新二进制。
			if commitHint == "" {
				commitHint = commitFromArchiveDir(top)
			}
			name = name[i+1:]
		}
		if name == "" {
			continue
		}
		// 防 zip-slip：绝对路径、..、盘符一律拒绝
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || strings.Contains(clean, ":") {
			return "", "", fmt.Errorf("源码包里有非法路径: %s", f.Name)
		}
		target := filepath.Join(tmpDir, clean)
		if !strings.HasPrefix(target, tmpDir+string(os.PathSeparator)) {
			return "", "", fmt.Errorf("源码包里有非法路径: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", "", err
			}
			continue
		}
		// 头部声明只是快速筛（大文件直接拒），真正的判定在 extractFile 里按实际字节数来
		if f.UncompressedSize64 > maxFileBytes {
			return "", "", fmt.Errorf("源码包里有超大文件: %s", f.Name)
		}
		fileCount++
		if fileCount > maxFileCount {
			return "", "", fmt.Errorf("源码包里文件过多（超过 %d 个）", maxFileCount)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", "", err
		}
		n, err := extractFile(f, target)
		if err != nil {
			return "", "", fmt.Errorf("解压 %s 失败: %w", f.Name, err)
		}
		totalWritten += n
		if totalWritten > maxTotalBytes {
			return "", "", fmt.Errorf("源码包解压后总大小超过上限（%s）", humanBytes(maxTotalBytes))
		}
	}

	final := filepath.Join(dir, "src")
	_ = os.RemoveAll(final)
	if err := os.Rename(tmpDir, final); err != nil {
		return "", "", err
	}
	return final, commitHint, nil
}

// commitFromArchiveDir 从归档首层目录名解析提交号。
// Gitee 的归档首层目录形如 video-<sha>/（sha 为 40 位十六进制）；
// 匿名下载的分支包是 video-master/，解析不出 sha，返回空串。
func commitFromArchiveDir(top string) string {
	// 形如 video-<sha>
	i := strings.LastIndexByte(top, '-')
	if i < 0 {
		return ""
	}
	sha := top[i+1:]
	if len(sha) < 7 || len(sha) > 40 {
		return ""
	}
	for _, c := range sha {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return ""
		}
	}
	return sha
}

// extractFile 解压单个文件，返回实际写入的字节数。
//
// 用 LimitReader 限制最多读 maxFileBytes+1 字节：读到第 maxFileBytes+1 字节
// 就说明文件超过单文件上限，直接报错——不能静默截断，
// 否则会产出一个「半个 .go 文件」，编译报错很难看出真实原因。
func extractFile(f *zip.File, target string) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(rc, maxFileBytes+1))
	// 必须先关闭文件再删除：文件句柄还开着时删除会失败（Windows 上直接报「拒绝访问」），
	// 半截文件就会留在源码目录里。
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(target)
		return n, err
	}
	if n > maxFileBytes {
		// 写了一半的文件不能留着：宁可删掉让更新失败，也不要让编译器去读半截源码
		if rmErr := os.Remove(target); rmErr != nil {
			return n, fmt.Errorf("文件超过单文件上限（%s），且清理半截文件失败: %w", humanBytes(maxFileBytes), rmErr)
		}
		return n, fmt.Errorf("文件超过单文件上限（%s）", humanBytes(maxFileBytes))
	}
	return n, nil
}

// dockerInstructions 生成「宿主机重建镜像」的提示。
// dataDir 用实际的数据目录渲染：写死 /app/videodata 会在 data_dir 被改过时误导用户。
func dockerInstructions(srcDir, dataDir string) []string {
	return []string{
		"最新源码已下载到（容器内路径）：" + srcDir,
		"容器内数据目录：" + dataDir + "（宿主机上就是 docker-compose 里挂载的那个 volume）",
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

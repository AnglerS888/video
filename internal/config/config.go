// Package config 集中管理程序配置：所有可调项都在配置文件里，不再用命令行参数。
//
// 文件格式是 YAML（原生支持 # 注释）；旧版的 JSON（允许 // 与 /* */ 注释）仍然能读，
// 因为 YAML 是 JSON 的超集，读取时按 YAML 解析即可。
//
// 配置分两个文件：
//   - config.yml：需要重启生效的项（端口、数据目录、日志、内存、数据库）
//   - <数据目录>/ui.yml：页面可改、保存即生效的项（采集 / 下载 / 封面 / ffmpeg）
//
// 配置文件查找顺序（config.yml）：
//  1. 环境变量 VDL_CONFIG 指定的路径；
//  2. 当前目录 ./config.yml（兼容 ./config.json）；
//  3. 程序所在目录/config.yml（兼容 config.json）；
//  4. 当前目录/videodata/config.yml（容器里通常用 VDL_CONFIG 指到这里）。
//
// lowmem 模式的处理：先按 lowmem 取一套低配默认值，再把配置文件里**实际写了**的字段覆盖上去，
// 这样既保留「一键低配」，又允许逐项覆盖（等价于以前「-lowmem 只改没显式指定的参数」）。
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// FileName 配置文件名（需要重启生效的项）
const FileName = "config.yml"

// LegacyFileName 旧版 JSON 配置名：仍然能读，保存时会写成 YAML
const LegacyFileName = "config.json"

// Config 全部配置。字段含义见 README「配置参数」。
type Config struct {
	Server     Server   `json:"server" yaml:"server"`
	DataDir    string   `json:"data_dir" yaml:"data_dir"`
	LogLevel   string   `json:"log_level" yaml:"log_level"`
	LowMem     bool     `json:"lowmem" yaml:"lowmem"`
	MemLimitMB int      `json:"mem_limit_mb" yaml:"mem_limit_mb"`
	MaxProcs   int      `json:"max_procs" yaml:"max_procs"`
	Collect    Collect  `json:"collect" yaml:"collect"`
	Download   Download `json:"download" yaml:"download"`
	Cover      Cover    `json:"cover" yaml:"cover"`
	FFmpeg     FFmpeg   `json:"ffmpeg" yaml:"ffmpeg"`
	Auth       Auth     `json:"auth" yaml:"auth"`
	DB         DB       `json:"db" yaml:"db"`
	dataDirSet bool     // 配置文件里是否显式写了 data_dir
	path       string   // 实际加载的文件路径
}

// Server HTTP 服务
type Server struct {
	Addr string `json:"addr" yaml:"addr"`
}

// Auth 访问控制（存在 ui.yml，信息页 → 运行配置可改，保存即生效）。
// Password 为空表示不启用：站点与接口都不需要密码。
type Auth struct {
	Password string `json:"password" yaml:"password"`
}

// Collect 采集
type Collect struct {
	Pages     int    `json:"pages" yaml:"pages"`           // 每个接口的分页并发
	Sources   int    `json:"sources" yaml:"sources"`       // 同时采集的接口数
	IncrEvery string `json:"incr_every" yaml:"incr_every"` // 定时增量间隔，Go duration 语法：1h / 30m
	IncrPages int    `json:"incr_pages" yaml:"incr_pages"` // 定时增量每次同步前 N 页，0=不限
}

// Download 下载与磁盘保护
type Download struct {
	Workers        int `json:"workers" yaml:"workers"`                 // 同时下载的集数（实际固定串行）
	SegConcurrency int `json:"seg_concurrency" yaml:"seg_concurrency"` // 单任务分片并发
	SegBudget      int `json:"seg_budget" yaml:"seg_budget"`           // 全局分片并发上限，0=workers×seg_concurrency
	MinFreeMB      int `json:"min_free_mb" yaml:"min_free_mb"`         // 磁盘剩余空间保护阈值（MiB），0=关闭
}

// Cover 封面
type Cover struct {
	Workers     int    `json:"workers" yaml:"workers"`
	Concurrency int    `json:"concurrency" yaml:"concurrency"`
	ImgProxy    string `json:"img_proxy" yaml:"img_proxy"`
}

// FFmpeg 合并/转码
type FFmpeg struct {
	Threads  int  `json:"threads" yaml:"threads"`
	Reencode bool `json:"reencode" yaml:"reencode"`
	Verify   bool `json:"verify" yaml:"verify"`
}

// DB SQLite
type DB struct {
	CacheMB   int `json:"cache_mb" yaml:"cache_mb"`
	MmapMB    int `json:"mmap_mb" yaml:"mmap_mb"`
	Conns     int `json:"conns" yaml:"conns"`
	PageSize  int `json:"page_size" yaml:"page_size"`
	VacuumPct int `json:"vacuum_pct" yaml:"vacuum_pct"`
}

// Defaults 常规默认值（与旧命令行参数默认值保持一致）
func Defaults() *Config {
	return &Config{
		Server:     Server{Addr: ":5200"},
		DataDir:    "./videodata",
		LogLevel:   "info",
		LowMem:     false,
		MemLimitMB: 0,
		MaxProcs:   0,
		Collect:    Collect{Pages: 6, Sources: 8, IncrEvery: "1h", IncrPages: 5},
		Download:   Download{Workers: 1, SegConcurrency: 16, SegBudget: 0, MinFreeMB: 2048},
		Cover:      Cover{Workers: 2, Concurrency: 4},
		FFmpeg:     FFmpeg{Threads: 0, Reencode: true, Verify: true},
		DB:         DB{CacheMB: 64, MmapMB: 1024, Conns: 4, PageSize: 0, VacuumPct: 15},
	}
}

// LowMemDefaults 低配模式（NAS / 4G 内存）默认值
func LowMemDefaults() *Config {
	c := Defaults()
	c.LowMem = true
	c.MemLimitMB = 768
	n := runtime.NumCPU()
	if n > 3 {
		n = 3
	}
	c.MaxProcs = n
	c.Collect.Pages = 3
	c.Collect.Sources = 2
	c.Download.Workers = 1
	c.Download.SegConcurrency = 6
	c.Cover.Workers = 1
	c.Cover.Concurrency = 2
	c.FFmpeg.Threads = 1
	c.FFmpeg.Reencode = false
	c.DB.CacheMB = 24
	c.DB.MmapMB = 256
	c.DB.Conns = 2
	return c
}

// Load 查找并加载配置文件；找不到时在可写目录生成一份默认配置。
// 返回配置、实际路径、错误（读失败/解析失败会返回默认值 + 错误，调用方可继续启动）。
func Load() (*Config, string, error) {
	// VDL_CONFIG 指定了路径就以它为准：不存在就在那里生成一份（容器里指向 volume）
	if p := strings.TrimSpace(os.Getenv("VDL_CONFIG")); p != "" {
		if !fileExists(p) {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
				def := baseDefaults()
				if err := Save(p, def); err == nil {
					cfg := def
					cfg.path = p
					return cfg, p, nil
				}
			}
		} else {
			return loadFile(p)
		}
	}

	path := findConfig()
	if path == "" {
		// 没有任何配置文件：生成一份默认的
		p, err := writeDefault()
		if err != nil {
			return baseDefaults(), "", fmt.Errorf("未找到配置文件，且生成默认配置失败: %w", err)
		}
		cfg := baseDefaults()
		cfg.path = p
		return cfg, p, nil
	}

	return loadFile(path)
}

// lowMemFromEnv VDL_LOWMEM=1/true 时强制套用低配预设。
// 预设值写死在代码里（LowMemDefaults），所以不需要额外的「低配配置文件」。
func lowMemFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VDL_LOWMEM"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// baseDefaults 没有配置文件 / 生成默认配置时用的基准值
func baseDefaults() *Config {
	if lowMemFromEnv() {
		return LowMemDefaults()
	}
	return Defaults()
}

// loadFile 读取并解析指定配置文件（YAML；旧版 JSON 也能解析）
func loadFile(path string) (*Config, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Defaults(), path, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	text := normalizeInput(string(raw))

	// 先看 lowmem（文件里写了 true，或 VDL_LOWMEM 强制），决定基础默认值用哪一套
	base := Defaults()
	if probeLowMem(text) || lowMemFromEnv() {
		base = LowMemDefaults()
	}
	// 再用文件里实际写了的字段覆盖（嵌套结构逐字段合并，没写的保留基础默认值）
	if err := applyDoc(text, base); err != nil {
		return Defaults(), path, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	base.path = path
	base.dataDirSet = hasDataDirKey(text)
	base.normalize()
	return base, path, nil
}

// Path 实际加载的配置文件路径
func (c *Config) Path() string { return c.path }

// DataDirExplicit 配置文件里是否显式指定了 data_dir（决定要不要自动找旧数据目录）
func (c *Config) DataDirExplicit() bool { return c.dataDirSet }

// IncrEveryDuration 解析 incr_every（Go duration 语法：1h / 30m / 1h30m）；
// 空值或写错时回退 1 小时。
func (c *Config) IncrEveryDuration() time.Duration {
	s := strings.TrimSpace(c.Collect.IncrEvery)
	if s == "" {
		return time.Hour
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return time.Hour
	}
	return d
}

// Normalize 导出给「信息」页表单保存用：把越界/空值修正成合法值，
// 前端不该因为填了个 0 就把服务写坏。
func (c *Config) Normalize() { c.normalize() }

// Check 校验无法自动修正的项（日志级别、定时增量间隔、监听地址）。
// 返回的错误文案直接展示给用户，所以写成人话并带上字段名。
func (c *Config) Check() error {
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("日志级别只能是 debug / info / warn / error")
	}
	if s := strings.TrimSpace(c.Collect.IncrEvery); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("定时增量间隔格式不对，应写成 30m / 1h / 6h 这种")
		}
		if d <= 0 {
			return fmt.Errorf("定时增量间隔必须大于 0")
		}
	}
	if strings.TrimSpace(c.Server.Addr) == "" {
		return fmt.Errorf("监听地址不能为空，例如 :5200")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("数据目录不能为空，例如 ./videodata")
	}
	if utf8.RuneCountInString(c.Auth.Password) > 128 {
		return fmt.Errorf("访问密码最长 128 个字符")
	}
	return nil
}

// normalize 兜底修正非法值，避免配置文件写错导致启动失败或行为异常
func (c *Config) normalize() {
	if strings.TrimSpace(c.Server.Addr) == "" {
		c.Server.Addr = ":5200"
	}
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = "./videodata"
	}
	if strings.TrimSpace(c.LogLevel) == "" {
		c.LogLevel = "info"
	}
	if c.Collect.Pages < 1 {
		c.Collect.Pages = 1
	}
	if c.Collect.Sources < 1 {
		c.Collect.Sources = 1
	}
	if c.Collect.IncrPages < 0 {
		c.Collect.IncrPages = 0
	}
	if c.Download.Workers < 1 {
		c.Download.Workers = 1
	}
	if c.Download.SegConcurrency < 1 {
		c.Download.SegConcurrency = 1
	}
	if c.Cover.Workers < 1 {
		c.Cover.Workers = 1
	}
	if c.Cover.Concurrency < 1 {
		c.Cover.Concurrency = 1
	}
	if c.DB.Conns < 1 {
		c.DB.Conns = 1
	}
	if c.DB.CacheMB < 0 {
		c.DB.CacheMB = 0
	}
	if c.DB.MmapMB < 0 {
		c.DB.MmapMB = 0
	}
	if c.DB.VacuumPct < 0 {
		c.DB.VacuumPct = 0
	}
	if c.Download.MinFreeMB < 0 {
		c.Download.MinFreeMB = 0
	}
	if c.MemLimitMB < 0 {
		c.MemLimitMB = 0
	}
	if c.MaxProcs < 0 {
		c.MaxProcs = 0
	}
	// 访问密码：去掉首尾空白（表单里误带的空格会让登录怎么输都不对），
	// 只去掉空白，不截断——超长由 Check 明确报错，避免静默改掉用户的密码。
	c.Auth.Password = strings.TrimSpace(c.Auth.Password)
	// 上限兜底：表单里手输 999 也不至于把机器拖死（正常用法碰不到这些数）
	c.Collect.Pages = clampInt(c.Collect.Pages, 1, 64)
	c.Collect.Sources = clampInt(c.Collect.Sources, 1, 64)
	c.Collect.IncrPages = clampInt(c.Collect.IncrPages, 0, 1000)
	c.Download.Workers = clampInt(c.Download.Workers, 1, 8)
	c.Download.SegConcurrency = clampInt(c.Download.SegConcurrency, 1, 64)
	c.Download.SegBudget = clampInt(c.Download.SegBudget, 0, 512)
	c.Download.MinFreeMB = clampInt(c.Download.MinFreeMB, 0, 1<<20)
	c.Cover.Workers = clampInt(c.Cover.Workers, 1, 8)
	c.Cover.Concurrency = clampInt(c.Cover.Concurrency, 1, 16)
	c.FFmpeg.Threads = clampInt(c.FFmpeg.Threads, 0, 64)
	c.DB.CacheMB = clampInt(c.DB.CacheMB, 0, 4096)
	c.DB.MmapMB = clampInt(c.DB.MmapMB, 0, 65536)
	c.DB.Conns = clampInt(c.DB.Conns, 1, 32)
	c.DB.VacuumPct = clampInt(c.DB.VacuumPct, 0, 100)
	c.MemLimitMB = clampInt(c.MemLimitMB, 0, 1<<20)
	c.MaxProcs = clampInt(c.MaxProcs, 0, 256)
	if c.DB.PageSize != 0 {
		c.DB.PageSize = clampInt(c.DB.PageSize, 512, 65536)
		// SQLite 只接受 2 的幂作为 page_size，写错会被静默忽略；
		// 而「设置后读回来比对」的判断逻辑在非 2 的幂下永远不相等 → 每次启动白跑一次全库 VACUUM。
		// 这里直接把非法值归零（= 用默认 4096），避免反复触发。
		if !isPowerOfTwo(c.DB.PageSize) {
			c.DB.PageSize = 0
		}
	}
}

// isPowerOfTwo 判断 n 是否为 2 的幂（n > 0）
func isPowerOfTwo(n int) bool {
	return n > 0 && n&(n-1) == 0
}

// clampInt 把 v 限制在 [lo, hi] 区间
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// findConfig 按顺序找配置文件（新名 config.yml 优先，其次旧名 config.json），找不到返回空串
func findConfig() string {
	var cands []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		cands = append(cands, filepath.Join(dir, FileName), filepath.Join(dir, LegacyFileName))
	}
	cands = append(cands,
		FileName, LegacyFileName,
		filepath.Join("videodata", FileName), filepath.Join("videodata", LegacyFileName),
	)
	for _, p := range cands {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// writeDefault 依次尝试 当前目录 → 程序目录 → videodata 目录，写一份默认配置（config.yml）
func writeDefault() (string, error) {
	var dirs []string
	dirs = append(dirs, ".")
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	dirs = append(dirs, "videodata")

	var lastErr error
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			lastErr = err
			continue
		}
		p := filepath.Join(dir, FileName)
		if fileExists(p) {
			return p, nil
		}
		if err := Save(p, baseDefaults()); err != nil {
			lastErr = err
			continue
		}
		return p, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可写目录")
	}
	return "", lastErr
}

// Parse 校验并解析一段配置文本（允许 // 与 /* */ 注释），用于在线编辑。
// 两段式（ui / advanced）和旧版扁平结构都认，返回值是合并后的完整配置。
func Parse(text string) (*Config, error) {
	body := normalizeInput(text)
	cfg := Defaults()
	if probeLowMem(body) || lowMemFromEnv() {
		cfg = LowMemDefaults()
	}
	if err := applyDoc(body, cfg); err != nil {
		return nil, err
	}
	cfg.normalize()
	return cfg, nil
}

// ParseInto 解析一段配置文本并**合并**到给定的基线配置上：
// 文本里出现的字段覆盖基线，文本里没出现的字段保持基线原值。
// 用于「贴文本保存」场景——基线必须是磁盘上当前生效的配置，而不是出厂默认值，
// 否则用户没在文本里提到的字段会被静默重置回默认。
func ParseInto(base *Config, text string) (*Config, error) {
	body := normalizeInput(text)
	var cfg *Config
	if base != nil {
		cp := *base // Config 字段全为值类型，浅拷贝即可隔离
		cfg = &cp
	} else {
		cfg = Defaults()
	}
	if err := applyDoc(body, cfg); err != nil {
		return nil, err
	}
	cfg.normalize()
	return cfg, nil
}

// UIFileName 页面配置文件名：放在数据目录，跟着数据走（换机器/换容器只要带上数据目录即可）
const UIFileName = "ui.yml"

// LegacyUIFileName 旧版页面配置名（JSON），启动时若存在会自动迁移成 ui.yml
const LegacyUIFileName = "ui.json"

// UIFilePath 页面配置文件在数据目录下的路径
func UIFilePath(dataDir string) string { return filepath.Join(dataDir, UIFileName) }

// LegacyUIFilePath 旧版 ui.json 的路径
func LegacyUIFilePath(dataDir string) string { return filepath.Join(dataDir, LegacyUIFileName) }

const uiHeader = `# 页面可改的配置（保存即生效），由「信息」页 →「运行配置」维护。
# 采集 / 下载 / 封面 / ffmpeg / 访问密码；改完点保存立即生效，不用重启。
`

const configHeader = `# 需要重启才生效的配置：端口 / 数据目录 / 日志 / 内存 / 数据库。
# 页面可改的项（采集 / 下载 / 封面 / ffmpeg）在数据目录的 ui.yml，这里不重复。
`

// Marshal 生成 config.yml 的内容：只保留 advanced 段（需要重启的项）。
func Marshal(c *Config) ([]byte, error) {
	doc := struct {
		Advanced AdvancedSection `json:"advanced" yaml:"advanced"`
	}{Advanced: c.Advanced()}
	return marshalYAML(configHeader, doc)
}

// MarshalUI 生成 ui.yml 的内容（collect / download / cover / ffmpeg 四段）
func MarshalUI(c *Config) ([]byte, error) {
	return marshalYAML(uiHeader, c.UI())
}

// marshalYAML 输出 2 空格缩进的 YAML，并在前面带上说明注释
func marshalYAML(header string, v any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// LoadUI 把数据目录里的 ui.yml 合进 cfg（只覆盖文件里出现了的段）。
// 文件不存在时返回 os.ErrNotExist，调用方可以据此生成一份。
func LoadUI(path string, c *Config) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return mergeUISections(normalizeInput(string(b)), c)
}

// mergeUISections 解析 ui 文件（顶层就是 collect/download/cover/ffmpeg/auth）
func mergeUISections(text string, c *Config) error {
	var p uiPatch
	if err := yaml.Unmarshal([]byte(text), &p); err != nil {
		return err
	}
	p.Collect.apply(c)
	p.Download.apply(c)
	p.Cover.apply(c)
	p.FFmpeg.apply(c)
	p.Auth.apply(c)
	return nil
}

// SaveUI 原子写入 ui.yml
func SaveUI(path string, c *Config) error {
	b, err := MarshalUI(c)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return writeFileAtomic(path, b)
}

// Save 把 advanced 段写成 YAML（原子写：先写临时文件再改名）
func Save(path string, c *Config) error {
	b, err := Marshal(c)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// writeFileAtomic 原子写文件：唯一临时名 → 写 → fsync → 改名 → fsync 目录。
//
// 为什么不用固定名 "path.tmp"：并发保存（页面连点两次 / 定时任务）会互相截断临时文件，
// 发布出半截配置。为什么必须 fsync：写完就改名但没落盘时，掉电后可能得到 0 字节配置，
// 回退默认值后数据目录就跑偏了（表现为"数据全没了"）。
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		// 出错时清掉临时文件；成功路径已被 Rename 移走，这里删不到东西
		_ = os.Remove(tmp)
	}()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Windows 上目标文件存在时 os.Rename 会失败，先删掉旧文件
	if _, statErr := os.Stat(path); statErr == nil && runtime.GOOS == "windows" {
		_ = os.Remove(path)
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// fsync 目录项，保证改名本身落盘（部分文件系统需要）
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// normalizeInput 统一输入文本：旧版 JSON（以 { 开头）先剥掉 // 与 /* */ 注释；
// YAML 原样返回（它的注释是 #，别去动，否则会误伤 https:// 这类值）。
func normalizeInput(s string) string {
	if strings.HasPrefix(strings.TrimSpace(s), "{") {
		return stripComments(s)
	}
	return s
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// ---------- 配置文件的两段式结构 ----------
//
// 从 1.4.0 起配置文件分成两段，职责分开：
//
//	"ui"       页面（信息页 → 运行配置）可改的项，保存后立即生效
//	"advanced" 只能在启动时定死的项，改了要重启；页面不展示
//
// 旧版扁平结构（server/data_dir/collect 全部平铺在顶层）仍然能读，
// 第一次从页面保存时会自动写成两段式。

// UISection 页面可改、保存即生效的项（存在 <数据目录>/ui.yml）
type UISection struct {
	Collect  Collect  `json:"collect" yaml:"collect"`
	Download Download `json:"download" yaml:"download"`
	Cover    Cover    `json:"cover" yaml:"cover"`
	FFmpeg   FFmpeg   `json:"ffmpeg" yaml:"ffmpeg"`
	Auth     Auth     `json:"auth" yaml:"auth"`
}

// AdvancedSection 需要重启才生效的项（存在 config.yml 的 advanced 段）
type AdvancedSection struct {
	Server     Server `json:"server" yaml:"server"`
	DataDir    string `json:"data_dir" yaml:"data_dir"`
	LogLevel   string `json:"log_level" yaml:"log_level"`
	LowMem     bool   `json:"lowmem" yaml:"lowmem"`
	MemLimitMB int    `json:"mem_limit_mb" yaml:"mem_limit_mb"`
	MaxProcs   int    `json:"max_procs" yaml:"max_procs"`
	DB         DB     `json:"db" yaml:"db"`
}

// UI 取出页面段
func (c *Config) UI() UISection {
	return UISection{Collect: c.Collect, Download: c.Download, Cover: c.Cover, FFmpeg: c.FFmpeg, Auth: c.Auth}
}

// ApplyUI 写回页面段
func (c *Config) ApplyUI(u UISection) {
	c.Collect, c.Download, c.Cover, c.FFmpeg = u.Collect, u.Download, u.Cover, u.FFmpeg
	c.Auth = u.Auth
}

// Advanced 取出高级段
func (c *Config) Advanced() AdvancedSection {
	return AdvancedSection{
		Server: c.Server, DataDir: c.DataDir, LogLevel: c.LogLevel,
		LowMem: c.LowMem, MemLimitMB: c.MemLimitMB, MaxProcs: c.MaxProcs,
		DB: c.DB,
	}
}

// ApplyAdvanced 写回高级段
func (c *Config) ApplyAdvanced(a AdvancedSection) {
	c.Server, c.DataDir, c.LogLevel = a.Server, a.DataDir, a.LogLevel
	c.LowMem, c.MemLimitMB, c.MaxProcs = a.LowMem, a.MemLimitMB, a.MaxProcs
	c.DB = a.DB
}

// 各段的「补丁」：用指针才能区分「没写」和「写了 0」，
// 否则只写一个 db.conns 会把 cache_mb/mmap_mb 一起清零。
type serverPatch struct {
	Addr *string `json:"addr" yaml:"addr"`
}

type collectPatch struct {
	Pages     *int    `json:"pages" yaml:"pages"`
	Sources   *int    `json:"sources" yaml:"sources"`
	IncrEvery *string `json:"incr_every" yaml:"incr_every"`
	IncrPages *int    `json:"incr_pages" yaml:"incr_pages"`
}

type downloadPatch struct {
	Workers        *int `json:"workers" yaml:"workers"`
	SegConcurrency *int `json:"seg_concurrency" yaml:"seg_concurrency"`
	SegBudget      *int `json:"seg_budget" yaml:"seg_budget"`
	MinFreeMB      *int `json:"min_free_mb" yaml:"min_free_mb"`
}

type coverPatch struct {
	Workers     *int    `json:"workers" yaml:"workers"`
	Concurrency *int    `json:"concurrency" yaml:"concurrency"`
	ImgProxy    *string `json:"img_proxy" yaml:"img_proxy"`
}

type ffmpegPatch struct {
	Threads  *int  `json:"threads" yaml:"threads"`
	Reencode *bool `json:"reencode" yaml:"reencode"`
	Verify   *bool `json:"verify" yaml:"verify"`
}

type dbPatch struct {
	CacheMB   *int `json:"cache_mb" yaml:"cache_mb"`
	MmapMB    *int `json:"mmap_mb" yaml:"mmap_mb"`
	Conns     *int `json:"conns" yaml:"conns"`
	PageSize  *int `json:"page_size" yaml:"page_size"`
	VacuumPct *int `json:"vacuum_pct" yaml:"vacuum_pct"`
}

type authPatch struct {
	Password *string `json:"password" yaml:"password"`
}

// docPatch 配置文件 / 接口 payload 里可能出现的一切字段（新旧结构都收在一个结构里）
type docPatch struct {
	// 新结构：config.yml 的 advanced 段
	Advanced *advPatch `json:"advanced" yaml:"advanced"`
	// 兼容旧结构：ui 段
	UI *uiPatch `json:"ui" yaml:"ui"`
	// 兼容旧结构 / 接口 payload：全部平铺在顶层
	Server     *serverPatch   `json:"server" yaml:"server"`
	DataDir    *string        `json:"data_dir" yaml:"data_dir"`
	LogLevel   *string        `json:"log_level" yaml:"log_level"`
	LowMem     *bool          `json:"lowmem" yaml:"lowmem"`
	MemLimitMB *int           `json:"mem_limit_mb" yaml:"mem_limit_mb"`
	MaxProcs   *int           `json:"max_procs" yaml:"max_procs"`
	Collect    *collectPatch  `json:"collect" yaml:"collect"`
	Download   *downloadPatch `json:"download" yaml:"download"`
	Cover      *coverPatch    `json:"cover" yaml:"cover"`
	FFmpeg     *ffmpegPatch   `json:"ffmpeg" yaml:"ffmpeg"`
	Auth       *authPatch     `json:"auth" yaml:"auth"`
	DB         *dbPatch       `json:"db" yaml:"db"`
}

// uiPatch 旧版 ui 段（现在 ui 段的字段直接写在 ui.yml 顶层）
type uiPatch struct {
	Collect  *collectPatch  `json:"collect" yaml:"collect"`
	Download *downloadPatch `json:"download" yaml:"download"`
	Cover    *coverPatch    `json:"cover" yaml:"cover"`
	FFmpeg   *ffmpegPatch   `json:"ffmpeg" yaml:"ffmpeg"`
	Auth     *authPatch     `json:"auth" yaml:"auth"`
}

type advPatch struct {
	Server     *serverPatch `json:"server" yaml:"server"`
	DataDir    *string      `json:"data_dir" yaml:"data_dir"`
	LogLevel   *string      `json:"log_level" yaml:"log_level"`
	LowMem     *bool        `json:"lowmem" yaml:"lowmem"`
	MemLimitMB *int         `json:"mem_limit_mb" yaml:"mem_limit_mb"`
	MaxProcs   *int         `json:"max_procs" yaml:"max_procs"`
	DB         *dbPatch     `json:"db" yaml:"db"`
	// Cover 旧版把 cover 写在 advanced 段，1.4.0 起归到 ui，这里保留兼容
	Cover *coverPatch `json:"cover" yaml:"cover"`
}

func (p *serverPatch) apply(c *Config) {
	if p != nil && p.Addr != nil {
		c.Server.Addr = *p.Addr
	}
}

func (p *collectPatch) apply(c *Config) {
	if p == nil {
		return
	}
	if p.Pages != nil {
		c.Collect.Pages = *p.Pages
	}
	if p.Sources != nil {
		c.Collect.Sources = *p.Sources
	}
	if p.IncrEvery != nil {
		c.Collect.IncrEvery = *p.IncrEvery
	}
	if p.IncrPages != nil {
		c.Collect.IncrPages = *p.IncrPages
	}
}

func (p *downloadPatch) apply(c *Config) {
	if p == nil {
		return
	}
	if p.Workers != nil {
		c.Download.Workers = *p.Workers
	}
	if p.SegConcurrency != nil {
		c.Download.SegConcurrency = *p.SegConcurrency
	}
	if p.SegBudget != nil {
		c.Download.SegBudget = *p.SegBudget
	}
	if p.MinFreeMB != nil {
		c.Download.MinFreeMB = *p.MinFreeMB
	}
}

func (p *coverPatch) apply(c *Config) {
	if p == nil {
		return
	}
	if p.Workers != nil {
		c.Cover.Workers = *p.Workers
	}
	if p.Concurrency != nil {
		c.Cover.Concurrency = *p.Concurrency
	}
	if p.ImgProxy != nil {
		c.Cover.ImgProxy = *p.ImgProxy
	}
}

func (p *ffmpegPatch) apply(c *Config) {
	if p == nil {
		return
	}
	if p.Threads != nil {
		c.FFmpeg.Threads = *p.Threads
	}
	if p.Reencode != nil {
		c.FFmpeg.Reencode = *p.Reencode
	}
	if p.Verify != nil {
		c.FFmpeg.Verify = *p.Verify
	}
}

func (p *dbPatch) apply(c *Config) {
	if p == nil {
		return
	}
	if p.CacheMB != nil {
		c.DB.CacheMB = *p.CacheMB
	}
	if p.MmapMB != nil {
		c.DB.MmapMB = *p.MmapMB
	}
	if p.Conns != nil {
		c.DB.Conns = *p.Conns
	}
	if p.PageSize != nil {
		c.DB.PageSize = *p.PageSize
	}
	if p.VacuumPct != nil {
		c.DB.VacuumPct = *p.VacuumPct
	}
}

// apply 访问密码只存在于 ui.yml，不参与 config.yml 的解析/保存。
func (p *authPatch) apply(c *Config) {
	if p != nil && p.Password != nil {
		c.Auth.Password = *p.Password
	}
}

// apply 把补丁合并到 c（只覆盖补丁里出现过的字段）
func (p *docPatch) apply(c *Config) {
	if p == nil {
		return
	}
	if u := p.UI; u != nil {
		u.Collect.apply(c)
		u.Download.apply(c)
		u.Cover.apply(c)
		u.FFmpeg.apply(c)
	}
	if a := p.Advanced; a != nil {
		a.Server.apply(c)
		if a.DataDir != nil {
			c.DataDir = *a.DataDir
		}
		if a.LogLevel != nil {
			c.LogLevel = *a.LogLevel
		}
		if a.LowMem != nil {
			c.LowMem = *a.LowMem
		}
		if a.MemLimitMB != nil {
			c.MemLimitMB = *a.MemLimitMB
		}
		if a.MaxProcs != nil {
			c.MaxProcs = *a.MaxProcs
		}
		a.DB.apply(c)
		// 旧版两段式把 cover 写在 advanced 里：只有 ui 段没给 cover 时才用它
		if a.Cover != nil && (p.UI == nil || p.UI.Cover == nil) {
			a.Cover.apply(c)
		}
	}
	// 旧扁平结构 / 接口 payload：顶层直接写 server / collect / …
	p.Server.apply(c)
	if p.DataDir != nil {
		c.DataDir = *p.DataDir
	}
	if p.LogLevel != nil {
		c.LogLevel = *p.LogLevel
	}
	if p.LowMem != nil {
		c.LowMem = *p.LowMem
	}
	if p.MemLimitMB != nil {
		c.MemLimitMB = *p.MemLimitMB
	}
	if p.MaxProcs != nil {
		c.MaxProcs = *p.MaxProcs
	}
	p.Collect.apply(c)
	p.Download.apply(c)
	p.Cover.apply(c)
	p.FFmpeg.apply(c)
	p.DB.apply(c)
}

// ApplyPatch 把一段配置补丁（YAML 或 JSON 都行）合并到 cfg 上，只覆盖出现过的字段。
// 给 web 层保存接口用：前端漏传 / 只传一个子字段时，同段其它字段不会被清零。
func ApplyPatch(cfg *Config, data []byte) error {
	var p docPatch
	if err := yaml.Unmarshal(data, &p); err != nil {
		return err
	}
	p.apply(cfg)
	return nil
}

// 页面可改的分组：只有这几段允许通过接口写入（保存即生效）。
var uiWritableSections = map[string]bool{"collect": true, "download": true, "cover": true, "ffmpeg": true, "auth": true}

// ApplyUIPatch 只允许修改「页面可改」的几段（collect / download / cover / ffmpeg / auth），
// 其余字段（advanced / server / data_dir / log_level / lowmem / mem_limit_mb /
// max_procs / db）一律拒绝。
//
// 为什么要有这个限制：docPatch 同时兼容新旧两种结构，接口如果直接用 ApplyPatch，
// 一次 PUT 就能把 data_dir 改到别处、server.addr 改到别的端口、db.conns 改到 32 ——
// 页面只是"改参数"，不该有这些权限（这些项按设计要重启才生效，本来也不该在页面上给）。
func ApplyUIPatch(cfg *Config, data []byte) error {
	// 先看原始 key：解析成结构体后 "advanced: {}"（空段）也会被判为"出现了"。
	// 注意：这里必须用 map[string]any —— YAML 不能反序列化到 json.RawMessage。
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k := range raw {
		if !uiWritableSections[k] {
			return fmt.Errorf("「%s」不能在页面上修改（该项需要重启才生效，请编辑配置文件）", k)
		}
	}
	var p docPatch
	if err := yaml.Unmarshal(data, &p); err != nil {
		return err
	}
	// 只应用四段：兼容 UI 段 / 顶层平铺 / advanced 里的旧版 cover
	if u := p.UI; u != nil {
		u.Collect.apply(cfg)
		u.Download.apply(cfg)
		u.Cover.apply(cfg)
		u.FFmpeg.apply(cfg)
		u.Auth.apply(cfg)
	}
	p.Collect.apply(cfg)
	p.Download.apply(cfg)
	p.Cover.apply(cfg)
	p.FFmpeg.apply(cfg)
	p.Auth.apply(cfg)
	if a := p.Advanced; a != nil && a.Cover != nil {
		a.Cover.apply(cfg)
	}
	return nil
}

// applyDoc 把文件内容解析进 base（只覆盖文件里出现了的字段，没写的保留 base 的值）。
// YAML / 旧版 JSON / 旧扁平结构都能解（YAML 是 JSON 的超集）。
func applyDoc(text string, base *Config) error {
	return ApplyPatch(base, []byte(text))
}

// probeLowMem 判断文件里 lowmem 是否为 true（两段式看 advanced.lowmem，旧结构看顶层 lowmem）
func probeLowMem(text string) bool {
	var p docPatch
	if err := yaml.Unmarshal([]byte(text), &p); err != nil {
		return false
	}
	if p.LowMem != nil {
		return *p.LowMem
	}
	return p.Advanced != nil && p.Advanced.LowMem != nil && *p.Advanced.LowMem
}

// hasDataDirKey 文件里是否显式写了 data_dir（两段式在 advanced 段里，旧结构在顶层）
func hasDataDirKey(text string) bool {
	var p docPatch
	if err := yaml.Unmarshal([]byte(text), &p); err != nil {
		return false
	}
	if p.DataDir != nil {
		return true
	}
	return p.Advanced != nil && p.Advanced.DataDir != nil
}

// HasUISections 配置文件里是否还残留页面项（ui 段，或平铺的 collect/download/cover/ffmpeg）。
// 新版本这些项只在 <数据目录>/ui.yml 里，config.yml 出现它们就是老版本留下的。
func HasUISections(text string) bool {
	var p docPatch
	if err := yaml.Unmarshal([]byte(normalizeInput(text)), &p); err != nil {
		return false
	}
	return p.UI != nil || p.Collect != nil || p.Download != nil || p.Cover != nil || p.FFmpeg != nil
}

// CleanUIFromConfig 去掉 config.yml 里残留的页面项，只留 advanced 段。
// 只有确实存在页面项时才写盘，旧文件备份为 .bak；返回是否改动。
// 调用前请确保 ui 段的值已经落到 ui.yml，否则会丢配置。
func CleanUIFromConfig(path string, c *Config) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if !HasUISections(string(b)) {
		return false, nil
	}
	if old, e := os.ReadFile(path); e == nil {
		_ = os.WriteFile(path+".bak", old, 0o644)
	}
	if err := Save(path, c); err != nil {
		return false, err
	}
	return true, nil
}

// stripComments 去掉 JSON 里的 // 行注释和 /* */ 块注释（字符串内的不动），
// 这样用户可以在配置文件里写说明。解析失败的原样返回交给 json 报错。
func stripComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(s) {
			next := s[i+1]
			if next == '/' {
				// 行注释：跳到行尾（保留换行）
				for i < len(s) && s[i] != '\n' {
					i++
				}
				if i < len(s) {
					b.WriteByte('\n')
				}
				continue
			}
			if next == '*' {
				i += 2
				for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
					i++
				}
				i++ // 指向 '/'
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

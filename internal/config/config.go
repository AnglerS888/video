// Package config 集中管理程序配置：所有可调项都在配置文件中，不再用命令行参数。
//
// 文件格式是 JSON，但允许 // 行注释与 /* */ 块注释（读取前会先剥掉），方便手改。
// 首次运行会在当前目录（不可写则程序目录、数据目录）生成一份带默认值的 config.json。
//
// 配置文件查找顺序：
//  1. 环境变量 VDL_CONFIG 指定的路径；
//  2. 当前目录 ./config.json；
//  3. 程序所在目录/config.json；
//  4. 当前目录/videodata/config.json（容器里通常用 VDL_CONFIG 指到这里）。
//
// lowmem 模式的处理：先按 lowmem 取一套低配默认值，再把配置文件里**实际写了**的字段覆盖上去，
// 这样既保留「一键低配」，又允许逐项覆盖（等价于以前「-lowmem 只改没显式指定的参数」）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// FileName 配置文件名
const FileName = "config.json"

// Config 全部配置。字段含义见 README「配置参数」。
type Config struct {
	Server     Server   `json:"server"`
	DataDir    string   `json:"data_dir"`
	LogLevel   string   `json:"log_level"`
	LowMem     bool     `json:"lowmem"`
	MemLimitMB int      `json:"mem_limit_mb"`
	MaxProcs   int      `json:"max_procs"`
	Collect    Collect  `json:"collect"`
	Download   Download `json:"download"`
	Cover      Cover    `json:"cover"`
	FFmpeg     FFmpeg   `json:"ffmpeg"`
	DB         DB       `json:"db"`
	Update     Update   `json:"update"`
	dataDirSet bool     // 配置文件里是否显式写了 data_dir
	path       string   // 实际加载的文件路径
}

// Server HTTP 服务
type Server struct {
	Addr string `json:"addr"`
}

// Collect 采集
type Collect struct {
	Pages     int    `json:"pages"`      // 每个接口的分页并发
	Sources   int    `json:"sources"`    // 同时采集的接口数
	IncrEvery string `json:"incr_every"` // 定时增量间隔，Go duration 语法：1h / 30m
	IncrPages int    `json:"incr_pages"` // 定时增量每次同步前 N 页，0=不限
}

// Download 下载与磁盘保护
type Download struct {
	Workers        int `json:"workers"`         // 同时下载的集数（实际固定串行）
	SegConcurrency int `json:"seg_concurrency"` // 单任务分片并发
	SegBudget      int `json:"seg_budget"`      // 全局分片并发上限，0=workers×seg_concurrency
	MinFreeMB      int `json:"min_free_mb"`     // 磁盘剩余空间保护阈值（MiB），0=关闭
}

// Cover 封面
type Cover struct {
	Workers     int    `json:"workers"`
	Concurrency int    `json:"concurrency"`
	ImgProxy    string `json:"img_proxy"`
}

// FFmpeg 合并/转码
type FFmpeg struct {
	Threads  int  `json:"threads"`
	Reencode bool `json:"reencode"`
	Verify   bool `json:"verify"`
}

// DB SQLite
type DB struct {
	CacheMB   int `json:"cache_mb"`
	MmapMB    int `json:"mmap_mb"`
	Conns     int `json:"conns"`
	PageSize  int `json:"page_size"`
	VacuumPct int `json:"vacuum_pct"`
}

// Update 在线更新（Gitee）
type Update struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Token  string `json:"token"`
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
		Update:     Update{},
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
				if err := Save(p, Defaults()); err == nil {
					cfg := Defaults()
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
			return Defaults(), "", fmt.Errorf("未找到配置文件，且生成默认配置失败: %w", err)
		}
		cfg := Defaults()
		cfg.path = p
		return cfg, p, nil
	}

	return loadFile(path)
}

// loadFile 读取并解析指定配置文件
func loadFile(path string) (*Config, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Defaults(), path, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	cleaned := stripComments(string(raw))

	// 先看 lowmem，决定基础默认值用哪一套
	var probe struct {
		LowMem *bool `json:"lowmem"`
	}
	_ = json.Unmarshal([]byte(cleaned), &probe)

	base := Defaults()
	if probe.LowMem != nil && *probe.LowMem {
		base = LowMemDefaults()
	}
	// 再用文件里实际写了的字段覆盖（嵌套结构逐字段合并，没写的保留基础默认值）
	if err := json.Unmarshal([]byte(cleaned), base); err != nil {
		return Defaults(), path, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	base.path = path
	base.dataDirSet = hasTopLevelKey(cleaned, "data_dir")
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
}

// findConfig 按顺序找配置文件，找不到返回空串
func findConfig() string {
	cands := []string{FileName, filepath.Join("videodata", FileName)}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		cands = append([]string{filepath.Join(dir, FileName)}, cands...)
	}
	for _, p := range cands {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// writeDefault 依次尝试 当前目录 → 程序目录 → videodata 目录，写一份默认配置
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
		if err := Save(p, Defaults()); err != nil {
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

// Save 把配置写成带缩进的 JSON（原子写：先写临时文件再改名）
func Save(path string, c *Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// hasTopLevelKey 判断 JSON 顶层是否出现某个字段（用于区分「写了默认值」和「没写」）
func hasTopLevelKey(s, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
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

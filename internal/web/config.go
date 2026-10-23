package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"videoDownLoad/internal/config"
)

// uiPath 页面配置（采集 / 下载 / 封面 / ffmpeg / 访问密码）存放位置：数据目录下的 ui.yml
func (s *Server) uiPath() string { return config.UIFilePath(s.DataDir) }

// configInfo 读取当前配置（给「信息」页的表单用）。
// 页面可改的项来自 <数据目录>/ui.yml，其余项来自 config.yml，这里合并成一份返回。
//
// 路径脱敏：只返回文件名与所在目录的最后一段，绝对路径不外泄
// （服务一旦暴露到公网，路径会暴露用户名与部署结构）。
func (s *Server) configInfo(w http.ResponseWriter, r *http.Request) {
	if s.ConfigPath == "" {
		fail(w, "当前没有可编辑的配置文件（可能是启动参数/默认值）")
		return
	}
	raw, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		fail(w, "读取配置文件失败："+publicErr(err))
		return
	}
	ok(w, map[string]any{
		"path":    maskPath(s.ConfigPath),
		"ui_path": maskPath(s.uiPath()),
		// 原文给前端做「贴文本编辑」用：里面可能含 data_dir 的绝对路径，
		// 这里把它遮掉（前端表单改的是分段字段，不依赖这个绝对路径）。
		"text":   maskConfigText(string(raw)),
		"config": s.liveConfig(),
		// 预设：低配/标准一键套用（表单顶部两个按钮）
		"presets": map[string]any{
			"default": config.Defaults(),
			"lowmem":  config.LowMemDefaults(),
		},
		"editable": true,
	})
}

// maskConfigText 把配置原文里的 data_dir 绝对路径遮掉（只留末两段）。
// 其余内容原样返回：data_dir 是唯一会暴露部署结构的字段。
func maskConfigText(text string) string {
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(trimmed, "data_dir:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(trimmed, "data_dir:"))
		val = strings.Trim(val, `"'`)
		if val == "" {
			continue
		}
		masked := maskPath(val)
		prefix := ln[:strings.Index(ln, "data_dir:")+len("data_dir:")]
		quote := ""
		if strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(trimmed, "data_dir:")), `"`) {
			quote = `"`
		} else if strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(trimmed, "data_dir:")), `'`) {
			quote = `'`
		}
		lines[i] = prefix + " " + quote + masked + quote
	}
	return strings.Join(lines, "\n")
}

// maskPath 把绝对路径压成「…/父目录/文件名」，保留可辨识度但不暴露完整路径。
func maskPath(p string) string {
	p = filepath.Clean(p)
	dir, file := filepath.Split(p)
	dir = filepath.ToSlash(filepath.Clean(dir))
	if dir == "" || dir == "." {
		return file
	}
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	if len(parts) <= 1 {
		return file
	}
	return "…/" + parts[len(parts)-2] + "/" + file
}

// configBody PUT /api/config 的请求体。
// 表单走 config（推荐）；text 保留给「直接贴 JSON」的老用法。
type configBody struct {
	Text   string          `json:"text"`
	Config json.RawMessage `json:"config"`
}

// hotSections 保存即生效（写在 ui.yml）的分组：采集 / 下载 / 封面 / 合并转码 / 访问密码。
// 其余项（server、data_dir、log_level、lowmem、内存、数据库）留在 config.yml，改了要重启。
var hotSections = map[string]bool{"collect": true, "download": true, "cover": true, "ffmpeg": true, "auth": true}

// saveConfig 保存配置（PUT /api/config）。
// 表单只提交「保存即生效」的分组 → 写进 ui.yml 并立刻热应用，config.yml 不动；
// 提交里带了其它分组 → 同时写 config.yml，并提示需要重启。
func (s *Server) saveConfig(w http.ResponseWriter, r *http.Request) {
	if s.ConfigPath == "" {
		fail(w, "当前没有可编辑的配置文件")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil {
		fail(w, "读取请求失败："+err.Error())
		return
	}
	var req configBody
	if err := json.Unmarshal(body, &req); err != nil {
		fail(w, "请求格式错误："+err.Error())
		return
	}

	var (
		cfg       *config.Config
		needStart bool // 提交里是否包含只能重启生效的项
	)
	if len(req.Config) > 0 {
		// 表单路径：以「磁盘上的完整配置」为底（含 ui.yml），把提交上来的字段覆盖上去。
		// 用 config.ApplyUIPatch 逐字段合并：只传了 db.conns 也不会把 cache_mb 清零；
		// 同时拒绝 advanced/data_dir/server/db 这些不该由页面改的字段。
		cfg = s.liveConfig()
		if err := config.ApplyUIPatch(cfg, req.Config); err != nil {
			fail(w, "配置取值有误："+err.Error())
			return
		}
		cfg.Normalize()
		if err := cfg.Check(); err != nil {
			fail(w, publicErr(err))
			return
		}
		needStart = hasRestartOnlyKey(req.Config)
	} else {
		// 兼容路径：整段原文，允许注释，解析失败直接拒绝。
		// 关键：以「当前磁盘上生效的配置」为基线合并，而不是出厂默认值 ——
		// 否则用户没在文本里提到的字段会被静默重置回默认（曾经的真 bug）。
		if strings.TrimSpace(req.Text) == "" {
			fail(w, "配置内容为空")
			return
		}
		c, err := config.ParseInto(s.liveConfig(), req.Text)
		if err != nil {
			fail(w, "配置格式有误："+err.Error())
			return
		}
		cfg = c
		cfg.Normalize()
		if err := cfg.Check(); err != nil {
			fail(w, publicErr(err))
			return
		}
		needStart = true // 整段原文无从判断改了什么，一律按需要重启处理
	}

	// 页面可改的项：写数据目录的 ui.yml（带 .bak 备份）
	if err := s.writeUIFile(cfg); err != nil {
		fail(w, publicErr(err))
		return
	}
	// 需要重启的项：只有确实带上了才动 config.yml，避免页面每次保存都重写它。
	// 注意：一律用 config.Marshal(合并后的完整配置) 写，绝不写用户原文 ——
	// 原文里没出现的 server/data_dir/log_level/db 会整段消失（重启后数据目录跑偏）。
	if needStart {
		text, err := config.Marshal(cfg)
		if err != nil {
			fail(w, "生成配置失败："+err.Error())
			return
		}
		if err := s.writeConfig(text); err != nil {
			fail(w, publicErr(err))
			return
		}
	}

	// 先落盘再热应用：万一套用过程 panic 也不会丢配置。
	// 即使这次带了「需要重启」的项，页面那几组也能立刻生效，所以总是调用。
	applied := []string(nil)
	if s.HotApply != nil {
		applied = s.HotApply(cfg)
	}
	// 访问密码：保存后立即生效（不用重启）。
	// 同时给当前浏览器续上 / 清除 Cookie：刚设完密码就把自己锁在门外很难受，
	// 而关闭密码后旧 Cookie 留着也没意义。
	s.SetAccessPassword(cfg.Auth.Password)
	if cfg.Auth.Password != "" {
		s.issueAuthCookie(w, r)
	} else {
		s.clearAuthCookie(w)
	}
	// auth 也是保存即生效的一组（没有可重启的分组时 applied 可能为空，这里如实补上）
	applied = append(applied, "auth")

	msg := "已保存并立即生效（写在数据目录的 ui.yml）"
	if needStart {
		msg = "已保存：需要重启的项写入 config.yml（重启后生效），页面项已立即生效"
	}
	ok(w, map[string]any{
		"msg":              msg,
		"restart_required": needStart,
		"saved_at":         time.Now().Format("2006-01-02 15:04:05"),
		"applied":          applied,
		"config":           cfg,
		"ui_path":          s.uiPath(),
	})
}

// hasRestartOnlyKey 提交的 JSON 里是否出现了「只能重启生效」的分组
func hasRestartOnlyKey(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return true // 解析不出来就当需要重启，宁可多提示
	}
	for k := range m {
		if !hotSections[k] {
			return true
		}
	}
	return false
}

// currentConfig 读 config.yml（只有 advanced 段）；读不到就用默认值
func (s *Server) currentConfig() *config.Config {
	if b, err := os.ReadFile(s.ConfigPath); err == nil {
		if c, e := config.Parse(string(b)); e == nil {
			return c
		}
	}
	return config.Defaults()
}

// liveConfig 当前实际生效的配置 = config.yml + 数据目录的 ui.yml
func (s *Server) liveConfig() *config.Config {
	cfg := s.currentConfig()
	// 文件不存在/读坏了都不影响：用 config.yml 里的值兜底
	_ = config.LoadUI(s.uiPath(), cfg)
	cfg.Normalize()
	return cfg
}

// writeUIFile 把页面可改的几段写进 <数据目录>/ui.yml（旧的备份为 ui.yml.bak）
func (s *Server) writeUIFile(cfg *config.Config) error {
	if strings.TrimSpace(s.DataDir) == "" {
		return errNoDataDir
	}
	p := s.uiPath()
	if old, e := os.ReadFile(p); e == nil {
		_ = os.WriteFile(p+".bak", old, 0o644)
	}
	if err := config.SaveUI(p, cfg); err != nil {
		return fmt.Errorf("写入页面配置失败：%w", err)
	}
	return nil
}

// writeConfig 备份旧 config.yml 后原子写入新内容
func (s *Server) writeConfig(b []byte) error {
	if old, e := os.ReadFile(s.ConfigPath); e == nil {
		_ = os.WriteFile(s.ConfigPath+".bak", old, 0o644)
	}
	// 唯一临时名 + fsync + 改名：并发保存不会互相截断，掉电也不会得到半截/0 字节配置
	tmp, err := os.CreateTemp(filepath.Dir(s.ConfigPath), filepath.Base(s.ConfigPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("写入临时文件失败：%w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时文件失败：%w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("刷盘失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败：%w", err)
	}
	// Windows 上目标存在时 Rename 会失败，先删旧的
	if _, err := os.Stat(s.ConfigPath); err == nil && runtime.GOOS == "windows" {
		_ = os.Remove(s.ConfigPath)
	}
	if err := os.Rename(name, s.ConfigPath); err != nil {
		return fmt.Errorf("保存配置失败：%w", err)
	}
	if d, err := os.Open(filepath.Dir(s.ConfigPath)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// errNoDataDir 数据目录没准备好时的提示（Server.DataDir 由 main 注入）
var errNoDataDir = errors.New("数据目录未就绪，无法保存页面配置")

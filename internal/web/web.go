package web

import (
	"database/sql"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"videoDownLoad/internal/collector"
	"videoDownLoad/internal/logx"
	"videoDownLoad/internal/updater"
)

//go:embed static
var staticFS embed.FS

// Server HTTP 服务：持有数据库、采集器、数据目录与各类短缓存。
type Server struct {
	DB      *sql.DB
	Col     *collector.Collector
	DataDir string
	// Version 构建版本（-ldflags "-X main.version=..."），显示在网站信息页
	Version string
	// StartedAt 进程启动时间
	StartedAt time.Time
	// VacuumPct 自动整理数据库的空闲页占比阈值（-vacuum-pct），信息页据此提示「建议整理」
	VacuumPct int
	// Up 在线更新器（检查/下载/编译/重启）
	Up *updater.Updater
	// Shutdown 优雅停机回调：重启应用新版本时由更新接口调用
	Shutdown func()

	listMu    sync.Mutex
	listCache map[string]listCacheItem
	listBytes int

	srcMu        sync.Mutex
	srcNameCache map[int64]string
	srcNameExp   time.Time

	// 已下载数量 / 系统统计的短缓存（前端会定时轮询，没必要每次都全表聚合）
	statMu   sync.Mutex
	doneExp  time.Time
	doneCnt  map[string]int64
	sysExp   time.Time
	sysCache map[string]any
}

// listCacheItem 列表查询结果缓存：同名合并的 GROUP BY 在大库上要几百毫秒，
// 同一条件短时间内重复访问（翻页、返回上一页）直接命中缓存。
type listCacheItem struct {
	data []byte
	exp  time.Time
}

const (
	listCacheTTL = 15 * time.Second
	// listCacheMaxBytes 缓存总量上限。旧的"按条数 300"在弱机上能吃到几十 MB
	// （每条 30~60KB），NAS 只有 4G 内存时按字节限容更安全。
	listCacheMaxBytes = 6 << 20
	listCacheMaxItems = 200
)

func (s *Server) cacheGet(key string) ([]byte, bool) {
	s.listMu.Lock()
	defer s.listMu.Unlock()
	it, ok := s.listCache[key]
	if !ok || time.Now().After(it.exp) {
		return nil, false
	}
	return it.data, true
}

func (s *Server) cachePut(key string, data []byte) {
	s.listMu.Lock()
	defer s.listMu.Unlock()
	if s.listCache == nil {
		s.listCache = map[string]listCacheItem{}
	}
	s.listBytes += len(data)
	// 按总字节数 + 条数双重限容，整体清空（简单但不会无限增长）
	if s.listBytes > listCacheMaxBytes || len(s.listCache) > listCacheMaxItems {
		s.listCache = map[string]listCacheItem{}
		s.listBytes = len(data)
	}
	s.listCache[key] = listCacheItem{data: data, exp: time.Now().Add(listCacheTTL)}
}

// invalidateListCache 数据变更（删除接口、采集完成）后清空列表缓存
func (s *Server) invalidateListCache() {
	s.listMu.Lock()
	s.listCache = nil
	s.listBytes = 0
	s.listMu.Unlock()
}

// httpLog 是否打印每个 API 请求的访问日志。
// 前端会每 1.5s 轮询 /api/progress、每 2.5s 轮询 /api/downloads，
// 默认逐条打印在 NAS 上既是磁盘/内存开销（docker 日志驱动），也刷屏影响排查；
// 平时只在慢请求（>1s）时打一行（WARN 级），VDL_HTTP_LOG=1 时全量输出（DEBUG 级）。
var httpLog = os.Getenv("VDL_HTTP_LOG") == "1"

// spaHandler 提供前端构建产物，并为前端路由做 index.html 回退。
// 前端是 history 模式的单页应用：/downloads、/v/12 这类地址直接刷新或分享出去时，
// 也要返回 index.html，由前端路由接管。
func spaHandler(root fs.FS) http.Handler {
	files := http.FileServer(http.FS(root))
	index, _ := fs.ReadFile(root, "index.html")
	serveIndex := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" || p == "." {
			serveIndex(w)
			return
		}
		if st, err := fs.Stat(root, p); err == nil && !st.IsDir() {
			// 带 hash 的构建产物可以长缓存
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		// 有扩展名的按 404 处理，别把缺失的 js/css 也回退成 html
		if path.Ext(p) != "" {
			http.NotFound(w, r)
			return
		}
		serveIndex(w)
	})
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			return
		}
		cost := time.Since(start)
		switch {
		case cost > time.Second:
			logx.Warnf("[http] 慢请求 %s %s %v", r.Method, r.URL.Path, cost)
		case httpLog:
			logx.Debugf("[http] %s %s %v", r.Method, r.URL.Path, cost)
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, v any) { writeJSON(w, 200, map[string]any{"ok": true, "data": v}) }

func fail(w http.ResponseWriter, msg string) {
	writeJSON(w, 200, map[string]any{"ok": false, "msg": msg})
}

// ---- 接口管理 ----

// srcStat 单个采集接口的数据规模
type srcStat struct {
	videos int64
	names  int64
	plays  int64
	last   string
}

var (
	srcStatMu    sync.Mutex
	srcStatCache map[int64]srcStat
	srcStatExp   time.Time
)

// imageContentType 按魔数判断图片类型，认不出时按 jpeg 处理
func imageContentType(b []byte) string {
	switch {
	case len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8:
		return "image/jpeg"
	case len(b) > 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) > 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	case len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return "image/jpeg"
}

// infoCache 网站信息缓存（目录体积本身有 60s 缓存，这里再挡一层 10s）
var (
	infoMu    sync.Mutex
	infoCache map[string]any
	infoExp   time.Time
)

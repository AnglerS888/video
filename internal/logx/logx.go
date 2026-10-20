// Package logx 统一日志输出，面向容器（Docker）环境：
//
//   - 默认写 **stdout**，单行、无 ANSI 颜色、带毫秒时间戳：
//     docker logs / Portainer / 群晖容器日志 都能直接看，不会被当成错误流红色刷屏
//   - 多行消息（例如 ffmpeg 的 stderr 片段）会被压成一行，避免一条日志炸成几十行
//   - 支持分级（debug/info/warn/error），默认 info；可用 -log-level 或 VDL_LOG_LEVEL 调整
//   - 进程日志是唯一输出通道，不做文件轮转（容器里交给 docker 的 json-file + max-size）
package logx

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Level int32

const (
	DebugLevel Level = iota
	InfoLevel
	WarnLevel
	ErrorLevel
)

var (
	cur   atomic.Int32
	mu    sync.Mutex
	width = 5 // 级别字段宽度，保证对齐
)

func init() { cur.Store(int32(InfoLevel)) }

var levelName = map[Level]string{
	DebugLevel: "DEBUG",
	InfoLevel:  "INFO",
	WarnLevel:  "WARN",
	ErrorLevel: "ERROR",
}

// ParseLevel 解析日志级别字符串（debug/info/warn/warning/error），无法识别时返回 false
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "dbg", "trace":
		return DebugLevel, true
	case "info", "", "notice":
		return InfoLevel, true
	case "warn", "warning":
		return WarnLevel, true
	case "error", "err", "fatal":
		return ErrorLevel, true
	}
	return InfoLevel, false
}

// SetLevel 设置最低输出级别
func SetLevel(l Level) { cur.Store(int32(l)) }

// Enabled 该级别是否会被输出
func Enabled(l Level) bool { return l >= Level(cur.Load()) }

// oneLine 把多行文本压成一行（Docker 里一条日志一行，便于 grep/检索）
func oneLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	r := strings.NewReplacer("\r\n", " | ", "\n", " | ", "\r", " | ", "\t", " ")
	return r.Replace(s)
}

func output(l Level, msg string) {
	if !Enabled(l) {
		return
	}
	line := fmt.Sprintf("%s %-*s %s\n",
		time.Now().Format("2006-01-02 15:04:05.000"), width, levelName[l], oneLine(msg))
	mu.Lock()
	_, _ = os.Stdout.WriteString(line)
	mu.Unlock()
}

// Debugf 调试日志（每页/每条级别的细节，默认不输出）
func Debugf(format string, args ...any) { output(DebugLevel, fmt.Sprintf(format, args...)) }

// Printf 等同于 Infof（保留标准库 log.Printf 的用法，便于替换）
func Printf(format string, args ...any) { output(InfoLevel, fmt.Sprintf(format, args...)) }

// Println 等同于 Infof，参数间用空格连接
func Println(args ...any) { output(InfoLevel, fmt.Sprint(args...)) }

// Infof 普通信息
func Infof(format string, args ...any) { output(InfoLevel, fmt.Sprintf(format, args...)) }

// Warnf 警告（可恢复的问题，例如某页抓取失败）
func Warnf(format string, args ...any) { output(WarnLevel, fmt.Sprintf(format, args...)) }

// Errorf 错误（功能受损，例如合并失败）
func Errorf(format string, args ...any) { output(ErrorLevel, fmt.Sprintf(format, args...)) }

// Fatalf 记录错误并退出进程（容器里由 restart 策略接管重启）
func Fatalf(format string, args ...any) {
	output(ErrorLevel, fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Fatalln 记录错误并退出进程
func Fatalln(args ...any) {
	output(ErrorLevel, fmt.Sprint(args...))
	os.Exit(1)
}

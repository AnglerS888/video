// dbupgrade 独立数据库升级工具。
//
// 用途：手上还有别的旧库（不同机器 / 不同数据目录 / 备份出来的库）需要升级到
// 当前版本的表结构时，用它把库升好 —— 不用启动整个服务。
//
// 用法：
//
//	go run ./tools/dbupgrade -db /path/to/videodata/videodl.db
//	go run ./tools/dbupgrade -db /path/to/videodata/videodl.db -backup-dir /path/to/backup
//
// 行为：
//   - 升级前自动备份 videodl.db / -wal / -shm 三个文件（带时间戳，源文件不动）
//   - 升级步骤与程序启动时的迁移完全一致（internal/db.UpgradeSchema）
//   - 幂等：同一个库可以反复跑，不会丢数据
//   - 升级的是**已有库**，不会新建空库；文件不存在会直接报错退出
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"videoDownLoad/internal/db"
)

func main() {
	dbPath := flag.String("db", "", "要升级的数据库文件路径（一般是 <数据目录>/videodl.db）")
	backupDir := flag.String("backup-dir", "", "备份目录（默认与数据库同目录）")
	noBackup := flag.Bool("no-backup", false, "跳过备份（不建议）")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "用法: go run ./tools/dbupgrade -db <videodl.db 路径>")
		fmt.Fprintln(os.Stderr, "      可选: -backup-dir <目录>  -no-backup")
		os.Exit(2)
	}
	abs, err := filepath.Abs(*dbPath)
	if err != nil {
		abs = *dbPath
	}
	if _, err := os.Stat(abs); err != nil {
		fmt.Fprintf(os.Stderr, "找不到数据库文件: %s\n", abs)
		os.Exit(1)
	}

	// 1) 备份
	if !*noBackup {
		dir := *backupDir
		if dir == "" {
			dir = filepath.Dir(abs)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "创建备份目录失败: %v\n", err)
			os.Exit(1)
		}
		stamp := time.Now().Format("20060102-150405")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			src := abs + suffix
			if _, err := os.Stat(src); err != nil {
				continue
			}
			dst := filepath.Join(dir, fmt.Sprintf("%s.%s.bak%s", filepath.Base(abs), stamp, suffix))
			if err := copyFile(src, dst); err != nil {
				fmt.Fprintf(os.Stderr, "备份 %s 失败: %v\n", src, err)
				os.Exit(1)
			}
			fmt.Printf("已备份 %s -> %s\n", src, dst)
		}
	}

	// 2) 升级
	report, err := db.UpgradeSchema(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "升级失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("升级完成：")
	for _, line := range splitLines(report) {
		fmt.Println("  - " + line)
	}
	fmt.Println("提示：升级后建议重启服务，让新的分享令牌与结构生效。")
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

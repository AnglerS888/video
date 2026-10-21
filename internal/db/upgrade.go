package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// UpgradeSchema 对一个已存在的库执行结构升级，步骤与 InitDB 内的迁移完全一致
// （建表 → 补列 → 下载表结构升级 → 播放表升级 → 冗余索引清理 → 去重 → 聚合表）。
//
// 单独暴露出来是为了给"手上还有别的旧库要升级"的用户用：
// 不用启动整个服务，直接跑 tools/dbupgrade 就能把库升到新结构。
//
// 返回人类可读的变更说明（出错时返回错误，调用方决定怎么退出）。
func UpgradeSchema(path string) (string, error) {
	d, err := sql.Open("sqlite", sqliteDSN(path, Options{}))
	if err != nil {
		return "", err
	}
	defer d.Close()
	if err := d.Ping(); err != nil {
		return "", fmt.Errorf("打开数据库失败: %w", err)
	}

	var notes []string
	before := tableColumns(d, "vod_download")
	hasShare := tableExists(d, "vod_share_token")

	// 建表（含新表 vod_share_token）；已存在的表不会被改动
	if _, err := d.Exec(schema); err != nil {
		return "", fmt.Errorf("建表失败: %w", err)
	}
	if !hasShare {
		notes = append(notes, "新增表 vod_share_token（分享令牌）")
	}
	migrate(d)
	ensurePlaySchema(d)
	dropRedundantIndexes(d)
	dedupePlays(d)
	ensureNameAgg(d)
	PurgeExpiredShareTokens(d)

	after := tableColumns(d, "vod_download")
	for _, c := range after {
		if !containsStr(before, c) {
			notes = append(notes, "vod_download 新增列 "+c)
		}
	}
	if len(notes) == 0 {
		notes = append(notes, "结构已是最新，无需变更")
	}
	return strings.Join(notes, "\n"), nil
}

// tableExists 判断表是否存在
func tableExists(d *sql.DB, table string) bool {
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

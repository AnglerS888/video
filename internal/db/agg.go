package db

import (
	"database/sql"
	"strings"
	"time"
	"videoDownLoad/internal/logx"
)

// 剧名聚合表：同名合并后的列表/搜索都走这张小表。
//
// 为什么需要：直接对 vod_info 做 GROUP BY + ORDER BY updated_at，SQLite 必须把
// 全部分组聚合完再排序（30 万条约 300ms，百万级要 1s+）；换成预聚合表后，列表
// 变成在几万行上做索引排序，冷查询也能压到 10ms 内。
const aggSchema = `
-- WITHOUT ROWID：vod_name 是主键，rowid 表会额外挂一棵 4.7MB 的 autoindex；
-- WITHOUT ROWID 直接按主键组织成一棵 B 树，省空间且点查少一次间接寻址。
CREATE TABLE IF NOT EXISTS vod_name_agg (
	vod_name    TEXT PRIMARY KEY,
	cnt         INTEGER NOT NULL DEFAULT 0,   -- 同名记录数（= 采集源数量）
	vod_pic     TEXT DEFAULT '',
	type_name   TEXT DEFAULT '',
	vod_remarks TEXT DEFAULT '',
	vod_year    TEXT DEFAULT '',
	vod_area    TEXT DEFAULT '',
	vod_actor   TEXT DEFAULT '',             -- 搜索用
	rid         INTEGER NOT NULL DEFAULT 0,  -- 代表性记录 id（优先有封面的）
	src_ids     TEXT DEFAULT '',             -- 形如 ",1,5,9," 便于 LIKE '%,5,%'
	updated_at  TEXT DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_agg_ua ON vod_name_agg(updated_at);
-- vod_name 是主键（自带唯一索引），不要再建 idx_agg_name（重复索引只会拖慢写入）
-- 筛选（年份 / 国家地区 / 类型）
CREATE INDEX IF NOT EXISTS idx_agg_year ON vod_name_agg(vod_year);
CREATE INDEX IF NOT EXISTS idx_agg_area ON vod_name_agg(vod_area);
CREATE INDEX IF NOT EXISTS idx_agg_type ON vod_name_agg(type_name);
`

// ensureNameAgg 建表；老库首次升级时把已有数据聚合一并补齐
func ensureNameAgg(d *sql.DB) {
	// 旧版是普通 rowid 表：整体重建为 WITHOUT ROWID（数据从 vod_info 重算，无损）
	var schemaSQL string
	if err := d.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='vod_name_agg'`).Scan(&schemaSQL); err == nil &&
		!strings.Contains(schemaSQL, "WITHOUT ROWID") {
		logx.Printf("[db] 聚合表升级为 WITHOUT ROWID 结构（重建中）…")
		if _, err := d.Exec(`DROP TABLE vod_name_agg`); err != nil {
			logx.Printf("[db] 重建聚合表失败: %v", err)
		}
	}
	mustExec(d, aggSchema)

	var aggCnt, vodCnt int
	d.QueryRow(`SELECT COUNT(*) FROM vod_name_agg`).Scan(&aggCnt)
	d.QueryRow(`SELECT COUNT(*) FROM vod_info`).Scan(&vodCnt)
	if aggCnt == 0 && vodCnt > 0 {
		logx.Printf("[db] 首次构建剧名聚合表（%d 条记录）…", vodCnt)
		start := time.Now()
		if err := RebuildNameAgg(d); err != nil {
			logx.Printf("[db] 聚合表构建失败（列表会回退到实时聚合）: %v", err)
			return
		}
		logx.Printf("[db] 剧名聚合表构建完成，耗时 %v", time.Since(start).Round(time.Millisecond))
	}
}

// RebuildNameAgg 全量重建聚合表（删接口、老库升级等场景）
func RebuildNameAgg(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM vod_name_agg`); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO vod_name_agg(vod_name, cnt, vod_pic, type_name, vod_remarks,
		vod_year, vod_area, vod_actor, rid, src_ids, updated_at)
		SELECT vod_name, COUNT(*), MAX(vod_pic), MAX(type_name), MAX(vod_remarks),
			MAX(vod_year), MAX(vod_area), MAX(vod_actor),
			COALESCE(MIN(CASE WHEN vod_pic<>'' THEN id END), MIN(id)),
			',' || GROUP_CONCAT(DISTINCT source_id) || ',', MAX(updated_at)
		FROM vod_info GROUP BY vod_name`); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// RefreshNameAgg 增量刷新指定剧名的聚合行（采集入库后调用，一次覆盖一页数据）
func RefreshNameAgg(d *sql.DB, names []string) {
	if len(names) == 0 {
		return
	}
	ph := strings.TrimRight(strings.Repeat("?,", len(names)), ",")
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}

	tx, err := d.Begin()
	if err != nil {
		logx.Printf("[db] 刷新聚合表失败: %v", err)
		return
	}
	// 1) 清掉已不存在的剧名（源站删除条目时）
	delArgs := append([]any{}, args...)
	delArgs = append(delArgs, args...)
	if _, err := tx.Exec(`DELETE FROM vod_name_agg WHERE vod_name IN (`+ph+`)
		AND NOT EXISTS (SELECT 1 FROM vod_info v WHERE v.vod_name = vod_name_agg.vod_name)`, delArgs...); err != nil {
		tx.Rollback()
		logx.Printf("[db] 刷新聚合表失败(delete): %v", err)
		return
	}
	// 2) 重算本页涉及的剧名
	if _, err := tx.Exec(`INSERT OR REPLACE INTO vod_name_agg(vod_name, cnt, vod_pic, type_name,
		vod_remarks, vod_year, vod_area, vod_actor, rid, src_ids, updated_at)
		SELECT vod_name, COUNT(*), MAX(vod_pic), MAX(type_name), MAX(vod_remarks),
			MAX(vod_year), MAX(vod_area), MAX(vod_actor),
			COALESCE(MIN(CASE WHEN vod_pic<>'' THEN id END), MIN(id)),
			',' || GROUP_CONCAT(DISTINCT source_id) || ',', MAX(updated_at)
		FROM vod_info WHERE vod_name IN (`+ph+`) GROUP BY vod_name`, args...); err != nil {
		tx.Rollback()
		logx.Printf("[db] 刷新聚合表失败(upsert): %v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		logx.Printf("[db] 刷新聚合表提交失败: %v", err)
	}
}

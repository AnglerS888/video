-- videoDownLoad 数据库升级脚本 v1.4.1（库已是新结构：vod_download 已有 ep_index 列）
--
-- 这种情况只需要做两件事：补两个可能缺失的列、建分享令牌表。
--
-- 先确认你的库是哪种结构（返回 0 = 已是新结构，用本脚本；返回 1 = 旧结构，
-- 改用 upgrade-v1.4.1-old-shape.sql）：
--
--   sqlite3 videodl.db "SELECT COUNT(*) FROM pragma_table_info('vod_download') WHERE name='ep_index';"
--
-- ⚠️ 执行前先备份 videodl.db* 三个文件。
-- 用法：sqlite3 /path/to/videodata/videodl.db < migrations/upgrade-v1.4.1-current.sql
--
-- 不方便装 sqlite3 时用 Go 工具（自带备份、幂等、自动判断结构）：
--   go run ./tools/dbupgrade -db /path/to/videodata/videodl.db

BEGIN;

-- 分享令牌表：分享链接改用令牌，避免 vod_download.id（自增整数）被直接枚举
CREATE TABLE IF NOT EXISTS vod_share_token (
	token       TEXT PRIMARY KEY,
	download_id INTEGER NOT NULL REFERENCES vod_download(id) ON DELETE CASCADE,
	expires_at  TEXT NOT NULL DEFAULT '',
	created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_share_dl ON vod_share_token(download_id);

-- 补列：列已存在时下面会报 duplicate column name，忽略即可（sqlite3 默认继续执行）
ALTER TABLE vod_download ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
ALTER TABLE vod_download ADD COLUMN share_expires_at TEXT NOT NULL DEFAULT '';
ALTER TABLE vod_info ADD COLUMN src_time TEXT DEFAULT '';
ALTER TABLE vod_source ADD COLUMN auto_incr INTEGER NOT NULL DEFAULT 1;

COMMIT;

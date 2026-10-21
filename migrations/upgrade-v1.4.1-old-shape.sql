-- videoDownLoad 数据库升级脚本 v1.4.1
--
-- 用途：把**旧结构**的库（vod_download 还没有 ep_index 列的版本）升级到当前结构。
-- 什么时候需要用这个脚本：程序启动时会自动做同样的事，但如果你手上有很多台机器的旧库，
-- 想在不启动服务的情况下批量升级，就用这个脚本。
--
-- ⚠️ 执行前务必先备份（videodl.db / videodl.db-wal / videodl.db-shm 三个文件一起拷）：
--
--   cp videodl.db* /path/to/backup/
--
-- 用法（需要 sqlite3 命令行）：
--
--   sqlite3 /path/to/videodata/videodl.db < migrations/upgrade-v1.4.1-old-shape.sql
--
-- 不方便装 sqlite3 时，用 Go 工具（自带备份、幂等、自动判断库的结构，最省事）：
--
--   go run ./tools/dbupgrade -db /path/to/videodata/videodl.db
--
-- 先确认你的库是哪种结构（返回 1 = 旧结构，用本脚本；返回 0 = 已是新结构，
-- 改用 upgrade-v1.4.1-current.sql）：
--
--   sqlite3 videodl.db "SELECT COUNT(*) FROM pragma_table_info('vod_download') WHERE name='ep_index';"

PRAGMA foreign_keys=OFF;

BEGIN;

-- 1) 备份观看历史：下面要 DROP TABLE vod_download，
--    在 foreign_keys=1 下会触发 ON DELETE CASCADE 把历史一起删掉（实测 1 行 → 0 行）
CREATE TABLE IF NOT EXISTS tmp_watch_backup AS
	SELECT download_id, position, duration, finished, updated_at FROM vod_watch_history;

-- 2) 重建下载表：注意这版把 priority / share_expires_at 两列也带上了
--    （旧版迁移脚本重建时漏了这两列 → 迁移完列消失，下载页与分享页报 no such column）
CREATE TABLE vod_download_new (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	vod_id           INTEGER NOT NULL REFERENCES vod_info(id) ON DELETE CASCADE,
	vod_name         TEXT NOT NULL,
	source_id        INTEGER NOT NULL,
	source_name      TEXT DEFAULT '',
	play_from        TEXT NOT NULL,
	ep_index         INTEGER NOT NULL DEFAULT 0,
	file_path        TEXT DEFAULT '',
	file_size        INTEGER DEFAULT 0,
	episodes         INTEGER DEFAULT 0,
	status           TEXT NOT NULL DEFAULT 'queued',
	priority         INTEGER NOT NULL DEFAULT 0,
	progress         INTEGER DEFAULT 0,
	speed            TEXT DEFAULT '',
	error            TEXT DEFAULT '',
	share_expires_at TEXT NOT NULL DEFAULT '',
	created_at       DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at       DATETIME DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(vod_id, play_from, ep_index)
);

INSERT INTO vod_download_new(id, vod_id, vod_name, source_id, source_name, play_from, ep_index,
	file_path, file_size, episodes, status, priority, progress, speed, error, share_expires_at,
	created_at, updated_at)
	SELECT id, vod_id, vod_name, source_id, source_name, play_from, 0,
		file_path, file_size, episodes, status, 0, progress, speed, error, '',
		created_at, updated_at FROM vod_download;

DROP TABLE vod_download;
ALTER TABLE vod_download_new RENAME TO vod_download;
CREATE INDEX IF NOT EXISTS idx_dl_vod ON vod_download(vod_id);
CREATE INDEX IF NOT EXISTS idx_dl_status ON vod_download(status);

-- 3) 观看历史写回
INSERT OR REPLACE INTO vod_watch_history(download_id, position, duration, finished, updated_at)
	SELECT download_id, position, duration, finished, updated_at FROM tmp_watch_backup;
DROP TABLE tmp_watch_backup;

-- 4) 分享令牌表（分享链接改用令牌，避免自增 id 被枚举）
CREATE TABLE IF NOT EXISTS vod_share_token (
	token       TEXT PRIMARY KEY,
	download_id INTEGER NOT NULL REFERENCES vod_download(id) ON DELETE CASCADE,
	expires_at  TEXT NOT NULL DEFAULT '',
	created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_share_dl ON vod_share_token(download_id);

-- 5) 老库补列（列已存在时会报 duplicate column name，忽略即可，sqlite3 默认继续执行）
ALTER TABLE vod_info ADD COLUMN src_time TEXT DEFAULT '';
ALTER TABLE vod_source ADD COLUMN auto_incr INTEGER NOT NULL DEFAULT 1;

COMMIT;

PRAGMA foreign_keys=ON;

-- 6) 回收空间（可选，大库会花一点时间）
VACUUM;

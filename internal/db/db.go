package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"videoDownLoad/internal/logx"

	_ "modernc.org/sqlite"
)

// Options SQLite 运行参数（NAS 上内存大小差异很大，全部可配）。
type Options struct {
	CacheMB        int // 每个连接的页缓存（MiB）；SQLite 的 cache_size 是「每连接」的
	PageSize       int // 库页大小（0=不改；非 0 且已有库会触发一次 VACUUM 重写）
	MmapMB         int // 内存映射大小（MiB，0=关闭）；读多写少的库开大能明显减少 read() 次数
	Conns          int // 最大连接数；WAL 下可以「多读 + 单写」，1 会让慢查询堵住所有请求
	AutoCheckpoint int // WAL 自动 checkpoint 阈值（页）；太小会频繁 fsync，拖慢读写
}

// InitDB 打开（或创建）SQLite 数据库并初始化表结构。
//
// 参数取舍（8G 内存 NAS 的默认值见 main.go）：
//   - 页缓存按「连接数 × CacheMB」占内存，默认 4 × 64MB ≈ 256MB 上限（按需分配，不会一次吃满）；
//   - mmap 交给 OS 页缓存管，1GB 对 500MB 的库足够覆盖热点，且可被内核回收；
//   - 不再设 soft_heap_limit：32MB 的软上限会让 SQLite 主动丢弃页缓存，
//     和「缓存开大」直接冲突（这是之前「时不时慢一下」的隐藏原因之一）。
func InitDB(path string, opt Options) *sql.DB {
	if opt.CacheMB <= 0 {
		opt.CacheMB = 64
	}
	if opt.Conns <= 0 {
		opt.Conns = 4
	}
	if opt.AutoCheckpoint <= 0 {
		opt.AutoCheckpoint = 2048
	}
	dsn := sqliteDSN(path, opt)
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		logx.Fatalf("open db: %v", err)
	}
	// WAL 下多读并发是安全的：一个写者 + 多个读者，慢查询不再堵住整站。
	// 写操作本身由 SQLite 串行化（busy_timeout=10s 兜住短暂排队）。
	d.SetMaxOpenConns(opt.Conns)
	d.SetMaxIdleConns(opt.Conns)
	d.SetConnMaxIdleTime(5 * time.Minute)
	if err := d.Ping(); err != nil {
		logx.Fatalf("ping db: %v", err)
	}
	mustExec(d, schema)
	migrate(d)
	ensurePlaySchema(d)
	dropRedundantIndexes(d)
	dedupePlays(d)
	ensureNameAgg(d)
	if opt.PageSize > 0 {
		applyPageSize(d, opt.PageSize)
	}
	// 统计信息：没有 ANALYZE 时 SQLite 只能按默认假设选索引，
	// 数据涨到几十万行后容易挑错执行计划（表现为「同一条查询时快时慢」）。
	// optimize 只在需要时做有限分析，启动时跑一次代价很小。
	OptimizeStats(d)
	logPragmas(d)
	return d
}

// sqliteDSN 拼 DSN。
//
// 必须用 file: URI 形式：驱动（modernc.org/sqlite）在 DSN 不以 "file:" 开头时
// 会按「第一个 ?」截断文件名 —— 数据目录里带 ?（Linux 合法，容器里很常见）时
// 会静默打开另一个库文件，表现就是"数据全没了"。URI 形式下 ? # % 需要转义。
func sqliteDSN(path string, opt Options) string {
	p := filepath.ToSlash(path)
	p = strings.ReplaceAll(p, "%", "%25")
	p = strings.ReplaceAll(p, "?", "%3F")
	p = strings.ReplaceAll(p, "#", "%23")
	dsn := "file:" + p + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)" +
		"&_pragma=cache_size(%d)&_pragma=temp_store(2)" +
		"&_pragma=wal_autocheckpoint(%d)&_pragma=journal_size_limit(67108864)" +
		"&_pragma=mmap_size(%d)"
	if opt.PageSize > 0 {
		// 只对"新建库"生效；已有库需要配合下面的 applyPageSize + VACUUM
		dsn += fmt.Sprintf("&_pragma=page_size(%d)", opt.PageSize)
	}
	return fmt.Sprintf(dsn, -opt.CacheMB*1024, opt.AutoCheckpoint, opt.MmapMB<<20)
}

// OptimizeStats 用 SQLite 推荐的 bounded ANALYZE 刷新统计信息（启动时与全量采集结束后各一次）。
func OptimizeStats(d *sql.DB) {
	if _, err := d.Exec(`PRAGMA analysis_limit=2000`); err != nil {
		logx.Debugf("[db] analysis_limit 设置失败: %v", err)
	}
	start := time.Now()
	if _, err := d.Exec(`PRAGMA optimize`); err != nil {
		logx.Printf("[db] PRAGMA optimize 失败（不影响运行）: %v", err)
		return
	}
	logx.Printf("[db] 统计信息已刷新（PRAGMA optimize，%v）", time.Since(start).Round(time.Millisecond))
}

// applyPageSize 把已有库的页大小改成 n：page_size 只能在 VACUUM 时真正生效，
// 所以这里触发一次完整重写。仅在调用方显式要求时执行。
//
// 注意：SQLite 只接受 2 的幂作为 page_size，非 2 的幂会被**静默忽略**
// （PRAGMA 不报错，值也不变）。旧实现只看「当前值 != 目标值」就 VACUUM，
// 于是配置里写错一个非 2 的幂的值 → 每次启动都白跑一次全库 VACUUM（分钟级）。
// 现在改成：设完再读一次确认真的生效了才 VACUUM。
func applyPageSize(d *sql.DB, n int) {
	if !isPowerOfTwo(n) {
		logx.Printf("[db] 页大小 %d 不是 2 的幂，SQLite 会静默忽略，跳过（可选值 512~65536）", n)
		return
	}
	var cur int
	if err := d.QueryRow(`PRAGMA page_size`).Scan(&cur); err != nil || cur == n {
		return
	}
	logx.Printf("[db] 页大小 %d -> %d：开始 VACUUM 重写（大库需要几分钟，期间服务阻塞）…", cur, n)
	start := time.Now()
	if _, err := d.Exec(fmt.Sprintf(`PRAGMA page_size=%d`, n)); err != nil {
		logx.Printf("[db] 设置 page_size 失败: %v", err)
		return
	}
	// 确认真的被接受了再 VACUUM，避免白跑一次全库重写
	var after int
	if err := d.QueryRow(`PRAGMA page_size`).Scan(&after); err != nil || after != n {
		logx.Printf("[db] 页大小仍为 %d（SQLite 只接受 2 的幂），跳过 VACUUM", cur)
		return
	}
	if _, err := d.Exec(`VACUUM`); err != nil {
		logx.Printf("[db] VACUUM 失败（库仍按原页大小可用）: %v", err)
		return
	}
	_, _ = d.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	logx.Printf("[db] 页大小重写完成，耗时 %v", time.Since(start).Round(time.Second))
}

// isPowerOfTwo 判断 n 是否是 2 的幂（SQLite 的 page_size 只接受 2 的幂）
func isPowerOfTwo(n int) bool {
	return n > 0 && n&(n-1) == 0
}

// logPragmas 打印实际生效的关键参数（DEBUG 级）。
// DSN 里的 _pragma 写错不会报错、只会静默失效，留个日志方便在 NAS 上确认真的落地了。
func logPragmas(d *sql.DB) {
	for _, p := range []string{"journal_mode", "cache_size", "mmap_size",
		"wal_autocheckpoint", "temp_store", "journal_size_limit"} {
		var v string
		if err := d.QueryRow("PRAGMA " + p).Scan(&v); err != nil {
			continue
		}
		logx.Debugf("[db] pragma %s = %s", p, v)
	}
}

// dropRedundantIndexes 删掉与其它索引/主键完全重复的索引。
// 每个冗余索引都要在每次 INSERT/UPDATE 时多维护一棵 B 树 —— 全量采集时是纯损耗。
// 这里只删"确定重复/确定无引用"的：
//   - idx_agg_name：与 vod_name_agg 的主键唯一索引一模一样
//   - idx_play_vod：被 UNIQUE(vod_id, ep_index, play_from) 的最左前缀完全覆盖
//   - idx_vod_name：被 idx_vod_name_group(vod_name, source_id) 的最左前缀完全覆盖
//   - idx_vod_type：年份/地区/类型筛选全部走 vod_name_agg 的 idx_agg_*，
//     没有任何查询用 vod_info.type_name 过滤 —— 纯浪费 3.6MB 和写入开销
func dropRedundantIndexes(d *sql.DB) {
	for _, idx := range []string{"idx_agg_name", "idx_play_vod", "idx_vod_name", "idx_vod_type"} {
		if _, err := d.Exec(`DROP INDEX IF EXISTS ` + idx); err != nil {
			logx.Printf("[db] 删除冗余索引 %s 失败: %v", idx, err)
		} else {
			logx.Debugf("[db] 冗余索引已确认移除: %s", idx)
		}
	}
}

const schema = `
-- 一次性维护任务的标记（例如历史数据清理做过没有），避免每次启动重复跑
CREATE TABLE IF NOT EXISTS app_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS vod_source (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL,                -- 接口显示名称
	api_url     TEXT NOT NULL UNIQUE,         -- 形如 https://域名/api.php/provide/vod/
	enabled     INTEGER NOT NULL DEFAULT 1,   -- 是否启用（不启用则不参与任何采集）
	auto_incr   INTEGER NOT NULL DEFAULT 1,   -- 是否参与定时自动增量（0=只手动）
	remark      TEXT DEFAULT '',
	created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 视频基本信息表
CREATE TABLE IF NOT EXISTS vod_info (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	source_id     INTEGER NOT NULL REFERENCES vod_source(id) ON DELETE CASCADE,
	source_vod_id INTEGER NOT NULL,             -- 源站影片ID vod_id
	vod_name      TEXT NOT NULL,
	type_name     TEXT DEFAULT '',              -- 分类
	vod_pic       TEXT DEFAULT '',
	vod_actor     TEXT DEFAULT '',
	vod_director  TEXT DEFAULT '',
	vod_year      TEXT DEFAULT '',
	vod_area      TEXT DEFAULT '',
	vod_lang      TEXT DEFAULT '',
	vod_remarks   TEXT DEFAULT '',              -- 更新备注 如"更新至20集"
	vod_score     TEXT DEFAULT '',
	src_time      TEXT DEFAULT '',              -- 源站更新时间 vod_time（增量去重依据）
	play_hash     TEXT DEFAULT '',              -- 播放地址指纹，用于增量判断是否变化
	local_file    TEXT DEFAULT '',              -- 本地已下载文件路径（空=未下载）
	down_status   TEXT NOT NULL DEFAULT 'none', -- none/downloading/done/failed
	created_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(source_id, source_vod_id)
);
-- 不要建 idx_vod_name(vod_name)：下面的 idx_vod_name_group(vod_name, source_id)
-- 最左前缀就是 vod_name，WHERE vod_name=? / vod_name IN (...) 都能走它。
-- 12.8 万行上单独一份要占 6MB+，且每次写入多维护一棵 B 树。
-- （idx_vod_type 也不要建：筛选全走 vod_name_agg，见 dropRedundantIndexes）

-- 简介单独一张表：vod_content 平均 143 字（UTF-8 中文 ~430B），只有详情页用它；
-- 拆出去后 vod_info 每行窄 ~400B，整棵 B 树、全表扫描、备份体积全部受益。
CREATE TABLE IF NOT EXISTS vod_detail (
	vod_id  INTEGER PRIMARY KEY REFERENCES vod_info(id) ON DELETE CASCADE,
	content TEXT DEFAULT ''
);

-- 播放地址表结构单独放在 playSchema（旧库升级时要整体重建，见 ensurePlaySchema）


-- 观看历史：每个已下载文件记录播放位置，用于"继续观看"
CREATE TABLE IF NOT EXISTS vod_watch_history (
	download_id INTEGER PRIMARY KEY REFERENCES vod_download(id) ON DELETE CASCADE,
	position    REAL NOT NULL DEFAULT 0,              -- 已看到第几秒
	duration    REAL NOT NULL DEFAULT 0,              -- 总时长（秒）
	finished    INTEGER NOT NULL DEFAULT 0,           -- 1=已看完（>=95%）
	updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_watch_updated ON vod_watch_history(updated_at);

-- 收藏：按剧名收藏（同名多源合并后一部剧一条）
CREATE TABLE IF NOT EXISTS vod_fav (
	vod_name        TEXT PRIMARY KEY,
	created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
	seen_remarks    TEXT DEFAULT '',   -- 收藏/上次查看时的更新备注快照
	seen_updated_at TEXT DEFAULT ''    -- 收藏/上次查看时的源站更新时间快照
);

-- 采集断点状态：全量采集可中断续采（6 万页级别的源站必需）
CREATE TABLE IF NOT EXISTS vod_sync_state (
	source_id   INTEGER NOT NULL,
	mode        TEXT NOT NULL,              -- full / incr
	last_page   INTEGER NOT NULL DEFAULT 0, -- 已完成的最后一页
	total_pages INTEGER NOT NULL DEFAULT 0,
	completed   INTEGER NOT NULL DEFAULT 0, -- 1=上次全量已跑完
	updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY(source_id, mode)
);

-- 下载记录表：下载以「视频 × 播放源」为单位（同一部片子不同源可各自下载/删除）
CREATE TABLE IF NOT EXISTS vod_download (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	vod_id      INTEGER NOT NULL REFERENCES vod_info(id) ON DELETE CASCADE,
	vod_name    TEXT NOT NULL,                 -- 冗余，便于列表展示与同名合并
	source_id   INTEGER NOT NULL,
	source_name TEXT DEFAULT '',
	play_from   TEXT NOT NULL,                 -- 播放源标识，如 zuidam3u8
	ep_index    INTEGER NOT NULL DEFAULT 0,    -- 单集下载：集号；勾选多集时每集一条记录
	file_path   TEXT DEFAULT '',               -- 合并后的本地 mp4 绝对路径
	file_size   INTEGER DEFAULT 0,             -- 字节
	episodes    INTEGER DEFAULT 0,             -- 合并的集数
	status      TEXT NOT NULL DEFAULT 'queued',-- queued/downloading/done/failed
	priority    INTEGER NOT NULL DEFAULT 0,    -- 1=用户点了「优先下载」，下一集先跑它
	progress    INTEGER DEFAULT 0,             -- 0-100
	speed       TEXT DEFAULT '',
	error       TEXT DEFAULT '',
	created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(vod_id, play_from, ep_index)
);
CREATE INDEX IF NOT EXISTS idx_dl_vod ON vod_download(vod_id);
CREATE INDEX IF NOT EXISTS idx_dl_status ON vod_download(status);
CREATE INDEX IF NOT EXISTS idx_vod_name_group ON vod_info(vod_name, source_id);

-- 分享令牌：分享链接的有效凭据。
-- 不能拿 vod_download.id 当分享凭据 —— id 是自增整数，能被直接枚举，
-- 拿到 /play/{id} 就等于拿到整个下载库。
-- 令牌是随机串，可单独撤销，文件访问必须带令牌。
-- expires_at 是旧版有效期功能的遗留列（旧库结构兼容），现在固定为空、不再校验：
-- 分享链接永久有效，删除下载记录时令牌随外键级联删除。
CREATE TABLE IF NOT EXISTS vod_share_token (
	token       TEXT PRIMARY KEY,                 -- URL 安全的随机串
	download_id INTEGER NOT NULL REFERENCES vod_download(id) ON DELETE CASCADE,
	expires_at  TEXT NOT NULL DEFAULT '',        -- 已废弃：空=永久有效
	created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_share_dl ON vod_share_token(download_id);

-- 增量同步记录：每次增量采集（手动/定时）每个接口写一条汇总，用于在接口页回看历史。
-- 只记录增量（全量采集不写），每个接口只保留最近 100 条（采集收尾时清理）。
-- 不建外键约束到 vod_source：来源被删除后历史记录仍然可读（source_id 仅作展示/过滤）。
CREATE TABLE IF NOT EXISTS vod_sync_log (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	source_id    INTEGER NOT NULL,               -- 接口 id（可能已删除，不做外键级联）
	source_name  TEXT NOT NULL DEFAULT '',       -- 冗余接口名，改名/删接口后仍可读
	trigger_type TEXT NOT NULL DEFAULT 'manual', -- manual=手动增量 / auto=定时增量
	started_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
	finished_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
	duration_ms  INTEGER NOT NULL DEFAULT 0,     -- 本次增量耗时
	fetched      INTEGER NOT NULL DEFAULT 0,     -- 抓到（比对过）的影片数
	added        INTEGER NOT NULL DEFAULT 0,     -- 新增影片数
	updated      INTEGER NOT NULL DEFAULT 0,     -- 有变化的影片数
	details      INTEGER NOT NULL DEFAULT 0,     -- 补详情请求的批次数
	status       TEXT NOT NULL DEFAULT 'done',   -- running/done/warning/failed/stopped
	error        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sync_log_src ON vod_sync_log(source_id, id DESC);
`

// playSchema 播放地址表（单独一份，因为旧库升级时要整体重建而不是补字段）。
const playSchema = `
-- 播放地址主机字典：源站的播放地址就那么几个 CDN 域名，但每条记录都要把完整 URL
-- 抄一遍。抽出来之后每行只存 1~2 字节的 host_id，226 万行能省 50MB 以上。
CREATE TABLE IF NOT EXISTS play_host (
	id   INTEGER PRIMARY KEY,
	host TEXT NOT NULL UNIQUE
);

-- 播放地址表（与 vod_info 通过 vod_id 关联）
-- WITHOUT ROWID：表本身就按主键组织成一棵 B 树，不再额外维护一份完整索引 ——
-- 旧结构的 UNIQUE(vod_id, ep_index, play_from) 等于第二棵 B 树，白占一份空间。
-- 顺带去掉两列：自增 id（一直靠主键定位，全代码零引用）和 created_at（同样零引用，
-- 226 万行 × 19 字节 ≈ 43MB）。
-- 主键顺序从 (vod_id, ep_index, play_from) 换成 (vod_id, play_from, ep_index)：
-- "取某条线路的全部集"是高频查询，这个顺序能走前缀，旧顺序要扫完整部片子。
CREATE TABLE IF NOT EXISTS vod_play (
	vod_id       INTEGER NOT NULL REFERENCES vod_info(id) ON DELETE CASCADE,
	play_from    TEXT NOT NULL,
	ep_index     INTEGER NOT NULL,
	ep_name      TEXT,             -- 能由 ep_index 推导时留空，见 db.EpName
	host_id      INTEGER,          -- NULL = path 里已是完整地址
	path         TEXT NOT NULL,    -- 去掉主机后的部分
	is_preferred INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(vod_id, play_from, ep_index)
) WITHOUT ROWID;
`

// ensurePlaySchema 播放表建表 / 旧结构升级。
// 旧结构是 rowid 表 + 完整 play_url + created_at，CRETATE TABLE IF NOT EXISTS
// 不会改动已存在的表，所以这里显式检测：带 play_url 列就是旧版，直接重建。
// 播放地址本来就是可以重新采集的数据，不做逐行迁移。
func ensurePlaySchema(d *sql.DB) {
	var hasURL, hasTable, hasSrcID bool
	rows, err := d.Query(`PRAGMA table_info(vod_play)`)
	if err == nil {
		for rows.Next() {
			var cid, notnull, pk int
			var name, typ string
			var dflt sql.NullString
			if rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk) == nil {
				hasTable = true
				if name == "play_url" {
					hasURL = true
				}
				if name == "source_id" {
					hasSrcID = true
				}
			}
		}
		rows.Close()
	}
	if hasTable && hasURL {
		logx.Printf("[db] 播放表升级为紧凑结构（旧播放地址丢弃，需重新采集）")
		if _, err := d.Exec(`DROP TABLE vod_play`); err != nil {
			logx.Printf("[db] 删除旧播放表失败: %v", err)
			return
		}
	} else if hasTable && hasSrcID {
		// source_id 可由 vod_id JOIN vod_info 推导，留着是 449 万行 × 1~5 字节的
		// 纯冗余。列不在主键/索引里，直接 DROP COLUMN 即可，无需重采。
		logx.Printf("[db] 播放表去掉冗余 source_id 列")
		if _, err := d.Exec(`ALTER TABLE vod_play DROP COLUMN source_id`); err != nil {
			logx.Printf("[db] 删除 source_id 列失败: %v", err)
		}
	}
	mustExec(d, playSchema)
}

// dedupePlays 清理历史遗留的重复播放地址（只跑一次，用 app_meta 记标记）。
//
// 源站偶尔把同一集在播放列表里列两遍（例如第 45、46 集给同一个 m3u8），
// 采集侧已经从源头跳过（见 collector.replacePlays），库里已有的重复行在这里清掉：
// 同一部片、同一条线路下，相同 host+path 只保留 ep_index 最小的一条。
func dedupePlays(d *sql.DB) {
	const marker = "dedupe_play_urls_v1"
	if metaGet(d, marker) == "1" {
		return
	}
	type dupKey struct {
		vodID  int64
		from   string
		host   sql.NullInt64
		path   string
		keepEp int64
	}
	var keys []dupKey
	// 必须先把结果读成切片再删除：SQLite 写事务会占住连接，
	// 同一连接上再发查询会自己把自己锁死（连接池默认 4 个连接，但这是同一条连接）
	rows, err := d.Query(`SELECT vod_id, play_from, host_id, path, MIN(ep_index)
		FROM vod_play GROUP BY vod_id, play_from, host_id, path HAVING COUNT(1) > 1`)
	if err != nil {
		logx.Printf("[db] 重复播放地址检查失败: %v", err)
		return
	}
	for rows.Next() {
		var k dupKey
		if rows.Scan(&k.vodID, &k.from, &k.host, &k.path, &k.keepEp) == nil {
			keys = append(keys, k)
		}
	}
	rows.Close()

	removed := 0
	if len(keys) > 0 {
		tx, terr := d.Begin()
		if terr != nil {
			logx.Printf("[db] 清理重复播放地址失败: %v", terr)
			return
		}
		stmt, serr := tx.Prepare(`DELETE FROM vod_play
			WHERE vod_id=? AND play_from=? AND host_id IS ? AND path=? AND ep_index<>?`)
		if serr != nil {
			_ = tx.Rollback()
			logx.Printf("[db] 清理重复播放地址失败: %v", serr)
			return
		}
		for _, k := range keys {
			res, e := stmt.Exec(k.vodID, k.from, k.host, k.path, k.keepEp)
			if e != nil {
				continue
			}
			if n, e2 := res.RowsAffected(); e2 == nil {
				removed += int(n)
			}
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			logx.Printf("[db] 提交重复播放地址清理失败: %v", err)
			// 提交失败时必须回滚：否则连接会带着未结束的事务回到池里，
			// 后续写操作持续 SQLITE_BUSY（连接池默认只有 4 个连接）
			_ = tx.Rollback()
			return
		}
	}
	metaSet(d, marker, "1")
	if removed > 0 {
		logx.Printf("[db] 已清理重复播放地址 %d 行（%d 组）", removed, len(keys))
	}
}

func metaGet(d *sql.DB, key string) string {
	var v string
	if err := d.QueryRow(`SELECT value FROM app_meta WHERE key=?`, key).Scan(&v); err != nil {
		return ""
	}
	return v
}

func metaSet(d *sql.DB, key, val string) {
	if _, err := d.Exec(`INSERT INTO app_meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, val); err != nil {
		logx.Printf("[db] 写维护标记失败 %s: %v", key, err)
	}
}

// 兼容旧库：已存在的表补加新字段（SQLite 无 ALTER ... IF NOT EXISTS，用 PRAGMA 判断）
func migrate(d *sql.DB) {
	ensureColumn(d, "vod_info", "src_time", "TEXT DEFAULT ''")
	// 接口级「自动增量」开关：老库补列，默认全开（与升级前行为一致）
	ensureColumn(d, "vod_source", "auto_incr", "INTEGER NOT NULL DEFAULT 1")
	// 下载优先级：点「优先下载」后插到队首，当前这一集下完就轮到它
	ensureColumn(d, "vod_download", "priority", "INTEGER NOT NULL DEFAULT 0")
	// 分享页失效时间（UTC "YYYY-MM-DD HH:mm:ss"，空=永久）：过期后 /s/{id} 提示已过期
	ensureColumn(d, "vod_download", "share_expires_at", "TEXT NOT NULL DEFAULT ''")
	migrateDetailContent(d)
	migrateDownloadEpIndex(d)
	// 兜底：整表重建（migrateDownloadEpIndex）之后再确认这两列还在。
	// 旧版实现在重建时把新表定义写漏了这两列，迁移完列直接消失，
	// 下载页与分享页会报 no such column —— 这两行让历史坏库也能自愈。
	ensureColumn(d, "vod_download", "priority", "INTEGER NOT NULL DEFAULT 0")
	ensureColumn(d, "vod_download", "share_expires_at", "TEXT NOT NULL DEFAULT ''")
}

// migrateDetailContent 旧库的 vod_content 还在 vod_info 里：
// 搬到 vod_detail 后把主表列删掉（不搬迁兼容负担，只做一次无损搬运）。
func migrateDetailContent(d *sql.DB) {
	if !hasColumn(d, "vod_info", "vod_content") {
		return
	}
	logx.Printf("[db] vod_content 迁移到 vod_detail…")
	if _, err := d.Exec(`INSERT OR IGNORE INTO vod_detail(vod_id, content)
		SELECT id, vod_content FROM vod_info WHERE vod_content<>''`); err != nil {
		logx.Printf("[db] 简介搬迁失败（保留旧列，新数据仍写 vod_detail）: %v", err)
		return
	}
	// 列不在索引/主键里，可直接 DROP COLUMN；删列后旧值占的页进 freelist，
	// 交给既有 auto-vacuum 回收即可
	if _, err := d.Exec(`ALTER TABLE vod_info DROP COLUMN vod_content`); err != nil {
		logx.Printf("[db] 删除 vod_info.vod_content 失败（不影响运行）: %v", err)
	}
}

// hasColumn 判断表里是否已有该列
func hasColumn(d *sql.DB, table, column string) bool {
	rows, err := d.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err == nil && name == column {
			return true
		}
	}
	return false
}

// migrateDownloadEpIndex 旧库的 vod_download 只有 UNIQUE(vod_id, play_from)，
// 无法表达"同一线路的多集分别下载"，整表重建一次（ep_index 缺失的老行补 0）
//
// 两个必须注意的点（2026-10-21 修复，都是实测踩出来的）：
//  1. 重建不能丢列：priority / share_expires_at 是 migrate() 先补上去的，
//     旧实现的新表定义里没有这两列 → 迁移完两列消失，下载页/分享页直接报
//     no such column。现在按「旧表实际有哪些列」动态生成 INSERT 列表，补过的列原样带走。
//  2. 重建不能丢观看历史：DROP TABLE 在 foreign_keys=1 下会触发 ON DELETE CASCADE，
//     把 vod_watch_history 里引用这些 id 的行一起删掉（实测 1 行 → 0 行）。
//     先把历史行备份到临时表，重建后写回。
func migrateDownloadEpIndex(d *sql.DB) {
	oldCols := tableColumns(d, "vod_download")
	if len(oldCols) == 0 {
		return
	}
	if containsStr(oldCols, "ep_index") {
		return
	}
	logx.Printf("[db] 升级 vod_download 表结构（支持单集下载）")

	// 1) 备份观看历史（DROP TABLE 会级联删除）
	if _, err := d.Exec(`DROP TABLE IF EXISTS tmp_watch_backup`); err != nil {
		logx.Printf("[db] 清理历史备份表失败，迁移中止: %v", err)
		return
	}
	if _, err := d.Exec(`CREATE TABLE tmp_watch_backup AS
		SELECT download_id, position, duration, finished, updated_at FROM vod_watch_history`); err != nil {
		logx.Printf("[db] 备份观看历史失败，迁移中止: %v", err)
		return
	}

	// 2) 动态生成列清单：只搬旧表里真实存在的列，其余交给新表默认值
	newCols := []string{
		"id", "vod_id", "vod_name", "source_id", "source_name", "play_from", "ep_index",
		"file_path", "file_size", "episodes", "status", "priority", "progress", "speed",
		"error", "share_expires_at", "created_at", "updated_at",
	}
	var sel, expr []string
	for _, c := range newCols {
		if !containsStr(oldCols, c) {
			continue
		}
		sel = append(sel, c)
		expr = append(expr, c)
	}
	insertSQL := `INSERT INTO vod_download_new(` + strings.Join(sel, ",") + `)
		SELECT ` + strings.Join(expr, ",") + ` FROM vod_download`

	tx, terr := d.Begin()
	if terr != nil {
		logx.Printf("[db] 迁移失败: %v", terr)
		return
	}
	stmts := []string{
		`CREATE TABLE vod_download_new (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			vod_id      INTEGER NOT NULL REFERENCES vod_info(id) ON DELETE CASCADE,
			vod_name    TEXT NOT NULL,
			source_id   INTEGER NOT NULL,
			source_name TEXT DEFAULT '',
			play_from   TEXT NOT NULL,
			ep_index    INTEGER NOT NULL DEFAULT 0,
			file_path   TEXT DEFAULT '',
			file_size   INTEGER DEFAULT 0,
			episodes    INTEGER DEFAULT 0,
			status      TEXT NOT NULL DEFAULT 'queued',
			priority    INTEGER NOT NULL DEFAULT 0,
			progress    INTEGER DEFAULT 0,
			speed       TEXT DEFAULT '',
			error       TEXT DEFAULT '',
			share_expires_at TEXT NOT NULL DEFAULT '',
			created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(vod_id, play_from, ep_index)
		)`,
		insertSQL,
		`DROP TABLE vod_download`,
		`ALTER TABLE vod_download_new RENAME TO vod_download`,
		`CREATE INDEX IF NOT EXISTS idx_dl_vod ON vod_download(vod_id)`,
		`CREATE INDEX IF NOT EXISTS idx_dl_status ON vod_download(status)`,
		// 3) 观看历史写回（id 保持不变，外键仍然成立）
		`INSERT OR REPLACE INTO vod_watch_history(download_id, position, duration, finished, updated_at)
			SELECT download_id, position, duration, finished, updated_at FROM tmp_watch_backup`,
		`DROP TABLE tmp_watch_backup`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			logx.Printf("[db] 迁移语句失败: %v | %s", err, q[:60])
			_ = tx.Rollback()
			return
		}
	}
	if err := tx.Commit(); err != nil {
		logx.Printf("[db] 迁移提交失败: %v", err)
		_ = tx.Rollback()
	}
}

// tableColumns 读表的列名（按 PRAGMA 顺序）
func tableColumns(d *sql.DB, table string) []string {
	rows, err := d.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err == nil {
			cols = append(cols, name)
		}
	}
	return cols
}

// containsStr 判断字符串是否在切片里
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func ensureColumn(d *sql.DB, table, column, def string) {
	rows, err := d.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err == nil && name == column {
			return
		}
	}
	if _, err := d.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + def); err != nil {
		logx.Printf("migrate %s.%s: %v", table, column, err)
	}
}

func mustExec(d *sql.DB, s string) {
	if _, err := d.Exec(s); err != nil {
		logx.Fatalf("exec schema: %v", err)
	}
}

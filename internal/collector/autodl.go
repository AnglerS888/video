package collector

import (
	"sync/atomic"
	"time"

	"videoDownLoad/internal/logx"
)

// ============================================================================
// 六、自动追更：收藏的剧在「指定来源 + 指定线路」上出现新集数时，自动加入下载
//
// 为什么放在采集收尾而不是每保存一页时判断：
//   - 一页只有 100 条，同一部剧的多条线路可能分散在不同页里，逐页判断会漏；
//   - 采集收尾时 vod_play 里已经是这条线路的完整集数，比较一次水位即可。
//
// 水位 auto_ep_max 存在 vod_fav 表上：
//   - 开启追更时锚定为「当前最大集号」，只追开启之后的新集数（不倒追已有全集）；
//   - 之后每次发现更大的集号，只把 (旧水位, 最大集号] 之间的集入队，再把水位推上去；
//   - 入队失败（磁盘不足 / 下载队列满）不推进水位，下一轮采集会再试，不会静默丢集。
// ============================================================================

// autoNewEpEnabled 自动追更总开关（download.auto_new_ep，信息页保存后热更新）。
// 默认开启：具体盯哪部剧还要在详情页手动打开，所以默认开着不会凭空产生下载任务。
var autoNewEpEnabled atomic.Bool

func init() { autoNewEpEnabled.Store(true) }

// SetAutoNewEpEnabled 设置自动追更总开关
func SetAutoNewEpEnabled(on bool) { autoNewEpEnabled.Store(on) }

// AutoNewEpEnabled 当前总开关状态（日志/页面展示用）
func AutoNewEpEnabled() bool { return autoNewEpEnabled.Load() }

// autoFav 一条开启了自动追更的收藏
type autoFav struct {
	name     string
	vodID    int64
	playFrom string
	epMax    int
}

// AutoDownloadNewEpisodes 把某个来源下「开启了追更」的收藏里新出现的集数加入下载队列。
// 返回本次新建的下载任务数。sourceID <= 0 表示检查全部来源。
func (c *Collector) AutoDownloadNewEpisodes(sourceID int64) int {
	if c == nil || c.DB == nil || !autoNewEpEnabled.Load() {
		return 0
	}
	q := `SELECT f.vod_name, f.auto_vod_id, f.auto_play_from, f.auto_ep_max
		FROM vod_fav f JOIN vod_info v ON v.id=f.auto_vod_id
		WHERE f.auto_dl=1 AND f.auto_play_from<>''`
	args := []any{}
	if sourceID > 0 {
		q += ` AND v.source_id=?`
		args = append(args, sourceID)
	}
	rows, err := c.DB.Query(q, args...)
	if err != nil {
		logx.Debugf("[auto-dl] 读取追更列表失败: %v", err)
		return 0
	}
	favs := make([]autoFav, 0, 8)
	for rows.Next() {
		var f autoFav
		if err := rows.Scan(&f.name, &f.vodID, &f.playFrom, &f.epMax); err == nil && f.vodID > 0 {
			favs = append(favs, f)
		}
	}
	// 必须读完就关：下面要写库，同一条连接上还开着结果集会把自己锁住
	rows.Close()

	created := 0
	for _, f := range favs {
		created += c.autoDownloadOne(f)
	}
	return created
}

// autoDownloadOne 对单个收藏做一次追更判断并入队
func (c *Collector) autoDownloadOne(f autoFav) int {
	rows, err := c.DB.Query(`SELECT ep_index FROM vod_play
		WHERE vod_id=? AND play_from=? AND path<>'' ORDER BY ep_index`, f.vodID, f.playFrom)
	if err != nil {
		return 0
	}
	eps := make([]int, 0, 32)
	for rows.Next() {
		var ep int
		if rows.Scan(&ep) == nil {
			eps = append(eps, ep)
		}
	}
	rows.Close()
	if len(eps) == 0 {
		// 线路被源站改名/暂时消失：保留水位等它回来（清零会导致恢复后重复下载整条线路）
		logx.Debugf("[auto-dl] %s 追更线路 %s 暂无可用剧集，本次跳过", f.name, f.playFrom)
		return 0
	}
	maxEp := eps[len(eps)-1]
	if f.epMax < 0 {
		// 水位没初始化（历史数据）：只记水位，不倒追已有剧集
		c.setAutoEpMax(f, maxEp)
		return 0
	}
	if maxEp <= f.epMax {
		return 0
	}
	idxs := make([]int, 0, maxEp-f.epMax)
	for _, ep := range eps {
		if ep > f.epMax {
			idxs = append(idxs, ep)
		}
	}
	if len(idxs) == 0 {
		c.setAutoEpMax(f, maxEp)
		return 0
	}
	created, skipped, err := EnqueueDownloadBatch(f.vodID, f.playFrom, idxs)
	if err != nil {
		// 磁盘不足 / 队列满：水位不动，下次采集重试（否则这些集会永久丢掉）
		logx.Printf("[auto-dl] %s 新 %d 集自动下载失败（下次采集重试）: %v", f.name, len(idxs), err)
		return 0
	}
	c.setAutoEpMax(f, maxEp)
	logx.Printf("[auto-dl] %s 第 %d~%d 集自动加入下载（新建 %d，跳过 %d）",
		f.name, idxs[0]+1, idxs[len(idxs)-1]+1, created, skipped)
	return created
}

// setAutoEpMax 推进追更水位。只增不减：并发采集 / 重复扫描时不会被旧值覆盖回去。
func (c *Collector) setAutoEpMax(f autoFav, epMax int) {
	if _, err := c.DB.Exec(`UPDATE vod_fav SET auto_ep_max=MAX(auto_ep_max, ?), auto_updated_at=?
		WHERE vod_name=?`, epMax, time.Now().UTC().Format(timeLayout), f.name); err != nil {
		logx.Debugf("[auto-dl] %s 追更水位保存失败: %v", f.name, err)
	}
}

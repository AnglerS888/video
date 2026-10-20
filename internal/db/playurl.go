package db

import (
	"fmt"
	"strings"
)

// 播放地址表是全库最大的表（226 万行时占有效数据的八成），这几项是纯空间优化：
// 主机抽成字典（源站就那么几个 CDN 域名，原本每行都要重复写一遍）、
// 集名能推导时留空。两者对上层完全透明 —— 读的时候由下面的 SQL 片段拼回去。

// EpName 按集号推导默认集名。
// ep_index 是 0 起算的（采集时按源站顺序编号），集名是 1 起算的，所以这里 +1。
// 采集时源站给的集名正好等于它就存空串，读的时候再推回来。
// 万一某个源站格式不是这样，也只会退化成"照原样存"，不会显示错集名。
func EpName(idx int) string {
	return fmt.Sprintf("第%02d集", idx+1)
}

// SQL 片段：把拆开的播放地址拼回完整 URL。
// 用法：`SELECT `+db.PlayURLExpr+` FROM vod_play p LEFT JOIN play_host h ON h.id=p.host_id`
const PlayURLExpr = `COALESCE(h.host,'') || p.path`

// SQL 片段：集名，空则按集号推导（推导式必须与 EpName 完全一致，故 +1）。
const EpNameExpr = `COALESCE(NULLIF(p.ep_name,''), '第' || printf('%02d', p.ep_index+1) || '集')`

// SplitURL 把播放地址拆成「主机」和「路径」。
// 没有路径时（如 https://host）整串都算主机，path 返回空。
func SplitURL(u string) (host, path string) {
	rest, scheme := u, ""
	if i := strings.Index(u, "://"); i >= 0 {
		scheme, rest = u[:i+3], u[i+3:]
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		return scheme + rest[:i], rest[i:]
	}
	return scheme + rest, ""
}

// JoinURL 拼回完整地址（host 为空时 path 本身就是完整地址）。
func JoinURL(host, path string) string {
	if host == "" {
		return path
	}
	return host + path
}

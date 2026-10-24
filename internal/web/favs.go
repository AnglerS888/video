package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// favTimeLayout 与采集侧一致的时间格式（UTC，页面按本地时区展示）
const favTimeLayout = "2006-01-02 15:04:05"

func nowUTC() string { return time.Now().UTC().Format(favTimeLayout) }

// listFavs 收藏列表：带上当前影片信息、「是否有更新」判断与自动追更状态
func (s *Server) listFavs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(`SELECT f.vod_name, f.created_at, f.seen_remarks, f.seen_updated_at,
		COALESCE(a.cnt,0), COALESCE(a.vod_pic,''), COALESCE(a.type_name,''), COALESCE(a.vod_year,''),
		COALESCE(a.vod_area,''), COALESCE(a.vod_remarks,''), COALESCE(a.updated_at,''), COALESCE(a.rid,0),
		COALESCE(f.auto_dl,0), COALESCE(f.auto_vod_id,0), COALESCE(f.auto_play_from,''), COALESCE(f.auto_ep_max,-1)
		FROM vod_fav f LEFT JOIN vod_name_agg a ON a.vod_name = f.vod_name
		ORDER BY f.created_at DESC`)
	if err != nil {
		fail(w, publicErr(err))
		return
	}
	defer rows.Close()

	list := []map[string]any{}
	updates := 0
	for rows.Next() {
		var name, created, seenRemarks, seenUpdated sql.NullString
		var cnt, rid, autoVodID int64
		var pic, typ, year, area, remarks, updated string
		var autoDL, autoEpMax int
		var autoFrom string
		if err := rows.Scan(&name, &created, &seenRemarks, &seenUpdated, &cnt, &pic, &typ,
			&year, &area, &remarks, &updated, &rid, &autoDL, &autoVodID, &autoFrom, &autoEpMax); err != nil {
			continue
		}
		gone := rid == 0 && remarks == ""
		hasUpdate := !gone && (updated != seenUpdated.String || remarks != seenRemarks.String)
		if hasUpdate {
			updates++
		}
		// 追更线路里「还没入队」的新集数：进度条之外给个明确提示（0 表示已追平）
		pending := 0
		if autoDL == 1 && autoVodID > 0 && autoFrom != "" && autoEpMax >= 0 {
			s.DB.QueryRow(`SELECT COUNT(*) FROM vod_play
				WHERE vod_id=? AND play_from=? AND path<>'' AND ep_index>?`,
				autoVodID, autoFrom, autoEpMax).Scan(&pending)
		}
		list = append(list, map[string]any{
			"vod_name": name.String, "id": rid, "vod_pic": pic, "type_name": typ,
			"vod_year": year, "vod_area": area, "vod_remarks": remarks,
			"source_count": cnt, "updated_at": updated, "created_at": created.String,
			"has_update": hasUpdate, "gone": gone,
			"auto_dl": autoDL, "auto_vod_id": autoVodID, "auto_play_from": autoFrom,
			"auto_new_eps": pending,
		})
	}
	ok(w, map[string]any{"list": list, "update_count": updates})
}

func (s *Server) addFav(w http.ResponseWriter, r *http.Request) {
	var in struct {
		VodName string `json:"vod_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.VodName) == "" {
		fail(w, "参数错误")
		return
	}
	name := strings.TrimSpace(in.VodName)
	remarks, updated := s.favSnapshot(name)
	if _, err := s.DB.Exec(`INSERT INTO vod_fav(vod_name, seen_remarks, seen_updated_at) VALUES(?,?,?)
		ON CONFLICT(vod_name) DO UPDATE SET seen_remarks=excluded.seen_remarks, seen_updated_at=excluded.seen_updated_at`,
		name, remarks, updated); err != nil {
		fail(w, publicErr(err))
		return
	}
	ok(w, "已收藏")
}

// favSnapshot 取某剧的当前更新状态（用于「有更新」提示的快照）。
// 查不到（剧被删/聚合表还没建）时返回空串 —— 注意必须检查错误：
// 旧实现忽略 Scan 的错误，查不到时快照为空 → 收藏列表永远显示「有更新」红点。
func (s *Server) favSnapshot(name string) (remarks, updated string) {
	err := s.DB.QueryRow(`SELECT COALESCE(vod_remarks,''), COALESCE(updated_at,'') FROM vod_name_agg WHERE vod_name=?`, name).
		Scan(&remarks, &updated)
	if err != nil {
		return "", ""
	}
	return remarks, updated
}

func (s *Server) delFav(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		fail(w, "缺少 name")
		return
	}
	if _, err := s.DB.Exec(`DELETE FROM vod_fav WHERE vod_name=?`, name); err != nil {
		fail(w, publicErr(err))
		return
	}
	ok(w, "已取消收藏")
}

// setFavAuto 开启/关闭某部剧的自动追更：盯住「某个来源的某条线路」，
// 采集结束后该线路出现新集数就自动加入下载队列。
//
// 开启时把水位锚定在当前最大集号：只追开启之后的新集，不倒追已有全集；
// 同一条线路再次开启时保留原来的水位（期间新增的集会补下），换线路才重新锚定。
func (s *Server) setFavAuto(w http.ResponseWriter, r *http.Request) {
	var in struct {
		VodName  string `json:"vod_name"`
		VodID    int64  `json:"vod_id"`
		PlayFrom string `json:"play_from"`
		Enabled  bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, "参数错误: "+err.Error())
		return
	}
	name := strings.TrimSpace(in.VodName)
	if name == "" {
		fail(w, "缺少 vod_name")
		return
	}
	if !in.Enabled {
		// 关闭只清开关，保留已选线路与水位：下次再开启能接着追同一部剧
		if _, err := s.DB.Exec(`UPDATE vod_fav SET auto_dl=0, auto_updated_at=? WHERE vod_name=?`,
			nowUTC(), name); err != nil {
			fail(w, publicErr(err))
			return
		}
		ok(w, map[string]any{"auto_dl": 0, "msg": "已关闭自动追更"})
		return
	}

	from := strings.TrimSpace(in.PlayFrom)
	if in.VodID <= 0 || from == "" {
		fail(w, "请先在详情页选择来源与线路")
		return
	}
	// 校验归属：vod_id 必须是这部剧的记录，避免前端传错时把追更挂到别的剧上
	var owner string
	if err := s.DB.QueryRow(`SELECT vod_name FROM vod_info WHERE id=?`, in.VodID).Scan(&owner); err != nil || owner != name {
		fail(w, "视频已变更，请刷新页面后重试")
		return
	}
	var cnt int
	var maxEp int
	if err := s.DB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(ep_index),-1) FROM vod_play
		WHERE vod_id=? AND play_from=? AND path<>''`, in.VodID, from).Scan(&cnt, &maxEp); err != nil || cnt == 0 {
		fail(w, "该线路没有可下载的剧集")
		return
	}

	// 没收藏过就顺手收藏（追更挂在收藏上），快照取当前的更新状态
	remarks, updated := s.favSnapshot(name)
	if _, err := s.DB.Exec(`INSERT INTO vod_fav(vod_name, seen_remarks, seen_updated_at, auto_dl,
			auto_vod_id, auto_play_from, auto_ep_max, auto_updated_at)
		VALUES(?,?,?,1,?,?,?,?)
		ON CONFLICT(vod_name) DO UPDATE SET
			auto_dl=1,
			auto_vod_id=excluded.auto_vod_id,
			auto_play_from=excluded.auto_play_from,
			auto_ep_max=CASE
				WHEN vod_fav.auto_vod_id=excluded.auto_vod_id
				 AND vod_fav.auto_play_from=excluded.auto_play_from
				 AND vod_fav.auto_ep_max>=0 THEN vod_fav.auto_ep_max
				ELSE excluded.auto_ep_max END,
			auto_updated_at=excluded.auto_updated_at`,
		name, remarks, updated, in.VodID, from, maxEp, nowUTC()); err != nil {
		fail(w, publicErr(err))
		return
	}
	ok(w, map[string]any{
		"auto_dl": 1, "auto_vod_id": in.VodID, "auto_play_from": from, "auto_ep_max": maxEp,
		"msg": "已开启自动追更：这条线路以后再出现新集数会自动加入下载队列",
	})
}

// seenFav 标记已读：把当前更新状态存为快照，清除"有更新"提示
func (s *Server) seenFav(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		fail(w, "缺少 name")
		return
	}
	remarks, updated := s.favSnapshot(name)
	if _, err := s.DB.Exec(`UPDATE vod_fav SET seen_remarks=?, seen_updated_at=? WHERE vod_name=?`,
		remarks, updated, name); err != nil {
		fail(w, publicErr(err))
		return
	}
	ok(w, nil)
}

package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
)

// listFavs 收藏列表：带上当前影片信息与"是否有更新"判断
func (s *Server) listFavs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(`SELECT f.vod_name, f.created_at, f.seen_remarks, f.seen_updated_at,
		COALESCE(a.cnt,0), COALESCE(a.vod_pic,''), COALESCE(a.type_name,''), COALESCE(a.vod_year,''),
		COALESCE(a.vod_area,''), COALESCE(a.vod_remarks,''), COALESCE(a.updated_at,''), COALESCE(a.rid,0)
		FROM vod_fav f LEFT JOIN vod_name_agg a ON a.vod_name = f.vod_name
		ORDER BY f.created_at DESC`)
	if err != nil {
		fail(w, err.Error())
		return
	}
	defer rows.Close()

	list := []map[string]any{}
	updates := 0
	for rows.Next() {
		var name, created, seenRemarks, seenUpdated sql.NullString
		var cnt, rid int64
		var pic, typ, year, area, remarks, updated string
		if err := rows.Scan(&name, &created, &seenRemarks, &seenUpdated, &cnt, &pic, &typ,
			&year, &area, &remarks, &updated, &rid); err != nil {
			continue
		}
		gone := rid == 0 && remarks == ""
		hasUpdate := !gone && (updated != seenUpdated.String || remarks != seenRemarks.String)
		if hasUpdate {
			updates++
		}
		list = append(list, map[string]any{
			"vod_name": name.String, "id": rid, "vod_pic": pic, "type_name": typ,
			"vod_year": year, "vod_area": area, "vod_remarks": remarks,
			"source_count": cnt, "updated_at": updated, "created_at": created.String,
			"has_update": hasUpdate, "gone": gone,
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
	var remarks, updated string
	s.DB.QueryRow(`SELECT COALESCE(vod_remarks,''), COALESCE(updated_at,'') FROM vod_name_agg WHERE vod_name=?`, name).
		Scan(&remarks, &updated)
	if _, err := s.DB.Exec(`INSERT INTO vod_fav(vod_name, seen_remarks, seen_updated_at) VALUES(?,?,?)
		ON CONFLICT(vod_name) DO UPDATE SET seen_remarks=excluded.seen_remarks, seen_updated_at=excluded.seen_updated_at`,
		name, remarks, updated); err != nil {
		fail(w, err.Error())
		return
	}
	ok(w, "已收藏")
}

func (s *Server) delFav(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		fail(w, "缺少 name")
		return
	}
	if _, err := s.DB.Exec(`DELETE FROM vod_fav WHERE vod_name=?`, name); err != nil {
		fail(w, err.Error())
		return
	}
	ok(w, "已取消收藏")
}

// seenFav 标记已读：把当前更新状态存为快照，清除"有更新"提示
func (s *Server) seenFav(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		fail(w, "缺少 name")
		return
	}
	var remarks, updated string
	s.DB.QueryRow(`SELECT COALESCE(vod_remarks,''), COALESCE(updated_at,'') FROM vod_name_agg WHERE vod_name=?`, name).
		Scan(&remarks, &updated)
	if _, err := s.DB.Exec(`UPDATE vod_fav SET seen_remarks=?, seen_updated_at=? WHERE vod_name=?`,
		remarks, updated, name); err != nil {
		fail(w, err.Error())
		return
	}
	ok(w, nil)
}

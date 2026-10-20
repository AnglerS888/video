package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// saveWatchProgress 记录播放位置（前端每 5 秒上报一次，暂停/结束也会补报）
func (s *Server) saveWatchProgress(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DownloadID int64   `json:"download_id"`
		Position   float64 `json:"position"`
		Duration   float64 `json:"duration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.DownloadID <= 0 {
		fail(w, "参数错误")
		return
	}
	if in.Position < 0 {
		in.Position = 0
	}
	// 走到 95% 视为看完：这类记录不再出现在"继续观看"
	finished := 0
	if in.Duration > 0 && in.Position >= in.Duration*0.95 {
		finished = 1
	}
	_, err := s.DB.Exec(`INSERT INTO vod_watch_history(download_id, position, duration, finished, updated_at)
		VALUES(?,?,?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(download_id) DO UPDATE SET
			position=excluded.position, duration=excluded.duration,
			finished=excluded.finished, updated_at=CURRENT_TIMESTAMP`,
		in.DownloadID, in.Position, in.Duration, finished)
	if err != nil {
		fail(w, err.Error())
		return
	}
	ok(w, map[string]any{"finished": finished})
}

// watchHistory 观看历史（只包含仍然存在的已下载剧集），按最近观看排序
func (s *Server) watchHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(`SELECT h.download_id, d.vod_id, d.vod_name, d.ep_index, d.source_name,
		COALESCE(d.file_path,''), COALESCE(v.vod_pic,''), h.position, h.duration, h.finished, h.updated_at
		FROM vod_watch_history h
		JOIN vod_download d ON d.id = h.download_id
		LEFT JOIN vod_info v ON v.id = d.vod_id
		WHERE d.status='done'
		ORDER BY h.updated_at DESC LIMIT 200`)
	if err != nil {
		fail(w, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, vodID int64
		var vodName, srcName, file, pic, updated string
		var epIdx, finished int
		var pos, dur float64
		if rows.Scan(&id, &vodID, &vodName, &epIdx, &srcName, &file, &pic, &pos, &dur, &finished, &updated) == nil {
			out = append(out, map[string]any{
				"download_id": id, "vod_id": vodID, "vod_name": vodName, "ep_index": epIdx,
				"source_name": srcName, "vod_pic": pic,
				"position": pos, "duration": dur, "finished": finished, "updated_at": updated,
			})
		}
	}
	ok(w, out)
}

// delWatchHistory 从观看历史里移除一条（只删进度记录，不动下载的文件）
func (s *Server) delWatchHistory(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id <= 0 {
		fail(w, "参数错误")
		return
	}
	res, err := s.DB.Exec(`DELETE FROM vod_watch_history WHERE download_id=?`, id)
	if err != nil {
		fail(w, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, "这条记录已经不在了")
		return
	}
	ok(w, "已从观看历史移除")
}

// clearWatchHistory 清空观看历史（「继续观看」也会一起清掉，文件不受影响）
func (s *Server) clearWatchHistory(w http.ResponseWriter, r *http.Request) {
	res, err := s.DB.Exec(`DELETE FROM vod_watch_history`)
	if err != nil {
		fail(w, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	ok(w, map[string]any{"deleted": n, "msg": fmt.Sprintf("已清空 %d 条观看记录", n)})
}

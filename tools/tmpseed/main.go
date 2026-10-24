// 临时验证程序：向测试库塞一部「追更测试剧」，然后由采集收尾的自动追更扫描处理。
// 用完即删，不属于项目代码。
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("用法: tmpseed <db> <source_id>")
		os.Exit(1)
	}
	db, err := sql.Open("sqlite", os.Args[1])
	if err != nil {
		panic(err)
	}
	defer db.Close()
	var sourceID int64
	fmt.Sscan(os.Args[2], &sourceID)

	res, err := db.Exec(`INSERT INTO vod_info(source_id, source_vod_id, vod_name, type_name, vod_remarks)
		VALUES(?,?,?,?,?)`, sourceID, 99001, "追更测试剧", "国产剧", "更新第2集")
	if err != nil {
		panic(err)
	}
	vodID, _ := res.LastInsertId()

	for i := 0; i < 3; i++ {
		if _, err := db.Exec(`INSERT INTO vod_play(vod_id, play_from, ep_index, ep_name, host_id, path)
			VALUES(?,?,?,?,?,?)`, vodID, "testm3u8", i, "", nil,
			fmt.Sprintf("http://127.0.0.1:59999/ep%d.m3u8", i+1)); err != nil {
			panic(err)
		}
	}
	// 水位 1：只该追 ep_index=2（第 3 集）
	if _, err := db.Exec(`INSERT INTO vod_fav(vod_name, seen_remarks, seen_updated_at, auto_dl,
			auto_vod_id, auto_play_from, auto_ep_max, auto_updated_at)
		VALUES(?,?,?,1,?,?,?,?)`, "追更测试剧", "更新第2集", "2026-10-24 00:00:00",
		vodID, "testm3u8", 1, "2026-10-24 00:00:00"); err != nil {
		panic(err)
	}
	fmt.Printf("seeded vod_id=%d source_id=%d\n", vodID, sourceID)
}

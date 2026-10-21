package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"videoDownLoad/internal/logx"
)

// shareTokenExpireLayout 分享失效时间格式（UTC）
const shareTokenExpireLayout = "2006-01-02 15:04:05"

// NewShareToken 生成分享令牌并写入库。
// 返回的令牌是 URL 安全的随机串（32 hex 字符 = 128 bit），可单独撤销、可过期。
func NewShareToken(d *sql.DB, downloadID int64, expires time.Time) (string, error) {
	if expires.IsZero() {
		expires = time.Now().UTC().Add(24 * time.Hour)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成分享令牌失败: %w", err)
	}
	token := hex.EncodeToString(raw)
	_, err := d.Exec(`INSERT INTO vod_share_token(token, download_id, expires_at) VALUES(?,?,?)`,
		token, downloadID, expires.UTC().Format(shareTokenExpireLayout))
	if err != nil {
		return "", err
	}
	return token, nil
}

// ShareTokenLookup 按令牌查对应的下载记录。
// 返回 downloadID、失效时间字符串、是否有效。
// 令牌不存在 / 已过期 → ok=false。
func ShareTokenLookup(d *sql.DB, token string) (downloadID int64, expires string, ok bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, "", false
	}
	var exp string
	err := d.QueryRow(`SELECT download_id, expires_at FROM vod_share_token WHERE token=?`, token).Scan(&downloadID, &exp)
	if err != nil {
		return 0, "", false
	}
	if ShareExpired(exp) {
		return downloadID, exp, false
	}
	return downloadID, exp, true
}

// ShareExpired 判断失效时间是否已过。空=永久有效（不过期）。
// 解析失败按"已过期"处理：脏数据不该让链接继续可用。
func ShareExpired(expires string) bool {
	expires = strings.TrimSpace(expires)
	if expires == "" {
		return false
	}
	t, err := time.Parse(shareTokenExpireLayout, expires)
	if err != nil {
		// 兼容带时区后缀的写法
		if t2, err2 := time.Parse(time.RFC3339, expires); err2 == nil {
			return time.Now().After(t2)
		}
		return true
	}
	return time.Now().UTC().After(t)
}

// RevokeShareTokens 删除某个下载记录的全部分享令牌
func RevokeShareTokens(d *sql.DB, downloadID int64) {
	if _, err := d.Exec(`DELETE FROM vod_share_token WHERE download_id=?`, downloadID); err != nil {
		logx.Printf("[db] 撤销分享令牌失败: %v", err)
	}
}

// PurgeExpiredShareTokens 清理过期令牌（启动时跑一次，避免表无限增长）
func PurgeExpiredShareTokens(d *sql.DB) {
	rows, err := d.Query(`SELECT token, expires_at FROM vod_share_token WHERE expires_at<>''`)
	if err != nil {
		return
	}
	var dead []string
	for rows.Next() {
		var tok, exp string
		if err := rows.Scan(&tok, &exp); err == nil && ShareExpired(exp) {
			dead = append(dead, tok)
		}
	}
	rows.Close()
	if len(dead) == 0 {
		return
	}
	for _, tok := range dead {
		if _, err := d.Exec(`DELETE FROM vod_share_token WHERE token=?`, tok); err != nil {
			return
		}
	}
	logx.Printf("[db] 清理过期分享令牌 %d 个", len(dead))
}

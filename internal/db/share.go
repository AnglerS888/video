package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	"videoDownLoad/internal/logx"
)

// NewShareToken 生成分享令牌并写入库。
// 返回的令牌是 URL 安全的随机串（32 hex 字符 = 128 bit），可单独撤销。
//
// 分享链接不再有有效期：expires_at 列保留（旧库结构兼容）但固定写空串，
// 需要让链接失效时直接删除下载记录（令牌随外键级联删除）。
func NewShareToken(d *sql.DB, downloadID int64) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成分享令牌失败: %w", err)
	}
	token := hex.EncodeToString(raw)
	_, err := d.Exec(`INSERT INTO vod_share_token(token, download_id, expires_at) VALUES(?,?,'')`,
		token, downloadID)
	if err != nil {
		return "", err
	}
	return token, nil
}

// ShareTokenLookup 按令牌查对应的下载记录。令牌不存在 → ok=false。
// 不再校验失效时间：旧库里残留的 expires_at 一律忽略，分享链接永久有效。
func ShareTokenLookup(d *sql.DB, token string) (downloadID int64, ok bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, false
	}
	err := d.QueryRow(`SELECT download_id FROM vod_share_token WHERE token=?`, token).Scan(&downloadID)
	if err != nil {
		return 0, false
	}
	return downloadID, true
}

// RevokeShareTokens 删除某个下载记录的全部分享令牌
func RevokeShareTokens(d *sql.DB, downloadID int64) {
	if _, err := d.Exec(`DELETE FROM vod_share_token WHERE download_id=?`, downloadID); err != nil {
		logx.Printf("[db] 撤销分享令牌失败: %v", err)
	}
}

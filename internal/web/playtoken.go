package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"videoDownLoad/internal/logx"
)

// 本地播放令牌：/play/{id} 需要带上它。
//
// 为什么要：vod_download.id 是自增整数，能被直接枚举（/play/1、/play/2…），
// 同一局域网里任何人都能把整个下载库拖走。以前分享链接也是拿 id 当凭据，
// 现在分享改用数据库里的长期令牌（vod_share_token），本地播放用这里的短期令牌。
//
// 令牌是进程内内存态：重启后失效，前端会自动重新取（见前端 api.ts 的 playToken）。
var (
	playTokens   sync.Map // token -> 过期时间戳（Unix 秒）
	playTokenTTL = 6 * time.Hour
)

// issuePlayToken 签发一个本地播放令牌
func issuePlayToken() (string, int64) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// 随机源出问题就退化成时间戳+随机数，够用（不是加密用途）
		return "pt" + time.Now().Format("20060102150405") + hex.EncodeToString(raw[:4]), time.Now().Add(playTokenTTL).Unix()
	}
	tok := "pt" + hex.EncodeToString(raw)
	exp := time.Now().Add(playTokenTTL).Unix()
	playTokens.Store(tok, exp)
	return tok, exp
}

// validPlayToken 校验本地播放令牌
func validPlayToken(tok string) bool {
	if tok == "" {
		return false
	}
	v, ok := playTokens.Load(tok)
	if !ok {
		return false
	}
	exp, _ := v.(int64)
	if time.Now().Unix() > exp {
		playTokens.Delete(tok)
		return false
	}
	return true
}

// adminAuthorized 请求是否带了管理口令（设了 VDL_ADMIN_TOKEN 时有效）
func adminAuthorized(r *http.Request) bool {
	if adminToken == "" {
		return false
	}
	tok := r.Header.Get("X-Admin-Token")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	return tok != "" && tok == adminToken
}

// playToken 签发本地播放令牌（前端用它拼 /play/{id}?t=...）
func (s *Server) playToken(w http.ResponseWriter, r *http.Request) {
	tok, exp := issuePlayToken()
	logx.Debugf("[api] 签发播放令牌，有效期至 %v", time.Unix(exp, 0).Format("2006-01-02 15:04:05"))
	ok(w, map[string]any{"token": tok, "expires_at": exp})
}

package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"videoDownLoad/internal/logx"
)

// ---- 访问密码：站点级鉴权 ----
//
// 在「信息」页 →「运行配置」里设置（存在数据目录的 ui.yml，保存即生效）。
// 密码为空时不启用；设置后：
//   - 浏览器：登录一次，服务端下发 HttpOnly Cookie（30 天），之后页面与接口请求自动带上；
//   - 脚本 / 第三方客户端：带 X-Access-Password 请求头，或 ?password= 查询参数；
//   - 只有 /healthz 与 /api/auth/* 不需要密码（登录页、容器健康检查）。
//
// Cookie 值是 HMAC 签名的过期时间，密钥由密码派生：改密码后旧 Cookie 全部失效，
// 不需要在服务端存会话表。这是「自建服务挡一下」的轻量口令，不是多用户账号体系。

const (
	authCookieName = "vdl_auth"
	authCookieTTL  = 30 * 24 * time.Hour
	// 同一个 IP 连续错 5 次锁 5 分钟：站点偶尔会暴露到公网，挡一下暴力猜解
	authFailLimit = 5
	authLockTime  = 5 * time.Minute
)

// authFailures 按来源 IP 记录登录失败次数（进程内，重启清零）
var authFailures = struct {
	sync.Mutex
	m map[string]*authFailure
}{m: map[string]*authFailure{}}

type authFailure struct {
	fails   int
	until   time.Time
	lastHit time.Time
}

// SetAccessPassword 更新访问密码（启动时注入；信息页保存配置时热更新）。
// 传空串表示关闭密码。
func (s *Server) SetAccessPassword(pwd string) {
	pwd = strings.TrimSpace(pwd)
	s.authMu.Lock()
	old := s.authPwd
	s.authPwd = pwd
	s.authMu.Unlock()
	if old == pwd {
		return
	}
	if pwd == "" {
		logx.Infof("[auth] 访问密码已关闭，站点与接口不再需要密码")
	} else {
		logx.Infof("[auth] 访问密码已启用（浏览器登录 / 接口带 X-Access-Password 或 ?password=）")
	}
}

// accessPassword 当前生效的访问密码（空串 = 不启用）
func (s *Server) accessPassword() string {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.authPwd
}

// authExempt 不需要访问密码的路径：登录相关与健康检查
func authExempt(p string) bool {
	switch p {
	case "/healthz", "/api/auth/status", "/api/auth/login", "/api/auth/logout":
		return true
	}
	return false
}

// authProtected 需要访问密码的路径：数据接口与播放文件流。
// 静态页（SPA 的 index.html / assets）放行：它本身不含数据，
// 前端要靠它启动才能在浏览器里渲染登录界面；真正取数据的都要过这里。
func authProtected(p string) bool {
	return strings.HasPrefix(p, "/api/") || p == "/play" || strings.HasPrefix(p, "/play/")
}

// explicitPassword 请求里直接带的密码（请求头优先，其次查询参数）
func explicitPassword(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Access-Password")); v != "" {
		return v
	}
	return strings.TrimSpace(r.URL.Query().Get("password"))
}

// passwordMatch 常量时间比较，避免按响应时间逐位猜密码
func passwordMatch(got, want string) bool {
	return hmac.Equal([]byte(got), []byte(want))
}

// authKey 由密码派生 Cookie 签名密钥
func authKey(pwd string) []byte {
	sum := sha256.Sum256([]byte("videoDownLoad/auth|" + pwd))
	return sum[:]
}

// authToken 生成 Cookie 值：base64(payload) + "." + base64(HMAC)
func authToken(pwd string, exp int64) string {
	payload := []byte("v1|" + strconv.FormatInt(exp, 10))
	mac := hmac.New(sha256.New, authKey(pwd))
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// validAuthToken 校验 Cookie：签名对得上且没过期
func validAuthToken(pwd, tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 2 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, authKey(pwd))
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	exp, err := strconv.ParseInt(strings.TrimPrefix(string(payload), "v1|"), 10, 64)
	return err == nil && time.Now().Unix() < exp
}

// issueAuthCookie 给当前浏览器下发登录 Cookie（密码为空时什么都不做）
func (s *Server) issueAuthCookie(w http.ResponseWriter, r *http.Request) {
	pwd := s.accessPassword()
	if pwd == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    authToken(pwd, time.Now().Add(authCookieTTL).Unix()),
		Path:     "/",
		MaxAge:   int(authCookieTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

// clearAuthCookie 退出登录 / 关闭密码时清除 Cookie
func (s *Server) clearAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clientIP 取来源 IP（只用于登录限流；不信任 X-Forwarded-For，避免被伪造绕过）
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginLocked 该 IP 是否处于锁定期
func loginLocked(ip string) (time.Duration, bool) {
	authFailures.Lock()
	defer authFailures.Unlock()
	st := authFailures.m[ip]
	if st == nil {
		return 0, false
	}
	if left := time.Until(st.until); left > 0 {
		return left, true
	}
	return 0, false
}

// noteAuthFail 记一次失败；达到上限时锁定并返回 true
func noteAuthFail(ip string) bool {
	authFailures.Lock()
	defer authFailures.Unlock()
	now := time.Now()
	st := authFailures.m[ip]
	if st == nil {
		// 机会式清理：正常使用不会积累很多 IP
		if len(authFailures.m) > 512 {
			for k, v := range authFailures.m {
				if now.Sub(v.lastHit) > time.Hour {
					delete(authFailures.m, k)
				}
			}
		}
		st = &authFailure{}
		authFailures.m[ip] = st
	}
	st.fails++
	st.lastHit = now
	if st.fails >= authFailLimit {
		st.fails = 0
		st.until = now.Add(authLockTime)
		return true
	}
	return false
}

// resetAuthFail 登录成功后清掉该 IP 的失败记录
func resetAuthFail(ip string) {
	authFailures.Lock()
	delete(authFailures.m, ip)
	authFailures.Unlock()
}

// writeUnauthorized 未通过访问密码：API 返回 401 JSON（前端据此切回登录页），
// 文件流等非 API 路径返回纯文本 401。
func writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"ok": false, "code": "auth_required", "msg": "需要访问密码",
		})
		return
	}
	http.Error(w, "需要访问密码", http.StatusUnauthorized)
}

// writeAuthLocked 密码错误次数过多
func writeAuthLocked(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Cache-Control", "no-store")
	sec := int(wait.Seconds()) + 1
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"ok": false, "code": "auth_locked",
		"msg": fmt.Sprintf("密码错误次数过多，请 %d 秒后再试", sec),
	})
}

// requireAccess 访问密码守卫：套在路由外层。
// 只拦 /api/* 与 /play/*：静态页本身不含数据，仍放行以便前端显示登录界面；
// 登录相关接口与 /healthz 也免密（否则登录页自己就被拦了）。
func (s *Server) requireAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pwd := s.accessPassword()
		if pwd == "" || !authProtected(r.URL.Path) || authExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r)
		if left, locked := loginLocked(ip); locked {
			writeAuthLocked(w, left)
			return
		}
		// 1) 请求头 / 查询参数里的密码（脚本、第三方客户端）
		if got := explicitPassword(r); got != "" {
			if passwordMatch(got, pwd) {
				resetAuthFail(ip)
				next.ServeHTTP(w, r)
				return
			}
			if noteAuthFail(ip) {
				logx.Warnf("[auth] 访问密码连续错误，暂时锁定 %s", ip)
				writeAuthLocked(w, authLockTime)
				return
			}
			writeUnauthorized(w, r)
			return
		}
		// 2) 浏览器 Cookie（登录接口下发）
		if c, err := r.Cookie(authCookieName); err == nil && validAuthToken(pwd, c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		writeUnauthorized(w, r)
	})
}

// authStatus 当前是否需要密码、这个浏览器是否已登录（本身不需要密码）
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	pwd := s.accessPassword()
	authed := true
	if pwd != "" {
		authed = false
		if c, err := r.Cookie(authCookieName); err == nil {
			authed = validAuthToken(pwd, c.Value)
		}
	}
	ok(w, map[string]any{"required": pwd != "", "authenticated": authed})
}

// authLogin 登录：校验密码后下发 Cookie。
// 这里不套 writeGuard（它要求管理口令），只做同源校验 + 请求体限额 + 失败限流。
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	pwd := s.accessPassword()
	if pwd == "" {
		ok(w, map[string]any{"required": false, "authenticated": true})
		return
	}
	// 防登录 CSRF：浏览器跨站 POST 直接拒绝（脚本/curl 不带 Origin）
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "msg": "跨站请求已拒绝"})
		return
	}
	ip := clientIP(r)
	if left, locked := loginLocked(ip); locked {
		writeAuthLocked(w, left)
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "请求格式错误"})
		return
	}
	if !passwordMatch(strings.TrimSpace(in.Password), pwd) {
		if noteAuthFail(ip) {
			logx.Warnf("[auth] 登录失败次数过多，暂时锁定 %s", ip)
			writeAuthLocked(w, authLockTime)
			return
		}
		logx.Warnf("[auth] 登录失败 ip=%s", ip)
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"ok": false, "code": "auth_required", "msg": "访问密码不正确",
		})
		return
	}
	resetAuthFail(ip)
	s.issueAuthCookie(w, r)
	logx.Infof("[auth] 登录成功 ip=%s", ip)
	ok(w, map[string]any{"required": true, "authenticated": true})
}

// authLogout 退出登录：清除 Cookie（本身不需要密码，避免过期后无法清理）
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.clearAuthCookie(w)
	ok(w, map[string]any{"msg": "已退出登录"})
}

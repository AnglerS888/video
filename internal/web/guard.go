package web

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// ---- 写接口的防护：管理口令 + CSRF + 请求体限额 ----
//
// 项目定位是「内网自用的媒体服务」，默认不强制登录（免得手机上每次都要输密码），
// 但下面两件事必须做：
//
//  1) CSRF 防护：浏览器可以用 `text/plain` 发"简单请求"（不触发预检），
//     所以只要写接口不校验 Content-Type/Origin，用户在内网机器上打开任意网页，
//     那个网页就能替他触发删接口、改配置、发起全量采集、导入源（导入的源默认
//     enabled + auto_incr → 服务端会自动去请求任意地址 = SSRF）。
//  2) 可选的管理口令：设了 VDL_ADMIN_TOKEN 后，所有写接口都要带
//     `X-Admin-Token` 头或 `?token=` 参数。适合把服务暴露到公网/半公网的场景。
//
// 读接口一律不鉴权（播放器、封面、列表都要用）。

// adminToken 管理口令（环境变量 VDL_ADMIN_TOKEN）。为空表示不启用鉴权。
var adminToken = strings.TrimSpace(os.Getenv("VDL_ADMIN_TOKEN"))

// AdminTokenEnabled 是否启用了管理口令
func AdminTokenEnabled() bool { return adminToken != "" }

// maxBodyBytes 普通接口的请求体上限
const maxBodyBytes = 1 << 20 // 1MB

// maxArrayItems 数组型字段的条数上限（批量接口防打爆）
const maxArrayItems = 500

// writeGuard 写接口守卫：校验管理口令 + CSRF + 限制请求体。
// 只挂在写方法上（POST/PUT/PATCH/DELETE）。
func writeGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1) 管理口令
		if adminToken != "" {
			tok := r.Header.Get("X-Admin-Token")
			if tok == "" {
				tok = r.URL.Query().Get("token")
			}
			if tok == "" || tok != adminToken {
				fail(w, "需要管理口令（请在请求头加 X-Admin-Token）")
				return
			}
		}
		// 2) CSRF：跨站来源直接拒。
		//    同源请求带 Origin（浏览器发起的 fetch/XHR 一定带），
		//    非浏览器客户端（curl/脚本）不带 Origin —— 放行（那是真正的使用者）。
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r) {
			fail(w, "跨站请求已拒绝")
			return
		}
		// 3) 带请求体的写操作必须是 JSON（text/plain 的简单请求会被上面拦掉，
		//    这里再加一道，避免表单提交也能打进来）
		if r.Method != http.MethodDelete && r.ContentLength > 0 {
			ct := r.Header.Get("Content-Type")
			if ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
				fail(w, "请求必须是 application/json")
				return
			}
		}
		// 4) 请求体限额
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin 判断 Origin 是否与请求同源
func sameOrigin(origin string, r *http.Request) bool {
	origin = strings.TrimSuffix(origin, "/")
	host := r.Host
	if host == "" {
		return false
	}
	// Origin 形如 http://host:port
	idx := strings.Index(origin, "://")
	if idx < 0 {
		return false
	}
	rest := origin[idx+3:]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return strings.EqualFold(rest, host)
}

// decodeJSON 解析 JSON 请求体（带大小限制）。
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

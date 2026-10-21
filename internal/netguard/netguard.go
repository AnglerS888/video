// Package netguard 防 SSRF：限制服务端代为发起的 HTTP 请求只能打到公网地址。
//
// 为什么要它：/api/live?u= 、/api/img?u= 、/api/sources/probe 这类接口
// 会让**服务端**去请求用户给的地址。没有防护时，同一局域网里的任何人都能
// 借这个服务去访问内网（路由器管理页、云厂商元数据接口 169.254.169.254 等），
// 并把响应内容读回来。
//
// 三层防护：
//  1. 协议只允许 http/https（挡住 file://、gopher://）
//  2. 主机名/IP 字面量快速判定：回环、私网、链路本地、保留地址直接拒
//  3. 连接层校验：自己解析 DNS，逐个检查解析出来的 IP，再直接连那个 IP。
//     这样「域名指向内网」和 DNS rebinding（第一次解析到公网、第二次解析到内网）
//     都被挡住，而且不依赖请求路径上的 DNS 预解析 —— NAS 没配 DNS 也不影响。
//
// 例外：确实要用局域网源站（自建 CMS 跑在家里）时，设环境变量
//
//	VDL_ALLOW_PRIVATE=1
//
// 即可放行私网地址（默认禁止）。
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// AllowPrivate 是否允许访问私网地址（环境变量 VDL_ALLOW_PRIVATE=1）
var AllowPrivate = os.Getenv("VDL_ALLOW_PRIVATE") == "1"

// blockedNames 明确指向本机/内网的主机名
var blockedNames = []string{
	"localhost", "localhost.localdomain", "ip6-localhost", "ip6-loopback",
	"metadata", "metadata.google.internal", "instance-data",
}

// blockedSuffixes 内网常见的域名后缀
var blockedSuffixes = []string{".local", ".internal", ".localhost", ".lan", ".home", ".localdomain"}

// IsPrivateIP 判断 IP 是否属于回环/私网/链路本地/保留地址
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast()
}

// HostAllowedFast 主机名的快速判定（不做 DNS 解析）。
// 只挡「明确是内网」的：IP 字面量与内网主机名。
func HostAllowedFast(host string) bool {
	if AllowPrivate {
		return true
	}
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "" {
		return false
	}
	for _, n := range blockedNames {
		if h == n {
			return false
		}
	}
	for _, suf := range blockedSuffixes {
		if strings.HasSuffix(h, suf) {
			return false
		}
	}
	if ip := net.ParseIP(h); ip != nil {
		return !IsPrivateIP(ip)
	}
	return true
}

// CheckURL 校验一个待请求的 URL 是否允许被服务端访问。
// 返回解析后的 URL；不允许时返回错误。
func CheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("地址格式不正确")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("只支持 http/https 地址")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("地址缺少主机名")
	}
	if !HostAllowedFast(u.Hostname()) {
		return nil, fmt.Errorf("不允许访问内网地址（%s）", u.Hostname())
	}
	return u, nil
}

// DialContext 返回一个「连接时校验」的 dial 函数：
// 自己解析域名 → 逐个检查解析出的 IP → 直接连检查通过的 IP。
func DialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if AllowPrivate {
			return dialer.DialContext(ctx, network, addr)
		}
		// IP 字面量：直接判
		if ip := net.ParseIP(host); ip != nil {
			if IsPrivateIP(ip) {
				return nil, fmt.Errorf("不允许访问内网地址（%s）", host)
			}
			return dialer.DialContext(ctx, network, addr)
		}
		// 域名：解析后逐个检查
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if IsPrivateIP(ip.IP) {
				return nil, fmt.Errorf("不允许访问内网地址（%s 解析到 %s）", host, ip.IP)
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("域名 %s 没有可用地址", host)
		}
		// 直接连已校验的 IP：避免「校验时解析一次、连接时再解析一次」的 rebinding
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

// NewClient 造一个带 SSRF 防护的 HTTP 客户端。
// respHeaderTimeout <= 0 时不设（流式转发用）。
func NewClient(timeout, respHeaderTimeout time.Duration) *http.Client {
	tr := &http.Transport{
		DialContext:           DialContext(&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}),
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if respHeaderTimeout > 0 {
		tr.ResponseHeaderTimeout = respHeaderTimeout
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		// 重定向每一跳都要复检：只校验初始 URL 的话，
		// 攻击者用一个正常公网地址 302 到内网就能绕过。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("重定向次数过多")
			}
			if !HostAllowedFast(req.URL.Hostname()) {
				return fmt.Errorf("重定向到不允许的地址（%s）", req.URL.Hostname())
			}
			return nil
		},
	}
}

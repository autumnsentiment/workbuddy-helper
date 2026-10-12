package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"workbuddy-helper/internal/model"
)

const (
	CNChatBase    = "https://copilot.tencent.com"
	CNBillingBase = "https://www.codebuddy.cn"
	GlobalBase    = "https://www.workbuddy.ai"
	// 国际版 billing 与国内版一样挂在 codebuddy 域（逆向自国际版客户端，实测确认）
	GlobalBillingBase = "https://www.codebuddy.ai"
	ClientUA          = "CLI/2.63.2 CodeBuddy/2.63.2"

	// 国际版桌面客户端特征（逆向自 WorkBuddy AI 5.5.2 主进程 CLIENT_INFO_* 注入与
	// CLI UserAgentHttpInterceptor 拼装算法，实测 plans-usage 按此识别客户端）
	IntlClientUA   = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/2.137.1"
	IntlClientName = "WorkBuddy"
	IntlClientVer  = "5.5.2"
)

type Kind string

const (
	KindAuthDead  Kind = "auth_dead"
	KindCredit    Kind = "credits_exhausted"
	KindRate      Kind = "rate_limited"
	KindNotFound  Kind = "not_found"
	KindServer    Kind = "server_error"
	KindBusiness  Kind = "business_error"
	KindProtocol  Kind = "protocol_error"
	KindTransport Kind = "transport_error"
)

type Error struct {
	Kind   Kind
	Status int
	Code   int
	Msg    string
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s (HTTP %d): %s", e.Kind, e.Status, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Msg)
}

func IsKind(err error, kind Kind) bool {
	var ce *Error
	return errors.As(err, &ce) && ce.Kind == kind
}

type Client struct {
	HTTP          *http.Client
	ChatBaseCN    string
	BillingBaseCN string
	GlobalBase    string
	GlobalBilling string
	// ownedTransport is set only for transports created by this package.
	// Service.New must replace that transport for the domestic fixed-DNS
	// client, but must preserve a caller-injected *http.Transport (for tests,
	// custom TLS, proxies, or tracing). A type assertion alone cannot make
	// that distinction.
	ownedTransport *http.Transport
}

// NewWithProxy 按代理 URL 构造客户端；proxyURL 为空等价于 New()（直连）。
// 支持协议：http:// https:// socks5:// socks5h://，可含认证 user:pass@host:port。
// 传入不支持的协议返回错误（由上层校验拦截）。
func NewWithProxy(proxyURL string) (*Client, error) {
	transport, err := proxyTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	return clientFromTransport(transport), nil
}

func New() *Client {
	return clientFromTransport(&http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		DialContext:         dnsFallbackResolver.DialContext,
	})
}

type fallbackProxyDialer struct{}

func (fallbackProxyDialer) Dial(network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return dnsFallbackResolver.DialContext(ctx, network, address)
}

func (fallbackProxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return dnsFallbackResolver.DialContext(ctx, network, address)
}

func proxyTransport(proxyURL string) (*http.Transport, error) {
	proxyURL = NormalizeProxyURL(proxyURL)
	if proxyURL == "" {
		return &http.Transport{
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			DialContext:         dnsFallbackResolver.DialContext,
		}, nil
	}
	if err := ValidateProxyURL(proxyURL); err != nil {
		return nil, err
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("代理地址格式不正确")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	transport := &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		Proxy:               http.ProxyURL(u),
		// 代理地址本身（如 socks5://192.168.5.1:1070 是 IP，域名网关也支持）
		// 与经代理后无需本地解析；但 http 代理模式下目标域名由代理解析。
		// 这里仍挂兜底拨号以覆盖代理主机为域名的情况。
		DialContext: dnsFallbackResolver.DialContext,
	}
	// socks5/socks5h 的代理握手由 x/net/proxy DialContext 完成（http.ProxyURL 只认 http/https）
	switch u.Scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pass, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pass}
		}
		proxyAddr := u.Host
		if u.Port() == "" {
			proxyAddr = net.JoinHostPort(u.Hostname(), "1080")
		}
		dialer, derr := proxy.SOCKS5("tcp", proxyAddr, auth, fallbackProxyDialer{})
		if derr != nil {
			return nil, fmt.Errorf("socks5 代理初始化失败: %w", derr)
		}
		transport.Proxy = nil
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			transport.DialContext = cd.DialContext
		}
	}
	return transport, nil
}

// fixedResolver 固定 DNS 解析器：跳过系统 resolv.conf，直接向指定 DNS 发查询。
// 用于国内版链路（DNS 可写 data/dns.conf 固定公共 DNS，容器 DNS 故障不影响）。
func fixedResolver(servers []string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 4 * time.Second}
			var lastErr error
			for _, ns := range servers {
				ns = normalizeDNSServer(ns)
				if ns == "" {
					continue
				}
				c, err := d.DialContext(ctx, network, ns)
				if err == nil {
					return c, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("未配置可用 DNS 服务器")
			}
			return nil, lastErr
		},
	}
}

// normalizeDNSServer makes the common shorthand forms usable by net.Dial.
// In particular, a bare IPv6 literal cannot carry a port without brackets;
// treat it as a DNS server on the conventional port 53.
func normalizeDNSServer(server string) string {
	server = strings.TrimSpace(server)
	if server == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	if strings.HasPrefix(server, "[") && strings.HasSuffix(server, "]") {
		return net.JoinHostPort(server[1:len(server)-1], "53")
	}
	if net.ParseIP(server) != nil || strings.Count(server, ":") > 1 {
		return net.JoinHostPort(server, "53")
	}
	if !strings.Contains(server, ":") {
		return net.JoinHostPort(server, "53")
	}
	return server
}

// defaultDNS 默认固定公共 DNS（可被 data/dns.conf 覆盖）
var defaultDNS = []string{"223.5.5.5:53", "119.29.29.29:53", "1.1.1.1:53"}

// SetDNSConfig 运行时更新固定 DNS（来自 data/dns.conf）。
// 影响之后新建的 transport；已存在的 transport 由调用方重建。
var dnsMu sync.RWMutex
var activeDNS = append([]string(nil), defaultDNS...)

func SetDNSConfig(servers []string) {
	dnsMu.Lock()
	defer dnsMu.Unlock()
	if len(servers) > 0 {
		activeDNS = append([]string(nil), servers...)
	} else {
		activeDNS = append([]string(nil), defaultDNS...)
	}
}

// ActiveDNS 返回当前固定 DNS 列表
func ActiveDNS() []string {
	dnsMu.RLock()
	defer dnsMu.RUnlock()
	return append([]string(nil), activeDNS...)
}

func lookupWithDNSServers(ctx context.Context, host string, servers []string) ([]net.IP, error) {
	var lastErr error
	for _, server := range servers {
		attemptCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		ips, err := fixedResolver([]string{server}).LookupIPAddr(attemptCtx, host)
		cancel()
		if err == nil && len(ips) > 0 {
			result := make([]net.IP, 0, len(ips))
			for _, ip := range ips {
				result = append(result, ip.IP)
			}
			return result, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("未配置可用 DNS 服务器")
	}
	return nil, lastErr
}

// dnsFallbackResolver 兜底解析器：优先系统路径，解析失败时用固定公共 DNS 重查。
// 注意不能只在 Dial 层切换服务器——系统服务器可达但查询超时时 dial 是成功的，
// 因此这里在解析层做两级尝试：先用系统 resolver，失败再用固定 DNS 解析出 IP。
var dnsFallbackResolver = &fallbackResolver{}

type fallbackResolver struct{}

func (r *fallbackResolver) resolve(ctx context.Context, host string) ([]net.IP, error) {
	// 第一级：系统解析（容器内为 Docker DNS）
	systemCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	ips, systemErr := net.DefaultResolver.LookupIPAddr(systemCtx, host)
	cancel()
	if systemErr == nil && len(ips) > 0 {
		addrs := make([]net.IP, 0, len(ips))
		for _, ip := range ips {
			addrs = append(addrs, ip.IP)
		}
		return addrs, nil
	}
	if systemErr == nil {
		systemErr = fmt.Errorf("系统 DNS 未返回地址")
	}
	// 第二级：固定公共 DNS
	dnsMu.RLock()
	servers := append([]string(nil), activeDNS...)
	dnsMu.RUnlock()
	if len(servers) == 0 {
		return nil, systemErr
	}
	addrs, err := lookupWithDNSServers(ctx, host, servers)
	if err != nil {
		return nil, fmt.Errorf("系统 DNS 失败 (%v)，固定 DNS 也失败: %w", systemErr, err)
	}
	return addrs, nil
}

// DialContext 拨号：解析（带兜底）后直连 IP
func (r *fallbackResolver) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	// IP 直接过
	if ip := net.ParseIP(host); ip != nil {
		d := net.Dialer{Timeout: 15 * time.Second}
		return d.DialContext(ctx, network, address)
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	ips, err := r.resolve(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		conn, dialErr := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	// A system resolver can return an unreachable IPv6 address while omitting
	// a usable A record. Retry with the configured fixed DNS servers after all
	// system-resolved addresses fail; this is the common container symptom
	// behind an otherwise misleading "无法连接 WorkBuddy" error.
	dnsMu.RLock()
	servers := append([]string(nil), activeDNS...)
	dnsMu.RUnlock()
	if len(servers) > 0 {
		fallbackIPs, dnsErr := lookupWithDNSServers(ctx, host, servers)
		if dnsErr == nil {
			for _, ip := range fallbackIPs {
				conn, dialErr := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dialErr == nil {
					return conn, nil
				}
				lastErr = dialErr
			}
		} else if lastErr == nil {
			lastErr = dnsErr
		}
	}
	return nil, lastErr
}

// NewTransportOnly 只构造传输层（供热更新：替换现有 client 的 Transport，保留其端点定制）。
func NewTransportOnly(proxyURL string) (*http.Transport, error) {
	return proxyTransport(proxyURL)
}

// CloneWithTransport 返回一个复用端点配置、但拥有独立 http.Client/Transport 的客户端。
// 调用方可以原子替换客户端指针，而不会在请求进行期间修改 http.Client.Transport。
func CloneWithTransport(src *Client, transport http.RoundTripper) *Client {
	if src == nil {
		src = New()
	}
	clone := *src
	// CloneWithTransport accepts caller-provided RoundTrippers, so the
	// replacement must not be considered package-owned.
	clone.ownedTransport = nil
	if src.HTTP == nil {
		clone.HTTP = &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		return &clone
	}
	httpClone := *src.HTTP
	httpClone.Transport = transport
	clone.HTTP = &httpClone
	return &clone
}

// CloseIdleConnections 释放客户端当前传输层的空闲连接。
func (c *Client) CloseIdleConnections() {
	if c == nil || c.HTTP == nil {
		return
	}
	c.HTTP.CloseIdleConnections()
}

// NewWithCNTransport 国内版专用客户端：固定 DNS 直连（无代理）。
// callerTransport 定制过（测试注入 upstream）时返回 nil，调用方保留原客户端。
func NewWithCNTransport(caller *Client) *Client {
	if caller != nil && caller.HTTP != nil && caller.HTTP.Transport != nil {
		transport, isHTTPTransport := caller.HTTP.Transport.(*http.Transport)
		if !isHTTPTransport || caller.ownedTransport == nil || transport != caller.ownedTransport {
			return nil // 外部定制了 transport，保留其 TLS/代理/拨号配置
		}
	}
	c := New()
	c.HTTP.Transport = NewCNTransport()
	// The returned client now owns the fixed-DNS transport. Keep the marker in
	// sync so a later service setup can distinguish it from a caller-injected
	// transport rather than observing the discarded generic transport.
	c.ownedTransport, _ = c.HTTP.Transport.(*http.Transport)
	// 保留调用方的端点定制（测试注入的 ChatBaseCN 等）
	if caller != nil {
		if strings.TrimSpace(caller.ChatBaseCN) != "" {
			c.ChatBaseCN = caller.ChatBaseCN
		}
		if strings.TrimSpace(caller.BillingBaseCN) != "" {
			c.BillingBaseCN = caller.BillingBaseCN
		}
		if strings.TrimSpace(caller.GlobalBase) != "" {
			c.GlobalBase = caller.GlobalBase
		}
		if strings.TrimSpace(caller.GlobalBilling) != "" {
			c.GlobalBilling = caller.GlobalBilling
		}
	}
	return c
}

// NewCNTransport 国内版专用传输层：纯固定 DNS 直连（绝不使用系统 DNS 与代理）。
// DNS 服务器来自 data/dns.conf（默认公共 DNS），域名解析全部走固定服务器，
// 容器内 Docker DNS（127.0.0.11）瘫痪也不影响国内版链路。
func NewCNTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		DialContext:         fixedDNSDialer{}.DialContext,
		Proxy:               nil, // 国内版永远直连
	}
}

type fixedDNSDialer struct{}

func (fixedDNSDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	if net.ParseIP(host) != nil {
		return dialer.DialContext(ctx, network, address)
	}
	servers := ActiveDNS()
	if len(servers) == 0 {
		return dialer.DialContext(ctx, network, address)
	}
	ips, err := lookupWithDNSServers(ctx, host, servers)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, lastErr
}

// SetDNSSystem restores the system resolver for the fixed-DNS transport.
// It is used when data/dns.conf explicitly contains "system".
func SetDNSSystem() {
	dnsMu.Lock()
	defer dnsMu.Unlock()
	activeDNS = []string{}
}

// NormalizeProxyURL converts the persistent/UI sentinel "direct" to the
// canonical empty string used by the client and state model. The comparison
// is case-insensitive so hand-edited proxy.conf files remain usable.
func NormalizeProxyURL(proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if strings.EqualFold(proxyURL, "direct") {
		return ""
	}
	return proxyURL
}

// ValidateProxyURL 校验代理地址：空串或 direct 合法（直连）；协议必须在
// http/https/socks5/socks5h 白名单。
func ValidateProxyURL(proxyURL string) error {
	proxyURL = NormalizeProxyURL(proxyURL)
	if proxyURL == "" {
		return nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("代理地址格式不正确")
	}
	if strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("代理端口不能为空")
	}
	scheme := strings.ToLower(u.Scheme)
	if !model.ValidProxyScheme(scheme) {
		return fmt.Errorf("代理协议仅支持 http/https/socks5/socks5h，收到 %q", scheme)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("代理地址只能包含协议、认证信息、主机和端口")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("代理端口必须在 1-65535 之间")
		}
	}
	return nil
}

func clientFromTransport(transport *http.Transport) *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		ChatBaseCN:     CNChatBase,
		BillingBaseCN:  CNBillingBase,
		GlobalBase:     GlobalBase,
		GlobalBilling:  GlobalBillingBase,
		ownedTransport: transport,
	}
}

func (c *Client) base(a *model.Account, billing bool) string {
	if isGlobalAccount(a) {
		if billing {
			return c.GlobalBilling
		}
		return c.GlobalBase
	}
	if billing {
		return c.BillingBaseCN
	}
	return c.ChatBaseCN
}

func isGlobalAccount(a *model.Account) bool {
	return a != nil && a.VersionMode() == model.ModeIntl
}

func commonHeaders(req *http.Request, origin string) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", ClientUA)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
}

func authHeaders(req *http.Request, a *model.Account, billing bool) {
	origin := "https://www.codebuddy.cn"
	if isGlobalAccount(a) {
		origin = "https://www.workbuddy.ai"
	}
	if billing {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
	} else {
		commonHeaders(req, origin)
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

var errResponseTooLarge = errors.New("response body too large")

func readResponseBody(body io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errResponseTooLarge
	}
	return raw, nil
}

// drainResponseBody consumes a streaming response without retaining it in
// memory. A cap is still important: an upstream that never closes an SSE
// stream must not keep an account operation busy forever (the HTTP client
// timeout is only a coarse upper bound).
func drainResponseBody(body io.Reader, limit int64) error {
	n, err := io.Copy(io.Discard, io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return errResponseTooLarge
	}
	return nil
}

func (c *Client) doEnvelope(req *http.Request) (json.RawMessage, int, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, &Error{Kind: KindTransport, Msg: err.Error()}
	}
	defer resp.Body.Close()
	raw, readErr := readResponseBody(resp.Body, 2<<20)
	if readErr != nil {
		if errors.Is(readErr, errResponseTooLarge) {
			return nil, resp.StatusCode, &Error{Kind: KindProtocol, Status: resp.StatusCode, Msg: "响应内容过大"}
		}
		return nil, resp.StatusCode, &Error{Kind: KindTransport, Status: resp.StatusCode, Msg: "读取响应失败: " + readErr.Error()}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var env envelope
		if json.Unmarshal(raw, &env) == nil && (env.Code != 0 || env.Msg != "") {
			return nil, resp.StatusCode, classifyBusiness(resp.StatusCode, env.Code, env.Msg)
		}
		return nil, resp.StatusCode, classifyHTTP(resp.StatusCode, string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, &Error{Kind: KindProtocol, Status: resp.StatusCode, Msg: "响应不是有效 JSON"}
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, classifyBusiness(resp.StatusCode, env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// isCreditExhausted 判断响应是否表示积分耗尽（余额不足而非限流）
func isCreditExhausted(code int, msg string) bool {
	if code == 14018 {
		return true
	}
	lower := strings.ToLower(msg)
	for _, marker := range []string{"credits exhausted", "insufficient credit", "no credit", "quota exceeded", "积分不足", "余额不足", "额度不足"} {
		if strings.Contains(lower, strings.ToLower(marker)) || strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func classifyHTTP(status int, body string) error {
	msg := short(body, 180)
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusUnauthorized || strings.Contains(body, "12153") || strings.Contains(lower, "offline user session") || strings.Contains(lower, "user session not found"):
		return &Error{Kind: KindAuthDead, Status: status, Msg: msg}
	case isCreditExhausted(0, body):
		return &Error{Kind: KindCredit, Status: status, Msg: msg}
	case status == http.StatusTooManyRequests:
		return &Error{Kind: KindRate, Status: status, Msg: msg}
	case status == http.StatusNotFound:
		return &Error{Kind: KindNotFound, Status: status, Msg: msg}
	case status >= 500:
		return &Error{Kind: KindServer, Status: status, Msg: msg}
	default:
		return &Error{Kind: KindBusiness, Status: status, Msg: msg}
	}
}

func classifyBusiness(status, code int, msg string) error {
	lower := strings.ToLower(msg)
	if status == http.StatusUnauthorized || code == 12153 || strings.Contains(lower, "offline user session") || strings.Contains(lower, "user session not found") {
		return &Error{Kind: KindAuthDead, Status: status, Code: code, Msg: msg}
	}
	switch {
	case isCreditExhausted(code, msg):
		return &Error{Kind: KindCredit, Status: status, Code: code, Msg: msg}
	case status == http.StatusTooManyRequests || strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many request") || strings.Contains(msg, "请求频繁") || strings.Contains(msg, "请求过于频繁") || strings.Contains(msg, "限流"):
		return &Error{Kind: KindRate, Status: status, Code: code, Msg: msg}
	case status == http.StatusNotFound:
		return &Error{Kind: KindNotFound, Status: status, Code: code, Msg: msg}
	case status >= 500:
		return &Error{Kind: KindServer, Status: status, Code: code, Msg: msg}
	}
	return &Error{Kind: KindBusiness, Status: status, Code: code, Msg: msg}
}

func short(s string, n int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n]) + "..."
	}
	return s
}

type LoginSession struct {
	State   string
	AuthURL string
	Client  *http.Client
	baseURL string
	// intl=true 时走国际版授权（workbuddy.ai，platform=workbuddy-ai）
	intl bool
}

// NewLoginSession version: ModeCN（国内版 CLI 授权）/ ModeIntl（国际版授权）
func NewLoginSession(version string) *LoginSession {
	return NewLoginSessionWithClient(version, nil)
}

// NewLoginSessionWithClient 创建拥有独立 Cookie Jar 的登录会话，同时复用给定
// API 客户端的传输层：国内版沿用固定 DNS 直连，国际版沿用当前代理配置。
func NewLoginSessionWithClient(version string, base *Client) *LoginSession {
	jar, _ := cookiejar.New(nil)
	intl := version == model.ModeIntl
	baseURL := CNChatBase
	loginClient := &http.Client{Timeout: 30 * time.Second}
	if intl {
		baseURL = GlobalBase
	}
	if base != nil {
		if base.HTTP != nil {
			if base.HTTP.Timeout > 0 {
				loginClient.Timeout = base.HTTP.Timeout
			}
			loginClient.Transport = base.HTTP.Transport
		}
		if intl && base.GlobalBase != "" {
			baseURL = base.GlobalBase
		} else if !intl && base.ChatBaseCN != "" {
			baseURL = base.ChatBaseCN
		}
	}
	loginClient.Jar = jar
	loginClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &LoginSession{Client: loginClient, baseURL: strings.TrimRight(baseURL, "/"), intl: intl}
}

func (s *LoginSession) chatBase() string {
	if s.baseURL != "" {
		return s.baseURL
	}
	if s.intl {
		return GlobalBase
	}
	return CNChatBase
}

// origin 登录接口的 Origin/Referer（对齐各版本客户端）
func (s *LoginSession) origin() string {
	if s.intl {
		return "https://www.workbuddy.ai"
	}
	return "https://www.codebuddy.cn"
}

// platform 授权页 platform 参数：国内 CLI / 国际 workbuddy-ai（逆向实测确认）
func (s *LoginSession) platform() string {
	if s.intl {
		return "workbuddy-ai"
	}
	return "CLI"
}

func (s *LoginSession) Start() error {
	req, err := http.NewRequest(http.MethodPost, s.chatBase()+"/v2/plugin/auth/state?platform="+url.QueryEscape(s.platform()), bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	commonHeaders(req, s.origin())
	req.Header.Set("X-Domain", strings.TrimPrefix(strings.TrimPrefix(s.chatBase(), "https://"), "http://"))
	data, _, err := (&Client{HTTP: s.Client}).doEnvelope(req)
	if err != nil {
		return err
	}
	var result struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.State == "" || result.AuthURL == "" {
		return &Error{Kind: KindProtocol, Msg: "登录服务未返回授权地址"}
	}
	if err := validateAuthURL(result.AuthURL, s.intl, s.chatBase()); err != nil {
		return &Error{Kind: KindProtocol, Msg: "登录服务返回了不受信任的授权地址"}
	}
	s.State, s.AuthURL = result.State, result.AuthURL
	return nil
}

// validateAuthURL prevents an upstream response or compromised proxy from
// turning the local login button into an arbitrary javascript/data/file link.
// The real services use HTTPS on the corresponding WorkBuddy/CodeBuddy host;
// tests and self-hosted gateways may use HTTP only when they keep the same
// product host name.
func validateAuthURL(raw string, intl bool, expectedBase string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Host == "" || u.Path == "" {
		return fmt.Errorf("invalid authorization URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return fmt.Errorf("unsupported authorization URL scheme")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return fmt.Errorf("missing authorization URL host")
	}
	expectedHost := ""
	expectedScheme := ""
	expectedPort := ""
	if base, baseErr := url.Parse(strings.TrimSpace(expectedBase)); baseErr == nil {
		expectedHost = strings.ToLower(strings.TrimSuffix(base.Hostname(), "."))
		expectedScheme = strings.ToLower(base.Scheme)
		expectedPort = base.Port()
	}
	if expectedHost != "" && host == expectedHost {
		if expectedScheme != "" && scheme != expectedScheme {
			return fmt.Errorf("authorization URL scheme does not match login endpoint")
		}
		if expectedPort != "" && u.Port() != expectedPort {
			return fmt.Errorf("authorization URL port does not match login endpoint")
		}
		return nil
	}
	// Production product hosts must never downgrade to HTTP. HTTP remains
	// available only for an explicitly configured HTTP test/self-hosted base
	// above.
	if scheme != "https" {
		return fmt.Errorf("authorization URL must use HTTPS")
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("authorization URL uses an unexpected port")
	}
	if intl {
		if host != "workbuddy.ai" && !strings.HasSuffix(host, ".workbuddy.ai") {
			return fmt.Errorf("authorization URL host is not WorkBuddy")
		}
	} else if host != "codebuddy.cn" && !strings.HasSuffix(host, ".codebuddy.cn") && host != "copilot.tencent.com" && !strings.HasSuffix(host, ".copilot.tencent.com") {
		return fmt.Errorf("authorization URL host is not CodeBuddy")
	}
	return nil
}

type LoginResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Domain       string `json:"domain"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"`
	Nickname     string `json:"nickname"`
}

func (s *LoginSession) Poll() (LoginResult, error) {
	if s.State == "" {
		return LoginResult{}, fmt.Errorf("登录会话不存在")
	}
	base := &Client{HTTP: s.Client}
	tokenURL := s.chatBase() + "/v2/plugin/auth/token?state=" + url.QueryEscape(s.State)
	req, err := http.NewRequest(http.MethodGet, tokenURL, nil)
	if err != nil {
		return LoginResult{}, err
	}
	commonHeaders(req, s.origin())
	data, _, err := base.doEnvelope(req)
	if err != nil {
		// The token endpoint uses a small business-error response while the
		// browser has not finished authorization. Do not treat every 4xx as
		// pending: 401/403/404/429 are actionable failures and must reach the UI.
		if isAuthorizationPendingError(err) {
			var pending *Error
			if errors.As(err, &pending) {
				return LoginResult{}, &Error{Kind: KindBusiness, Status: pending.Status, Code: pending.Code, Msg: "登录尚未完成，请先在浏览器完成授权"}
			}
		}
		return LoginResult{}, err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return LoginResult{}, &Error{Kind: KindProtocol, Msg: "登录服务未返回有效令牌"}
	}
	result := LoginResult{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, ExpiresIn: tok.ExpiresIn, Domain: tok.Domain}
	// 国际版 token 响应未回传 domain 时，按授权域补齐（避免误判为国内账号）
	if result.Domain == "" && s.intl {
		result.Domain = "www.workbuddy.ai"
	}
	acctURL := s.chatBase() + "/v2/plugin/login/account?state=" + url.QueryEscape(s.State)
	acctReq, err := http.NewRequest(http.MethodGet, acctURL, nil)
	if err != nil {
		return result, err
	}
	commonHeaders(acctReq, s.origin())
	acctReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	acctData, _, acctErr := base.doEnvelope(acctReq)
	if acctErr != nil {
		return LoginResult{}, acctErr
	}
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if err := json.Unmarshal(acctData, &acct); err != nil {
		return LoginResult{}, &Error{Kind: KindProtocol, Msg: "账号资料响应格式发生变化"}
	}
	result.UID, result.EnterpriseID, result.Nickname = acct.UID, acct.EnterpriseID, acct.Nickname
	if result.UID == "" {
		return LoginResult{}, &Error{Kind: KindProtocol, Msg: "登录成功但未取得账号 UID"}
	}
	return result, nil
}

// isAuthorizationPendingError recognizes only the token endpoint's explicit
// pending responses. A generic 4xx must never keep the browser polling loop
// alive indefinitely because it may indicate an expired or invalid session.
func isAuthorizationPendingError(err error) bool {
	var ce *Error
	if !errors.As(err, &ce) || ce.Kind != KindBusiness {
		return false
	}
	// The current token endpoint reports an unfinished browser authorization as
	// HTTP 200 with business code 11217 ("login ing...").
	if ce.Code == 11217 {
		return true
	}
	if ce.Status != http.StatusBadRequest && ce.Status != http.StatusConflict {
		return false
	}
	// 1001 is the token endpoint's authorization-in-progress response used by
	// both domestic and international login services. Keep message matching
	// as a compatibility fallback for deployments that omit the numeric code.
	if ce.Code == 1001 {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(ce.Msg))
	for _, marker := range []string{
		"authorization pending", "login pending", "waiting for authorization",
		"waiting for login", "not yet authorized", "尚未完成", "等待授权",
		"等待登录", "授权中", "登录中", "请先在浏览器",
	} {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

func (c *Client) Refresh(a *model.Account) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return &Error{Kind: KindAuthDead, Msg: "缺少 refresh token，请重新添加账号"}
	}
	req, err := http.NewRequest(http.MethodPost, c.base(a, false)+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return err
	}
	// 国际版客户端刷新头对齐：X-Auth-Refresh-Source 为 plugin（逆向自国际版客户端 refreshSession）
	source := "workbuddy"
	if isGlobalAccount(a) {
		source = "plugin"
	}
	commonHeaders(req, originFor(a))
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", source)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	data, _, err := c.doEnvelope(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return &Error{Kind: KindProtocol, Msg: "刷新登录态失败，请重新添加账号"}
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	now := time.Now()
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = model.ResolveTokenExpiresAt(tok.AccessToken, tok.ExpiresIn, now)
	} else if expiresAt, ok := model.JWTExpiryUnix(tok.AccessToken); ok {
		a.ExpiresAt = expiresAt
	} else if a.ExpiresAt <= now.Unix() {
		a.ExpiresAt = now.Add(15 * time.Minute).Unix()
	}
	return nil
}

func originFor(a *model.Account) string {
	if isGlobalAccount(a) {
		return "https://www.workbuddy.ai"
	}
	return "https://www.codebuddy.cn"
}

func (c *Client) DailyCheckin(a *model.Account) error {
	req, err := http.NewRequest(http.MethodPost, c.base(a, true)+"/v2/billing/meter/daily-checkin", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	authHeaders(req, a, true)
	_, _, err = c.doEnvelope(req)
	return err
}

// machineID 返回稳定的本机标识（对齐客户端 machineId：机器级 UUID，进程内缓存）
var machineIDOnce sync.Once
var machineIDValue string

func machineID() string {
	machineIDOnce.Do(func() {
		// 优先读取宿主已注入的 CLIENT_INFO_MACHINE_ID（与真实客户端同源）
		if v := strings.TrimSpace(os.Getenv("CLIENT_INFO_MACHINE_ID")); v != "" {
			machineIDValue = v
			return
		}
		// 回退：按主机名+用户名派生稳定哈希（保证同一部署下不变）
		host, _ := os.Hostname()
		machineIDValue = fmt.Sprintf("%x", sha256.Sum256([]byte("workbuddy-helper:"+host)))
	})
	return machineIDValue
}

// newUUID v4 随机 UUID（对齐客户端 generateUUUID，带连字符）
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// DefaultActivePrompt 活跃会话默认消息（用户自定义留空时的兜底）
const DefaultActivePrompt = "你是谁，谁开发你的，现在是什么时间，什么天气"

// ActiveChat 以客户端特征发送一次对话会话。
// 国际版（workbuddy.ai）的每日积分按“活跃账号”发放：每天至少发起一次对话；
// 请求完全对齐国际版客户端（CLI User-Agent、X-Product: SaaS、stream、system 消息在前），
// 逆向实测：第一条消息必须是 system，否则返回 400 code=11128。
// prompt 为用户消息内容：过短问候（如 "hi"）可能被服务端判定为非有效会话，
// 自定义更自然的多句提问可提高会话有效性；prompt 留空使用 DefaultActivePrompt。
type ActiveChatResult struct {
	ReportWarning error
	// ConversationWarning 记录云端会话创建失败（会话登记失败不影响对话本身，
	// 但真实客户端一定会先登记会话，缺失时活跃可能不被认定）。
	ConversationWarning error
}

// ConversationInfo 是云端会话登记结果（对齐桌面端 EdgeSync syncCreateConversation）。
type ConversationInfo struct {
	// ConversationID 服务端会话 ID；登记成功后用作对话请求的 X-Conversation-ID。
	ConversationID string
	// SessionID 服务端回传的会话标识（部分部署与 ConversationID 相同）。
	SessionID string
}

// CreateConversation 复刻桌面端 EdgeSync 的 syncCreateConversation：
// POST /console/as/conversations/v2 在云端登记一个本地会话。
//
// 真实客户端的活跃链路是「会话登记 → 对话 → 活跃」，会话登记让服务端确认
// 该账号正以桌面客户端身份在本地新开会话；缺少这一步时，服务端可能不把
// 随后的对话计为有效活跃会话。每次活跃都应新开一个会话（sessionID 每次不同）。
func (c *Client) CreateConversation(a *model.Account, sessionID, title, modelName string) (ConversationInfo, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ConversationInfo{}, &Error{Kind: KindProtocol, Msg: "会话 ID 不能为空"}
	}
	title = strings.TrimSpace(title)
	if title == "" {
		// 对齐 EdgeSync resolveTitleForSync 的默认标题
		title = "新任务"
	}
	now := time.Now().UnixMilli()
	body, _ := json.Marshal(map[string]any{
		"type":               "local",
		"sessionId":          sessionID,
		"name":               title,
		"conversationOrigin": "legacy_workbuddy_local",
		"workDir":            "",
		// 无工作目录的本地会话在真实客户端记为 playground
		"isPlayground":     1,
		"createdAtMs":      now,
		"updatedAtMs":      now,
		"lastActivityAtMs": now,
		"clientContext": map[string]any{
			"localStatus":    "active",
			"hostId":         machineID(),
			"sessionKind":    "manual",
			"model":          modelName,
			"sourceMode":     "",
			"permissionMode": "",
			"expert": map[string]any{
				"id":              "",
				"locale":          "",
				"runtimeIdentity": "",
				"marketplace":     "",
			},
		},
	})
	req, err := http.NewRequest(http.MethodPost, c.base(a, false)+"/console/as/conversations/v2", bytes.NewReader(body))
	if err != nil {
		return ConversationInfo{}, err
	}
	conversationHeaders(req, a)
	data, _, err := c.doEnvelope(req)
	if err != nil {
		return ConversationInfo{}, err
	}
	var result struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return ConversationInfo{}, &Error{Kind: KindProtocol, Msg: "会话登记响应格式发生变化"}
	}
	return ConversationInfo{ConversationID: result.ID, SessionID: result.SessionID}, nil
}

// conversationHeaders 会话登记请求头：与对话请求共用客户端身份特征，
// 但不带会话维度头（会话尚未创建）。
func conversationHeaders(req *http.Request, a *model.Account) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", ClientUA)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Request-ID", strings.ReplaceAll(newUUID(), "-", ""))
	if isGlobalAccount(a) {
		req.Header.Set("User-Agent", IntlClientUA)
		req.Header.Set("X-IDE-Type", IntlClientName)
		req.Header.Set("X-IDE-Name", IntlClientName)
		req.Header.Set("X-IDE-Version", IntlClientVer)
	}
}

func (c *Client) ActiveChat(a *model.Account, modelName, prompt string) error {
	_, err := c.ActiveChatWithResult(a, modelName, prompt)
	return err
}

func (c *Client) ActiveChatWithResult(a *model.Account, modelName, prompt string) (ActiveChatResult, error) {
	if strings.TrimSpace(modelName) == "" {
		modelName = "hy3"
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = DefaultActivePrompt
	}
	// max_tokens：hy3 / hy4-preview 在国际版 product.json 中均为 onlyReasoning
	// 纯推理模型（onlyReasoning=true、reasoning.effort=high），推理内容
	// （delta.reasoning_content）同样占用输出预算。真实客户端按模型
	// maxOutputTokens（hy3=64000）下发；预算过小会让推理先耗尽额度、
	// 正式回答 content 为空，从而被误判为“未返回有效回答”。
	maxTokens := 4096
	// 会话 ID：客户端本地生成（用于 growthEvent.id 与请求头，服务端据此登记活跃）。
	// 每次活跃都新开一个会话：sessionID 每次调用都不同，绝不复用历史会话。
	sessionID := newUUID()
	convID := sessionID
	reqID := strings.ReplaceAll(newUUID(), "-", "")
	// 会话登记：对齐真实客户端 EdgeSync，先 POST /console/as/conversations/v2
	// 让服务端确认账号正以桌面客户端身份在本地新开会话，再发对话。
	// 登记失败不阻断对话（尽力而为），但会作为警告回传给上层日志。
	var conversationWarning error
	if isGlobalAccount(a) {
		info, err := c.CreateConversation(a, sessionID, "", modelName)
		if err != nil {
			conversationWarning = err
		} else if info.ConversationID != "" {
			// 服务端会话 ID 与本地 sessionID 映射：对话请求使用服务端 ID，
			// growthEvent.id 仍用本地 sessionID（与真实客户端一致）。
			convID = info.ConversationID
		}
	}
	// growthEvent：真实客户端（WorkBuddy 桌面场景）把 chat_request_send 事件
	// 以 JSON 字符串放在请求体 extra_vars.growthEvent 随对话一起上报——
	// 服务端从该字段读取活跃事件（独立 /v2/report 不是桌面端路径）。
	growthEvents := []map[string]any{{
		"eventCode": "chat_request_send",
		"id":        sessionID,
		"extra": map[string]any{
			"inputLength":          len([]rune(prompt)),
			"requestModelId":       modelName,
			"requestModelName":     modelName,
			"mode":                 "craft",
			"command":              "",
			"expertId":             "",
			"recommendId":          "",
			"skillId":              "",
			"skillCount":           0,
			"totalCount":           0,
			"rootRequestId":        reqID,
			"parentConversationId": sessionID,
			"agentName":            "craft",
			"agentType":            "default",
		},
	}}
	growthJSON, _ := json.Marshal(growthEvents)
	body, _ := json.Marshal(map[string]any{
		"model":      modelName,
		"stream":     true,
		"max_tokens": maxTokens,
		"extra_vars": map[string]any{"growthEvent": string(growthJSON)},
		// 对齐真实客户端消息格式：用户提问包 <user_query> 标签
		// （UserQueryInterceptor: `${prefix} <user_query> ${text} </user_query>`），
		// content 为 typed block 数组；服务端扣费页据此提取请求内容展示。
		"messages": []map[string]any{
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": []map[string]string{
				{"type": "text", "text": "<user_query> " + prompt + " </user_query>"},
			}},
		},
	})
	req, err := http.NewRequest(http.MethodPost, c.base(a, false)+"/v2/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ActiveChatResult{}, err
	}
	chatHeaders(req, a)
	req.Header.Set("X-Conversation-ID", convID)
	req.Header.Set("X-Conversation-Request-ID", reqID)
	req.Header.Set("Accept", "text/event-stream")
	if isGlobalAccount(a) {
		// 活跃记录按客户端识别：必须带 WorkBuddy 桌面端特征（X-IDE-* + 客户端 UA），
		// 缺失会在 plans-usage 里显示为未知客户端
		req.Header.Set("User-Agent", IntlClientUA)
		req.Header.Set("X-IDE-Type", IntlClientName)
		req.Header.Set("X-IDE-Name", IntlClientName)
		req.Header.Set("X-IDE-Version", IntlClientVer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ActiveChatResult{}, &Error{Kind: KindTransport, Msg: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		raw, readErr := readResponseBody(resp.Body, 2<<20)
		if readErr != nil {
			if errors.Is(readErr, errResponseTooLarge) {
				return ActiveChatResult{}, &Error{Kind: KindProtocol, Status: resp.StatusCode, Msg: "响应内容过大"}
			}
			return ActiveChatResult{}, &Error{Kind: KindTransport, Status: resp.StatusCode, Msg: "读取响应失败: " + readErr.Error()}
		}
		var env envelope
		if json.Unmarshal(raw, &env) == nil && (env.Code != 0 || env.Msg != "") {
			return ActiveChatResult{}, classifyBusiness(resp.StatusCode, env.Code, env.Msg)
		}
		return ActiveChatResult{}, classifyHTTP(resp.StatusCode, string(raw))
	}
	// A successful HTTP status alone does not mean the streamed chat completed.
	raw, err := readResponseBody(resp.Body, 2<<20)
	if err != nil {
		if errors.Is(err, errResponseTooLarge) {
			return ActiveChatResult{}, &Error{Kind: KindProtocol, Status: resp.StatusCode, Msg: "对话响应内容过大"}
		}
		return ActiveChatResult{}, &Error{Kind: KindTransport, Status: resp.StatusCode, Msg: "读取对话响应失败: " + err.Error()}
	}
	if err := validateActiveChatResponse(raw); err != nil {
		return ActiveChatResult{}, err
	}

	// 备用通道：桌面端主路径是请求体 extra_vars.growthEvent（上方已带），
	// 这里再补一次独立遥测上报。失败应可见，但不能抹掉已成功的对话。
	warning := c.reportChatEvent(a, prompt, modelName, convID, reqID, req.Header.Get("X-Conversation-Message-ID"))
	return ActiveChatResult{ReportWarning: warning, ConversationWarning: conversationWarning}, nil
}

func validateActiveChatResponse(raw []byte) error {
	var sawAnswer bool
	var eventName string
	var dataLines []string
	// snippet 记录最后一段业务负载，仅在判定失败时用于定位上游实际返回内容。
	var snippet string
	checkEvent := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = nil
		name := eventName
		eventName = ""
		if data == "[DONE]" {
			return nil
		}
		if snippet == "" {
			snippet = short(data, 160)
		}
		var packet struct {
			Code    int             `json:"code"`
			Msg     string          `json:"msg"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta struct {
					Content          json.RawMessage `json:"content"`
					ReasoningContent string          `json:"reasoning_content"`
					Reasoning        string          `json:"reasoning"`
				} `json:"delta"`
				Message struct {
					Content          json.RawMessage `json:"content"`
					ReasoningContent string          `json:"reasoning_content"`
					Reasoning        string          `json:"reasoning"`
				} `json:"message"`
				Text string `json:"text"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &packet); err != nil {
			return &Error{Kind: KindProtocol, Msg: "对话流包含无效 JSON"}
		}
		if packet.Code != 0 {
			return classifyBusiness(http.StatusOK, packet.Code, packet.Msg)
		}
		if len(packet.Error) > 0 && string(packet.Error) != "null" {
			var detail struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Msg     string `json:"msg"`
			}
			if json.Unmarshal(packet.Error, &detail) != nil {
				_ = json.Unmarshal(packet.Error, &detail.Message)
			}
			message := detail.Message
			if message == "" {
				message = detail.Msg
			}
			if message == "" {
				message = "对话流返回错误"
			}
			return classifyBusiness(http.StatusOK, detail.Code, short(message, 180))
		}
		if name == "error" {
			return &Error{Kind: KindBusiness, Msg: short(data, 180)}
		}
		for _, choice := range packet.Choices {
			if strings.TrimSpace(chatContentText(choice.Delta.Content)) != "" ||
				strings.TrimSpace(choice.Delta.ReasoningContent) != "" ||
				strings.TrimSpace(choice.Delta.Reasoning) != "" ||
				strings.TrimSpace(chatContentText(choice.Message.Content)) != "" ||
				strings.TrimSpace(choice.Message.ReasoningContent) != "" ||
				strings.TrimSpace(choice.Message.Reasoning) != "" ||
				strings.TrimSpace(choice.Text) != "" {
				sawAnswer = true
			}
		}
		return nil
	}
	body := strings.ReplaceAll(string(raw), "\r\n", "\n")
	hasSSE := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data:") || strings.HasPrefix(line, "event:") {
			hasSSE = true
			break
		}
	}
	// 部分部署（代理或自建网关）直接返回整段 JSON，而不是 SSE 事件流。
	if !hasSSE {
		if trimmed := strings.TrimSpace(body); trimmed != "" && trimmed != "[DONE]" {
			dataLines = append(dataLines, trimmed)
		}
	} else {
		for _, line := range strings.Split(body, "\n") {
			switch {
			case line == "":
				if err := checkEvent(); err != nil {
					return err
				}
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}
	if err := checkEvent(); err != nil {
		return err
	}
	if !sawAnswer {
		msg := "对话未返回有效回答，不能确认活跃"
		if snippet != "" {
			msg += "（响应片段：" + snippet + "）"
		}
		return &Error{Kind: KindProtocol, Msg: msg}
	}
	return nil
}

// chatContentText flattens a chat content value, which may be a plain string or
// an array of typed blocks ({"type":"text","text":"..."}) as used by the
// client-side message format.
func chatContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var sb strings.Builder
		for _, block := range blocks {
			sb.WriteString(block.Text)
		}
		return sb.String()
	}
	return ""
}

// reportChatEvent 复刻 CLI StandardEventService：上报 chat_request_send 事件
// 结构 [{code:"chat_request_send", event:{...}}]；失败不阻断活跃流程（尽力而为）。
func (c *Client) reportChatEvent(a *model.Account, prompt, modelName, convID, reqID, msgID string) error {
	now := time.Now().UnixMilli()
	promptLen := 0
	for range prompt {
		promptLen++
	}
	event := map[string]any{
		"eventCode":   "chat_request_send",
		"timestamp":   now,
		"reportDelay": 0,
		// telemetryClientInfo 展开字段（对齐桌面端 ACP _meta 注入的 15 字段，
		// CLI 的 telemetry_client_info_event_processor 会将其展开进事件；
		// 缺少这些字段服务端可能不认定为桌面端活跃会话）
		"ideType":               IntlClientName,
		"ideName":               IntlClientName,
		"ideVersion":            IntlClientVer,
		"machineId":             machineID(),
		"sessionId":             convID,
		"userId":                a.UID,
		"username":              a.Nickname,
		"userNickname":          a.Nickname,
		"enterpriseId":          a.EnterpriseID,
		"extName":               "workbuddy-desktop",
		"extVersion":            IntlClientVer,
		"downloadChannel":       "official",
		"userAgent":             IntlClientUA,
		"featureModule":         "",
		"mode":                  "craft",
		"conversationId":        convID,
		"requestId":             reqID,
		"inputLength":           promptLen,
		"requestModelId":        modelName,
		"requestModelName":      modelName,
		"isPlan":                false,
		"isAutoExecuteTerminal": false,
		"isAutoModify":          false,
		"codebaseEnable":        false,
		"maxToken":              0,
		"maxSteps":              0,
		"temperature":           0,
		"maxRetries":            0,
		"mentionContexts":       []any{},
		"knowledgeId":           []any{},
		"knowledgeName":         []any{},
		"codebaseId":            "",
		"mentionContextCount":   0,
		"command":               "",
		"expertId":              "",
		"recommendId":           "",
		"skillId":               "",
		"skillCount":            0,
		"totalCount":            0,
		"fileUri":               "",
		"presentAt":             now,
		"rootRequestId":         reqID,
		"parentConversationId":  convID,
		"agentName":             "craft",
		"agentType":             "default",
	}
	// 请求体结构对齐 CLI StandardEventService：事件对象数组，eventCode 在顶层
	// （report() 里 `{eventCode, timestamp, reportDelay, ...payload}` 直接入批），
	// 而不是 {code, event} 嵌套；嵌套会被服务端判为 "eventCode is empty or not string"。
	body, _ := json.Marshal([]map[string]any{event})
	req, err := http.NewRequest(http.MethodPost, c.base(a, false)+"/v2/report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	// 标准事件服务头：对齐桌面端 createDesktopHostReportHeadersProvider
	// （X-Request-ID / X-Product / User-Agent + 鉴权头），仅 Content-Type +
	// Authorization 会被服务端按非客户端来源处理。
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Request-ID", strings.ReplaceAll(newUUID(), "-", ""))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", IntlClientUA)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Kind: KindTransport, Msg: err.Error()}
	}
	defer resp.Body.Close()
	raw, err := readResponseBody(resp.Body, 2<<20)
	if err != nil {
		if errors.Is(err, errResponseTooLarge) {
			return &Error{Kind: KindProtocol, Status: resp.StatusCode, Msg: "遥测响应内容过大"}
		}
		return &Error{Kind: KindTransport, Status: resp.StatusCode, Msg: "读取遥测响应失败: " + err.Error()}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var env envelope
		if json.Unmarshal(raw, &env) == nil && (env.Code != 0 || env.Msg != "") {
			return classifyBusiness(resp.StatusCode, env.Code, env.Msg)
		}
		return classifyHTTP(resp.StatusCode, string(raw))
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		var env envelope
		if json.Unmarshal(raw, &env) == nil && env.Code != 0 {
			return classifyBusiness(resp.StatusCode, env.Code, env.Msg)
		}
	}
	return nil
}

// chatHeaders 模拟桌面客户端对话请求头：活跃判定只认客户端请求，网页端请求无效。
// 头集合与实测请求一致：Authorization / X-User-Id / X-Domain / X-Product。
func chatHeaders(req *http.Request, a *model.Account) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ClientUA)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
	req.Header.Set("X-Product", "SaaS")
	// 会话维度头（复刻 CLI axiosToFetchAdapter）：conversation/request/message ID 均为
	// 客户端本地生成 UUID，服务端按此登记会话；缺失时请求不被计为有效活跃会话
	req.Header.Set("X-Conversation-ID", newUUID())
	req.Header.Set("X-Conversation-Request-ID", strings.ReplaceAll(newUUID(), "-", ""))
	req.Header.Set("X-Conversation-Message-ID", strings.ReplaceAll(newUUID(), "-", ""))
	req.Header.Set("X-Request-ID", strings.ReplaceAll(newUUID(), "-", ""))
	req.Header.Set("X-Agent-Intent", "craft")
}

type PointResult struct {
	Balance  int64
	Used     int64
	Total    int64
	Packages []model.PointPackage
}

const (
	pointsPageSize = 100
	pointsMaxPages = 100
)

type pointsPackage struct {
	PackageName         string `json:"PackageName"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CapacitySize        int64  `json:"CapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	PackageEndTime      string `json:"PackageEndTime"`
	CycleEndTime        string `json:"CycleEndTime"`
}

func (c *Client) pointsPage(a *model.Account, now time.Time, pageNumber int) ([]pointsPackage, *int, error) {
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               pageNumber,
		"PageSize":                 pointsPageSize,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(101 * 365 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	req, err := http.NewRequest(http.MethodPost, c.base(a, true)+"/v2/billing/meter/get-user-resource", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	authHeaders(req, a, true)
	data, _, err := c.doEnvelope(req)
	if err != nil {
		return nil, nil, err
	}
	var envelopeData struct {
		Response *struct {
			Data *struct {
				TotalCount *int             `json:"TotalCount"`
				Accounts   *[]pointsPackage `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &envelopeData); err != nil {
		return nil, nil, &Error{Kind: KindProtocol, Msg: "积分响应格式发生变化"}
	}
	if envelopeData.Response == nil || envelopeData.Response.Data == nil || envelopeData.Response.Data.Accounts == nil {
		return nil, nil, &Error{Kind: KindProtocol, Msg: "积分响应缺少套餐列表"}
	}
	return *envelopeData.Response.Data.Accounts, envelopeData.Response.Data.TotalCount, nil
}

func appendPointsPackages(result *PointResult, packages []pointsPackage) {
	for _, p := range packages {
		remain, used, total := packageValues(p.CycleCapacityRemain, p.CycleCapacityUsed, p.CycleCapacitySize, p.CapacityRemain, p.CapacityUsed, p.CapacitySize)
		result.Balance += remain
		result.Used += used
		result.Total += total
		expiresAt := p.PackageEndTime
		if expiresAt == "" {
			expiresAt = p.CycleEndTime
		}
		result.Packages = append(result.Packages, model.PointPackage{Name: p.PackageName, Remain: remain, Used: used, Total: total, ExpiresAt: expiresAt})
	}
}

func (c *Client) Points(a *model.Account) (PointResult, error) {
	now := time.Now()
	result := PointResult{}
	expectedTotal := -1

	for pageNumber := 1; pageNumber <= pointsMaxPages; pageNumber++ {
		packages, totalCount, err := c.pointsPage(a, now, pageNumber)
		if err != nil {
			return PointResult{}, err
		}
		if totalCount != nil {
			if *totalCount < 0 || *totalCount > pointsPageSize*pointsMaxPages {
				return PointResult{}, &Error{Kind: KindProtocol, Msg: "积分套餐总数异常"}
			}
			if expectedTotal < 0 {
				expectedTotal = *totalCount
			} else if expectedTotal != *totalCount {
				return PointResult{}, &Error{Kind: KindProtocol, Msg: "积分分页总数在查询过程中发生变化，请重试"}
			}
		}

		if len(packages) == 0 {
			if expectedTotal < 0 || len(result.Packages) >= expectedTotal {
				return result, nil
			}
			return PointResult{}, &Error{Kind: KindProtocol, Msg: "积分包分页不完整，请重试"}
		}
		appendPointsPackages(&result, packages)

		if expectedTotal >= 0 {
			if len(result.Packages) > expectedTotal {
				return PointResult{}, &Error{Kind: KindProtocol, Msg: "积分分页返回了重复或超量套餐"}
			}
			if len(result.Packages) == expectedTotal {
				return result, nil
			}
			continue
		}

		if len(packages) < pointsPageSize {
			return result, nil
		}
		// 旧服务端偶尔省略 TotalCount。满页时继续探测下一页，直到短页/空页；
		// 外层 pointsMaxPages 仍限制最坏情况下的请求次数。
		continue
	}

	return PointResult{}, &Error{Kind: KindProtocol, Msg: "积分包分页超过安全上限"}
}

func packageValues(cycleRemain, cycleUsed, cycleSize, remain, used, size int64) (int64, int64, int64) {
	if cycleSize > 0 || cycleRemain != 0 || cycleUsed != 0 {
		if cycleRemain < 0 {
			cycleRemain = 0
		}
		if cycleRemain > cycleSize {
			cycleRemain = cycleSize
		}
		derivedUsed := cycleSize - cycleRemain
		if cycleUsed > derivedUsed {
			derivedUsed = cycleUsed
			if cycleSize >= derivedUsed {
				cycleRemain = cycleSize - derivedUsed
			}
		}
		return cycleRemain, derivedUsed, cycleSize
	}
	if remain < 0 {
		remain = 0
	}
	if size > 0 && remain > size {
		remain = size
	}
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
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
		DialContext:         (&net.Dialer{Timeout: 15 * time.Second, Resolver: dnsFallbackResolver}).DialContext,
	})
}

func proxyTransport(proxyURL string) (*http.Transport, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return &http.Transport{
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			DialContext:         (&net.Dialer{Timeout: 15 * time.Second, Resolver: dnsFallbackResolver}).DialContext,
		}, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("代理地址格式不正确: %w", err)
	}
	if !model.ValidProxyScheme(u.Scheme) {
		return nil, fmt.Errorf("代理协议仅支持 http/https/socks5/socks5h，收到 %q", u.Scheme)
	}
	transport := &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		Proxy:               http.ProxyURL(u),
		// 代理地址本身（如 socks5://192.168.5.1:1070 是 IP，域名网关也支持）
		// 与经代理后无需本地解析；但 http 代理模式下目标域名由代理解析。
		// 这里仍挂兜底解析器以覆盖代理主机为域名的情况。
		DialContext: (&net.Dialer{Timeout: 15 * time.Second, Resolver: dnsFallbackResolver}).DialContext,
	}
	// socks5/socks5h 的代理握手由 x/net/proxy DialContext 完成（http.ProxyURL 只认 http/https）
	switch u.Scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pass, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pass}
		}
		dialer, derr := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
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

// dnsFallbackResolver 带兜底的解析器：系统 DNS 失败时回退公共 DNS
// （阿里/腾讯/CF），解决容器内 Docker DNS（127.0.0.11）上游失效导致的
// "lookup ... i/o timeout" 全量解析失败。
var dnsFallbackResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
		// 依次尝试系统地址（原样）→ 公共 DNS
		if c, err := d.DialContext(ctx, network, address); err == nil {
			return c, nil
		}
		var lastErr error
		for _, ns := range []string{"223.5.5.5:53", "119.29.29.29:53", "1.1.1.1:53"} {
			c, err := d.DialContext(ctx, network, ns)
			if err == nil {
				return c, nil
			}
			lastErr = err
		}
		return nil, lastErr
	},
}

// NewTransportOnly 只构造传输层（供热更新：替换现有 client 的 Transport，保留其端点定制）。
func NewTransportOnly(proxyURL string) (*http.Transport, error) {
	return proxyTransport(proxyURL)
}

// ValidateProxyURL 校验代理地址：空串合法（直连）；协议必须在 http/https/socks5/socks5h 白名单。
func ValidateProxyURL(proxyURL string) error {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("代理地址格式不正确: %s", proxyURL)
	}
	if !model.ValidProxyScheme(u.Scheme) {
		return fmt.Errorf("代理协议仅支持 http/https/socks5/socks5h，收到 %q", u.Scheme)
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
		ChatBaseCN:    CNChatBase,
		BillingBaseCN: CNBillingBase,
		GlobalBase:    GlobalBase,
		GlobalBilling: GlobalBillingBase,
	}
}

func (c *Client) base(a *model.Account, billing bool) string {
	if a != nil && isGlobal(a.Domain) {
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

func isGlobal(domain string) bool {
	return model.IsGlobalDomain(domain)
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
	if isGlobal(a.Domain) {
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

func (c *Client) doEnvelope(req *http.Request) (json.RawMessage, int, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, &Error{Kind: KindTransport, Msg: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
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

func classifyHTTP(status int, body string) error {
	msg := short(body, 180)
	switch {
	case status == http.StatusUnauthorized || strings.Contains(body, "12153") || strings.Contains(strings.ToLower(body), "offline user session"):
		return &Error{Kind: KindAuthDead, Status: status, Msg: msg}
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
	if status == http.StatusUnauthorized || code == 12153 || strings.Contains(lower, "offline user session") || strings.Contains(lower, "session not found") {
		return &Error{Kind: KindAuthDead, Status: status, Code: code, Msg: msg}
	}
	switch {
	case status == http.StatusTooManyRequests:
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
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

type LoginSession struct {
	State   string
	AuthURL string
	Client  *http.Client
	// intl=true 时走国际版授权（workbuddy.ai，platform=workbuddy-ai）
	intl bool
}

// NewLoginSession version: ModeCN（国内版 CLI 授权）/ ModeIntl（国际版授权）
func NewLoginSession(version string) *LoginSession {
	jar, _ := cookiejar.New(nil)
	return &LoginSession{
		Client: &http.Client{
			Timeout: 30 * time.Second,
			Jar:     jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		intl: version == model.ModeIntl,
	}
}

func (s *LoginSession) chatBase() string {
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
	s.State, s.AuthURL = result.State, result.AuthURL
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
	data, status, err := base.doEnvelope(req)
	if err != nil {
		if status >= 400 && status < 500 {
			return LoginResult{}, &Error{Kind: KindBusiness, Status: status, Msg: "登录尚未完成，请先在浏览器完成授权"}
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
	if isGlobal(a.Domain) {
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
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	} else if a.ExpiresAt <= time.Now().Unix() {
		a.ExpiresAt = time.Now().Add(15 * time.Minute).Unix()
	}
	return nil
}

func originFor(a *model.Account) string {
	if a != nil && isGlobal(a.Domain) {
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
func (c *Client) ActiveChat(a *model.Account, modelName, prompt string) error {
	if strings.TrimSpace(modelName) == "" {
		modelName = "hy3"
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = DefaultActivePrompt
	}
	// max_tokens 按提示词长度放宽：保证模型能完整作答，会话被判有效
	maxTokens := 64
	if r := []rune(prompt); len(r) > 40 {
		maxTokens = 160
	}
	body, _ := json.Marshal(map[string]any{
		"model":      modelName,
		"stream":     true,
		"max_tokens": maxTokens,
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
		return err
	}
	chatHeaders(req, a)
	req.Header.Set("Accept", "text/event-stream")
	if isGlobal(a.Domain) {
		// 活跃记录按客户端识别：必须带 WorkBuddy 桌面端特征（X-IDE-* + 客户端 UA），
		// 缺失会在 plans-usage 里显示为未知客户端
		req.Header.Set("User-Agent", IntlClientUA)
		req.Header.Set("X-IDE-Type", IntlClientName)
		req.Header.Set("X-IDE-Name", IntlClientName)
		req.Header.Set("X-IDE-Version", IntlClientVer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Kind: KindTransport, Msg: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		return classifyHTTP(resp.StatusCode, string(raw))
	}
	// 2xx：读完整个 SSE 流（max_tokens 很小，秒级结束），确保服务端完整受理本次会话
	_, _ = io.Copy(io.Discard, resp.Body)

	// 活跃判定闭环：真实客户端发完消息会上报 chat_request_send 遥测事件
	// （POST {endpoint}/v2/report，batch 数组）。缺失该上报，服务端不记为有效活跃会话。
	convID := req.Header.Get("X-Conversation-ID")
	reqID := req.Header.Get("X-Conversation-Request-ID")
	msgID := req.Header.Get("X-Conversation-Message-ID")
	_ = c.reportChatEvent(a, prompt, modelName, convID, reqID, msgID)
	return nil
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
		"eventCode":             "chat_request_send",
		"timestamp":             now,
		"reportDelay":           0,
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
	body, _ := json.Marshal([]map[string]any{{"code": "chat_request_send", "event": event}})
	req, err := http.NewRequest(http.MethodPost, c.base(a, false)+"/v2/report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	chatHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Kind: KindTransport, Msg: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
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

func (c *Client) Points(a *model.Account) (PointResult, error) {
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(101 * 365 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	req, err := http.NewRequest(http.MethodPost, c.base(a, true)+"/v2/billing/meter/get-user-resource", bytes.NewReader(body))
	if err != nil {
		return PointResult{}, err
	}
	authHeaders(req, a, true)
	data, _, err := c.doEnvelope(req)
	if err != nil {
		return PointResult{}, err
	}
	var envelopeData struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CapacitySize        int64  `json:"CapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					PackageEndTime      string `json:"PackageEndTime"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &envelopeData); err != nil {
		return PointResult{}, &Error{Kind: KindProtocol, Msg: "积分响应格式发生变化"}
	}
	result := PointResult{}
	for _, p := range envelopeData.Response.Data.Accounts {
		remain, used, total := packageValues(p.CycleCapacityRemain, p.CycleCapacityUsed, p.CycleCapacitySize, p.CapacityRemain, p.CapacityUsed, p.CapacitySize)
		result.Balance += remain
		result.Used += used
		result.Total += total
		result.Packages = append(result.Packages, model.PointPackage{Name: p.PackageName, Remain: remain, Used: used, Total: total, ExpiresAt: p.PackageEndTime})
	}
	return result, nil
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

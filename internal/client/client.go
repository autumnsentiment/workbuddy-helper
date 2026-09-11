package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"workbuddy-helper/internal/model"
)

const (
	CNChatBase    = "https://copilot.tencent.com"
	CNBillingBase = "https://www.codebuddy.cn"
	GlobalBase    = "https://www.workbuddy.ai"
	ClientUA      = "CLI/2.63.2 CodeBuddy/2.63.2"
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
}

func New() *Client {
	transport := &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	}
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
	}
}

func (c *Client) base(a *model.Account, billing bool) string {
	if a != nil && isGlobal(a.Domain) {
		return c.GlobalBase
	}
	if billing {
		return c.BillingBaseCN
	}
	return c.ChatBaseCN
}

func isGlobal(domain string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	return domain == "workbuddy.ai" || strings.HasSuffix(domain, ".workbuddy.ai")
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
}

func NewLoginSession() *LoginSession {
	jar, _ := cookiejar.New(nil)
	return &LoginSession{Client: &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (s *LoginSession) Start() error {
	req, err := http.NewRequest(http.MethodPost, CNChatBase+"/v2/plugin/auth/state?platform=CLI", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	commonHeaders(req, "https://www.codebuddy.cn")
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
	tokenURL := CNChatBase + "/v2/plugin/auth/token?state=" + url.QueryEscape(s.State)
	req, err := http.NewRequest(http.MethodGet, tokenURL, nil)
	if err != nil {
		return LoginResult{}, err
	}
	commonHeaders(req, "https://www.codebuddy.cn")
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
	acctURL := CNChatBase + "/v2/plugin/login/account?state=" + url.QueryEscape(s.State)
	acctReq, err := http.NewRequest(http.MethodGet, acctURL, nil)
	if err != nil {
		return result, err
	}
	commonHeaders(acctReq, "https://www.codebuddy.cn")
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
	commonHeaders(req, originFor(a))
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
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

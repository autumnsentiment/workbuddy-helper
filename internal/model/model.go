package model

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const StateVersion = 1

// 版本模式：cn = 国内版（每日签到领取积分）；intl = 国际版（每日活跃对话获取积分）
const (
	ModeCN   = "cn"
	ModeIntl = "intl"
)

func IsGlobalDomain(domain string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	return domain == "workbuddy.ai" || strings.HasSuffix(domain, ".workbuddy.ai")
}

type Account struct {
	ID           string `json:"id"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	Alias        string `json:"alias,omitempty"`
	Domain       string `json:"domain,omitempty"`
	// Version 账号归属版本：cn（国内版列表）或 intl（国际版列表），两套列表独立管理
	Version         string    `json:"version,omitempty"`
	AccessToken     string    `json:"accessToken"`
	RefreshToken    string    `json:"refreshToken,omitempty"`
	ExpiresAt       int64     `json:"expiresAt,omitempty"`
	Enabled         bool      `json:"enabled"`
	Status          string    `json:"status,omitempty"`
	LastError       string    `json:"lastError,omitempty"`
	LastCheckinAt   time.Time `json:"lastCheckinAt,omitempty"`
	LastCheckinDate string    `json:"lastCheckinDate,omitempty"`
	LastCheckin     string    `json:"lastCheckin,omitempty"`
	LastCheckinMsg  string    `json:"lastCheckinMessage,omitempty"`
	LastActiveAt    time.Time `json:"lastActiveAt,omitempty"`
	LastActiveDate  string    `json:"lastActiveDate,omitempty"`
	LastActive      string    `json:"lastActive,omitempty"`
	LastActiveMsg   string    `json:"lastActiveMessage,omitempty"`
	// 国际版活跃积分复核：积分非即时到账，活跃后按 Settings.RecheckDelayMinutes 延迟复查
	PendingRecheckAt    time.Time      `json:"pendingRecheckAt,omitempty"`
	BalanceBeforeActive int64          `json:"balanceBeforeActive,omitempty"`
	RecheckAttempts     int            `json:"recheckAttempts,omitempty"`
	LastPointsAt        time.Time      `json:"lastPointsAt,omitempty"`
	Balance             int64          `json:"balance"`
	Used                int64          `json:"used"`
	Total               int64          `json:"total"`
	Packages            []PointPackage `json:"packages,omitempty"`
	CreatedAt           time.Time      `json:"createdAt"`
	UpdatedAt           time.Time      `json:"updatedAt"`
}

// JWTExpiryUnix returns the exp claim from a compact JWT as Unix seconds.
//
// The access token payload is decoded only for display and refresh bookkeeping;
// no signature verification is attempted because the helper does not have the
// issuer's signing keys.
func JWTExpiryUnix(token string) (int64, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[1] == "" {
		return 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Be tolerant of JWTs that use padded base64url encoding.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return 0, false
	}
	var claims struct {
		Exp json.RawMessage `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0, false
	}
	raw := strings.TrimSpace(string(claims.Exp))
	if raw == "" || raw == "null" {
		return 0, false
	}
	if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value > 0 {
		return value, true
	}
	// A few issuers serialize NumericDate as a quoted integer. Accept that
	// representation while rejecting arbitrary text.
	var quoted string
	if json.Unmarshal(claims.Exp, &quoted) == nil {
		if value, err := strconv.ParseInt(strings.TrimSpace(quoted), 10, 64); err == nil && value > 0 {
			return value, true
		}
	}
	return 0, false
}

// ResolveTokenExpiresAt prefers the access token's exp claim and falls back
// to the OAuth-style expiresIn response field. A missing/non-positive
// expiresIn is represented as zero (unknown) so callers can refresh
// conservatively without displaying a fabricated current-time expiry.
func ResolveTokenExpiresAt(accessToken string, expiresIn int64, now time.Time) int64 {
	if expiresAt, ok := JWTExpiryUnix(accessToken); ok {
		return expiresAt
	}
	if expiresIn <= 0 {
		return 0
	}
	return now.Add(time.Duration(expiresIn) * time.Second).Unix()
}

func (a Account) Region() string {
	if a.VersionMode() == ModeIntl {
		return "global"
	}
	return "cn"
}

// VersionMode returns the account's effective product version. Older state
// files may not have persisted Version, so infer it from the login domain.
func (a Account) VersionMode() string {
	if a.Version == ModeCN || a.Version == ModeIntl {
		return a.Version
	}
	if IsGlobalDomain(a.Domain) {
		return ModeIntl
	}
	return ModeCN
}

func (a Account) DisplayName() string {
	if a.Alias != "" {
		return a.Alias
	}
	if a.Nickname != "" {
		return a.Nickname
	}
	if len(a.UID) > 8 {
		return a.UID[:8]
	}
	return a.UID
}

type PointPackage struct {
	Name      string `json:"name,omitempty"`
	Remain    int64  `json:"remain"`
	Used      int64  `json:"used"`
	Total     int64  `json:"total"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

type Settings struct {
	ScheduleEnabled  bool   `json:"scheduleEnabled"`
	ScheduleTime     string `json:"scheduleTime"`
	LastScheduledDay string `json:"lastScheduledDay,omitempty"`
	// Mode: cn（默认，每日签到）或 intl（国际版活跃获取）
	Mode        string `json:"mode"`
	ActiveModel string `json:"activeModel"`
	// 活跃后积分复核延迟（分钟），积分非即时到账，默认 840（约 14 小时），可按实测调整
	RecheckDelayMinutes int `json:"recheckDelayMinutes"`
	// ProxyURL 自定义代理：支持 http:// https:// socks5:// socks5h://（含认证 user:pass@host:port）
	// 为空直连；作用于国际版上游请求，国内版始终固定 DNS 直连
	ProxyURL string `json:"proxyUrl"`
	// ActivePrompt 国际版活跃会话的用户消息内容；为空使用 client.DefaultActivePrompt。
	// 太短的问候可能被服务端判定为非有效会话，可自定义更自然的多句提问。
	ActivePrompt string `json:"activePrompt"`
}

// SettingsPatch 是 PUT /api/settings 使用的请求模型。
// 指针字段用于区分“未提交”与“明确提交零值/空字符串”，避免局部更新误清空设置，
// 同时允许用户显式清除代理、提示词或关闭定时任务。
type SettingsPatch struct {
	ScheduleEnabled     *bool   `json:"scheduleEnabled"`
	ScheduleTime        *string `json:"scheduleTime"`
	Mode                *string `json:"mode"`
	ActiveModel         *string `json:"activeModel"`
	RecheckDelayMinutes *int    `json:"recheckDelayMinutes"`
	ProxyURL            *string `json:"proxyUrl"`
	ActivePrompt        *string `json:"activePrompt"`
}

// ValidProxyScheme 代理协议白名单
func ValidProxyScheme(scheme string) bool {
	switch scheme {
	case "http", "https", "socks5", "socks5h":
		return true
	}
	return false
}

type LogEntry struct {
	Time      time.Time `json:"time"`
	Level     string    `json:"level"`
	AccountID string    `json:"accountId,omitempty"`
	Message   string    `json:"message"`
}

type State struct {
	Version  int        `json:"version"`
	Settings Settings   `json:"settings"`
	Accounts []Account  `json:"accounts"`
	Logs     []LogEntry `json:"logs,omitempty"`
}

func NewState() State {
	return State{
		Version: StateVersion,
		Settings: Settings{
			ScheduleTime:        "09:15",
			Mode:                ModeCN,
			ActiveModel:         "hy3",
			RecheckDelayMinutes: 840,
		},
		Accounts: []Account{},
		Logs:     []LogEntry{},
	}
}

type PublicAccount struct {
	ID               string    `json:"id"`
	UID              string    `json:"uid"`
	Nickname         string    `json:"nickname,omitempty"`
	Alias            string    `json:"alias,omitempty"`
	DisplayName      string    `json:"displayName"`
	Region           string    `json:"region"`
	Version          string    `json:"version,omitempty"`
	Enabled          bool      `json:"enabled"`
	Status           string    `json:"status"`
	LastError        string    `json:"lastError,omitempty"`
	LastCheckinAt    time.Time `json:"lastCheckinAt,omitempty"`
	LastCheckinDate  string    `json:"lastCheckinDate,omitempty"`
	LastCheckin      string    `json:"lastCheckin,omitempty"`
	LastCheckinMsg   string    `json:"lastCheckinMessage,omitempty"`
	LastActiveAt     time.Time `json:"lastActiveAt,omitempty"`
	LastActiveDate   string    `json:"lastActiveDate,omitempty"`
	LastActive       string    `json:"lastActive,omitempty"`
	LastActiveMsg    string    `json:"lastActiveMessage,omitempty"`
	PendingRecheckAt time.Time `json:"pendingRecheckAt,omitempty"`
	RecheckAttempts  int       `json:"recheckAttempts,omitempty"`
	LastPointsAt     time.Time `json:"lastPointsAt,omitempty"`
	// TokenExpiresAt contains only the access token expiration timestamp; the
	// credential itself is intentionally never part of PublicAccount.
	TokenExpiresAt int64          `json:"tokenExpiresAt,omitempty"`
	Balance        int64          `json:"balance"`
	Used           int64          `json:"used"`
	Total          int64          `json:"total"`
	Packages       []PointPackage `json:"packages,omitempty"`
}

func (a Account) Public() PublicAccount {
	tokenExpiresAt := a.ExpiresAt
	if expiresAt, ok := JWTExpiryUnix(a.AccessToken); ok {
		tokenExpiresAt = expiresAt
	}
	return PublicAccount{
		ID:               a.ID,
		UID:              a.UID,
		Nickname:         a.Nickname,
		Alias:            a.Alias,
		DisplayName:      a.DisplayName(),
		Region:           a.Region(),
		Version:          a.VersionMode(),
		Enabled:          a.Enabled,
		Status:           a.Status,
		LastError:        a.LastError,
		LastCheckinAt:    a.LastCheckinAt,
		LastCheckinDate:  a.LastCheckinDate,
		LastCheckin:      a.LastCheckin,
		LastCheckinMsg:   a.LastCheckinMsg,
		LastActiveAt:     a.LastActiveAt,
		LastActiveDate:   a.LastActiveDate,
		LastActive:       a.LastActive,
		LastActiveMsg:    a.LastActiveMsg,
		PendingRecheckAt: a.PendingRecheckAt,
		RecheckAttempts:  a.RecheckAttempts,
		LastPointsAt:     a.LastPointsAt,
		TokenExpiresAt:   tokenExpiresAt,
		Balance:          a.Balance,
		Used:             a.Used,
		Total:            a.Total,
		Packages:         append([]PointPackage(nil), a.Packages...),
	}
}

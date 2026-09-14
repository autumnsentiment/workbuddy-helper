package model

import (
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

func (a Account) Region() string {
	if IsGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
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
	Mode        string `json:"mode,omitempty"`
	ActiveModel string `json:"activeModel,omitempty"`
	// 活跃后积分复核延迟（分钟），积分非即时到账，默认 60，可用于实测到账延迟
	RecheckDelayMinutes int `json:"recheckDelayMinutes,omitempty"`
	// ProxyURL 自定义代理：支持 http:// https:// socks5:// socks5h://（含认证 user:pass@host:port）
	// 为空直连；作用于全部上游请求（国内版与国际版域名均适用）
	ProxyURL string `json:"proxyUrl,omitempty"`
	// ActivePrompt 国际版活跃会话的用户消息内容；为空使用默认 "hi"。
	// 太短的问候可能被服务端判定为非有效会话，可自定义更自然的多句提问。
	ActivePrompt string `json:"activePrompt,omitempty"`
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
			RecheckDelayMinutes: 60,
		},
		Accounts: []Account{},
		Logs:     []LogEntry{},
	}
}

type PublicAccount struct {
	ID               string         `json:"id"`
	UID              string         `json:"uid"`
	Nickname         string         `json:"nickname,omitempty"`
	Alias            string         `json:"alias,omitempty"`
	DisplayName      string         `json:"displayName"`
	Region           string         `json:"region"`
	Version          string         `json:"version,omitempty"`
	Enabled          bool           `json:"enabled"`
	Status           string         `json:"status"`
	LastError        string         `json:"lastError,omitempty"`
	LastCheckinAt    time.Time      `json:"lastCheckinAt,omitempty"`
	LastCheckinDate  string         `json:"lastCheckinDate,omitempty"`
	LastCheckin      string         `json:"lastCheckin,omitempty"`
	LastCheckinMsg   string         `json:"lastCheckinMessage,omitempty"`
	LastActiveAt     time.Time      `json:"lastActiveAt,omitempty"`
	LastActiveDate   string         `json:"lastActiveDate,omitempty"`
	LastActive       string         `json:"lastActive,omitempty"`
	LastActiveMsg    string         `json:"lastActiveMessage,omitempty"`
	PendingRecheckAt time.Time      `json:"pendingRecheckAt,omitempty"`
	RecheckAttempts  int            `json:"recheckAttempts,omitempty"`
	LastPointsAt     time.Time      `json:"lastPointsAt,omitempty"`
	Balance          int64          `json:"balance"`
	Used             int64          `json:"used"`
	Total            int64          `json:"total"`
	Packages         []PointPackage `json:"packages,omitempty"`
}

func (a Account) Public() PublicAccount {
	return PublicAccount{
		ID:               a.ID,
		UID:              a.UID,
		Nickname:         a.Nickname,
		Alias:            a.Alias,
		DisplayName:      a.DisplayName(),
		Region:           a.Region(),
		Version:          a.Version,
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
		Balance:          a.Balance,
		Used:             a.Used,
		Total:            a.Total,
		Packages:         append([]PointPackage(nil), a.Packages...),
	}
}

package model

import "time"

const StateVersion = 1

type Account struct {
	ID              string         `json:"id"`
	UID             string         `json:"uid"`
	EnterpriseID    string         `json:"enterpriseId,omitempty"`
	Nickname        string         `json:"nickname,omitempty"`
	Alias           string         `json:"alias,omitempty"`
	Domain          string         `json:"domain,omitempty"`
	AccessToken     string         `json:"accessToken"`
	RefreshToken    string         `json:"refreshToken,omitempty"`
	ExpiresAt       int64          `json:"expiresAt,omitempty"`
	Enabled         bool           `json:"enabled"`
	Status          string         `json:"status,omitempty"`
	LastError       string         `json:"lastError,omitempty"`
	LastCheckinAt   time.Time      `json:"lastCheckinAt,omitempty"`
	LastCheckinDate string         `json:"lastCheckinDate,omitempty"`
	LastCheckin     string         `json:"lastCheckin,omitempty"`
	LastCheckinMsg  string         `json:"lastCheckinMessage,omitempty"`
	LastPointsAt    time.Time      `json:"lastPointsAt,omitempty"`
	Balance         int64          `json:"balance"`
	Used            int64          `json:"used"`
	Total           int64          `json:"total"`
	Packages        []PointPackage `json:"packages,omitempty"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
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
			ScheduleTime: "09:15",
		},
		Accounts: []Account{},
		Logs:     []LogEntry{},
	}
}

type PublicAccount struct {
	ID              string         `json:"id"`
	UID             string         `json:"uid"`
	Nickname        string         `json:"nickname,omitempty"`
	Alias           string         `json:"alias,omitempty"`
	DisplayName     string         `json:"displayName"`
	Enabled         bool           `json:"enabled"`
	Status          string         `json:"status"`
	LastError       string         `json:"lastError,omitempty"`
	LastCheckinAt   time.Time      `json:"lastCheckinAt,omitempty"`
	LastCheckinDate string         `json:"lastCheckinDate,omitempty"`
	LastCheckin     string         `json:"lastCheckin,omitempty"`
	LastCheckinMsg  string         `json:"lastCheckinMessage,omitempty"`
	LastPointsAt    time.Time      `json:"lastPointsAt,omitempty"`
	Balance         int64          `json:"balance"`
	Used            int64          `json:"used"`
	Total           int64          `json:"total"`
	Packages        []PointPackage `json:"packages,omitempty"`
}

func (a Account) Public() PublicAccount {
	return PublicAccount{
		ID:              a.ID,
		UID:             a.UID,
		Nickname:        a.Nickname,
		Alias:           a.Alias,
		DisplayName:     a.DisplayName(),
		Enabled:         a.Enabled,
		Status:          a.Status,
		LastError:       a.LastError,
		LastCheckinAt:   a.LastCheckinAt,
		LastCheckinDate: a.LastCheckinDate,
		LastCheckin:     a.LastCheckin,
		LastCheckinMsg:  a.LastCheckinMsg,
		LastPointsAt:    a.LastPointsAt,
		Balance:         a.Balance,
		Used:            a.Used,
		Total:           a.Total,
		Packages:        append([]PointPackage(nil), a.Packages...),
	}
}

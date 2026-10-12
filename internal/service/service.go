package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"workbuddy-helper/internal/client"
	"workbuddy-helper/internal/model"
	"workbuddy-helper/internal/store"
)

var locChina = time.FixedZone("Asia/Shanghai", 8*60*60)

var (
	ErrAccountNotFound     = errors.New("account not found")
	ErrLoginSessionExpired = errors.New("登录会话不存在或已过期")
	ErrLoginSessionLimit   = errors.New("登录会话数量已达上限，请稍后重试")
	ErrAccountModeMismatch = errors.New("账号版本与当前操作不匹配")
)

const (
	loginSessionTTL  = 15 * time.Minute
	maxLoginSessions = 32
)

type settingsValidationError struct{ message string }

func (e *settingsValidationError) Error() string { return e.message }

func settingsValidation(message string) error {
	return &settingsValidationError{message: message}
}

func IsSettingsValidation(err error) bool {
	var target *settingsValidationError
	return errors.As(err, &target)
}

type Service struct {
	mu       sync.RWMutex
	accounts map[string]*sync.Mutex
	state    model.State
	store    *store.Store
	// clientCN 国内版链路客户端：永久直连，不受代理设置影响
	clientCN *client.Client
	// clientIntl 国际版链路客户端：按设置走代理（可为直连），可热替换
	clientIntl    *client.Client
	logs          []model.LogEntry
	loginMu       sync.Mutex
	logins        map[string]*client.LoginSession
	loginAt       map[string]time.Time
	loginBusy     map[string]bool
	loginStarting int
	loginCommitMu sync.Mutex
	appToken      string
	stop          chan struct{}
	stopOnce      sync.Once
	schedulerOnce sync.Once
	scheduleMu    sync.Mutex
	runMu         sync.Mutex
}

func New(st *store.Store, c *client.Client) (*Service, error) {
	if c == nil {
		c = client.New()
	}
	state, err := st.Load()
	if err != nil {
		return nil, err
	}
	if state.Version == 0 {
		state.Version = model.StateVersion
	}
	// 兼容旧状态文件：补齐版本模式相关默认值
	if state.Settings.Mode != model.ModeCN && state.Settings.Mode != model.ModeIntl {
		state.Settings.Mode = model.ModeCN
	}
	if !validActiveModel(state.Settings.ActiveModel) {
		state.Settings.ActiveModel = "hy3"
	}
	if state.Settings.RecheckDelayMinutes < 60 || state.Settings.RecheckDelayMinutes > 1440 {
		// 实测积分到账延迟约 14 小时（次日凌晨发放），默认 840 分钟
		state.Settings.RecheckDelayMinutes = 840
	}
	if _, err := time.Parse("15:04", state.Settings.ScheduleTime); err != nil {
		state.Settings.ScheduleTime = "09:15"
	}
	if runes := []rune(state.Settings.ActivePrompt); len(runes) > 200 {
		state.Settings.ActivePrompt = string(runes[:200])
	}
	if state.Accounts == nil {
		state.Accounts = []model.Account{}
	}
	if state.Logs == nil {
		state.Logs = []model.LogEntry{}
	}
	for i := range state.Accounts {
		if state.Accounts[i].Version != model.ModeCN && state.Accounts[i].Version != model.ModeIntl {
			state.Accounts[i].Version = state.Accounts[i].VersionMode()
		}
		if state.Accounts[i].Status == "" {
			state.Accounts[i].Status = "ready"
		}
	}
	token, err := randomID(24)
	if err != nil {
		return nil, err
	}
	s := &Service{
		accounts:  make(map[string]*sync.Mutex),
		state:     state,
		store:     st,
		clientCN:  c,
		logs:      append([]model.LogEntry(nil), state.Logs...),
		logins:    make(map[string]*client.LoginSession),
		loginAt:   make(map[string]time.Time),
		loginBusy: make(map[string]bool),
		appToken:  token,
		stop:      make(chan struct{}),
	}
	// 固定 DNS：读 data/dns.conf（默认公共 DNS），国内版链路与解析兜底共用。
	// 容器内 Docker DNS 瘫痪时国内版仍可解析连接。
	if dnsServers, found, derr := st.LoadDNSConfWithPresence(); derr != nil {
		return nil, derr
	} else if found {
		if len(dnsServers) == 0 {
			client.SetDNSSystem()
		} else {
			client.SetDNSConfig(dnsServers)
		}
	} else {
		// 无 dns.conf：以代码内默认公共 DNS 落盘（保证容器 DNS 故障时仍可解析）
		client.SetDNSConfig(nil)
		if err := st.SaveDNSConf(client.ActiveDNS()); err != nil {
			return nil, err
		}
	}
	// 启动时初始化双客户端：国内版永远直连；国际版按代理设置。
	// 代理配置优先读 data/proxy.conf（独立明文配置，容器重建不丢），
	// 无 conf 时回退加密 state 内的设置并回写 conf 使两者对齐。
	proxyURL, foundProxy, perr := st.LoadProxyConfWithPresence()
	if perr != nil {
		return nil, perr
	}
	if !foundProxy {
		proxyURL = strings.TrimSpace(state.Settings.ProxyURL)
		if err := st.SaveProxyConf(proxyURL); err != nil {
			return nil, err
		}
	}
	proxyURL = client.NormalizeProxyURL(proxyURL)
	if err := client.ValidateProxyURL(proxyURL); err != nil {
		return nil, err
	}
	state.Settings.ProxyURL = proxyURL
	intlTransport, cerr := client.NewTransportOnly(proxyURL)
	if cerr != nil {
		return nil, cerr
	}
	s.clientIntl = client.CloneWithTransport(c, intlTransport)
	s.state.Settings.ProxyURL = proxyURL
	// 国内版链路：独立 transport（固定 DNS + 永直连），不与代理/系统 DNS 耦合；
	// 调用方定制过 transport（测试 upstream）时保留原客户端
	if cnClient := client.NewWithCNTransport(c); cnClient != nil {
		s.clientCN = cnClient
	}
	if err := st.Save(s.state); err != nil {
		return nil, err
	}
	return s, nil
}

func randomID(bytesN int) (string, error) {
	b := make([]byte, bytesN)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Service) AppToken() string { return s.appToken }

func (s *Service) Snapshot() (model.State, []model.PublicAccount) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := s.state
	state.Accounts = nil
	state.Logs = append([]model.LogEntry(nil), s.logs...)
	accounts := make([]model.PublicAccount, 0, len(s.state.Accounts))
	for _, a := range s.state.Accounts {
		accounts = append(accounts, a.Public())
	}
	return state, accounts
}

func (s *Service) Logs(limit int) []model.LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.logs) {
		limit = len(s.logs)
	}
	start := len(s.logs) - limit
	return append([]model.LogEntry(nil), s.logs[start:]...)
}

func (s *Service) appendLogLocked(level, accountID, message string) {
	message = sanitize(message)
	s.logs = append(s.logs, model.LogEntry{Time: time.Now(), Level: level, AccountID: accountID, Message: message})
	if len(s.logs) > 300 {
		s.logs = s.logs[len(s.logs)-300:]
	}
	s.state.Logs = append([]model.LogEntry(nil), s.logs...)
	log.Printf("%s account=%s %s", level, accountID, message)
}

func sanitize(message string) string {
	message = strings.ReplaceAll(message, "\n", " ")
	message = strings.ReplaceAll(message, "\r", " ")
	for _, key := range []string{"accessToken", "refreshToken", "Authorization", "X-Refresh-Token", "token"} {
		lower := strings.ToLower(message)
		idx := strings.Index(lower, strings.ToLower(key))
		if idx >= 0 {
			message = message[:idx] + key + "=[redacted]"
		}
	}
	if runes := []rune(message); len(runes) > 240 {
		message = string(runes[:240]) + "..."
	}
	return message
}

func (s *Service) saveLocked() error {
	s.state.Logs = append([]model.LogEntry(nil), s.logs...)
	if err := s.store.Save(s.state); err != nil {
		// 持久化失败必须可见：此前静默吞掉会导致"保存后刷新回退"且无任何痕迹
		log.Printf("state save failed: %v", err)
		s.logs = append(s.logs, model.LogEntry{Time: time.Now(), Level: "error", Message: "数据保存失败: " + err.Error()})
		if len(s.logs) > 300 {
			s.logs = s.logs[len(s.logs)-300:]
		}
		s.state.Logs = append([]model.LogEntry(nil), s.logs...)
		return err
	}
	return nil
}

func cloneState(state model.State) model.State {
	clone := state
	clone.Accounts = append([]model.Account(nil), state.Accounts...)
	for i := range clone.Accounts {
		clone.Accounts[i].Packages = append([]model.PointPackage(nil), state.Accounts[i].Packages...)
	}
	clone.Logs = append([]model.LogEntry(nil), state.Logs...)
	return clone
}

// saveMutationLocked persists a mutation performed while s.mu is held. When
// persistence fails, restore the previous in-memory state so a later save does
// not accidentally commit an operation that was already reported as failed.
func (s *Service) saveMutationLocked(before model.State, beforeLogs []model.LogEntry) error {
	if err := s.saveLocked(); err != nil {
		var failureLog model.LogEntry
		if len(s.logs) > 0 {
			failureLog = s.logs[len(s.logs)-1]
		}
		s.state = cloneState(before)
		s.logs = append([]model.LogEntry(nil), beforeLogs...)
		if failureLog.Message != "" {
			s.logs = append(s.logs, failureLog)
			if len(s.logs) > 300 {
				s.logs = s.logs[len(s.logs)-300:]
			}
		}
		s.state.Logs = append([]model.LogEntry(nil), s.logs...)
		return err
	}
	return nil
}

func (s *Service) getAccount(id string) (model.Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.state.Accounts {
		if a.ID == id {
			return a, nil
		}
	}
	return model.Account{}, ErrAccountNotFound
}

func (s *Service) accountLock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.accounts[id]; m != nil {
		return m
	}
	found := false
	for i := range s.state.Accounts {
		if s.state.Accounts[i].ID == id {
			found = true
			break
		}
	}
	m := &sync.Mutex{}
	if found {
		s.accounts[id] = m
	}
	return m
}

func (s *Service) updateAccount(id string, fn func(*model.Account)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Accounts {
		if s.state.Accounts[i].ID == id {
			before := cloneState(s.state)
			beforeLogs := append([]model.LogEntry(nil), s.logs...)
			fn(&s.state.Accounts[i])
			s.state.Accounts[i].UpdatedAt = time.Now()
			return s.saveMutationLocked(before, beforeLogs)
		}
	}
	return ErrAccountNotFound
}

// StartLogin 发起登录授权；version 为空按每日任务版本，cn=国内 CLI 授权，
// intl=国际版授权（authUrl 为 workbuddy.ai/login?platform=workbuddy-ai&state=...）
func (s *Service) StartLogin(version string) (id, authURL string, err error) {
	if version != model.ModeCN && version != model.ModeIntl {
		version = s.mode()
	}
	if err := s.reserveLoginStart(); err != nil {
		return "", "", err
	}
	s.mu.RLock()
	loginClient := s.clientCN
	if version == model.ModeIntl {
		loginClient = s.clientIntl
	}
	s.mu.RUnlock()
	session := client.NewLoginSessionWithClient(version, loginClient)
	if err := session.Start(); err != nil {
		s.releaseLoginStart()
		return "", "", err
	}
	id, err = randomID(12)
	if err != nil {
		s.releaseLoginStart()
		return "", "", err
	}
	s.loginMu.Lock()
	s.cleanupLoginSessionsLocked(time.Now())
	if s.loginStarting > 0 {
		s.loginStarting--
	}
	if len(s.logins) >= maxLoginSessions {
		s.loginMu.Unlock()
		return "", "", ErrLoginSessionLimit
	}
	s.logins[id] = session
	s.loginAt[id] = time.Now()
	s.loginMu.Unlock()
	return id, session.AuthURL, nil
}

func (s *Service) reserveLoginStart() error {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.cleanupLoginSessionsLocked(time.Now())
	if len(s.logins)+s.loginStarting >= maxLoginSessions {
		return ErrLoginSessionLimit
	}
	s.loginStarting++
	return nil
}

func (s *Service) releaseLoginStart() {
	s.loginMu.Lock()
	if s.loginStarting > 0 {
		s.loginStarting--
	}
	s.loginMu.Unlock()
}

func (s *Service) cleanupLoginSessionsLocked(now time.Time) {
	for key, created := range s.loginAt {
		if now.Sub(created) > loginSessionTTL && !s.loginBusy[key] {
			delete(s.loginAt, key)
			delete(s.logins, key)
			delete(s.loginBusy, key)
		}
	}
}

func (s *Service) PollLogin(id string) (model.PublicAccount, error) {
	s.loginMu.Lock()
	s.cleanupLoginSessionsLocked(time.Now())
	session := s.logins[id]
	if session == nil {
		s.loginMu.Unlock()
		return model.PublicAccount{}, ErrLoginSessionExpired
	}
	if s.loginBusy[id] {
		s.loginMu.Unlock()
		return model.PublicAccount{}, &client.Error{Kind: client.KindBusiness, Msg: "登录尚未完成：已有轮询请求正在处理"}
	}
	if created := s.loginAt[id]; created.IsZero() || time.Since(created) > loginSessionTTL {
		delete(s.logins, id)
		delete(s.loginAt, id)
		delete(s.loginBusy, id)
		s.loginMu.Unlock()
		return model.PublicAccount{}, ErrLoginSessionExpired
	}
	s.loginBusy[id] = true
	s.loginMu.Unlock()

	consumeSession := false
	defer func() {
		s.loginMu.Lock()
		delete(s.loginBusy, id)
		if consumeSession && s.logins[id] == session {
			delete(s.logins, id)
			delete(s.loginAt, id)
		}
		s.loginMu.Unlock()
	}()

	result, err := session.Poll()
	if err != nil {
		return model.PublicAccount{}, err
	}
	account, err := s.commitLoginResult(result)
	if err != nil {
		return account, err
	}
	consumeSession = true
	return account, nil
}

func (s *Service) commitLoginResult(result client.LoginResult) (model.PublicAccount, error) {
	// 不同登录会话可能同时返回同一 UID。串行提交可保证 UID 去重和
	// 50 个账号上限检查与 append 处于同一个线性化顺序。
	s.loginCommitMu.Lock()
	defer s.loginCommitMu.Unlock()

	now := time.Now()
	resultVersion := versionForDomain(result.Domain)
	for {
		s.mu.RLock()
		existingID := ""
		for i := range s.state.Accounts {
			if s.state.Accounts[i].UID == result.UID && s.state.Accounts[i].VersionMode() == resultVersion {
				existingID = s.state.Accounts[i].ID
				break
			}
		}
		s.mu.RUnlock()

		if existingID != "" {
			// 与签到、积分刷新、编辑和删除复用账号锁，避免旧请求在登录
			// 完成后把新令牌或账号状态覆盖回去。
			lock := s.accountLock(existingID)
			lock.Lock()
			s.mu.Lock()
			for i := range s.state.Accounts {
				if s.state.Accounts[i].ID != existingID || s.state.Accounts[i].UID != result.UID || s.state.Accounts[i].VersionMode() != resultVersion {
					continue
				}
				before := cloneState(s.state)
				beforeLogs := append([]model.LogEntry(nil), s.logs...)
				previous := s.state.Accounts[i].Public()
				a := &s.state.Accounts[i]
				a.EnterpriseID = result.EnterpriseID
				if result.Nickname != "" {
					a.Nickname = result.Nickname
				}
				a.Domain = result.Domain
				a.Version = resultVersion
				a.AccessToken = result.AccessToken
				a.RefreshToken = result.RefreshToken
				a.ExpiresAt = model.ResolveTokenExpiresAt(result.AccessToken, result.ExpiresIn, now)
				a.Enabled = true
				a.Status = "ready"
				a.LastError = ""
				a.UpdatedAt = now
				account := a.Public()
				s.appendLogLocked("info", a.ID, "账号登录已更新")
				err := s.saveMutationLocked(before, beforeLogs)
				s.mu.Unlock()
				lock.Unlock()
				if err != nil {
					return previous, err
				}
				return account, err
			}
			s.mu.Unlock()
			lock.Unlock()
			// 账号可能在查找和取得账号锁之间被删除，按新的当前状态重试。
			continue
		}

		accountID, err := randomID(12)
		if err != nil {
			return model.PublicAccount{}, err
		}
		a := model.Account{
			ID:           accountID,
			UID:          result.UID,
			EnterpriseID: result.EnterpriseID,
			Nickname:     result.Nickname,
			Domain:       result.Domain,
			Version:      resultVersion,
			AccessToken:  result.AccessToken,
			RefreshToken: result.RefreshToken,
			ExpiresAt:    model.ResolveTokenExpiresAt(result.AccessToken, result.ExpiresIn, now),
			Enabled:      true,
			Status:       "ready",
			CreatedAt:    now,
			UpdatedAt:    now,
		}

		s.mu.Lock()
		duplicate := false
		for i := range s.state.Accounts {
			if s.state.Accounts[i].UID == result.UID && s.state.Accounts[i].VersionMode() == resultVersion {
				duplicate = true
				break
			}
		}
		if duplicate {
			s.mu.Unlock()
			continue
		}
		if len(s.state.Accounts) >= 50 {
			s.mu.Unlock()
			return model.PublicAccount{}, fmt.Errorf("单个助手最多管理 50 个账号")
		}
		before := cloneState(s.state)
		beforeLogs := append([]model.LogEntry(nil), s.logs...)
		s.state.Accounts = append(s.state.Accounts, a)
		s.appendLogLocked("info", accountID, "账号已添加")
		err = s.saveMutationLocked(before, beforeLogs)
		s.mu.Unlock()
		if err != nil {
			return model.PublicAccount{}, err
		}
		return a.Public(), nil
	}
}

func (s *Service) UpdateAccount(id string, alias *string, enabled *bool) error {
	lock := s.accountLock(id)
	lock.Lock()
	defer lock.Unlock()
	return s.updateAccount(id, func(a *model.Account) {
		if alias != nil {
			a.Alias = strings.TrimSpace(*alias)
		}
		if enabled != nil {
			a.Enabled = *enabled
			if !*enabled {
				a.Status = "disabled"
			} else if a.Status == "disabled" {
				a.Status = "ready"
			}
		}
	})
}

func (s *Service) DeleteAccount(id string) error {
	lock := s.accountLock(id)
	lock.Lock()
	defer lock.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Accounts {
		if s.state.Accounts[i].ID == id {
			before := cloneState(s.state)
			beforeLogs := append([]model.LogEntry(nil), s.logs...)
			s.state.Accounts = append(s.state.Accounts[:i], s.state.Accounts[i+1:]...)
			s.appendLogLocked("info", id, "账号已删除；浏览器/本地会话未被导出")
			err := s.saveMutationLocked(before, beforeLogs)
			if err == nil {
				delete(s.accounts, id)
			}
			return err
		}
	}
	return ErrAccountNotFound
}

func (s *Service) UpdateSettings(patch model.SettingsPatch) (model.Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.state.Settings
	if patch.ScheduleEnabled != nil {
		next.ScheduleEnabled = *patch.ScheduleEnabled
	}
	if patch.ScheduleTime != nil {
		next.ScheduleTime = strings.TrimSpace(*patch.ScheduleTime)
	}
	if _, err := time.Parse("15:04", next.ScheduleTime); err != nil {
		return model.Settings{}, settingsValidation("scheduleTime 必须是 HH:MM")
	}
	if patch.Mode != nil {
		next.Mode = strings.TrimSpace(*patch.Mode)
	}
	if next.Mode != model.ModeCN && next.Mode != model.ModeIntl {
		return model.Settings{}, settingsValidation("mode 必须是 cn（国内版）或 intl（国际版）")
	}
	if patch.ActiveModel != nil {
		next.ActiveModel = strings.TrimSpace(*patch.ActiveModel)
	}
	if !validActiveModel(next.ActiveModel) {
		return model.Settings{}, settingsValidation("activeModel 仅支持 hy3 / hy4-preview")
	}
	if patch.RecheckDelayMinutes != nil {
		next.RecheckDelayMinutes = *patch.RecheckDelayMinutes
	}
	if next.RecheckDelayMinutes < 60 || next.RecheckDelayMinutes > 1440 {
		return model.Settings{}, settingsValidation("recheckDelayMinutes 需在 60-1440 之间（实测到账延迟约 14 小时）")
	}
	if patch.ProxyURL != nil {
		next.ProxyURL = client.NormalizeProxyURL(*patch.ProxyURL)
	}
	if err := client.ValidateProxyURL(next.ProxyURL); err != nil {
		return model.Settings{}, settingsValidation(err.Error())
	}
	if patch.ActivePrompt != nil {
		next.ActivePrompt = strings.TrimSpace(*patch.ActivePrompt)
	}
	if len([]rune(next.ActivePrompt)) > 200 {
		return model.Settings{}, settingsValidation("activePrompt 最多 200 个字符")
	}

	oldSettings := s.state.Settings
	oldClient := s.clientIntl
	newClient := oldClient
	if next.ProxyURL != oldSettings.ProxyURL {
		transport, err := client.NewTransportOnly(next.ProxyURL)
		if err != nil {
			return model.Settings{}, settingsValidation(err.Error())
		}
		newClient = client.CloneWithTransport(oldClient, transport)
	}
	newClientPending := newClient != oldClient
	if newClientPending {
		defer func() {
			if newClientPending {
				newClient.CloseIdleConnections()
			}
		}()
	}

	// proxy.conf 是容器启动时的代理真源，先写它，再原子保存候选 state；
	// 任一步失败都不发布新的内存状态，并尽力恢复旧代理配置。
	if err := s.store.SaveProxyConf(next.ProxyURL); err != nil {
		return model.Settings{}, fmt.Errorf("保存代理配置: %w", err)
	}
	candidate := s.state
	candidate.Settings = next
	candidate.Logs = append([]model.LogEntry(nil), s.logs...)
	if err := s.store.Save(candidate); err != nil {
		if rollbackErr := s.store.SaveProxyConf(oldSettings.ProxyURL); rollbackErr != nil {
			return model.Settings{}, fmt.Errorf("保存设置: %v；恢复旧代理配置失败: %w", err, rollbackErr)
		}
		return model.Settings{}, err
	}
	s.state.Settings = next
	s.clientIntl = newClient
	newClientPending = false
	if oldClient != nil && oldClient != newClient {
		oldClient.CloseIdleConnections()
	}
	return next, nil
}

// activeModels 活跃对话可选模型（与前端下拉一致）
var activeModels = map[string]bool{
	"hy3":         true,
	"hy4-preview": true,
}

func validActiveModel(name string) bool {
	return activeModels[name]
}

// mode 返回当前版本模式：cn（每日签到）或 intl（活跃对话获取积分）
// apiclient 按账号域名返回对应客户端：国内版域名永远直连（clientCN），
// 国际版域名走 clientIntl（代理可热更新）。
func (s *Service) apiclientFor(a *model.Account) *client.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a != nil && a.VersionMode() == model.ModeIntl {
		return s.clientIntl
	}
	return s.clientCN
}

func (s *Service) mode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state.Settings.Mode == model.ModeIntl {
		return model.ModeIntl
	}
	return model.ModeCN
}

// accountVersion 返回账号归属版本：优先 Version 字段，缺省按 domain 推断（兼容旧数据）
func (s *Service) accountVersion(a model.Account) string {
	return a.VersionMode()
}

func (s *Service) activeConfig() (modelName, prompt string, delay time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	modelName = strings.TrimSpace(s.state.Settings.ActiveModel)
	if modelName == "" {
		modelName = "hy3"
	}
	prompt = strings.TrimSpace(s.state.Settings.ActivePrompt)
	minutes := s.state.Settings.RecheckDelayMinutes
	if minutes < 60 || minutes > 1440 {
		minutes = 840
	}
	return modelName, prompt, time.Duration(minutes) * time.Minute
}

func (s *Service) recheckDelay() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	minutes := s.state.Settings.RecheckDelayMinutes
	if minutes < 60 || minutes > 1440 {
		minutes = 840
	}
	return time.Duration(minutes) * time.Minute
}

func (s *Service) shouldRefresh(a model.Account) bool {
	if a.AccessToken == "" {
		return true
	}
	return a.ExpiresAt == 0 || time.Now().Add(30*time.Minute).Unix() >= a.ExpiresAt
}

func (s *Service) prepareAccount(id string) (model.Account, error) {
	a, err := s.getAccount(id)
	if err != nil {
		return a, err
	}
	if !a.Enabled {
		return a, fmt.Errorf("账号已停用")
	}
	if s.shouldRefresh(a) {
		if err := s.refreshAccount(id, &a); err != nil {
			if latest, getErr := s.getAccount(id); getErr == nil {
				return latest, err
			}
			return a, err
		}
	}
	return a, nil
}

func (s *Service) refreshAccount(id string, a *model.Account) error {
	if err := s.apiclientFor(a).Refresh(a); err != nil {
		s.markError(id, err)
		return err
	}
	if err := s.updateAccount(id, func(dst *model.Account) {
		dst.AccessToken = a.AccessToken
		dst.RefreshToken = a.RefreshToken
		dst.ExpiresAt = a.ExpiresAt
		dst.Domain = a.Domain
		dst.Status = "ready"
		dst.LastError = ""
	}); err != nil {
		return err
	}
	return nil
}

func (s *Service) markError(id string, err error) {
	status := "error"
	switch {
	case client.IsKind(err, client.KindAuthDead):
		status = "needs_login"
	case client.IsKind(err, client.KindCredit):
		status = "credits_exhausted"
	case client.IsKind(err, client.KindRate):
		status = "rate_limited"
	}
	s.mu.Lock()
	before := cloneState(s.state)
	beforeLogs := append([]model.LogEntry(nil), s.logs...)
	for i := range s.state.Accounts {
		if s.state.Accounts[i].ID == id {
			s.state.Accounts[i].Status = status
			s.state.Accounts[i].LastError = sanitize(err.Error())
			break
		}
	}
	s.appendLogLocked("error", id, err.Error())
	_ = s.saveMutationLocked(before, beforeLogs)
	s.mu.Unlock()
}

func (s *Service) markStatus(id, status, message string) {
	s.mu.Lock()
	before := cloneState(s.state)
	beforeLogs := append([]model.LogEntry(nil), s.logs...)
	for i := range s.state.Accounts {
		if s.state.Accounts[i].ID == id {
			s.state.Accounts[i].Status = status
			s.state.Accounts[i].LastError = ""
			break
		}
	}
	s.appendLogLocked("info", id, message)
	_ = s.saveMutationLocked(before, beforeLogs)
	s.mu.Unlock()
}

func today() string { return time.Now().In(locChina).Format("2006-01-02") }

func isAlreadyMessage(err error) bool {
	if err == nil {
		return false
	}
	var ce *client.Error
	if errors.As(err, &ce) {
		if ce.Code == 14001 || ce.Code == 10001 {
			return true
		}
		err = ce
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "已签到") || strings.Contains(lower, "already") || strings.Contains(lower, "今日已")
}

func (s *Service) Checkin(id string) (model.PublicAccount, error) {
	lock := s.accountLock(id)
	lock.Lock()
	defer lock.Unlock()
	if account, lookupErr := s.getAccount(id); lookupErr != nil {
		return account.Public(), lookupErr
	} else if account.VersionMode() != model.ModeCN {
		return account.Public(), fmt.Errorf("%w：国际版账号只能执行活跃会话", ErrAccountModeMismatch)
	}
	a, err := s.prepareAccount(id)
	if err != nil {
		return a.Public(), err
	}
	if a.LastCheckinDate == today() && (a.LastCheckin == "confirmed" || a.LastCheckin == "already_done") {
		s.markStatus(id, a.LastCheckin, "今日已完成，跳过重复签到")
		_ = s.refreshPoints(id, a)
		updated, _ := s.getAccount(id)
		return updated.Public(), nil
	}
	s.markStatus(id, "running", "正在签到")
	err = s.apiclientFor(&a).DailyCheckin(&a)
	if client.IsKind(err, client.KindAuthDead) {
		if refreshErr := s.refreshAccount(id, &a); refreshErr == nil {
			err = s.apiclientFor(&a).DailyCheckin(&a)
		} else {
			return a.Public(), refreshErr
		}
	}
	status := "confirmed"
	message := "签到成功"
	if err != nil {
		if isAlreadyMessage(err) {
			status = "already_done"
			message = "今日已签到"
		} else {
			s.markError(id, err)
			return a.Public(), err
		}
	}
	now := time.Now()
	if err := s.updateAccount(id, func(dst *model.Account) {
		dst.LastCheckinAt = now
		dst.LastCheckinDate = today()
		dst.LastCheckin = status
		dst.LastCheckinMsg = message
		dst.Status = status
		dst.LastError = ""
	}); err != nil {
		return a.Public(), err
	}
	s.appendLog("info", id, message)
	_ = s.refreshPoints(id, a)
	updated, _ := s.getAccount(id)
	return updated.Public(), nil
}

// versionForDomain 按登录域判定账号归属版本列表
func versionForDomain(domain string) string {
	if model.IsGlobalDomain(domain) {
		return model.ModeIntl
	}
	return model.ModeCN
}

// ActiveChat 国际版活跃流程：以客户端特征发送一次对话（hy3/hy4-preview），
// 使账号被认定为当日活跃；积分非即时到账，按设置延迟排期复核并记录到账变化。
// force=true 跳过"今日已活跃"幂等（手动重发，用于验证请求链路）。
func (s *Service) ActiveChat(id string, force bool) (model.PublicAccount, error) {
	lock := s.accountLock(id)
	lock.Lock()
	defer lock.Unlock()
	if account, lookupErr := s.getAccount(id); lookupErr != nil {
		return account.Public(), lookupErr
	} else if account.VersionMode() != model.ModeIntl {
		return account.Public(), fmt.Errorf("%w：国内版账号只能执行每日签到", ErrAccountModeMismatch)
	}
	a, err := s.prepareAccount(id)
	if err != nil {
		return a.Public(), err
	}
	if !force && a.LastActiveDate == today() &&
		(a.LastActive == "confirmed" || a.LastActive == "credited") {
		s.markStatus(id, a.LastActive, "今日已发送活跃会话，跳过重复对话")
		_ = s.refreshPoints(id, a)
		updated, _ := s.getAccount(id)
		return updated.Public(), nil
	}
	activeModel, activePrompt, delay := s.activeConfig()
	// A working token and billing session are prerequisites, not an optional
	// baseline. Do not claim activity when this account cannot query its points.
	if err := s.refreshPoints(id, a); err != nil {
		if !client.IsKind(err, client.KindAuthDead) {
			updated, _ := s.getAccount(id)
			return updated.Public(), err
		}
		if refreshErr := s.refreshAccount(id, &a); refreshErr != nil {
			updated, _ := s.getAccount(id)
			return updated.Public(), refreshErr
		}
		if err = s.refreshPoints(id, a); err != nil {
			updated, _ := s.getAccount(id)
			return updated.Public(), err
		}
	}
	s.markStatus(id, "running", "正在发送活跃会话")
	result, err := s.apiclientFor(&a).ActiveChatWithResult(&a, activeModel, activePrompt)
	if client.IsKind(err, client.KindAuthDead) {
		if refreshErr := s.refreshAccount(id, &a); refreshErr == nil {
			result, err = s.apiclientFor(&a).ActiveChatWithResult(&a, activeModel, activePrompt)
		} else {
			updated, _ := s.getAccount(id)
			return updated.Public(), refreshErr
		}
	}
	if err != nil {
		s.markError(id, err)
		updated, _ := s.getAccount(id)
		return updated.Public(), err
	}
	now := time.Now()
	if err := s.updateAccount(id, func(dst *model.Account) {
		dst.LastActiveAt = now
		dst.LastActiveDate = today()
		dst.LastActive = "confirmed"
		dst.LastActiveMsg = "会话已发送，待积分复核"
		dst.Status = "ready"
		dst.LastError = ""
		dst.BalanceBeforeActive = dst.Balance
		dst.PendingRecheckAt = now.Add(delay)
		dst.RecheckAttempts = 0
	}); err != nil {
		return a.Public(), err
	}
	if result.ReportWarning != nil {
		s.appendLog("warn", id, "活跃对话已返回有效回答，但备用遥测上报失败："+sanitize(result.ReportWarning.Error()))
	}
	if result.ConversationWarning != nil {
		// 会话登记失败不阻断对话，但真实客户端一定会先登记会话；
		// 缺失时活跃可能不被服务端认定，需要明确告警。
		s.appendLog("warn", id, "云端会话登记失败（活跃可能不被认定）："+sanitize(result.ConversationWarning.Error()))
	}
	promptDesc := activePrompt
	if promptDesc == "" {
		promptDesc = client.DefaultActivePrompt
	}
	if r := []rune(promptDesc); len(r) > 18 {
		promptDesc = string(r[:18]) + "…"
	}
	s.appendLog("info", id, fmt.Sprintf("活跃会话已发送（模型 %s，提示词「%s」），%d 分钟后自动复核积分到账", activeModel, promptDesc, int(delay.Minutes())))
	updated, _ := s.getAccount(id)
	return updated.Public(), nil
}

func (s *Service) refreshPoints(id string, a model.Account) error {
	result, err := s.apiclientFor(&a).Points(&a)
	if err != nil {
		s.markError(id, err)
		return err
	}
	now := time.Now()
	credited := false
	var before int64
	err = s.updateAccount(id, func(dst *model.Account) {
		dst.Balance = result.Balance
		dst.Used = result.Used
		dst.Total = result.Total
		dst.Packages = result.Packages
		dst.LastPointsAt = now
		dst.LastError = ""
		if dst.Status == "running" || dst.Status == "error" || dst.Status == "rate_limited" {
			dst.Status = "ready"
		}
		if !dst.PendingRecheckAt.IsZero() &&
			result.Balance > dst.BalanceBeforeActive {
			before = dst.BalanceBeforeActive
			dst.PendingRecheckAt = time.Time{}
			dst.RecheckAttempts = 0
			dst.BalanceBeforeActive = 0
			dst.LastActive = "credited"
			dst.LastActiveMsg = fmt.Sprintf("积分已到账（+%d）", result.Balance-before)
			credited = true
		}
	})
	if err == nil && credited {
		s.appendLog("info", id, fmt.Sprintf("积分已到账：%d → %d（+%d）", before, result.Balance, result.Balance-before))
	}
	return err
}

func (s *Service) Points(id string) (model.PublicAccount, error) {
	lock := s.accountLock(id)
	lock.Lock()
	defer lock.Unlock()
	a, err := s.prepareAccount(id)
	if err != nil {
		return a.Public(), err
	}
	if err := s.refreshPoints(id, a); err != nil {
		if client.IsKind(err, client.KindAuthDead) {
			if refreshErr := s.refreshAccount(id, &a); refreshErr != nil {
				return a.Public(), refreshErr
			}
			if retryErr := s.refreshPoints(id, a); retryErr != nil {
				return a.Public(), retryErr
			}
		} else {
			return a.Public(), err
		}
	}
	updated, _ := s.getAccount(id)
	return updated.Public(), nil
}

// RunAll 对指定版本的启用账号批量执行：cn=全部签到，intl=全部活跃。
// version 必须为 cn/intl（前端按当前查看的版本列表传入）；
// 非法值回退每日任务版本（兼容旧调用方）。
func (s *Service) RunAll(version string) []model.PublicAccount {
	// Manual clicks and the scheduler share the same account set. Serialize
	// whole-batch runs so a second request cannot overlap the first one and
	// trigger duplicate upstream check-ins/active sessions.
	s.runMu.Lock()
	defer s.runMu.Unlock()

	if version != model.ModeCN && version != model.ModeIntl {
		version = s.mode()
	}
	mode := version
	s.mu.RLock()
	ids := make([]string, 0, len(s.state.Accounts))
	for _, a := range s.state.Accounts {
		// 版本切换 = 切换账号列表：只对指定版本列表执行
		if a.Enabled && s.accountVersion(a) == mode {
			ids = append(ids, a.ID)
		}
	}
	s.mu.RUnlock()
	results := make([]model.PublicAccount, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			time.Sleep(1200 * time.Millisecond)
		}
		var account model.PublicAccount
		var err error
		if mode == model.ModeIntl {
			account, err = s.ActiveChat(id, false)
		} else {
			account, err = s.Checkin(id)
		}
		if err != nil {
			if latest, getErr := s.getAccount(id); getErr == nil {
				account = latest.Public()
			}
		}
		results = append(results, account)
	}
	return results
}

// RefreshAllPoints 一键刷新已启用账户的积分（不执行签到/活跃）。
// version 必须为 cn 或 intl：严格只刷新该版本账号列表（与前端当前查看版本一致），
// 绝不波及另一版本；非法/缺失值按 cn 处理（不会跨版本刷新）。
func (s *Service) RefreshAllPoints(version string) []model.PublicAccount {
	if version != model.ModeIntl {
		version = model.ModeCN
	}
	s.mu.RLock()
	ids := make([]string, 0, len(s.state.Accounts))
	for _, a := range s.state.Accounts {
		if a.Enabled && s.accountVersion(a) == version {
			ids = append(ids, a.ID)
		}
	}
	s.mu.RUnlock()
	results := make([]model.PublicAccount, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		account, err := s.Points(id)
		if err != nil {
			if latest, getErr := s.getAccount(id); getErr == nil {
				account = latest.Public()
			}
		}
		results = append(results, account)
	}
	return results
}

func (s *Service) StartScheduler() {
	s.schedulerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(20 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					s.runScheduledIfDue()
					s.runPendingRechecks()
				case <-s.stop:
					return
				}
			}
		}()
	})
}

func (s *Service) StopScheduler() { s.stopOnce.Do(func() { close(s.stop) }) }

func (s *Service) runScheduledIfDue() {
	if !s.scheduleMu.TryLock() {
		return
	}
	defer s.scheduleMu.Unlock()
	s.mu.RLock()
	settings := s.state.Settings
	s.mu.RUnlock()
	if !settings.ScheduleEnabled {
		return
	}
	wantClock, err := time.ParseInLocation("15:04", settings.ScheduleTime, locChina)
	if err != nil {
		return
	}
	now := time.Now().In(locChina)
	want := time.Date(now.Year(), now.Month(), now.Day(), wantClock.Hour(), wantClock.Minute(), 0, 0, locChina)
	if now.Before(want) || now.After(want.Add(30*time.Minute)) {
		return
	}
	day := now.Format("2006-01-02")
	if settings.LastScheduledDay == day {
		// LastScheduledDay is updated in memory before its persistence result is
		// known.  If the state write failed, do not run the remote task again;
		// verify the on-disk marker and retry only the save on a later tick.
		persisted, loadErr := s.store.Load()
		if loadErr != nil || persisted.Settings.LastScheduledDay != day {
			s.mu.Lock()
			// Re-check under the write lock because an account/settings mutation
			// may have saved a newer state while the disk read was in flight.
			if s.state.Settings.LastScheduledDay == day {
				_ = s.saveLocked()
			}
			s.mu.Unlock()
		}
		return
	}
	taskMode := s.mode()
	if taskMode == model.ModeIntl {
		s.appendLog("info", "", "开始执行每日批量活跃任务")
	} else {
		s.appendLog("info", "", "开始执行每日批量签到和积分刷新")
	}
	s.RunAll(taskMode)
	s.mu.Lock()
	s.state.Settings.LastScheduledDay = day
	_ = s.saveLocked()
	s.mu.Unlock()
}

// runPendingRechecks 执行到期的积分到账复核：积分非即时到账，
// 活跃后按延迟复查；无变化时最多复核 6 次，避免无限轮询。
func (s *Service) runPendingRechecks() {
	now := time.Now()
	var dueIDs []string
	s.mu.RLock()
	for _, a := range s.state.Accounts {
		if a.Enabled && !a.PendingRecheckAt.IsZero() && !a.PendingRecheckAt.After(now) {
			dueIDs = append(dueIDs, a.ID)
		}
	}
	s.mu.RUnlock()
	for _, id := range dueIDs {
		lock := s.accountLock(id)
		if !lock.TryLock() {
			continue
		}
		s.recheckAccount(id)
		lock.Unlock()
	}
}

func (s *Service) recheckAccount(id string) {
	a, err := s.getAccount(id)
	if err != nil || a.PendingRecheckAt.IsZero() || a.PendingRecheckAt.After(time.Now()) {
		return
	}
	// delay 须在 updateAccount（持有写锁）之外取好，回调内不能再调 s.recheckDelay()
	delay := s.recheckDelay()
	if refreshErr := s.refreshPoints(id, a); refreshErr != nil {
		// 与手动积分查询保持一致：积分接口返回会话失效时，先刷新令牌，
		// 再重试一次查询。否则失效账号会永远按延迟重排，既无法到账复核，
		// 也不会给用户一个可恢复的状态。
		if client.IsKind(refreshErr, client.KindAuthDead) {
			if tokenErr := s.refreshAccount(id, &a); tokenErr == nil {
				refreshErr = s.refreshPoints(id, a)
			} else {
				refreshErr = tokenErr
			}
		}
		if refreshErr != nil {
			// 查询失败顺延到下一个周期再试
			_ = s.updateAccount(id, func(dst *model.Account) {
				dst.PendingRecheckAt = time.Now().Add(delay)
			})
			return
		}
	}
	latest, err := s.getAccount(id)
	if err != nil {
		return
	}
	if latest.PendingRecheckAt.IsZero() {
		return
	}
	attempts := latest.RecheckAttempts + 1
	if attempts >= 6 {
		if err := s.updateAccount(id, func(dst *model.Account) {
			dst.PendingRecheckAt = time.Time{}
			dst.RecheckAttempts = attempts
			dst.BalanceBeforeActive = 0
			dst.LastActiveMsg = "积分复核未见到账，可稍后手动刷新"
		}); err != nil {
			return
		}
		s.appendLog("info", id, fmt.Sprintf("积分复核 %d 次暂无变化（余额 %d），停止自动复核，可稍后手动刷新", attempts, latest.Balance))
		return
	}
	if err := s.updateAccount(id, func(dst *model.Account) {
		dst.RecheckAttempts = attempts
		dst.PendingRecheckAt = time.Now().Add(delay)
	}); err != nil {
		return
	}
	s.appendLog("info", id, fmt.Sprintf("积分暂未到账（余额 %d），已排期第 %d 次复核", latest.Balance, attempts+1))
}

func (s *Service) appendLog(level, accountID, message string) {
	s.mu.Lock()
	before := cloneState(s.state)
	beforeLogs := append([]model.LogEntry(nil), s.logs...)
	s.appendLogLocked(level, accountID, message)
	_ = s.saveMutationLocked(before, beforeLogs)
	s.mu.Unlock()
}

func (s *Service) AccountByID(id string) (model.PublicAccount, error) {
	a, err := s.getAccount(id)
	if err != nil {
		return model.PublicAccount{}, err
	}
	return a.Public(), nil
}

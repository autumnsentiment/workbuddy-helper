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

type Service struct {
	mu         sync.RWMutex
	accounts   map[string]*sync.Mutex
	state      model.State
	store      *store.Store
	client     *client.Client
	logs       []model.LogEntry
	loginMu    sync.Mutex
	logins     map[string]*client.LoginSession
	loginAt    map[string]time.Time
	appToken   string
	stop       chan struct{}
	stopOnce   sync.Once
	scheduleMu sync.Mutex
}

func New(st *store.Store, c *client.Client) (*Service, error) {
	state, err := st.Load()
	if err != nil {
		return nil, err
	}
	if state.Version == 0 {
		state.Version = model.StateVersion
	}
	if state.Accounts == nil {
		state.Accounts = []model.Account{}
	}
	if state.Logs == nil {
		state.Logs = []model.LogEntry{}
	}
	for i := range state.Accounts {
		if state.Accounts[i].Status == "" {
			state.Accounts[i].Status = "ready"
		}
	}
	token, err := randomID(24)
	if err != nil {
		return nil, err
	}
	s := &Service{
		accounts: make(map[string]*sync.Mutex),
		state:    state,
		store:    st,
		client:   c,
		logs:     append([]model.LogEntry(nil), state.Logs...),
		logins:   make(map[string]*client.LoginSession),
		loginAt:  make(map[string]time.Time),
		appToken: token,
		stop:     make(chan struct{}),
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
	if len(message) > 240 {
		message = message[:240] + "..."
	}
	return message
}

func (s *Service) saveLocked() error {
	s.state.Logs = append([]model.LogEntry(nil), s.logs...)
	return s.store.Save(s.state)
}

func (s *Service) getAccount(id string) (model.Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.state.Accounts {
		if a.ID == id {
			return a, nil
		}
	}
	return model.Account{}, fmt.Errorf("account not found")
}

func (s *Service) accountLock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.accounts[id]; m != nil {
		return m
	}
	m := &sync.Mutex{}
	s.accounts[id] = m
	return m
}

func (s *Service) updateAccount(id string, fn func(*model.Account)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Accounts {
		if s.state.Accounts[i].ID == id {
			fn(&s.state.Accounts[i])
			s.state.Accounts[i].UpdatedAt = time.Now()
			return s.saveLocked()
		}
	}
	return fmt.Errorf("account not found")
}

func (s *Service) StartLogin() (id, authURL string, err error) {
	session := client.NewLoginSession()
	if err := session.Start(); err != nil {
		return "", "", err
	}
	id, err = randomID(12)
	if err != nil {
		return "", "", err
	}
	s.loginMu.Lock()
	for key, created := range s.loginAt {
		if time.Since(created) > 15*time.Minute {
			delete(s.loginAt, key)
			delete(s.logins, key)
		}
	}
	s.logins[id] = session
	s.loginAt[id] = time.Now()
	s.loginMu.Unlock()
	return id, session.AuthURL, nil
}

func (s *Service) PollLogin(id string) (model.PublicAccount, error) {
	s.loginMu.Lock()
	session := s.logins[id]
	s.loginMu.Unlock()
	if session == nil {
		return model.PublicAccount{}, fmt.Errorf("登录会话不存在或已过期")
	}
	result, err := session.Poll()
	if err != nil {
		return model.PublicAccount{}, err
	}
	s.loginMu.Lock()
	delete(s.logins, id)
	delete(s.loginAt, id)
	s.loginMu.Unlock()
	now := time.Now()
	s.mu.Lock()
	for i := range s.state.Accounts {
		if s.state.Accounts[i].UID != result.UID {
			continue
		}
		a := &s.state.Accounts[i]
		a.EnterpriseID = result.EnterpriseID
		if result.Nickname != "" {
			a.Nickname = result.Nickname
		}
		a.Domain = result.Domain
		a.AccessToken = result.AccessToken
		a.RefreshToken = result.RefreshToken
		a.ExpiresAt = now.Add(time.Duration(result.ExpiresIn) * time.Second).Unix()
		a.Enabled = true
		a.Status = "ready"
		a.LastError = ""
		a.UpdatedAt = now
		account := a.Public()
		s.appendLogLocked("info", a.ID, "账号登录已更新")
		err = s.saveLocked()
		s.mu.Unlock()
		return account, err
	}
	if len(s.state.Accounts) >= 50 {
		s.mu.Unlock()
		return model.PublicAccount{}, fmt.Errorf("单个助手最多管理 50 个账号")
	}
	s.mu.Unlock()

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
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    now.Add(time.Duration(result.ExpiresIn) * time.Second).Unix(),
		Enabled:      true,
		Status:       "ready",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	s.mu.Lock()
	s.state.Accounts = append(s.state.Accounts, a)
	s.appendLogLocked("info", accountID, "账号已添加")
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return model.PublicAccount{}, err
	}
	return a.Public(), nil
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
			s.state.Accounts = append(s.state.Accounts[:i], s.state.Accounts[i+1:]...)
			delete(s.accounts, id)
			s.appendLogLocked("info", id, "账号已删除；浏览器/本地会话未被导出")
			return s.saveLocked()
		}
	}
	return fmt.Errorf("account not found")
}

func (s *Service) UpdateSettings(settings model.Settings) error {
	if _, err := time.Parse("15:04", settings.ScheduleTime); err != nil {
		return fmt.Errorf("scheduleTime 必须是 HH:MM")
	}
	s.mu.Lock()
	s.state.Settings.ScheduleEnabled = settings.ScheduleEnabled
	s.state.Settings.ScheduleTime = settings.ScheduleTime
	err := s.saveLocked()
	s.mu.Unlock()
	return err
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
			return a, err
		}
	}
	return a, nil
}

func (s *Service) refreshAccount(id string, a *model.Account) error {
	if err := s.client.Refresh(a); err != nil {
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
	case client.IsKind(err, client.KindRate):
		status = "rate_limited"
	}
	s.mu.Lock()
	for i := range s.state.Accounts {
		if s.state.Accounts[i].ID == id {
			s.state.Accounts[i].Status = status
			s.state.Accounts[i].LastError = sanitize(err.Error())
			break
		}
	}
	s.appendLogLocked("error", id, err.Error())
	_ = s.saveLocked()
	s.mu.Unlock()
}

func (s *Service) markStatus(id, status, message string) {
	s.mu.Lock()
	for i := range s.state.Accounts {
		if s.state.Accounts[i].ID == id {
			s.state.Accounts[i].Status = status
			s.state.Accounts[i].LastError = ""
			break
		}
	}
	s.appendLogLocked("info", id, message)
	_ = s.saveLocked()
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
	err = s.client.DailyCheckin(&a)
	if client.IsKind(err, client.KindAuthDead) {
		if refreshErr := s.refreshAccount(id, &a); refreshErr == nil {
			err = s.client.DailyCheckin(&a)
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
	s.mu.Lock()
	s.appendLogLocked("info", id, message)
	_ = s.saveLocked()
	s.mu.Unlock()
	_ = s.refreshPoints(id, a)
	updated, _ := s.getAccount(id)
	return updated.Public(), nil
}

func (s *Service) refreshPoints(id string, a model.Account) error {
	result, err := s.client.Points(&a)
	if err != nil {
		s.markError(id, err)
		return err
	}
	now := time.Now()
	return s.updateAccount(id, func(dst *model.Account) {
		dst.Balance = result.Balance
		dst.Used = result.Used
		dst.Total = result.Total
		dst.Packages = result.Packages
		dst.LastPointsAt = now
		dst.LastError = ""
		if dst.Status == "running" || dst.Status == "error" || dst.Status == "rate_limited" {
			dst.Status = "ready"
		}
	})
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

func (s *Service) RunAll() []model.PublicAccount {
	s.mu.RLock()
	ids := make([]string, 0, len(s.state.Accounts))
	for _, a := range s.state.Accounts {
		if a.Enabled {
			ids = append(ids, a.ID)
		}
	}
	s.mu.RUnlock()
	results := make([]model.PublicAccount, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			time.Sleep(1200 * time.Millisecond)
		}
		account, err := s.Checkin(id)
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
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.runScheduledIfDue()
			case <-s.stop:
				return
			}
		}
	}()
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
		return
	}
	s.appendLog("info", "", "开始执行每日批量签到和积分刷新")
	s.RunAll()
	s.mu.Lock()
	s.state.Settings.LastScheduledDay = day
	_ = s.saveLocked()
	s.mu.Unlock()
}

func (s *Service) appendLog(level, accountID, message string) {
	s.mu.Lock()
	s.appendLogLocked(level, accountID, message)
	_ = s.saveLocked()
	s.mu.Unlock()
}

func (s *Service) AccountByID(id string) (model.PublicAccount, error) {
	a, err := s.getAccount(id)
	if err != nil {
		return model.PublicAccount{}, err
	}
	return a.Public(), nil
}

package service

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy-helper/internal/client"
	"workbuddy-helper/internal/model"
	"workbuddy-helper/internal/store"
)

func newAccountService(t *testing.T, handler http.HandlerFunc) (*Service, func()) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	state := model.NewState()
	state.Accounts = []model.Account{{
		ID:           "a1",
		UID:          "u1",
		Version:      model.ModeCN,
		Nickname:     "测试账户",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		Enabled:      true,
		Status:       "ready",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}}
	st := store.New(t.TempDir())
	if err := st.Save(state); err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	c := client.New()
	c.HTTP = upstream.Client()
	c.ChatBaseCN = upstream.URL
	c.BillingBaseCN = upstream.URL
	c.GlobalBase = upstream.URL
	c.GlobalBilling = upstream.URL
	svc, err := New(st, c)
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	return svc, upstream.Close
}

func fullSettingsPatch(settings model.Settings) model.SettingsPatch {
	return model.SettingsPatch{
		ScheduleEnabled:     &settings.ScheduleEnabled,
		ScheduleTime:        &settings.ScheduleTime,
		Mode:                &settings.Mode,
		ActiveModel:         &settings.ActiveModel,
		RecheckDelayMinutes: &settings.RecheckDelayMinutes,
		ProxyURL:            &settings.ProxyURL,
		ActivePrompt:        &settings.ActivePrompt,
	}
}

func writeEnvelope(w http.ResponseWriter, status, code int, msg, data string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data == "" {
		data = "{}"
	}
	_, _ = fmt.Fprintf(w, `{"code":%d,"msg":%q,"data":%s}`, code, msg, data)
}

const pointsDataInitial = `{"Response":{"Data":{"Accounts":[{"PackageName":"体验包","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}]}}}`
const pointsDataCredited = `{"Response":{"Data":{"Accounts":[{"PackageName":"体验包","CycleCapacitySize":130,"CycleCapacityRemain":110,"CycleCapacityUsed":20}]}}}`

func TestCheckinIsIdempotentForBeijingDay(t *testing.T) {
	checkins, pointQueries := 0, 0
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			checkins++
			writeEnvelope(w, http.StatusOK, 0, "ok", `{}`)
		case "/v2/billing/meter/get-user-resource":
			pointQueries++
			writeEnvelope(w, http.StatusOK, 0, "ok", pointsDataInitial)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()

	first, err := svc.Checkin("a1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Checkin("a1")
	if err != nil {
		t.Fatal(err)
	}
	if checkins != 1 || pointQueries != 2 {
		t.Fatalf("checkins=%d pointQueries=%d", checkins, pointQueries)
	}
	if first.LastCheckin != "confirmed" || second.LastCheckinDate != today() || second.Balance != 80 {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func TestAlreadyCheckedInIsSuccessful(t *testing.T) {
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			writeEnvelope(w, http.StatusBadRequest, 10001, "今天已签到", `{}`)
		case "/v2/billing/meter/get-user-resource":
			writeEnvelope(w, http.StatusOK, 0, "ok", pointsDataInitial)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()

	account, err := svc.Checkin("a1")
	if err != nil {
		t.Fatal(err)
	}
	if account.LastCheckin != "already_done" || account.Balance != 80 {
		t.Fatalf("unexpected account: %+v", account)
	}
}

func TestCheckinRefreshesOnceAfterUnauthorized(t *testing.T) {
	checkins, refreshes := 0, 0
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			checkins++
			if checkins == 1 {
				writeEnvelope(w, http.StatusUnauthorized, 12153, "Offline user session not found", `{}`)
				return
			}
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Errorf("retry used wrong authorization header: %q", r.Header.Get("Authorization"))
			}
			writeEnvelope(w, http.StatusOK, 0, "ok", `{}`)
		case "/v2/plugin/auth/token/refresh":
			refreshes++
			if r.Header.Get("X-Refresh-Token") != "old-refresh" || r.Header.Get("X-Auth-Refresh-Source") != "workbuddy" {
				t.Errorf("refresh headers are incomplete")
			}
			writeEnvelope(w, http.StatusOK, 0, "ok", `{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}`)
		case "/v2/billing/meter/get-user-resource":
			writeEnvelope(w, http.StatusOK, 0, "ok", pointsDataInitial)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()

	account, err := svc.Checkin("a1")
	if err != nil {
		t.Fatal(err)
	}
	if checkins != 2 || refreshes != 1 || account.LastCheckin != "confirmed" {
		t.Fatalf("checkins=%d refreshes=%d account=%+v", checkins, refreshes, account)
	}
}

const chatOK = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"

func TestActiveChatSchedulesPointsRecheck(t *testing.T) {
	chats, pointQueries := 0, 0
	currentPointsData := pointsDataInitial
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/chat/completions":
			chats++
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(chatOK))
		case "/v2/billing/meter/get-user-resource":
			pointQueries++
			writeEnvelope(w, http.StatusOK, 0, "ok", currentPointsData)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()
	// This fixture is shared with the domestic check-in tests; switch the
	// account to the international product before exercising ActiveChat.
	svc.mu.Lock()
	svc.state.Accounts[0].Version = model.ModeIntl
	svc.state.Accounts[0].Domain = "www.workbuddy.ai"
	oldIntl := svc.clientIntl
	intlClient := *oldIntl
	intlClient.HTTP = svc.clientCN.HTTP
	svc.clientIntl = &intlClient
	svc.mu.Unlock()
	oldIntl.CloseIdleConnections()

	account, err := svc.ActiveChat("a1", false)
	if err != nil {
		t.Fatal(err)
	}
	if chats != 1 || pointQueries != 1 {
		t.Fatalf("chats=%d pointQueries=%d", chats, pointQueries)
	}
	if account.LastActive != "confirmed" || account.LastActiveDate != today() || account.PendingRecheckAt.IsZero() {
		t.Fatalf("unexpected account: %+v", account)
	}
	if account.LastActiveMsg != "会话已发送，待积分复核" {
		t.Fatalf("activity was presented as credited: %q", account.LastActiveMsg)
	}
	if account.Balance != 80 {
		t.Fatalf("baseline balance=%d", account.Balance)
	}
	if internal, _ := svc.getAccount("a1"); internal.BalanceBeforeActive != 80 {
		t.Fatalf("balanceBeforeActive=%d", internal.BalanceBeforeActive)
	}

	// 复核到期但积分无变化 → 重试一次排期
	if err := svc.updateAccount("a1", func(a *model.Account) { a.PendingRecheckAt = time.Now().Add(-time.Minute) }); err != nil {
		t.Fatal(err)
	}
	svc.runPendingRechecks()
	updated, _ := svc.getAccount("a1")
	if updated.RecheckAttempts != 1 || updated.PendingRecheckAt.IsZero() {
		t.Fatalf("attempts=%d pending=%v", updated.RecheckAttempts, updated.PendingRecheckAt)
	}

	// 积分到账（+30）→ 复核完成并清空排期
	currentPointsData = pointsDataCredited
	if err := svc.updateAccount("a1", func(a *model.Account) { a.PendingRecheckAt = time.Now().Add(-time.Minute) }); err != nil {
		t.Fatal(err)
	}
	svc.runPendingRechecks()
	updated, _ = svc.getAccount("a1")
	if !updated.PendingRecheckAt.IsZero() || updated.Balance != 110 || updated.LastActive != "credited" {
		t.Fatalf("pending=%v balance=%d active=%s", updated.PendingRecheckAt, updated.Balance, updated.LastActive)
	}
}

func TestManualPointsRefreshConfirmsDelayedCredit(t *testing.T) {
	currentPointsData := pointsDataInitial
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/chat/completions":
			_, _ = w.Write([]byte(chatOK))
		case "/v2/billing/meter/get-user-resource":
			writeEnvelope(w, http.StatusOK, 0, "ok", currentPointsData)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()
	svc.mu.Lock()
	svc.state.Accounts[0].Version = model.ModeIntl
	oldIntl := svc.clientIntl
	intlClient := *oldIntl
	intlClient.HTTP = svc.clientCN.HTTP
	svc.clientIntl = &intlClient
	svc.mu.Unlock()
	oldIntl.CloseIdleConnections()

	if _, err := svc.ActiveChat("a1", false); err != nil {
		t.Fatal(err)
	}
	currentPointsData = pointsDataCredited
	account, err := svc.Points("a1")
	if err != nil {
		t.Fatal(err)
	}
	if account.LastActive != "credited" || !account.PendingRecheckAt.IsZero() || account.Balance != 110 {
		t.Fatalf("manual refresh did not confirm credited state: %+v", account)
	}
}

func TestActiveChatRequiresSuccessfulPointsPreflight(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		code      int
		wantLogin bool
	}{
		{name: "billing unavailable", status: http.StatusServiceUnavailable, code: 500},
		{name: "session expired", status: http.StatusUnauthorized, code: 12153, wantLogin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chats := 0
			svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/billing/meter/get-user-resource":
					writeEnvelope(w, tc.status, tc.code, "session unavailable", `{}`)
				case "/v2/plugin/auth/token/refresh":
					writeEnvelope(w, http.StatusUnauthorized, 12153, "session expired", `{}`)
				case "/v2/chat/completions":
					chats++
					_, _ = w.Write([]byte(chatOK))
				default:
					http.NotFound(w, r)
				}
			})
			defer closeUpstream()
			svc.mu.Lock()
			svc.state.Accounts[0].Version = model.ModeIntl
			svc.state.Accounts[0].Domain = "workbuddy.ai"
			oldIntl := svc.clientIntl
			intlClient := *oldIntl
			intlClient.HTTP = svc.clientCN.HTTP
			svc.clientIntl = &intlClient
			svc.mu.Unlock()
			oldIntl.CloseIdleConnections()

			account, err := svc.ActiveChat("a1", false)
			if err == nil || chats != 0 || account.LastActive == "confirmed" || !account.PendingRecheckAt.IsZero() {
				t.Fatalf("err=%v chats=%d account=%+v", err, chats, account)
			}
			if tc.wantLogin && account.Status != "needs_login" {
				t.Fatalf("expired session status=%s", account.Status)
			}
		})
	}
}

func TestActionsRejectWrongAccountVersion(t *testing.T) {
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("wrong-version action reached upstream: %s", r.URL.Path)
		http.Error(w, "unexpected upstream request", http.StatusInternalServerError)
	})
	defer closeUpstream()

	if account, err := svc.ActiveChat("a1", true); !errors.Is(err, ErrAccountModeMismatch) || account.ID != "a1" {
		t.Fatalf("domestic account was not rejected by ActiveChat: account=%+v err=%v", account, err)
	}

	svc.mu.Lock()
	svc.state.Accounts[0].Version = model.ModeIntl
	svc.state.Accounts[0].Domain = "www.workbuddy.ai"
	svc.mu.Unlock()
	if account, err := svc.Checkin("a1"); !errors.Is(err, ErrAccountModeMismatch) || account.ID != "a1" {
		t.Fatalf("international account was not rejected by Checkin: account=%+v err=%v", account, err)
	}
}

func TestScheduledRunRetriesMarkerWithoutRepeatingRemoteTask(t *testing.T) {
	checkins := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/billing/meter/daily-checkin" {
			http.NotFound(w, r)
			return
		}
		checkins++
		writeEnvelope(w, http.StatusOK, 0, "ok", `{}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	st := store.New(dir)
	state := model.NewState()
	state.Settings.ScheduleEnabled = true
	state.Settings.ScheduleTime = time.Now().In(locChina).Add(-2 * time.Minute).Format("15:04")
	state.Accounts = []model.Account{{
		ID:          "a1",
		UID:         "u1",
		Domain:      "codebuddy.cn",
		Version:     model.ModeCN,
		AccessToken: "access",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
		Enabled:     true,
		Status:      "ready",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}

	c := client.New()
	c.ChatBaseCN = upstream.URL
	c.BillingBaseCN = upstream.URL
	c.GlobalBase = upstream.URL
	svc, err := New(st, c)
	if err != nil {
		t.Fatal(err)
	}

	// Make the test independent of the minute in which setup completed.
	scheduleTime := time.Now().In(locChina).Add(-2 * time.Minute).Format("15:04")
	if _, err := svc.UpdateSettings(model.SettingsPatch{ScheduleTime: &scheduleTime}); err != nil {
		t.Fatal(err)
	}

	// Force every state save to fail after the remote task has been sent.
	tmpPath := filepath.Join(dir, "state.bin.tmp")
	if err := os.Mkdir(tmpPath, 0o700); err != nil {
		t.Fatal(err)
	}
	svc.runScheduledIfDue()
	if checkins != 1 {
		t.Fatalf("expected one remote task on first run, got %d", checkins)
	}
	day := time.Now().In(locChina).Format("2006-01-02")
	snapshot, _ := svc.Snapshot()
	if snapshot.Settings.LastScheduledDay != day {
		t.Fatalf("in-memory scheduled marker was not retained: %+v", snapshot.Settings)
	}
	persisted, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Settings.LastScheduledDay == day {
		t.Fatal("scheduled marker unexpectedly persisted while state writes were blocked")
	}

	if err := os.RemoveAll(tmpPath); err != nil {
		t.Fatal(err)
	}
	// The next tick must only repair state.bin. Re-running Checkin would send
	// a duplicate remote request because the first account mutation did not save.
	svc.runScheduledIfDue()
	if checkins != 1 {
		t.Fatalf("scheduled retry repeated remote task: checkins=%d", checkins)
	}
	persisted, err = st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Settings.LastScheduledDay != day {
		t.Fatalf("scheduled marker was not repaired: %+v", persisted.Settings)
	}
}

func TestPendingPointsRecheckRefreshesExpiredToken(t *testing.T) {
	pointQueries, refreshes := 0, 0
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/get-user-resource":
			pointQueries++
			if pointQueries == 1 {
				writeEnvelope(w, http.StatusUnauthorized, 12153, "Offline user session not found", `{}`)
				return
			}
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Errorf("recheck retry used wrong authorization: %q", r.Header.Get("Authorization"))
			}
			writeEnvelope(w, http.StatusOK, 0, "ok", pointsDataCredited)
		case "/v2/plugin/auth/token/refresh":
			refreshes++
			if r.Header.Get("X-Refresh-Token") != "old-refresh" {
				t.Errorf("refresh used wrong token: %q", r.Header.Get("X-Refresh-Token"))
			}
			writeEnvelope(w, http.StatusOK, 0, "ok", `{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}`)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()

	if err := svc.updateAccount("a1", func(a *model.Account) {
		a.Balance = 80
		a.BalanceBeforeActive = 80
		a.PendingRecheckAt = time.Now().Add(-time.Minute)
		a.RecheckAttempts = 1
	}); err != nil {
		t.Fatal(err)
	}
	svc.runPendingRechecks()
	updated, err := svc.getAccount("a1")
	if err != nil {
		t.Fatal(err)
	}
	if pointQueries != 2 || refreshes != 1 {
		t.Fatalf("pointQueries=%d refreshes=%d", pointQueries, refreshes)
	}
	if updated.Balance != 110 || !updated.PendingRecheckAt.IsZero() || updated.RecheckAttempts != 0 || updated.BalanceBeforeActive != 0 {
		t.Fatalf("recheck did not complete after token refresh: %+v", updated)
	}
	if updated.AccessToken != "new-access" || updated.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed token was not persisted: %+v", updated)
	}
}

func TestRunAllUsesActiveChatInIntlMode(t *testing.T) {
	chats, checkins := 0, 0
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/chat/completions":
			chats++
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(chatOK))
		case "/v2/billing/meter/daily-checkin":
			checkins++
			writeEnvelope(w, http.StatusOK, 0, "ok", `{}`)
		case "/v2/billing/meter/get-user-resource":
			writeEnvelope(w, http.StatusOK, 0, "ok", pointsDataInitial)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()

	if _, err := svc.UpdateSettings(fullSettingsPatch(model.Settings{ScheduleEnabled: false, ScheduleTime: "09:15", Mode: model.ModeIntl, ActiveModel: "hy3", RecheckDelayMinutes: 60})); err != nil {
		t.Fatal(err)
	}
	// RunAll 只对当前版本（intl）列表执行；a1 是 CN 账号，先将其并入 intl 列表
	if err := svc.updateAccount("a1", func(a *model.Account) { a.Version = model.ModeIntl }); err != nil {
		t.Fatal(err)
	}
	results := svc.RunAll(model.ModeIntl)
	if len(results) != 1 || chats != 1 || checkins != 0 {
		t.Fatalf("results=%d chats=%d checkins=%d", len(results), chats, checkins)
	}
	if results[0].LastActive != "confirmed" {
		t.Fatalf("unexpected result: %+v", results[0])
	}

	// 指定 cn 版本时：intl 账号不参与，不应再触发活跃
	chats = 0
	cnResults := svc.RunAll(model.ModeCN)
	if len(cnResults) != 0 || chats != 0 {
		t.Fatalf("cn run-all should skip intl accounts: results=%d chats=%d", len(cnResults), chats)
	}
}

func TestUpdateSettingsValidatesModeAndModel(t *testing.T) {
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	defer closeUpstream()

	if _, err := svc.UpdateSettings(fullSettingsPatch(model.Settings{ScheduleEnabled: false, ScheduleTime: "09:15", Mode: "jp", ActiveModel: "hy3", RecheckDelayMinutes: 840})); err == nil {
		t.Fatal("invalid mode should be rejected")
	}
	if _, err := svc.UpdateSettings(fullSettingsPatch(model.Settings{ScheduleEnabled: false, ScheduleTime: "09:15", Mode: model.ModeIntl, ActiveModel: "gpt-4", RecheckDelayMinutes: 840})); err == nil {
		t.Fatal("invalid model should be rejected")
	}
	if _, err := svc.UpdateSettings(fullSettingsPatch(model.Settings{ScheduleEnabled: false, ScheduleTime: "09:15", Mode: model.ModeIntl, ActiveModel: "hy4-preview", RecheckDelayMinutes: 90})); err != nil {
		t.Fatal(err)
	}
	state, _ := svc.Snapshot()
	settings := state.Settings
	if settings.Mode != model.ModeIntl || settings.ActiveModel != "hy4-preview" || settings.RecheckDelayMinutes != 90 {
		t.Fatalf("unexpected settings: %+v", settings)
	}
}

func TestLoginKeepsSameUIDInSeparateVersionLists(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir)
	if err := st.Save(model.NewState()); err != nil {
		t.Fatal(err)
	}
	svc, err := New(st, client.New())
	if err != nil {
		t.Fatal(err)
	}

	cn, err := svc.commitLoginResult(client.LoginResult{
		UID: "shared-uid", Domain: "codebuddy.cn", AccessToken: "cn-access", RefreshToken: "cn-refresh", ExpiresIn: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	intl, err := svc.commitLoginResult(client.LoginResult{
		UID: "shared-uid", Domain: "workbuddy.ai", AccessToken: "intl-access", RefreshToken: "intl-refresh", ExpiresIn: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cn.ID == intl.ID || cn.Version != model.ModeCN || intl.Version != model.ModeIntl {
		t.Fatalf("cross-version login was merged: cn=%+v intl=%+v", cn, intl)
	}
	_, accounts := svc.Snapshot()
	if len(accounts) != 2 {
		t.Fatalf("same UID should be retained once per version, got %+v", accounts)
	}

	updated, err := svc.commitLoginResult(client.LoginResult{
		UID: "shared-uid", Domain: "workbuddy.ai", AccessToken: "intl-access-2", RefreshToken: "intl-refresh-2", ExpiresIn: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != intl.ID {
		t.Fatalf("same-version relogin created a third account: old=%q new=%q", intl.ID, updated.ID)
	}
	_, accounts = svc.Snapshot()
	if len(accounts) != 2 {
		t.Fatalf("same-version relogin changed account count: %+v", accounts)
	}
}

func TestProxyConfDirectOverridesLegacyState(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir)
	state := model.NewState()
	state.Settings.ProxyURL = "http://old-proxy.invalid:8080"
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProxyConf(""); err != nil {
		t.Fatal(err)
	}

	svc, err := New(st, client.New())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := svc.Snapshot()
	if snapshot.Settings.ProxyURL != "" {
		t.Fatalf("direct proxy.conf was ignored: %+v", snapshot.Settings)
	}
	loaded, err := st.Load()
	if err != nil || loaded.Settings.ProxyURL != "" {
		t.Fatalf("state was not reconciled: proxy=%q err=%v", loaded.Settings.ProxyURL, err)
	}
	value, found, err := st.LoadProxyConfWithPresence()
	if err != nil || !found || value != "" {
		t.Fatalf("proxy.conf: value=%q found=%v err=%v", value, found, err)
	}
}

func TestUpdateSettingsSupportsPartialPatchAndClearingProxy(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir)
	state := model.NewState()
	state.Settings = model.Settings{
		ScheduleEnabled:     true,
		ScheduleTime:        "10:20",
		Mode:                model.ModeIntl,
		ActiveModel:         "hy4-preview",
		RecheckDelayMinutes: 90,
		ProxyURL:            "http://old-proxy.invalid:8080",
		ActivePrompt:        "保留这个提示词",
	}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProxyConf(state.Settings.ProxyURL); err != nil {
		t.Fatal(err)
	}
	svc, err := New(st, client.New())
	if err != nil {
		t.Fatal(err)
	}
	oldClient := svc.clientIntl
	blank := ""
	saved, err := svc.UpdateSettings(model.SettingsPatch{ProxyURL: &blank})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ProxyURL != "" || saved.ScheduleTime != "10:20" || saved.Mode != model.ModeIntl || saved.ActiveModel != "hy4-preview" || saved.RecheckDelayMinutes != 90 || saved.ActivePrompt != "保留这个提示词" {
		t.Fatalf("partial patch changed unrelated settings: %+v", saved)
	}
	if svc.clientIntl == oldClient {
		t.Fatal("proxy change did not atomically replace the international client")
	}
	transport, ok := svc.clientIntl.HTTP.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("cleared proxy did not produce a direct transport: %#v", svc.clientIntl.HTTP.Transport)
	}
	reloaded, err := New(st, client.New())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := reloaded.Snapshot()
	if got.Settings.ProxyURL != "" {
		t.Fatalf("proxy returned after restart: %q", got.Settings.ProxyURL)
	}
}

func TestUpdateSettingsFailureDoesNotPublishState(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir)
	state := model.NewState()
	state.Settings.ProxyURL = "http://old-proxy.invalid:8080"
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProxyConf(state.Settings.ProxyURL); err != nil {
		t.Fatal(err)
	}
	svc, err := New(st, client.New())
	if err != nil {
		t.Fatal(err)
	}
	proxyPath := filepath.Join(dir, store.ProxyConfFileName)
	if err := os.Remove(proxyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(proxyPath, 0o700); err != nil {
		t.Fatal(err)
	}
	newProxy := "http://new-proxy.invalid:8080"
	if _, err := svc.UpdateSettings(model.SettingsPatch{ProxyURL: &newProxy}); err == nil {
		t.Fatal("expected proxy persistence failure")
	}
	snapshot, _ := svc.Snapshot()
	if snapshot.Settings.ProxyURL != state.Settings.ProxyURL {
		t.Fatalf("failed update leaked into memory: %q", snapshot.Settings.ProxyURL)
	}
}

func TestUpdateSettingsStateSaveFailureRollsBackProxyConf(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir)
	state := model.NewState()
	state.Settings.ProxyURL = "http://old-proxy.invalid:8080"
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProxyConf(state.Settings.ProxyURL); err != nil {
		t.Fatal(err)
	}
	svc, err := New(st, client.New())
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	oldClient := svc.clientIntl
	if err := os.Mkdir(filepath.Join(dir, "state.bin.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	newProxy := "http://new-proxy.invalid:8080"
	newSchedule := "10:45"
	if _, err := svc.UpdateSettings(model.SettingsPatch{
		ProxyURL:     &newProxy,
		ScheduleTime: &newSchedule,
	}); err == nil {
		t.Fatal("expected state persistence failure")
	}

	snapshot, _ := svc.Snapshot()
	if snapshot.Settings != before.Settings {
		t.Fatalf("failed update leaked into memory: got=%+v want=%+v", snapshot.Settings, before.Settings)
	}
	if svc.clientIntl != oldClient {
		t.Fatal("failed update published the replacement international client")
	}
	persisted, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Settings != before.Settings {
		t.Fatalf("failed update changed state.bin: got=%+v want=%+v", persisted.Settings, before.Settings)
	}
	proxyURL, found, err := st.LoadProxyConfWithPresence()
	if err != nil {
		t.Fatal(err)
	}
	if !found || proxyURL != before.Settings.ProxyURL {
		t.Fatalf("proxy.conf was not rolled back: value=%q found=%v", proxyURL, found)
	}
}

func TestAccountMutationsRollBackWhenStateSaveFails(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir)
	state := model.NewState()
	state.Accounts = []model.Account{{
		ID:           "a1",
		UID:          "u1",
		Nickname:     "原账号",
		Alias:        "原别名",
		Domain:       "codebuddy.cn",
		Version:      model.ModeCN,
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		Enabled:      true,
		Status:       "ready",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	svc, err := New(st, client.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "state.bin.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	alias := "未落盘别名"
	if err := svc.UpdateAccount("a1", &alias, nil); err == nil {
		t.Fatal("expected account update persistence failure")
	}
	account, err := svc.AccountByID("a1")
	if err != nil {
		t.Fatal(err)
	}
	if account.Alias != "原别名" {
		t.Fatalf("failed update leaked into memory: %+v", account)
	}

	result := client.LoginResult{
		UID:          "u1",
		Nickname:     "重新登录",
		Domain:       "codebuddy.cn",
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		ExpiresIn:    3600,
	}
	returned, err := svc.commitLoginResult(result)
	if err == nil {
		t.Fatal("expected login commit persistence failure")
	}
	if returned.ID != "a1" || returned.Alias != "原别名" {
		t.Fatalf("failed login returned uncommitted account: %+v", returned)
	}
	internal, err := svc.getAccount("a1")
	if err != nil {
		t.Fatal(err)
	}
	if internal.AccessToken != "old-access" || internal.RefreshToken != "old-refresh" || internal.Nickname != "原账号" {
		t.Fatalf("failed login leaked into memory: %+v", internal)
	}

	if err := svc.DeleteAccount("a1"); err == nil {
		t.Fatal("expected account delete persistence failure")
	}
	if _, err := svc.AccountByID("a1"); err != nil {
		t.Fatalf("failed delete removed account from memory: %v", err)
	}
	persisted, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Accounts) != 1 || persisted.Accounts[0].Alias != "原别名" || persisted.Accounts[0].AccessToken != "old-access" {
		t.Fatalf("failed mutations changed state.bin: %+v", persisted.Accounts)
	}
}

func TestPrepareAccountReturnsPersistedErrorState(t *testing.T) {
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/token/refresh" {
			writeEnvelope(w, http.StatusUnauthorized, 12153, "Offline user session not found", `{}`)
			return
		}
		http.NotFound(w, r)
	})
	defer closeUpstream()
	if err := svc.updateAccount("a1", func(a *model.Account) { a.ExpiresAt = 0 }); err != nil {
		t.Fatal(err)
	}
	account, err := svc.Checkin("a1")
	if err == nil {
		t.Fatal("expected refresh failure")
	}
	if account.Status != "needs_login" || account.LastError == "" {
		t.Fatalf("returned account did not reflect persisted error: %+v", account)
	}
}

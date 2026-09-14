package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
	svc, err := New(st, c)
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	return svc, upstream.Close
}

func writeEnvelope(w http.ResponseWriter, status, code int, msg, data string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data == "" {
		data = "{}"
	}
	_, _ = fmt.Fprintf(w, `{"code":%d,"msg":%q,"data":%s}`, code, msg, data)
}

var pointsData = `{"Response":{"Data":{"Accounts":[{"PackageName":"体验包","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}]}}}`

func TestCheckinIsIdempotentForBeijingDay(t *testing.T) {
	checkins, pointQueries := 0, 0
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			checkins++
			writeEnvelope(w, http.StatusOK, 0, "ok", `{}`)
		case "/v2/billing/meter/get-user-resource":
			pointQueries++
			writeEnvelope(w, http.StatusOK, 0, "ok", pointsData)
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
			writeEnvelope(w, http.StatusOK, 0, "ok", pointsData)
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
			writeEnvelope(w, http.StatusOK, 0, "ok", pointsData)
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

const chatOK = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"

func TestActiveChatSchedulesPointsRecheck(t *testing.T) {
	chats, pointQueries := 0, 0
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/chat/completions":
			chats++
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(chatOK))
		case "/v2/billing/meter/get-user-resource":
			pointQueries++
			writeEnvelope(w, http.StatusOK, 0, "ok", pointsData)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()

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
	pointsData = `{"Response":{"Data":{"Accounts":[{"PackageName":"体验包","CycleCapacitySize":130,"CycleCapacityRemain":110,"CycleCapacityUsed":20}]}}}`
	if err := svc.updateAccount("a1", func(a *model.Account) { a.PendingRecheckAt = time.Now().Add(-time.Minute) }); err != nil {
		t.Fatal(err)
	}
	svc.runPendingRechecks()
	updated, _ = svc.getAccount("a1")
	if !updated.PendingRecheckAt.IsZero() || updated.Balance != 110 {
		t.Fatalf("pending=%v balance=%d", updated.PendingRecheckAt, updated.Balance)
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
			writeEnvelope(w, http.StatusOK, 0, "ok", pointsData)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()

	if err := svc.UpdateSettings(model.Settings{ScheduleEnabled: false, ScheduleTime: "09:15", Mode: model.ModeIntl, ActiveModel: "hy3", RecheckDelayMinutes: 60}); err != nil {
		t.Fatal(err)
	}
	// RunAll 只对当前版本（intl）列表执行；a1 是 CN 账号，先将其并入 intl 列表
	if err := svc.updateAccount("a1", func(a *model.Account) { a.Version = model.ModeIntl }); err != nil {
		t.Fatal(err)
	}
	results := svc.RunAll()
	if len(results) != 1 || chats != 1 || checkins != 0 {
		t.Fatalf("results=%d chats=%d checkins=%d", len(results), chats, checkins)
	}
	if results[0].LastActive != "confirmed" {
		t.Fatalf("unexpected result: %+v", results[0])
	}
}

func TestUpdateSettingsValidatesModeAndModel(t *testing.T) {
	svc, closeUpstream := newAccountService(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	defer closeUpstream()

	if err := svc.UpdateSettings(model.Settings{ScheduleEnabled: false, ScheduleTime: "09:15", Mode: "jp"}); err == nil {
		t.Fatal("invalid mode should be rejected")
	}
	if err := svc.UpdateSettings(model.Settings{ScheduleEnabled: false, ScheduleTime: "09:15", Mode: model.ModeIntl, ActiveModel: "gpt-4"}); err == nil {
		t.Fatal("invalid model should be rejected")
	}
	if err := svc.UpdateSettings(model.Settings{ScheduleEnabled: false, ScheduleTime: "09:15", Mode: model.ModeIntl, ActiveModel: "hy4-preview", RecheckDelayMinutes: 90}); err != nil {
		t.Fatal(err)
	}
	state, _ := svc.Snapshot()
	settings := state.Settings
	if settings.Mode != model.ModeIntl || settings.ActiveModel != "hy4-preview" || settings.RecheckDelayMinutes != 90 {
		t.Fatalf("unexpected settings: %+v", settings)
	}
}

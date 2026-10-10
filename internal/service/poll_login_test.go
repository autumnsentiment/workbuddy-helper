package service

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy-helper/internal/client"
	"workbuddy-helper/internal/model"
	"workbuddy-helper/internal/store"
)

func TestLoginStartReservationCapsPendingSessions(t *testing.T) {
	svc := &Service{
		logins:    make(map[string]*client.LoginSession),
		loginAt:   make(map[string]time.Time),
		loginBusy: make(map[string]bool),
	}
	for i := 0; i < maxLoginSessions; i++ {
		id := fmt.Sprintf("login-%d", i)
		svc.logins[id] = &client.LoginSession{}
		svc.loginAt[id] = time.Now()
	}
	if err := svc.reserveLoginStart(); !errors.Is(err, ErrLoginSessionLimit) {
		t.Fatalf("reserveLoginStart error=%v, want %v", err, ErrLoginSessionLimit)
	}

	// Expired, non-busy sessions are reclaimed before applying the cap.
	svc.loginMu.Lock()
	for id := range svc.loginAt {
		svc.loginAt[id] = time.Now().Add(-loginSessionTTL - time.Second)
	}
	svc.loginMu.Unlock()
	if err := svc.reserveLoginStart(); err != nil {
		t.Fatalf("expired sessions should be reclaimed: %v", err)
	}
	if svc.loginStarting != 1 {
		t.Fatalf("loginStarting=%d, want 1", svc.loginStarting)
	}
	svc.releaseLoginStart()
}

type pollLoginOutcome struct {
	account model.PublicAccount
	err     error
}

func newPollLoginService(t *testing.T, accounts []model.Account, handler http.HandlerFunc) (*Service, func()) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	state := model.NewState()
	state.Accounts = append([]model.Account(nil), accounts...)
	st := store.New(t.TempDir())
	if err := st.Save(state); err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	base := client.New()
	base.HTTP = upstream.Client()
	base.ChatBaseCN = upstream.URL
	base.BillingBaseCN = upstream.URL
	base.GlobalBase = upstream.URL
	svc, err := New(st, base)
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	return svc, upstream.Close
}

func registerLoginSession(svc *Service, id, state string) {
	session := client.NewLoginSessionWithClient(model.ModeCN, svc.clientCN)
	session.State = state
	svc.loginMu.Lock()
	svc.logins[id] = session
	svc.loginAt[id] = time.Now()
	svc.loginMu.Unlock()
}

func writeLoginEnvelope(w http.ResponseWriter, status, code int, data string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"code":%d,"msg":"ok","data":%s}`, code, data)
}

func waitPollLoginOutcome(t *testing.T, ch <-chan pollLoginOutcome) pollLoginOutcome {
	t.Helper()
	select {
	case outcome := <-ch:
		return outcome
	case <-time.After(3 * time.Second):
		t.Fatal("等待登录轮询完成超时")
		return pollLoginOutcome{}
	}
}

func TestPollLoginRejectsConcurrentPollForSameSession(t *testing.T) {
	tokenStarted := make(chan struct{})
	releaseToken := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseToken) }) }
	defer release()
	var tokenCalls atomic.Int32

	svc, closeUpstream := newPollLoginService(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			if tokenCalls.Add(1) == 1 {
				close(tokenStarted)
			}
			<-releaseToken
			writeLoginEnvelope(w, http.StatusOK, 0, `{"accessToken":"login-access","refreshToken":"login-refresh","expiresIn":3600,"domain":"codebuddy.cn"}`)
		case "/v2/plugin/login/account":
			writeLoginEnvelope(w, http.StatusOK, 0, `{"uid":"same-session-user","enterpriseId":"enterprise","nickname":"测试账号"}`)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()
	registerLoginSession(svc, "login-1", "state-1")

	firstDone := make(chan pollLoginOutcome, 1)
	go func() {
		account, err := svc.PollLogin("login-1")
		firstDone <- pollLoginOutcome{account: account, err: err}
	}()
	select {
	case <-tokenStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("首次轮询未到达登录服务")
	}

	secondDone := make(chan pollLoginOutcome, 1)
	go func() {
		account, err := svc.PollLogin("login-1")
		secondDone <- pollLoginOutcome{account: account, err: err}
	}()
	var second pollLoginOutcome
	select {
	case second = <-secondDone:
		if second.err == nil || !client.IsKind(second.err, client.KindBusiness) || !strings.Contains(second.err.Error(), "已有轮询") {
			t.Fatalf("并发轮询未返回占用提示: %v", second.err)
		}
	case <-time.After(300 * time.Millisecond):
		release()
		_ = waitPollLoginOutcome(t, firstDone)
		_ = waitPollLoginOutcome(t, secondDone)
		t.Fatal("第二个同会话轮询被发送到了上游")
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("同一会话触发了 %d 次上游轮询", got)
	}

	release()
	first := waitPollLoginOutcome(t, firstDone)
	if first.err != nil || first.account.UID != "same-session-user" {
		t.Fatalf("首次轮询结果异常: account=%+v err=%v", first.account, first.err)
	}
	if _, err := svc.PollLogin("login-1"); err == nil || !strings.Contains(err.Error(), "不存在或已过期") {
		t.Fatalf("成功后的会话未被消费: %v", err)
	}
}

func TestPollLoginPendingResponseReleasesSessionClaim(t *testing.T) {
	var tokenCalls atomic.Int32
	svc, closeUpstream := newPollLoginService(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			if tokenCalls.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, `{"code":11217,"msg":"login ing...","data":{}}`)
				return
			}
			writeLoginEnvelope(w, http.StatusOK, 0, `{"accessToken":"login-access","refreshToken":"login-refresh","expiresIn":3600,"domain":"codebuddy.cn"}`)
		case "/v2/plugin/login/account":
			writeLoginEnvelope(w, http.StatusOK, 0, `{"uid":"pending-user","enterpriseId":"enterprise","nickname":"等待账号"}`)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()
	registerLoginSession(svc, "pending-login", "pending-state")

	if _, err := svc.PollLogin("pending-login"); err == nil || !strings.Contains(err.Error(), "尚未完成") {
		t.Fatalf("HTTP 200 + code 11217 未被识别为等待授权: %v", err)
	}
	svc.loginMu.Lock()
	sessionRetained := svc.logins["pending-login"] != nil
	busy := svc.loginBusy["pending-login"]
	svc.loginMu.Unlock()
	if !sessionRetained || busy {
		t.Fatalf("未完成轮询未正确释放占用: retained=%v busy=%v", sessionRetained, busy)
	}

	account, err := svc.PollLogin("pending-login")
	if err != nil || account.UID != "pending-user" {
		t.Fatalf("后续轮询未成功: account=%+v err=%v", account, err)
	}
}

func TestPollLoginConcurrentSessionsDeduplicateUID(t *testing.T) {
	profilesReady := make(chan struct{})
	var profileCalls atomic.Int32
	svc, closeUpstream := newPollLoginService(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			state := r.URL.Query().Get("state")
			writeLoginEnvelope(w, http.StatusOK, 0, fmt.Sprintf(`{"accessToken":"access-%s","refreshToken":"refresh-%s","expiresIn":3600,"domain":"codebuddy.cn"}`, state, state))
		case "/v2/plugin/login/account":
			if profileCalls.Add(1) == 2 {
				close(profilesReady)
			}
			select {
			case <-profilesReady:
			case <-time.After(3 * time.Second):
				http.Error(w, "concurrent profile request timeout", http.StatusGatewayTimeout)
				return
			}
			writeLoginEnvelope(w, http.StatusOK, 0, `{"uid":"shared-user","enterpriseId":"enterprise","nickname":"共享账号"}`)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()
	registerLoginSession(svc, "login-a", "state-a")
	registerLoginSession(svc, "login-b", "state-b")

	outcomes := make(chan pollLoginOutcome, 2)
	for _, id := range []string{"login-a", "login-b"} {
		id := id
		go func() {
			account, err := svc.PollLogin(id)
			outcomes <- pollLoginOutcome{account: account, err: err}
		}()
	}
	first := waitPollLoginOutcome(t, outcomes)
	second := waitPollLoginOutcome(t, outcomes)
	if first.err != nil || second.err != nil {
		t.Fatalf("并发登录失败: first=%v second=%v", first.err, second.err)
	}
	if first.account.ID == "" || first.account.ID != second.account.ID {
		t.Fatalf("同 UID 被映射为不同账号: first=%q second=%q", first.account.ID, second.account.ID)
	}
	_, accounts := svc.Snapshot()
	if len(accounts) != 1 || accounts[0].UID != "shared-user" {
		t.Fatalf("同 UID 未去重: %+v", accounts)
	}
}

func TestPollLoginAccountLimitIsAtomic(t *testing.T) {
	accounts := make([]model.Account, 49)
	now := time.Now()
	for i := range accounts {
		accounts[i] = model.Account{
			ID:        fmt.Sprintf("account-%02d", i),
			UID:       fmt.Sprintf("user-%02d", i),
			Enabled:   true,
			Status:    "ready",
			CreatedAt: now,
			UpdatedAt: now,
		}
	}
	profilesReady := make(chan struct{})
	var profileCalls atomic.Int32
	svc, closeUpstream := newPollLoginService(t, accounts, func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			writeLoginEnvelope(w, http.StatusOK, 0, fmt.Sprintf(`{"accessToken":"access-%s","refreshToken":"refresh-%s","expiresIn":3600,"domain":"codebuddy.cn"}`, state, state))
		case "/v2/plugin/login/account":
			if profileCalls.Add(1) == 2 {
				close(profilesReady)
			}
			select {
			case <-profilesReady:
			case <-time.After(3 * time.Second):
				http.Error(w, "concurrent profile request timeout", http.StatusGatewayTimeout)
				return
			}
			writeLoginEnvelope(w, http.StatusOK, 0, fmt.Sprintf(`{"uid":"new-%s","enterpriseId":"enterprise","nickname":"新增账号"}`, state))
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()
	registerLoginSession(svc, "limit-a", "limit-a")
	registerLoginSession(svc, "limit-b", "limit-b")

	outcomes := make(chan pollLoginOutcome, 2)
	for _, id := range []string{"limit-a", "limit-b"} {
		id := id
		go func() {
			account, err := svc.PollLogin(id)
			outcomes <- pollLoginOutcome{account: account, err: err}
		}()
	}
	successes, limitErrors := 0, 0
	for i := 0; i < 2; i++ {
		outcome := waitPollLoginOutcome(t, outcomes)
		if outcome.err == nil {
			successes++
		} else if strings.Contains(outcome.err.Error(), "最多管理 50 个账号") {
			limitErrors++
		} else {
			t.Fatalf("意外的登录错误: %v", outcome.err)
		}
	}
	_, gotAccounts := svc.Snapshot()
	if successes != 1 || limitErrors != 1 || len(gotAccounts) != 50 {
		t.Fatalf("账号上限提交不原子: successes=%d limitErrors=%d accounts=%d", successes, limitErrors, len(gotAccounts))
	}
	svc.loginMu.Lock()
	remainingSessions := len(svc.logins)
	busySessions := len(svc.loginBusy)
	svc.loginMu.Unlock()
	if remainingSessions != 1 || busySessions != 0 {
		t.Fatalf("失败会话应可重试且不能保持占用: sessions=%d busy=%d", remainingSessions, busySessions)
	}
}

func TestPollLoginWaitsForInFlightAccountOperation(t *testing.T) {
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	loginProfileServed := make(chan struct{})
	var refreshOnce sync.Once
	var profileOnce sync.Once
	now := time.Now()
	initial := []model.Account{{
		ID:           "existing-account",
		UID:          "existing-user",
		Nickname:     "旧账号",
		Domain:       "codebuddy.cn",
		Version:      model.ModeCN,
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    now.Add(time.Hour).Unix(),
		Enabled:      true,
		Status:       "ready",
		CreatedAt:    now,
		UpdatedAt:    now,
	}}
	svc, closeUpstream := newPollLoginService(t, initial, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			switch r.Header.Get("Authorization") {
			case "Bearer old-access":
				writeLoginEnvelope(w, http.StatusUnauthorized, 12153, `{}`)
			case "Bearer refreshed-access":
				writeLoginEnvelope(w, http.StatusOK, 0, `{}`)
			default:
				http.Error(w, "unexpected checkin token", http.StatusUnauthorized)
			}
		case "/v2/plugin/auth/token/refresh":
			refreshOnce.Do(func() { close(refreshStarted) })
			<-releaseRefresh
			writeLoginEnvelope(w, http.StatusOK, 0, `{"accessToken":"refreshed-access","refreshToken":"refreshed-refresh","expiresIn":3600}`)
		case "/v2/billing/meter/get-user-resource":
			writeLoginEnvelope(w, http.StatusOK, 0, `{"Response":{"Data":{"Accounts":[{"PackageName":"体验包","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}]}}}`)
		case "/v2/plugin/auth/token":
			writeLoginEnvelope(w, http.StatusOK, 0, `{"accessToken":"login-access","refreshToken":"login-refresh","expiresIn":3600,"domain":"codebuddy.cn"}`)
		case "/v2/plugin/login/account":
			profileOnce.Do(func() { close(loginProfileServed) })
			writeLoginEnvelope(w, http.StatusOK, 0, `{"uid":"existing-user","enterpriseId":"enterprise","nickname":"新登录账号"}`)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeUpstream()
	registerLoginSession(svc, "relogin", "relogin-state")

	checkinDone := make(chan error, 1)
	go func() {
		_, err := svc.Checkin("existing-account")
		checkinDone <- err
	}()
	select {
	case <-refreshStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("签到请求未进入令牌刷新")
	}

	pollDone := make(chan pollLoginOutcome, 1)
	go func() {
		account, err := svc.PollLogin("relogin")
		pollDone <- pollLoginOutcome{account: account, err: err}
	}()
	select {
	case <-loginProfileServed:
	case <-time.After(3 * time.Second):
		close(releaseRefresh)
		t.Fatal("重新登录未完成上游轮询")
	}

	pollCompletedEarly := false
	var poll pollLoginOutcome
	select {
	case poll = <-pollDone:
		pollCompletedEarly = true
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseRefresh)
	select {
	case err := <-checkinDone:
		if err != nil {
			t.Fatalf("签到流程失败: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("签到流程未完成")
	}
	if !pollCompletedEarly {
		poll = waitPollLoginOutcome(t, pollDone)
	}
	if pollCompletedEarly {
		t.Fatal("重新登录在已有账号操作释放账号锁前完成")
	}
	if poll.err != nil || poll.account.ID != "existing-account" {
		t.Fatalf("重新登录结果异常: account=%+v err=%v", poll.account, poll.err)
	}
	internal, err := svc.getAccount("existing-account")
	if err != nil {
		t.Fatal(err)
	}
	if internal.AccessToken != "login-access" || internal.RefreshToken != "login-refresh" {
		t.Fatalf("登录凭据被并发账号操作覆盖: access=%q refresh=%q", internal.AccessToken, internal.RefreshToken)
	}
}

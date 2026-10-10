package server

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"workbuddy-helper/internal/client"
	"workbuddy-helper/internal/model"
	"workbuddy-helper/internal/service"
	"workbuddy-helper/internal/store"
)

func testServer(t *testing.T) (*service.Service, http.Handler) {
	t.Helper()
	svc, err := service.New(store.New(t.TempDir()), client.New())
	if err != nil {
		t.Fatal(err)
	}
	web := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html><head></head><body>ok</body></html>")},
		"app.js":     &fstest.MapFile{Data: []byte("console.log('ok')")},
	}
	var root fs.FS = web
	return svc, New(svc, root, "", nil).Handler()
}

func TestStateDoesNotExposeTokens(t *testing.T) {
	svc, handler := testServer(t)
	// Service starts with no accounts; the assertion below is about the token
	// injected into HTML and the absence of any credential field in state JSON.
	_ = svc
	r := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "accessToken") || strings.Contains(w.Body.String(), "refreshToken") {
		t.Fatalf("unsafe state response: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStateExposesTokenExpiryWithoutCredentials(t *testing.T) {
	const expiry int64 = 1893456000
	st := store.New(t.TempDir())
	state := model.NewState()
	state.Accounts = []model.Account{{
		ID:           "expiry-account",
		UID:          "u-expiry",
		AccessToken:  "access-secret",
		RefreshToken: "refresh-secret",
		ExpiresAt:    expiry,
		Enabled:      true,
		Status:       "ready",
	}}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	svc, err := service.New(st, client.New())
	if err != nil {
		t.Fatal(err)
	}
	handler := New(svc, fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html><head></head></html>")},
	}, "", nil).Handler()

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("state status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "accessToken") || strings.Contains(w.Body.String(), "refreshToken") {
		t.Fatalf("state response exposed credentials: %s", w.Body.String())
	}
	var response struct {
		CN []struct {
			TokenExpiresAt int64 `json:"tokenExpiresAt"`
		} `json:"cnAccounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.CN) != 1 {
		t.Fatalf("expected one domestic account, got %+v", response.CN)
	}
	if response.CN[0].TokenExpiresAt != expiry {
		t.Fatalf("token expiry=%d, want %d; body=%s", response.CN[0].TokenExpiresAt, expiry, w.Body.String())
	}
}

func TestWriteRequiresAppToken(t *testing.T) {
	svc, handler := testServer(t)
	r := httptest.NewRequest(http.MethodPost, "/api/run-all", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest(http.MethodPost, "/api/run-all", strings.NewReader("{}"))
	r.Header.Set("X-App-Token", svc.AppToken())
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("authorized status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestHTMLTokenUsesMetaAndCSP(t *testing.T) {
	svc, handler := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `name="app-token"`) || !strings.Contains(w.Body.String(), svc.AppToken()) {
		t.Fatalf("missing token meta: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "<script>window.__APP_TOKEN__") {
		t.Fatal("token must not be injected as inline script")
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("missing restrictive CSP")
	}
}

func TestHealthzContractAndSecurityHeaders(t *testing.T) {
	_, handler := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "ok ") {
		t.Fatalf("healthz response: status=%d body=%q", w.Code, w.Body.String())
	}
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := w.Header().Get(name); got != want {
			t.Fatalf("%s=%q, want %q", name, got, want)
		}
	}
	if got := w.Header().Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") {
		t.Fatalf("healthz missing CSP: %q", got)
	}
}

func TestOptionalBasicAuthProtectsPanelButNotHealthz(t *testing.T) {
	t.Setenv("WBH_AUTH_USER", "admin")
	t.Setenv("WBH_AUTH_PASSWORD", "secret")
	_, handler := testServer(t)

	unauthenticated := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	unauthenticatedW := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedW, unauthenticated)
	if unauthenticatedW.Code != http.StatusUnauthorized || unauthenticatedW.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("missing basic auth challenge: status=%d headers=%v", unauthenticatedW.Code, unauthenticatedW.Header())
	}

	authorized := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	authorized.SetBasicAuth("admin", "secret")
	authorizedW := httptest.NewRecorder()
	handler.ServeHTTP(authorizedW, authorized)
	if authorizedW.Code != http.StatusOK {
		t.Fatalf("authorized state request failed: status=%d body=%s", authorizedW.Code, authorizedW.Body.String())
	}

	health := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthW := httptest.NewRecorder()
	handler.ServeHTTP(healthW, health)
	if healthW.Code != http.StatusOK {
		t.Fatalf("healthz should remain available for container checks: status=%d", healthW.Code)
	}
}

func TestPartialBasicAuthConfigurationFailsClosed(t *testing.T) {
	t.Setenv("WBH_AUTH_USER", "admin")
	t.Setenv("WBH_AUTH_PASSWORD", "")
	_, handler := testServer(t)

	r := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("partial auth config should fail closed: status=%d body=%s", w.Code, w.Body.String())
	}
	health := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthW := httptest.NewRecorder()
	handler.ServeHTTP(healthW, health)
	if healthW.Code != http.StatusServiceUnavailable {
		t.Fatalf("misconfigured auth should make healthz unhealthy: status=%d", healthW.Code)
	}
}

func TestEmbeddedStaticAssetsAreNotCached(t *testing.T) {
	_, handler := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("static asset status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("static asset should not be cached: %q", got)
	}
}

func TestHostGuardRejectsUntrustedHost(t *testing.T) {
	svc, _ := testServer(t)
	web := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html><head></head></html>")}}
	handler := New(svc, web, "127.0.0.1:18080", nil).Handler()
	r := httptest.NewRequest(http.MethodGet, "http://evil.example:18080/", nil)
	r.Host = "evil.example:18080"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusMisdirectedRequest || strings.Contains(w.Body.String(), svc.AppToken()) {
		t.Fatalf("untrusted host was served: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestWildcardHostGuardAcceptsIPAndRejectsDNS(t *testing.T) {
	svc, _ := testServer(t)
	web := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html><head></head></html>")}}
	handler := New(svc, web, "0.0.0.0:18080", nil).Handler()
	for _, host := range []string{"192.0.2.10:18080", "localhost:18080"} {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		r.Host = host
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="app-token"`) {
			t.Fatalf("allowed host %q was rejected: status=%d body=%s", host, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://evil.example:18080/", nil)
	r.Host = "evil.example:18080"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("wildcard listener accepted arbitrary DNS host: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestHostGuardAllowsConfiguredProxyHost(t *testing.T) {
	t.Setenv("WBH_ALLOWED_HOSTS", "panel.example:8443")
	svc, _ := testServer(t)
	web := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html><head></head></html>")}}
	handler := New(svc, web, "0.0.0.0:18080", nil).Handler()
	r := httptest.NewRequest(http.MethodGet, "https://panel.example:8443/", nil)
	r.Host = "panel.example:8443"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="app-token"`) {
		t.Fatalf("configured proxy host was rejected: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestWriteAllowsSameOriginNonLoopback(t *testing.T) {
	svc, handler := testServer(t)
	// 模拟浏览器通过局域网 IP 访问：请求 Host = 192.0.2.10:18080，
	// Origin 与 Host 同源。应通过 CSRF 校验（token 正确）。
	r := httptest.NewRequest(http.MethodPost, "/api/run-all", strings.NewReader("{}"))
	r.Host = "192.0.2.10:18080"
	r.Header.Set("X-App-Token", svc.AppToken())
	r.Header.Set("Origin", "http://192.0.2.10:18080")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("same-origin non-loopback should be allowed: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestWriteRejectsCrossOrigin(t *testing.T) {
	svc, handler := testServer(t)
	r := httptest.NewRequest(http.MethodPost, "/api/run-all", strings.NewReader("{}"))
	r.Host = "192.0.2.10:18080"
	r.Header.Set("X-App-Token", svc.AppToken())
	r.Header.Set("Origin", "http://evil.example")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin should be rejected: status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "不受信任") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestWriteAllowsHTTPSSameOrigin(t *testing.T) {
	svc, handler := testServer(t)
	r := httptest.NewRequest(http.MethodPost, "/api/run-all", strings.NewReader("{}"))
	r.Host = "192.0.2.10:18080"
	r.Header.Set("X-App-Token", svc.AppToken())
	r.Header.Set("Origin", "https://192.0.2.10:18080")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("same-origin HTTPS should be allowed: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestWriteRejectsSameHostOnDifferentPort(t *testing.T) {
	svc, handler := testServer(t)
	r := httptest.NewRequest(http.MethodPost, "/api/run-all", strings.NewReader("{}"))
	r.Host = "192.0.2.10:18080"
	r.Header.Set("X-App-Token", svc.AppToken())
	r.Header.Set("Origin", "http://192.0.2.10:19090")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("different origin port should be rejected: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAllowedOriginNormalizesDefaultPorts(t *testing.T) {
	s := &Server{}
	tests := []struct {
		name        string
		origin      string
		requestHost string
		want        bool
	}{
		{name: "http origin omits port", origin: "http://example.test", requestHost: "example.test:80", want: true},
		{name: "http host omits port", origin: "http://example.test:80", requestHost: "example.test", want: true},
		{name: "https origin omits port", origin: "https://example.test", requestHost: "example.test:443", want: true},
		{name: "https host omits port", origin: "https://example.test:443", requestHost: "example.test", want: true},
		{name: "uppercase scheme", origin: "HTTPS://EXAMPLE.TEST", requestHost: "example.test:443", want: true},
		{name: "http wrong explicit host port", origin: "http://example.test", requestHost: "example.test:443", want: false},
		{name: "https wrong explicit host port", origin: "https://example.test", requestHost: "example.test:80", want: false},
		{name: "explicit non-default origin port", origin: "https://example.test:8443", requestHost: "example.test", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.allowedOrigin(tt.origin, tt.requestHost); got != tt.want {
				t.Fatalf("allowedOrigin(%q, %q) = %v, want %v", tt.origin, tt.requestHost, got, tt.want)
			}
		})
	}
}

func TestMutatingEndpointsRejectMalformedJSON(t *testing.T) {
	svc, handler := testServer(t)
	paths := []string{"/api/login/start", "/api/run-all", "/api/refresh-points"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{} {}`))
			r.Header.Set("X-App-Token", svc.AppToken())
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("malformed JSON should be rejected: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestVersionEndpointsAllowOmittedBody(t *testing.T) {
	svc, handler := testServer(t)
	for _, path := range []string{"/api/run-all", "/api/refresh-points"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, nil)
			r.Header.Set("X-App-Token", svc.AppToken())
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("omitted body should use default version: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSettingsRejectsOversizedJSON(t *testing.T) {
	svc, handler := testServer(t)
	body := `{"activePrompt":"` + strings.Repeat("a", (1<<20)+1) + `"}`
	r := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body))
	r.Header.Set("X-App-Token", svc.AppToken())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized JSON should be rejected: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestExpiredLoginSessionReturnsGone(t *testing.T) {
	svc, handler := testServer(t)
	r := httptest.NewRequest(http.MethodPost, "/api/login/poll", strings.NewReader(`{"loginId":"missing"}`))
	r.Header.Set("X-App-Token", svc.AppToken())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusGone {
		t.Fatalf("expired login should terminate polling: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestLoginPollErrorStatusStopsTerminalErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "auth", err: &client.Error{Kind: client.KindAuthDead, Status: http.StatusUnauthorized}, want: http.StatusUnauthorized},
		{name: "not found", err: &client.Error{Kind: client.KindNotFound, Status: http.StatusNotFound}, want: http.StatusGone},
		{name: "forbidden", err: &client.Error{Kind: client.KindBusiness, Status: http.StatusForbidden, Msg: "login forbidden"}, want: http.StatusForbidden},
		{name: "rate limit", err: &client.Error{Kind: client.KindRate, Status: http.StatusTooManyRequests}, want: http.StatusTooManyRequests},
		{name: "transport", err: &client.Error{Kind: client.KindTransport}, want: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := loginPollErrorStatus(tt.err); got != tt.want {
				t.Fatalf("loginPollErrorStatus(%v)=%d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestLoginStartSessionLimitReturnsTooManyRequests(t *testing.T) {
	if got := loginStartErrorStatus(service.ErrLoginSessionLimit); got != http.StatusTooManyRequests {
		t.Fatalf("loginStartErrorStatus=%d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestFriendlyProtocolErrorIsSanitized(t *testing.T) {
	message := strings.Repeat("令牌响应 ", 100)
	got := friendlyError(&client.Error{Kind: client.KindProtocol, Msg: message})
	if len([]rune(got)) > 183 {
		t.Fatalf("protocol error was not bounded: %d runes", len([]rune(got)))
	}
}

func TestIsLoginPendingOnlyAcceptsExplicitAuthorizationPending(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "local concurrent poll", err: &client.Error{Kind: client.KindBusiness, Msg: "登录尚未完成：已有轮询请求正在处理"}, want: true},
		{name: "known code", err: &client.Error{Kind: client.KindBusiness, Status: http.StatusBadRequest, Code: 1001, Msg: "temporary"}, want: true},
		{name: "login in progress HTTP 200", err: &client.Error{Kind: client.KindBusiness, Status: http.StatusOK, Code: 11217, Msg: "login ing..."}, want: true},
		{name: "unrecognized HTTP 200 business code", err: &client.Error{Kind: client.KindBusiness, Status: http.StatusOK, Code: 1002, Msg: "login forbidden"}, want: false},
		{name: "known message", err: &client.Error{Kind: client.KindBusiness, Status: http.StatusConflict, Msg: "authorization pending"}, want: true},
		{name: "auth failure", err: &client.Error{Kind: client.KindAuthDead, Status: http.StatusUnauthorized, Msg: "waiting for login"}, want: false},
		{name: "forbidden waiting text", err: &client.Error{Kind: client.KindBusiness, Status: http.StatusForbidden, Msg: "waiting for authorization"}, want: false},
		{name: "not found waiting text", err: &client.Error{Kind: client.KindBusiness, Status: http.StatusNotFound, Msg: "waiting for login state"}, want: false},
		{name: "generic business", err: &client.Error{Kind: client.KindBusiness, Status: http.StatusBadRequest, Code: 1002, Msg: "bad request"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLoginPending(tt.err); got != tt.want {
				t.Fatalf("isLoginPending(%v)=%v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestMissingAccountMutationReturnsNotFound(t *testing.T) {
	svc, handler := testServer(t)
	r := httptest.NewRequest(http.MethodPut, "/api/accounts/missing", strings.NewReader(`{"nickname":"name"}`))
	r.Header.Set("X-App-Token", svc.AppToken())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "账号不存在") {
		t.Fatalf("missing account response: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestMissingAccountActionReturnsNotFound(t *testing.T) {
	svc, handler := testServer(t)
	r := httptest.NewRequest(http.MethodPost, "/api/accounts/missing/points", strings.NewReader(`{}`))
	r.Header.Set("X-App-Token", svc.AppToken())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "账号不存在") {
		t.Fatalf("missing account action response: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestSettingsResponseIsCanonicalAndSupportsClearingProxy(t *testing.T) {
	svc, handler := testServer(t)
	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body))
		r.Header.Set("X-App-Token", svc.AppToken())
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	first := put(`{"scheduleEnabled":true,"scheduleTime":"10:30","mode":"intl","activeModel":"hy4-preview","recheckDelayMinutes":90,"proxyUrl":"http://proxy.invalid:8080","activePrompt":"保留"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("initial settings failed: %d %s", first.Code, first.Body.String())
	}
	second := put(`{"proxyUrl":""}`)
	if second.Code != http.StatusOK {
		t.Fatalf("partial settings failed: %d %s", second.Code, second.Body.String())
	}
	var response struct {
		Settings model.Settings `json:"settings"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Settings.ProxyURL != "" || response.Settings.ScheduleTime != "10:30" || response.Settings.Mode != model.ModeIntl || response.Settings.ActivePrompt != "保留" {
		t.Fatalf("response was not canonical: %+v", response.Settings)
	}
	get := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	getW := httptest.NewRecorder()
	handler.ServeHTTP(getW, get)
	var state struct {
		Settings model.Settings `json:"settings"`
	}
	if err := json.Unmarshal(getW.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Settings != response.Settings {
		t.Fatalf("PUT response and GET state differ: put=%+v get=%+v", response.Settings, state.Settings)
	}
	if second.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing no-store header: %q", second.Header().Get("Cache-Control"))
	}
}

func TestSettingsAcceptsDirectProxySentinel(t *testing.T) {
	svc, handler := testServer(t)
	r := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"proxyUrl":"direct","scheduleTime":"10:30"}`))
	r.Header.Set("X-App-Token", svc.AppToken())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("direct proxy sentinel should be accepted: status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Settings model.Settings `json:"settings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Settings.ProxyURL != "" {
		t.Fatalf("direct sentinel was not canonicalized: %q", response.Settings.ProxyURL)
	}
	state, _ := svc.Snapshot()
	if state.Settings.ProxyURL != "" {
		t.Fatalf("direct sentinel leaked into in-memory state: %q", state.Settings.ProxyURL)
	}
}

func TestSettingsResponsesHideProxyCredentials(t *testing.T) {
	svc, handler := testServer(t)
	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body))
		r.Header.Set("X-App-Token", svc.AppToken())
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	configured := put(`{"proxyUrl":"socks5://proxy-user:proxy-secret@proxy.invalid:1080"}`)
	if configured.Code != http.StatusOK {
		t.Fatalf("proxy settings failed: %d %s", configured.Code, configured.Body.String())
	}
	if strings.Contains(configured.Body.String(), "proxy-user") || strings.Contains(configured.Body.String(), "proxy-secret") {
		t.Fatalf("PUT response exposed proxy credentials: %s", configured.Body.String())
	}
	var response struct {
		Settings model.Settings `json:"settings"`
	}
	if err := json.Unmarshal(configured.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Settings.ProxyURL != "socks5://proxy.invalid:1080" {
		t.Fatalf("unexpected public proxy URL: %q", response.Settings.ProxyURL)
	}

	get := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	getW := httptest.NewRecorder()
	handler.ServeHTTP(getW, get)
	if strings.Contains(getW.Body.String(), "proxy-user") || strings.Contains(getW.Body.String(), "proxy-secret") {
		t.Fatalf("state response exposed proxy credentials: %s", getW.Body.String())
	}

	// A partial settings update must preserve the private URL in storage even
	// though public responses only contain its credential-free form.
	updated := put(`{"scheduleTime":"10:40"}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("partial settings update failed: %d %s", updated.Code, updated.Body.String())
	}
	internal, _ := svc.Snapshot()
	if internal.Settings.ProxyURL != "socks5://proxy-user:proxy-secret@proxy.invalid:1080" {
		t.Fatalf("partial update replaced private proxy URL: %q", internal.Settings.ProxyURL)
	}
}

func TestSettingsRejectsUnknownStateFields(t *testing.T) {
	svc, handler := testServer(t)
	r := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"lastScheduledDay":"2099-01-01"}`))
	r.Header.Set("X-App-Token", svc.AppToken())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown internal field should be rejected: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestLegacyGlobalAccountIsGroupedIntoInternationalList(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir)
	state := model.NewState()
	state.Accounts = []model.Account{{
		ID:          "legacy-intl",
		UID:         "u-legacy-intl",
		Domain:      "workbuddy.ai",
		AccessToken: "secret",
		Enabled:     true,
		Status:      "ready",
	}}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	svc, err := service.New(st, client.New())
	if err != nil {
		t.Fatal(err)
	}
	handler := New(svc, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html><head></head></html>")}}, "", nil).Handler()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("state status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		CN   []model.PublicAccount `json:"cnAccounts"`
		Intl []model.PublicAccount `json:"intlAccounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.CN) != 0 || len(response.Intl) != 1 || response.Intl[0].Version != model.ModeIntl {
		t.Fatalf("legacy global account was grouped incorrectly: cn=%+v intl=%+v", response.CN, response.Intl)
	}
}

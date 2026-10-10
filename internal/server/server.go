package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"workbuddy-helper/internal/client"
	"workbuddy-helper/internal/model"
	"workbuddy-helper/internal/service"
)

type Server struct {
	service           *service.Service
	web               fs.FS
	host              string
	version           string
	allowedHosts      []string
	authUser          string
	authPassword      string
	authConfigured    bool
	authMisconfigured bool
	shutdown          func()
}

func New(svc *service.Service, web fs.FS, host string, shutdown func()) *Server {
	authUser := strings.TrimSpace(os.Getenv("WBH_AUTH_USER"))
	authPassword := os.Getenv("WBH_AUTH_PASSWORD")
	authConfigured := authUser != "" || authPassword != ""
	return &Server{
		service:           svc,
		web:               web,
		host:              host,
		version:           buildVersion,
		allowedHosts:      parseAllowedHosts(os.Getenv("WBH_ALLOWED_HOSTS")),
		authUser:          authUser,
		authPassword:      authPassword,
		authConfigured:    authConfigured,
		authMisconfigured: authConfigured && (authUser == "" || authPassword == ""),
		shutdown:          shutdown,
	}
}

// buildVersion 由 main 通过 SetVersion 注入（ldflags 构建注入）
var buildVersion = "dev"

// SetVersion 注入构建版本号（main 在启动时调用）
func SetVersion(v string) {
	if v != "" {
		buildVersion = v
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok "+buildVersion)
	})
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("POST /api/login/start", s.handleLoginStart)
	mux.HandleFunc("POST /api/login/poll", s.handleLoginPoll)
	mux.HandleFunc("POST /api/run-all", s.handleRunAll)
	mux.HandleFunc("POST /api/refresh-points", s.handleRefreshPoints)
	mux.HandleFunc("PUT /api/settings", s.handleSettings)
	mux.HandleFunc("POST /api/shutdown", s.handleShutdown)
	mux.HandleFunc("/api/accounts/", s.handleAccount)
	mux.HandleFunc("/", s.handleWeb)
	return s.securityHeaders(s.basicAuth(s.hostGuard(s.csrf(mux))))
}

// basicAuth is opt-in so local desktop use remains frictionless. When both
// WBH_AUTH_USER and WBH_AUTH_PASSWORD are set, every route except healthz is
// protected before the app token/CSRF checks run.
func (s *Server) basicAuth(next http.Handler) http.Handler {
	if !s.authConfigured {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authMisconfigured {
			status := http.StatusInternalServerError
			if r.URL.Path == "/healthz" {
				status = http.StatusServiceUnavailable
			}
			writeError(w, status, "WBH_AUTH_USER 与 WBH_AUTH_PASSWORD 必须同时设置")
			return
		}
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		user, password, ok := r.BasicAuth()
		userOK := ok && subtle.ConstantTimeCompare([]byte(user), []byte(s.authUser)) == 1
		passwordOK := ok && subtle.ConstantTimeCompare([]byte(password), []byte(s.authPassword)) == 1
		if !userOK || !passwordOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="WorkBuddy Helper"`)
			writeError(w, http.StatusUnauthorized, "需要登录面板")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostGuard prevents a DNS-rebinding page from loading the local UI under an
// attacker-controlled Host header and extracting the app token from index.html.
// An empty configured host is used by tests/embedded callers to opt out; the
// real entrypoint always passes listener.Addr().String().
func (s *Server) hostGuard(next http.Handler) http.Handler {
	if strings.TrimSpace(s.host) == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedRequestHost(r.Host) {
			writeError(w, http.StatusMisdirectedRequest, "请求主机不受信任")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func parseAllowedHosts(raw string) []string {
	var hosts []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if host, port := splitAuthority(item); host != "" {
			if port != "" {
				hosts = append(hosts, net.JoinHostPort(host, port))
			} else {
				hosts = append(hosts, host)
			}
		}
	}
	return hosts
}

func (s *Server) allowedRequestHost(raw string) bool {
	requestHost, requestPort := splitAuthority(raw)
	if requestHost == "" {
		return false
	}
	for _, allowed := range s.allowedHosts {
		allowedHost, allowedPort := splitAuthority(allowed)
		if allowedHost == requestHost && (allowedPort == "" || allowedPort == requestPort) {
			return true
		}
	}

	boundHost, boundPort := splitAuthority(s.host)
	if boundHost == "" {
		return false
	}
	if boundPort != "" && requestPort != "" && boundPort != requestPort {
		return false
	}
	if !isWildcardHost(boundHost) {
		if requestHost == boundHost {
			return true
		}
		return isLoopbackHost(boundHost) && isLoopbackHost(requestHost)
	}

	// A wildcard listener has no canonical hostname. Accept IP literals and
	// localhost aliases, but reject arbitrary DNS names unless explicitly
	// listed in WBH_ALLOWED_HOSTS.
	return isLoopbackHost(requestHost) || net.ParseIP(requestHost) != nil
}

func isWildcardHost(host string) bool {
	host = strings.Trim(host, "[]")
	return host == "" || host == "0.0.0.0" || host == "::"
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if r.Header.Get("X-App-Token") != s.service.AppToken() {
				writeError(w, http.StatusForbidden, "本地会话校验失败，请刷新页面")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && !s.allowedOrigin(origin, r.Host) {
				writeError(w, http.StatusForbidden, "请求来源不受信任")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) allowedOrigin(raw, requestHost string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	// 同源浏览器请求：Origin 主机与请求 Host 一致（含局域网 IP / 反代域名）。
	if (scheme == "http" || scheme == "https") && sameHost(u.Host, requestHost, scheme) {
		return true
	}
	// 本机回环访问始终放行。
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return (scheme == "http" || scheme == "https") && (host == "127.0.0.1" || host == "localhost" || host == "::1")
}

func sameHost(a, b, scheme string) bool {
	aHost, aPort := splitAuthority(a)
	bHost, bPort := splitAuthority(b)
	defaultPort := ""
	switch strings.ToLower(scheme) {
	case "http":
		defaultPort = "80"
	case "https":
		defaultPort = "443"
	}
	if aPort == "" {
		aPort = defaultPort
	}
	if bPort == "" {
		bPort = defaultPort
	}
	return aHost != "" && aHost == bHost && aPort == bPort
}

func splitAuthority(authority string) (string, string) {
	authority = strings.ToLower(strings.TrimSpace(authority))
	if host, port, err := net.SplitHostPort(authority); err == nil {
		return strings.TrimSuffix(strings.Trim(host, "[]"), "."), port
	}
	if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		authority = strings.Trim(authority, "[]")
	}
	return strings.TrimSuffix(authority, "."), ""
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	state, accounts := s.service.Snapshot()
	var balance, used, total int64
	enabled := 0
	for _, a := range accounts {
		balance += a.Balance
		used += a.Used
		total += a.Total
		if a.Enabled {
			enabled++
		}
	}
	cnAccounts := make([]model.PublicAccount, 0, len(accounts))
	intlAccounts := make([]model.PublicAccount, 0, len(accounts))
	for _, a := range accounts {
		if a.Version == model.ModeIntl {
			intlAccounts = append(intlAccounts, a)
		} else {
			cnAccounts = append(cnAccounts, a)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":     accounts,
		"cnAccounts":   cnAccounts,
		"intlAccounts": intlAccounts,
		"settings":     publicSettings(state.Settings),
		"summary": map[string]any{
			"accounts": len(accounts), "enabled": enabled, "balance": balance, "used": used, "total": total,
		},
		"serverTime": time.Now(),
	})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	} else if limit > 300 {
		limit = 300
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.service.Logs(limit)})
}

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version string `json:"version"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "登录请求格式不正确")
		return
	}
	id, authURL, err := s.service.StartLogin(strings.TrimSpace(body.Version))
	if err != nil {
		writeError(w, loginStartErrorStatus(err), friendlyError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"loginId": id, "authUrl": authURL})
}

func (s *Server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LoginID string `json:"loginId"`
	}
	if err := decodeJSON(r, &body); err != nil || body.LoginID == "" {
		writeError(w, http.StatusBadRequest, "缺少 loginId")
		return
	}
	account, err := s.service.PollLogin(body.LoginID)
	if err != nil {
		if errors.Is(err, service.ErrLoginSessionExpired) {
			writeError(w, http.StatusGone, err.Error())
			return
		}
		if isLoginPending(err) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "message": "等待你在浏览器完成登录"})
			return
		}
		// Preserve actionable login errors so the browser can stop polling.
		// A missing/invalid authorization state is terminal; transport and
		// upstream 5xx errors remain retryable (502).
		writeError(w, loginPollErrorStatus(err), friendlyError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "complete", "account": account})
}

func isLoginPending(err error) bool {
	var ce *client.Error
	if !errorsAs(err, &ce) || ce.Kind != client.KindBusiness {
		return false
	}
	// A second request for the same in-flight session is a local, transient
	// condition (it has no upstream HTTP status). Keep it pending without
	// broadening the 4xx rules below.
	if ce.Status == 0 && strings.TrimSpace(ce.Msg) == "登录尚未完成：已有轮询请求正在处理" {
		return true
	}
	// The upstream token endpoint uses HTTP 200 + code 11217 while the user
	// is still completing browser authorization.
	if ce.Code == 11217 {
		return true
	}
	// Keep this in sync with the client-side token poll predicate. An error
	// from a different 4xx endpoint must not turn into an endless pending loop
	// merely because its message happens to contain "waiting".
	if ce.Status != http.StatusBadRequest && ce.Status != http.StatusConflict {
		return false
	}
	if ce.Code == 1001 {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(ce.Msg))
	for _, marker := range []string{
		"authorization pending", "login pending", "waiting for authorization",
		"waiting for login", "not yet authorized", "尚未完成", "等待授权",
		"等待登录", "授权中", "登录中", "请先在浏览器",
	} {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

func loginPollErrorStatus(err error) int {
	if errors.Is(err, service.ErrLoginSessionExpired) {
		return http.StatusGone
	}
	var ce *client.Error
	if !errors.As(err, &ce) {
		return http.StatusBadGateway
	}
	switch ce.Kind {
	case client.KindAuthDead:
		return http.StatusUnauthorized
	case client.KindNotFound:
		return http.StatusGone
	case client.KindRate:
		return http.StatusTooManyRequests
	case client.KindBusiness:
		// Preserve terminal client errors (400/403/409/404). Explicit pending
		// responses are handled before this function and return 200.
		if ce.Status >= 400 && ce.Status < 500 {
			return ce.Status
		}
	}
	return http.StatusBadGateway
}

func loginStartErrorStatus(err error) int {
	if errors.Is(err, service.ErrLoginSessionLimit) || client.IsKind(err, client.KindRate) {
		return http.StatusTooManyRequests
	}
	return http.StatusBadGateway
}

func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}

func (s *Server) handleRunAll(w http.ResponseWriter, r *http.Request) {
	// 按前端当前查看的版本列表执行：cn=全部签到，intl=全部活跃
	var body struct {
		Version string `json:"version"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "批量任务请求格式不正确")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.service.RunAll(strings.TrimSpace(body.Version))})
}

func (s *Server) handleRefreshPoints(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version string `json:"version"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "积分刷新请求格式不正确")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.service.RefreshAllPoints(strings.TrimSpace(body.Version))})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var patch model.SettingsPatch
	if err := decodeJSON(r, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "设置格式不正确")
		return
	}
	settings, err := s.service.UpdateSettings(patch)
	if err != nil {
		if service.IsSettingsValidation(err) {
			writeError(w, http.StatusBadRequest, err.Error())
		} else {
			log.Printf("settings save failed: %v", err)
			writeError(w, http.StatusInternalServerError, "设置保存失败，请检查容器 data 目录是否可写")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": publicSettings(settings)})
}

func publicSettings(settings model.Settings) model.Settings {
	settings.ProxyURL = proxyURLWithoutCredentials(settings.ProxyURL)
	return settings
}

func proxyURLWithoutCredentials(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	u.User = nil
	return u.String()
}

func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "stopping"})
	if s.shutdown != nil {
		go func() {
			time.Sleep(200 * time.Millisecond)
			s.shutdown()
		}()
	}
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodDelete:
			if err := s.service.DeleteAccount(id); err != nil {
				s.writeAccountMutationError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
		case http.MethodPut:
			var body struct {
				Enabled  *bool   `json:"enabled"`
				Alias    *string `json:"alias"`
				Nickname *string `json:"nickname"`
			}
			if err := decodeJSON(r, &body); err != nil {
				writeError(w, http.StatusBadRequest, "账号设置格式不正确")
				return
			}
			if body.Alias == nil {
				body.Alias = body.Nickname
			}
			if err := s.service.UpdateAccount(id, body.Alias, body.Enabled); err != nil {
				s.writeAccountMutationError(w, err)
				return
			}
			account, _ := s.service.AccountByID(id)
			writeJSON(w, http.StatusOK, map[string]any{"account": account})
		default:
			writeError(w, http.StatusMethodNotAllowed, "不支持的操作")
		}
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "接口不存在")
		return
	}
	var account model.PublicAccount
	var err error
	switch parts[1] {
	case "checkin":
		account, err = s.service.Checkin(id)
	case "active":
		// 手动触发视为强制：用户主动点击即真实发送（绕过"今日已活跃"跳过）
		account, err = s.service.ActiveChat(id, true)
	case "points":
		account, err = s.service.Points(id)
	default:
		writeError(w, http.StatusNotFound, "接口不存在")
		return
	}
	if err != nil {
		writeJSON(w, statusFor(err), map[string]any{"error": friendlyError(err), "account": account})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": account})
}

func (s *Server) writeAccountMutationError(w http.ResponseWriter, err error) {
	if errors.Is(err, service.ErrAccountNotFound) {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	log.Printf("account save failed: %v", err)
	writeError(w, http.StatusInternalServerError, "账号保存失败，请检查容器 data 目录是否可写")
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, service.ErrAccountNotFound):
		return http.StatusNotFound
	case client.IsKind(err, client.KindAuthDead):
		return http.StatusUnauthorized
	case client.IsKind(err, client.KindCredit):
		return http.StatusPaymentRequired
	case client.IsKind(err, client.KindRate):
		return http.StatusTooManyRequests
	case client.IsKind(err, client.KindServer), client.IsKind(err, client.KindTransport):
		return http.StatusBadGateway
	default:
		return http.StatusBadRequest
	}
}

func friendlyError(err error) string {
	if errors.Is(err, service.ErrAccountNotFound) {
		return "账号不存在"
	}
	var ce *client.Error
	if errorsAs(err, &ce) {
		switch ce.Kind {
		case client.KindAuthDead:
			return "登录已失效，请删除后重新添加该账号"
		case client.KindCredit:
			return "积分已耗尽，请先获取积分（等待每日活跃奖励到账或购买套餐）后再试"
		case client.KindRate:
			return "请求过于频繁，请稍后再试"
		case client.KindNotFound:
			return "登录会话不存在或已失效，请重新开始登录"
		case client.KindServer:
			return "WorkBuddy 服务暂时不可用"
		case client.KindTransport:
			return "无法连接 WorkBuddy，请检查网络"
		case client.KindProtocol:
			return sanitizePublic(ce.Msg)
		}
	}
	return sanitizePublic(err.Error())
}

func sanitizePublic(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if runes := []rune(message); len(runes) > 180 {
		message = string(runes[:180]) + "..."
	}
	return message
}

func decodeJSON(r *http.Request, dst any) error {
	return decodeJSONWithEmpty(r, dst, false)
}

// decodeOptionalJSON preserves the API behavior where an omitted body means
// "use defaults" for endpoints whose fields are optional.
func decodeOptionalJSON(r *http.Request, dst any) error {
	return decodeJSONWithEmpty(r, dst, true)
}

func decodeJSONWithEmpty(r *http.Request, dst any, allowEmpty bool) error {
	defer r.Body.Close()
	const maxJSONBody = 1 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxJSONBody {
		return fmt.Errorf("request body exceeds %d bytes", maxJSONBody)
	}
	if allowEmpty && len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("request body contains multiple JSON values")
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func (s *Server) handleWeb(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	if strings.Contains(path, "..") || strings.HasPrefix(path, "api/") {
		http.NotFound(w, r)
		return
	}
	if path == "index.html" {
		raw, err := fs.ReadFile(s.web, "index.html")
		if err != nil {
			http.Error(w, "UI unavailable", http.StatusInternalServerError)
			return
		}
		injection := fmt.Sprintf(`<meta name="app-token" content="%s">`, htmlEscapeAttr(s.service.AppToken()))
		html := strings.Replace(string(raw), "</head>", injection+"</head>", 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, html)
		return
	}
	raw, err := fs.ReadFile(s.web, path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	contentType := "application/octet-stream"
	if strings.HasSuffix(path, ".js") {
		contentType = "text/javascript; charset=utf-8"
	} else if strings.HasSuffix(path, ".css") {
		contentType = "text/css; charset=utf-8"
	} else if strings.HasSuffix(path, ".svg") {
		contentType = "image/svg+xml"
	}
	w.Header().Set("Content-Type", contentType)
	// Static assets are embedded into the executable and may change on every
	// deployment. Avoid a stale cached app.js keeping old API/settings logic.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

func htmlEscapeAttr(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, `"`, "&quot;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	return value
}

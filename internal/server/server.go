package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"workbuddy-helper/internal/client"
	"workbuddy-helper/internal/model"
	"workbuddy-helper/internal/service"
)

type Server struct {
	service  *service.Service
	web      fs.FS
	host     string
	version  string
	shutdown func()
}

func New(svc *service.Service, web fs.FS, host string, shutdown func()) *Server {
	return &Server{service: svc, web: web, host: host, version: buildVersion, shutdown: shutdown}
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
	return s.securityHeaders(s.csrf(mux))
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
	if err != nil {
		return false
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = u.Host
	}
	// 同源浏览器请求：Origin 主机与请求 Host 一致（含局域网 IP / 反代域名）。
	if u.Scheme == "http" && sameHost(host, requestHost) {
		return true
	}
	// 本机回环访问始终放行。
	return u.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "[::1]")
}

func sameHost(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	aHost, _, errA := net.SplitHostPort(a)
	if errA == nil {
		a = aHost
	}
	bHost, _, errB := net.SplitHostPort(b)
	if errB == nil {
		b = bHost
	}
	return a == b
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
		"settings":     state.Settings,
		"summary": map[string]any{
			"accounts": len(accounts), "enabled": enabled, "balance": balance, "used": used, "total": total,
		},
		"serverTime": time.Now(),
	})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 100
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.service.Logs(limit)})
}

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version string `json:"version"`
	}
	_ = decodeJSON(r, &body)
	id, authURL, err := s.service.StartLogin(strings.TrimSpace(body.Version))
	if err != nil {
		writeError(w, http.StatusBadGateway, friendlyError(err))
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
		if isLoginPending(err) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "message": "等待你在浏览器完成登录"})
			return
		}
		writeError(w, http.StatusBadGateway, friendlyError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "complete", "account": account})
}

func isLoginPending(err error) bool {
	var ce *client.Error
	if !errorsAs(err, &ce) || ce.Kind != client.KindBusiness {
		return false
	}
	lower := strings.ToLower(ce.Msg)
	return strings.Contains(lower, "login ing") || strings.Contains(lower, "waiting") || strings.Contains(lower, "尚未完成") || strings.Contains(lower, "请先在浏览器")
}

func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}

func (s *Server) handleRunAll(w http.ResponseWriter, r *http.Request) {
	// 按前端当前查看的版本列表执行：cn=全部签到，intl=全部活跃
	var body struct {
		Version string `json:"version"`
	}
	_ = decodeJSON(r, &body)
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.service.RunAll(strings.TrimSpace(body.Version))})
}

func (s *Server) handleRefreshPoints(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version string `json:"version"`
	}
	_ = decodeJSON(r, &body)
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.service.RefreshAllPoints(strings.TrimSpace(body.Version))})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var settings model.Settings
	if err := decodeJSON(r, &settings); err != nil {
		writeError(w, http.StatusBadRequest, "设置格式不正确")
		return
	}
	if err := s.service.UpdateSettings(settings); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
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
				writeError(w, http.StatusNotFound, err.Error())
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
				writeError(w, http.StatusNotFound, err.Error())
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

func statusFor(err error) int {
	switch {
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
	var ce *client.Error
	if errorsAs(err, &ce) {
		switch ce.Kind {
		case client.KindAuthDead:
			return "登录已失效，请删除后重新添加该账号"
		case client.KindCredit:
			return "积分已耗尽，请先获取积分（等待每日活跃奖励到账或购买套餐）后再试"
		case client.KindRate:
			return "请求过于频繁，请稍后再试"
		case client.KindServer:
			return "WorkBuddy 服务暂时不可用"
		case client.KindTransport:
			return "无法连接 WorkBuddy，请检查网络"
		case client.KindProtocol:
			return ce.Msg
		}
	}
	return sanitizePublic(err.Error())
}

func sanitizePublic(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 180 {
		message = message[:180] + "..."
	}
	return message
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
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
	_, _ = w.Write(raw)
}

func htmlEscapeAttr(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, `"`, "&quot;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	return value
}

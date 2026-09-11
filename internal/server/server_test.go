package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"workbuddy-helper/internal/client"
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
	return svc, New(svc, root, "127.0.0.1:1234", nil).Handler()
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

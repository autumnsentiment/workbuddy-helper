package client

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy-helper/internal/model"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{"application/json"}}}
}

func testClient(rt roundTripFunc) *Client {
	return &Client{HTTP: &http.Client{Transport: rt}, ChatBaseCN: "https://chat.example", BillingBaseCN: "https://billing.example", GlobalBase: "https://global.example"}
}

func TestPointsAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "get-user-resource") {
			return nil, errors.New("unexpected path")
		}
		return response(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"PackageName":"签到包","CycleCapacitySize":1000,"CycleCapacityRemain":700,"CycleCapacityUsed":300},{"PackageName":"体验包","CapacitySize":500,"CapacityRemain":200,"CapacityUsed":300}]}}}}`), nil
	})
	got, err := c.Points(&model.Account{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Balance != 900 || got.Used != 600 || got.Total != 1500 || len(got.Packages) != 2 {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestClassifyAuth(t *testing.T) {
	if !IsKind(classifyHTTP(401, `{"code":12153,"msg":"Offline user session not found"}`), KindAuthDead) {
		t.Fatal("expected auth dead")
	}
	if !IsKind(classifyHTTP(429, "slow down"), KindRate) {
		t.Fatal("expected rate")
	}
	if !IsKind(classifyBusiness(429, 10002, "请求频繁"), KindRate) {
		t.Fatal("expected JSON rate response to remain rate limited")
	}
}

func TestDailyCheckinAlreadyHTTPErrorKeepsBusinessCode(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return response(400, `{"code":10001,"msg":"今天已签到"}`), nil
	})
	err := c.DailyCheckin(&model.Account{AccessToken: "at", UID: "u1"})
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != 10001 || !strings.Contains(ce.Msg, "已签到") {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestRefreshDoesNotFollowRedirect(t *testing.T) {
	called := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		called++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://evil.example/steal"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
		}, nil
	})
	c.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	err := c.Refresh(&model.Account{RefreshToken: "sensitive"})
	if err == nil || called != 1 {
		t.Fatalf("redirect should fail without following: calls=%d err=%v", called, err)
	}
}

func TestActiveChatSendsClientConversation(t *testing.T) {
	var gotPath, gotUA, gotProduct, gotAuth, gotUID, gotBody, gotIDEType string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotIDEType = r.Header.Get("X-IDE-Type")
		gotPath = r.URL.Path
		gotUA = r.Header.Get("User-Agent")
		gotProduct = r.Header.Get("X-Product")
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		return response(200, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"), nil
	})
	err := c.ActiveChat(&model.Account{AccessToken: "at", UID: "u1", Domain: "workbuddy.ai"}, "hy4-preview")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v2/chat/completions" {
		t.Fatalf("path=%s", gotPath)
	}
	if gotUA != IntlClientUA {
		t.Fatalf("活跃请求必须带 WorkBuddy 桌面端 UA，got=%q", gotUA)
	}
	if gotProduct != "SaaS" {
		t.Fatalf("缺少客户端特征头 X-Product: %q", gotProduct)
	}
	if gotAuth != "Bearer at" || gotUID != "u1" {
		t.Fatalf("auth=%q uid=%q", gotAuth, gotUID)
	}
	if gotIDEType != IntlClientName {
		t.Fatalf("X-IDE-Type=%q, want %q（plans-usage 按此识别客户端）", gotIDEType, IntlClientName)
	}
	if !strings.Contains(gotBody, `"stream":true`) || !strings.Contains(gotBody, `"model":"hy4-preview"`) {
		t.Fatalf("body=%s", gotBody)
	}
}

func TestValidateProxyURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"", true},
		{"http://192.168.5.1:8080", true},
		{"https://proxy.example.com:3128", true},
		{"socks5://192.168.5.1:1070", true},
		{"socks5h://user:pass@gateway:1080", true},
		{"ftp://bad.example:21", false},
		{"not a url", false},
		{"://missing-scheme", false},
	}
	for _, c := range cases {
		err := ValidateProxyURL(c.url)
		if (err == nil) != c.want {
			t.Errorf("ValidateProxyURL(%q) err=%v wantOK=%v", c.url, err, c.want)
		}
	}
}

func TestNewWithProxySocks5Transport(t *testing.T) {
	c, err := NewWithProxy("socks5://192.168.5.1:1070")
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP == nil || c.HTTP.Transport == nil {
		t.Fatal("expected transport")
	}
	if _, err := NewWithProxy("ftp://bad:21"); err == nil {
		t.Fatal("invalid scheme should be rejected")
	}
	// 空代理等价直连
	direct, err := NewWithProxy("")
	if err != nil || direct == nil {
		t.Fatalf("empty proxy should succeed: %v", err)
	}
}

func TestNewWithProxyHTTPTunneling(t *testing.T) {
	// 起一个本地 HTTP 代理，验证 http:// 代理真的被使用
	var proxied bool
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = true
		if r.Method == http.MethodConnect {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxySrv.Close()
	c, err := NewWithProxy(proxySrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c.ChatBaseCN = "http://upstream.invalid"
	_, _ = c.HTTP.Get("http://upstream.invalid/x") // 结果不重要，验证是否经过代理
	if !proxied {
		t.Fatal("request did not go through the configured http proxy")
	}
}

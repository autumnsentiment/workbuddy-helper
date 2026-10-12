package client

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy-helper/internal/model"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{"application/json"}}}
}

func testClient(rt roundTripFunc) *Client {
	return &Client{HTTP: &http.Client{
		Transport: rt,
		// Production clients never follow upstream redirects. Keep the test
		// transport on the same contract so a 3xx response is classified by
		// the client instead of being retried against an attacker-controlled URL.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, ChatBaseCN: "https://chat.example", BillingBaseCN: "https://billing.example", GlobalBase: "https://global.example"}
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

func TestPointsFetchesAllPages(t *testing.T) {
	var requestedPages []int
	c := testClient(func(r *http.Request) (*http.Response, error) {
		var request struct {
			PageNumber int `json:"PageNumber"`
			PageSize   int `json:"PageSize"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requestedPages = append(requestedPages, request.PageNumber)
		if request.PageSize != pointsPageSize {
			t.Fatalf("PageSize=%d", request.PageSize)
		}
		count := pointsPageSize
		if request.PageNumber == 2 {
			count = 1
		}
		accounts := make([]map[string]any, count)
		for i := range accounts {
			accounts[i] = map[string]any{
				"PackageName":         "积分包",
				"CycleCapacitySize":   10,
				"CycleCapacityRemain": 7,
				"CycleCapacityUsed":   3,
				"CycleEndTime":        "2026-12-31 23:59:59",
			}
		}
		raw, err := json.Marshal(map[string]any{
			"code": 0,
			"data": map[string]any{"Response": map[string]any{"Data": map[string]any{
				"TotalCount": 101,
				"Accounts":   accounts,
			}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return response(http.StatusOK, string(raw)), nil
	})

	got, err := c.Points(&model.Account{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(requestedPages) != 2 || requestedPages[0] != 1 || requestedPages[1] != 2 {
		t.Fatalf("requested pages=%v", requestedPages)
	}
	if got.Balance != 707 || got.Used != 303 || got.Total != 1010 || len(got.Packages) != 101 {
		t.Fatalf("unexpected paged result: %+v packages=%d", got, len(got.Packages))
	}
	if got.Packages[100].ExpiresAt != "2026-12-31 23:59:59" {
		t.Fatalf("CycleEndTime fallback was not used: %+v", got.Packages[100])
	}
}

func TestPointsRejectsIncompletePagination(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		var request struct {
			PageNumber int `json:"PageNumber"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		accounts := make([]map[string]any, 0)
		if request.PageNumber == 1 {
			accounts = make([]map[string]any, pointsPageSize)
			for i := range accounts {
				accounts[i] = map[string]any{"PackageName": "积分包", "CapacitySize": 1, "CapacityRemain": 1}
			}
		}
		raw, _ := json.Marshal(map[string]any{
			"code": 0,
			"data": map[string]any{"Response": map[string]any{"Data": map[string]any{
				"TotalCount": 101,
				"Accounts":   accounts,
			}}},
		})
		return response(http.StatusOK, string(raw)), nil
	})

	_, err := c.Points(&model.Account{AccessToken: "at", UID: "u1"})
	if !IsKind(err, KindProtocol) || !strings.Contains(err.Error(), "分页不完整") {
		t.Fatalf("expected incomplete pagination protocol error, got %v", err)
	}
}

func TestPointsRejectsMissingAccountsShape(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"code":0,"data":{"Response":{"Data":{"TotalCount":0}}}}`), nil
	})
	_, err := c.Points(&model.Account{AccessToken: "at", UID: "u1"})
	if !IsKind(err, KindProtocol) || !strings.Contains(err.Error(), "缺少套餐列表") {
		t.Fatalf("expected missing accounts protocol error, got %v", err)
	}
}

func TestPointsWithoutTotalCountProbesPastFullPage(t *testing.T) {
	var requestedPages []int
	c := testClient(func(r *http.Request) (*http.Response, error) {
		var request struct {
			PageNumber int `json:"PageNumber"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requestedPages = append(requestedPages, request.PageNumber)
		count := 100
		if request.PageNumber == 2 {
			count = 1
		}
		accounts := make([]map[string]any, count)
		for i := range accounts {
			accounts[i] = map[string]any{"PackageName": "积分包", "CapacitySize": 1, "CapacityRemain": 1}
		}
		raw, _ := json.Marshal(map[string]any{
			"code": 0,
			"data": map[string]any{"Response": map[string]any{"Data": map[string]any{
				"Accounts": accounts,
			}}},
		})
		return response(http.StatusOK, string(raw)), nil
	})

	got, err := c.Points(&model.Account{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(requestedPages) != 2 || requestedPages[0] != 1 || requestedPages[1] != 2 {
		t.Fatalf("requested pages=%v", requestedPages)
	}
	if len(got.Packages) != 101 {
		t.Fatalf("expected all packages, got %d", len(got.Packages))
	}
}

func TestCloneWithTransportSafeDefaultsWhenSourceHTTPMissing(t *testing.T) {
	clone := CloneWithTransport(&Client{}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, "{}"), nil
	}))
	if clone.HTTP == nil || clone.HTTP.Timeout != 30*time.Second || clone.HTTP.CheckRedirect == nil {
		t.Fatalf("clone defaults not preserved: %#v", clone.HTTP)
	}
}

func TestNewWithCNTransportPreservesCallerOwnedHTTPTransport(t *testing.T) {
	// A caller may inject a *http.Transport for custom TLS, proxy, or tracing.
	// It must not be mistaken for the package-owned default merely because it
	// has the same concrete type.
	custom := &http.Transport{MaxIdleConns: 7}
	caller := &Client{HTTP: &http.Client{Transport: custom}}
	if got := NewWithCNTransport(caller); got != nil {
		t.Fatal("custom *http.Transport was unexpectedly replaced")
	}

	owned := New()
	if owned.ownedTransport == nil {
		t.Fatal("New did not record its owned transport")
	}
	if got := NewWithCNTransport(owned); got == nil {
		t.Fatal("package-owned transport was not replaced")
	}
}

func TestClientRoutingUsesEffectiveAccountVersion(t *testing.T) {
	c := &Client{
		ChatBaseCN:    "https://cn-chat.example",
		BillingBaseCN: "https://cn-billing.example",
		GlobalBase:    "https://intl-chat.example",
		GlobalBilling: "https://intl-billing.example",
	}
	intl := &model.Account{Version: model.ModeIntl, Domain: "codebuddy.cn"}
	if got := c.base(intl, false); got != c.GlobalBase {
		t.Fatalf("explicit international account used chat endpoint %q", got)
	}
	if got := c.base(intl, true); got != c.GlobalBilling {
		t.Fatalf("explicit international account used billing endpoint %q", got)
	}
	cn := &model.Account{Version: model.ModeCN, Domain: "workbuddy.ai"}
	if got := c.base(cn, false); got != c.ChatBaseCN {
		t.Fatalf("explicit domestic account used chat endpoint %q", got)
	}
}

func TestClassifyAuth(t *testing.T) {
	if !IsKind(classifyHTTP(401, `{"code":12153,"msg":"Offline user session not found"}`), KindAuthDead) {
		t.Fatal("expected auth dead")
	}
	if !IsKind(classifyHTTP(400, `{"error":"User session not found"}`), KindAuthDead) {
		t.Fatal("session-not-found HTTP errors should invalidate the account")
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

func TestRefreshUsesJWTExpiryClaim(t *testing.T) {
	const expiry int64 = 1893456000
	// The signature is intentionally not verified by the helper; this fixture
	// only exercises extraction of the NumericDate `exp` claim for display.
	const accessToken = "eyJhbGciOiJub25lIn0.eyJleHAiOjE4OTM0NTYwMDB9.signature"
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			return nil, errors.New("unexpected path: " + r.URL.Path)
		}
		return response(http.StatusOK, `{"code":0,"data":{"accessToken":"`+accessToken+`","refreshToken":"next-refresh","expiresIn":3600}}`), nil
	})
	account := &model.Account{RefreshToken: "old-refresh"}
	if err := c.Refresh(account); err != nil {
		t.Fatal(err)
	}
	if account.AccessToken != accessToken || account.RefreshToken != "next-refresh" {
		t.Fatalf("refresh credentials not updated: %+v", account)
	}
	if account.ExpiresAt != expiry {
		t.Fatalf("expiresAt=%d, want JWT exp %d", account.ExpiresAt, expiry)
	}
}

func TestActiveChatSendsClientConversation(t *testing.T) {
	var gotPath, gotUA, gotProduct, gotAuth, gotUID, gotBody, gotIDEType string
	var paths []string
	var reportBody, convIDSeen, createBody, createConvID string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v2/report" {
			raw, _ := io.ReadAll(r.Body)
			reportBody = string(raw)
			return response(200, `{"code":0}`), nil
		}
		if r.URL.Path == "/console/as/conversations/v2" {
			raw, _ := io.ReadAll(r.Body)
			createBody = string(raw)
			createConvID = r.Header.Get("X-Conversation-ID")
			return response(200, `{"code":0,"data":{"id":"server-conv-1","sessionId":"server-conv-1"}}`), nil
		}
		gotIDEType = r.Header.Get("X-IDE-Type")
		convIDSeen = r.Header.Get("X-Conversation-ID")
		gotPath = r.URL.Path
		gotUA = r.Header.Get("User-Agent")
		gotProduct = r.Header.Get("X-Product")
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		return response(200, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"), nil
	})
	err := c.ActiveChat(&model.Account{AccessToken: "at", UID: "u1", Domain: "workbuddy.ai"}, "hy4-preview", "")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v2/chat/completions" {
		t.Fatalf("path=%s", gotPath)
	}
	// 活跃闭环：先登记云端会话，再发对话，最后补遥测（对齐真实客户端）。
	if len(paths) != 3 || paths[0] != "/console/as/conversations/v2" || paths[1] != "/v2/chat/completions" || paths[2] != "/v2/report" {
		t.Fatalf("expected create->chat->report, got paths=%v", paths)
	}
	// 会话登记请求体必须带本地 sessionId（服务端据此登记新会话）
	var createPayload struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal([]byte(createBody), &createPayload); err != nil {
		t.Fatalf("会话登记请求体不是有效 JSON: %v body=%s", err, createBody)
	}
	if createPayload.Type != "local" || createPayload.SessionID == "" || createPayload.Name == "" {
		t.Fatalf("会话登记请求体不完整: %s", createBody)
	}
	// 会话登记请求不应携带会话维度头（会话尚未创建）
	if createConvID != "" {
		t.Fatalf("会话登记请求不应带 X-Conversation-ID: %q", createConvID)
	}
	// 对话请求使用服务端返回的会话 ID
	if convIDSeen != "server-conv-1" {
		t.Fatalf("对话请求应使用服务端会话 ID，got=%q", convIDSeen)
	}
	if !strings.Contains(reportBody, "chat_request_send") || !strings.Contains(reportBody, convIDSeen) {
		t.Fatalf("report body missing event: %s", reportBody)
	}
	// /v2/report 需要扁平事件数组（eventCode 在顶层）；嵌套 {code,event}
	// 会被服务端判为 "eventCode is empty or not string"。
	var reportEvents []map[string]any
	if err := json.Unmarshal([]byte(reportBody), &reportEvents); err != nil {
		t.Fatalf("report body is not a JSON array: %v body=%s", err, reportBody)
	}
	if len(reportEvents) != 1 {
		t.Fatalf("report should carry exactly one event, got %d", len(reportEvents))
	}
	if code, _ := reportEvents[0]["eventCode"].(string); code != "chat_request_send" {
		t.Fatalf("eventCode must be a top-level string, got %#v", reportEvents[0]["eventCode"])
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
	// hy3/hy4-preview 是 onlyReasoning 纯推理模型：推理内容同样占用输出预算，
	// 预算过小会让 content 为空。必须与真实客户端同量级地下发 max_tokens。
	var sentBody struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal([]byte(gotBody), &sentBody); err != nil {
		t.Fatalf("请求体不是有效 JSON: %v", err)
	}
	if sentBody.MaxTokens < 2048 {
		t.Fatalf("max_tokens=%d 过小，推理模型会因推理耗尽预算而返回空 content", sentBody.MaxTokens)
	}
	if !strings.Contains(gotBody, DefaultActivePrompt) {
		t.Fatalf("默认提示词缺失: %s", gotBody)
	}
	// 默认提示词即实质提问（单字问候会被服务端判为非有效会话）
	if !strings.Contains(DefaultActivePrompt, "你是谁") {
		t.Fatalf("默认提示词应为实质提问: %q", DefaultActivePrompt)
	}
	// 自定义提示词覆盖
	gotBody = ""
	err = c.ActiveChat(&model.Account{AccessToken: "at", UID: "u1", Domain: "workbuddy.ai"}, "hy3", "自定义测试提问")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, "自定义测试提问") || strings.Contains(gotBody, DefaultActivePrompt) {
		t.Fatalf("自定义提示词未生效: %s", gotBody)
	}
}

func TestActiveChatCreatesFreshConversationEachTime(t *testing.T) {
	// 每次活跃都必须在云端新开一个会话，绝不复用上一次的会话 ID。
	var sessionIDs []string
	var convIDs []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/console/as/conversations/v2":
			raw, _ := io.ReadAll(r.Body)
			var payload struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(raw, &payload)
			sessionIDs = append(sessionIDs, payload.SessionID)
			// 服务端为每个本地会话分配独立 ID
			serverID := "server-" + payload.SessionID
			return response(200, `{"code":0,"data":{"id":"`+serverID+`","sessionId":"`+serverID+`"}}`), nil
		case "/v2/report":
			return response(200, `{"code":0}`), nil
		default:
			convIDs = append(convIDs, r.Header.Get("X-Conversation-ID"))
			return response(200, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"), nil
		}
	})
	account := &model.Account{AccessToken: "at", UID: "u1", Domain: "workbuddy.ai"}
	for i := 0; i < 2; i++ {
		if err := c.ActiveChat(account, "hy3", ""); err != nil {
			t.Fatalf("第 %d 次活跃失败: %v", i+1, err)
		}
	}
	if len(sessionIDs) != 2 || len(convIDs) != 2 {
		t.Fatalf("expected 2 create+chat rounds, sessions=%v convs=%v", sessionIDs, convIDs)
	}
	if sessionIDs[0] == sessionIDs[1] {
		t.Fatalf("两次活跃复用了同一本地会话: %v", sessionIDs)
	}
	if convIDs[0] == convIDs[1] {
		t.Fatalf("两次活跃复用了同一服务端会话: %v", convIDs)
	}
}

func TestActiveChatConversationFailureStillChats(t *testing.T) {
	// 会话登记失败（例如接口不可用）不应阻断对话，但必须回传警告。
	var chatCalled bool
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/console/as/conversations/v2":
			return response(http.StatusServiceUnavailable, `{"code":10001,"msg":"unavailable"}`), nil
		case "/v2/report":
			return response(200, `{"code":0}`), nil
		default:
			chatCalled = true
			return response(200, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"), nil
		}
	})
	result, err := c.ActiveChatWithResult(&model.Account{AccessToken: "at", UID: "u1", Domain: "workbuddy.ai"}, "hy3", "")
	if err != nil {
		t.Fatalf("会话登记失败不应阻断对话: %v", err)
	}
	if !chatCalled {
		t.Fatal("会话登记失败后未继续发送对话")
	}
	if result.ConversationWarning == nil {
		t.Fatal("会话登记失败必须回传警告")
	}
}

func TestActiveChatAcceptsReasoningOnlyStream(t *testing.T) {
	// hy3 等 onlyReasoning 模型的流式回答可能只带 reasoning_content
	// （推理尚未产出正式 content 即被判定为有效会话）。
	for _, body := range []string{
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"reasoning\":\"thinking\"}}]}\n\n",
		"data: {\"choices\":[{\"message\":{\"reasoning_content\":\"thinking\"}}]}\n\n",
		// 整段 JSON（非 SSE）与 typed block 数组形式的 content
		"{\"choices\":[{\"message\":{\"content\":\"hello\"}}]}",
		"data: {\"choices\":[{\"delta\":{\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}]}\n\n",
	} {
		c := testClient(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/v2/report" {
				return response(200, `{"code":0}`), nil
			}
			return response(http.StatusOK, body), nil
		})
		err := c.ActiveChat(&model.Account{AccessToken: "at", Version: model.ModeIntl}, "hy3", "")
		if err != nil {
			t.Fatalf("推理内容应被认定为有效回答: body=%q err=%v", body, err)
		}
	}
}

func TestActiveChatEmptyStreamReportsResponseSnippet(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v2/report" {
			return response(200, `{"code":0}`), nil
		}
		return response(http.StatusOK, "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}],\"note\":\"upstream-empty\"}\n\n"), nil
	})
	err := c.ActiveChat(&model.Account{AccessToken: "at", Version: model.ModeIntl}, "hy3", "")
	if err == nil || !IsKind(err, KindProtocol) {
		t.Fatalf("空回答应判定为协议错误: %v", err)
	}
	// 失败信息需带响应片段，便于线上定位上游真实返回结构。
	if !strings.Contains(err.Error(), "upstream-empty") {
		t.Fatalf("错误信息缺少响应片段: %v", err)
	}
}

func TestActiveChatRejectsRedirectStatus(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://evil.example/"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
		}, nil
	})
	c.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	err := c.ActiveChat(&model.Account{AccessToken: "at", UID: "u1", Domain: "workbuddy.ai"}, "hy3", "test prompt")
	if err == nil || !IsKind(err, KindBusiness) {
		t.Fatalf("redirect response was accepted: %v", err)
	}
}

func TestActiveChatRejectsStreamWithoutAnswer(t *testing.T) {
	for _, body := range []string{
		"",
		"data: [DONE]\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n",
	} {
		reports := 0
		c := testClient(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/v2/report" {
				reports++
			}
			return response(http.StatusOK, body), nil
		})
		err := c.ActiveChat(&model.Account{AccessToken: "at", Version: model.ModeIntl}, "hy3", "")
		if !IsKind(err, KindProtocol) || reports != 0 {
			t.Fatalf("body=%q err=%v reports=%d", body, err, reports)
		}
	}
}

func TestActiveChatRejectsSSEBusinessError(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, "event: error\ndata: {\"code\":12153,\"msg\":\"Offline user session not found\"}\n\n"), nil
	})
	err := c.ActiveChat(&model.Account{AccessToken: "at", Version: model.ModeIntl}, "hy3", "")
	if !IsKind(err, KindAuthDead) {
		t.Fatalf("stream auth error should be actionable: %v", err)
	}
}

func TestActiveChatReportsTelemetryWarningWithoutDiscardingAnswer(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v2/report" {
			return response(http.StatusServiceUnavailable, `{"code":0,"msg":"unavailable"}`), nil
		}
		return response(http.StatusOK, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"), nil
	})
	result, err := c.ActiveChatWithResult(&model.Account{AccessToken: "at", Version: model.ModeIntl}, "hy3", "")
	if err != nil || !IsKind(result.ReportWarning, KindServer) {
		t.Fatalf("chat err=%v, report warning=%v", err, result.ReportWarning)
	}
}

func TestValidateProxyURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"", true},
		{"direct", true},
		{"DIRECT", true},
		{"http://192.168.5.1:8080", true},
		{"https://proxy.example.com:3128", true},
		{"http://proxy.example.com:8080/", true},
		{"socks5://192.168.5.1:1070", true},
		{"socks5h://user:pass@gateway:1080", true},
		{"ftp://bad.example:21", false},
		{"not a url", false},
		{"://missing-scheme", false},
		{"http:///missing-host", false},
		{"http://proxy.example:", false},
		{"http://proxy.example:70000", false},
		{"http://proxy.example/path", false},
		{"http://[::1]:8080", true},
		{"socks5://[::1]:1080", true},
	}
	for _, c := range cases {
		err := ValidateProxyURL(c.url)
		if (err == nil) != c.want {
			t.Errorf("ValidateProxyURL(%q) err=%v wantOK=%v", c.url, err, c.want)
		}
	}
}

func TestNormalizeProxyURLTreatsDirectAsCanonicalEmpty(t *testing.T) {
	for _, value := range []string{"direct", "DIRECT", " Direct ", ""} {
		if got := NormalizeProxyURL(value); got != "" {
			t.Fatalf("NormalizeProxyURL(%q) = %q, want empty", value, got)
		}
	}
	if got := NormalizeProxyURL("http://proxy.example:8080"); got != "http://proxy.example:8080" {
		t.Fatalf("proxy URL changed unexpectedly: %q", got)
	}
}

func TestNormalizeDNSServer(t *testing.T) {
	cases := map[string]string{
		"1.1.1.1":        "1.1.1.1:53",
		"dns.example":    "dns.example:53",
		"[::1]":          "[::1]:53",
		"2001:db8::1":    "[2001:db8::1]:53",
		"2001:db8::1:53": "[2001:db8::1:53]:53",
		"[::1]:5353":     "[::1]:5353",
	}
	for input, want := range cases {
		if got := normalizeDNSServer(input); got != want {
			t.Errorf("normalizeDNSServer(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestLoginSessionUsesConfiguredTransportAndEndpoint(t *testing.T) {
	called := false
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.URL.Host != "login.test" || r.URL.Path != "/v2/plugin/auth/state" {
			t.Fatalf("unexpected login URL: %s", r.URL.String())
		}
		return response(http.StatusOK, `{"code":0,"data":{"state":"s1","authUrl":"https://login.test/authorize"}}`), nil
	})
	base := testClient(rt)
	base.GlobalBase = "https://login.test"
	session := NewLoginSessionWithClient(model.ModeIntl, base)
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	if !called || session.State != "s1" {
		t.Fatalf("configured transport was not used: called=%v state=%q", called, session.State)
	}
}

func TestLoginSessionKeepsTimeoutWhenBaseTimeoutIsUnset(t *testing.T) {
	base := &Client{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"code":0,"data":{"state":"s1","authUrl":"https://login.test/authorize"}}`), nil
	})}, GlobalBase: "https://login.test"}
	session := NewLoginSessionWithClient(model.ModeIntl, base)
	if session.Client.Timeout != 30*time.Second {
		t.Fatalf("unset base timeout changed login timeout to %s", session.Client.Timeout)
	}
}

func TestNewWithCNTransportPreservesDefaultsForEmptyCaller(t *testing.T) {
	got := NewWithCNTransport(&Client{})
	if got == nil || got.ChatBaseCN == "" || got.BillingBaseCN == "" || got.GlobalBase == "" || got.GlobalBilling == "" {
		t.Fatalf("empty caller cleared default endpoints: %+v", got)
	}
}

func TestValidateAuthURLRejectsUntrustedSchemesAndHosts(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		intl bool
		ok   bool
	}{
		{name: "global product host", raw: "https://login.workbuddy.ai/authorize?state=x", intl: true, ok: true},
		{name: "domestic product host", raw: "https://login.codebuddy.cn/authorize", intl: false, ok: true},
		{name: "javascript scheme", raw: "javascript:alert(1)", intl: true, ok: false},
		{name: "foreign host", raw: "https://evil.example/authorize", intl: true, ok: false},
		{name: "userinfo", raw: "https://user:pass@login.workbuddy.ai/authorize", intl: true, ok: false},
		{name: "missing path", raw: "https://login.workbuddy.ai", intl: true, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAuthURL(tt.raw, tt.intl, "")
			if (err == nil) != tt.ok {
				t.Fatalf("validateAuthURL(%q) err=%v wantOK=%v", tt.raw, err, tt.ok)
			}
		})
	}
	if err := validateAuthURL("https://login.test/authorize", true, "https://login.test"); err != nil {
		t.Fatalf("configured endpoint should be accepted: %v", err)
	}
}

func TestLoginSessionRejectsUntrustedAuthURL(t *testing.T) {
	base := testClient(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"code":0,"data":{"state":"s1","authUrl":"https://evil.example/authorize"}}`), nil
	})
	base.GlobalBase = "https://login.test"
	session := NewLoginSessionWithClient(model.ModeIntl, base)
	if err := session.Start(); err == nil || !IsKind(err, KindProtocol) {
		t.Fatalf("untrusted authorization URL was accepted: %v", err)
	}
}

func TestLoginSessionPollClassifiesPendingAndFatalStatuses(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantKind    Kind
		wantCode    int
		wantPending bool
	}{
		{name: "authorization pending code", status: http.StatusBadRequest, body: `{"code":1001,"msg":"authorization pending"}`, wantKind: KindBusiness, wantCode: 1001, wantPending: true},
		{name: "authorization in progress code", status: http.StatusOK, body: `{"code":11217,"msg":"login ing..."}`, wantKind: KindBusiness, wantCode: 11217, wantPending: true},
		{name: "other HTTP 200 business code", status: http.StatusOK, body: `{"code":1002,"msg":"login forbidden"}`, wantKind: KindBusiness, wantCode: 1002},
		{name: "authorization pending message", status: http.StatusConflict, body: `{"code":0,"msg":"waiting for authorization"}`, wantKind: KindBusiness, wantPending: true},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"code":12153,"msg":"offline user session not found"}`, wantKind: KindAuthDead},
		{name: "forbidden", status: http.StatusForbidden, body: `{"code":1002,"msg":"login forbidden"}`, wantKind: KindBusiness},
		{name: "not found", status: http.StatusNotFound, body: `{"error":"login state not found"}`, wantKind: KindNotFound},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"code":1003,"msg":"too many requests"}`, wantKind: KindRate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := testClient(func(*http.Request) (*http.Response, error) {
				return response(tt.status, tt.body), nil
			})
			session := NewLoginSessionWithClient(model.ModeCN, base)
			session.State = "poll-state"
			_, err := session.Poll()
			if err == nil || !IsKind(err, tt.wantKind) {
				t.Fatalf("Poll error=%v, want kind %s", err, tt.wantKind)
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("Poll error does not expose client.Error: %v", err)
			}
			if tt.wantCode != 0 && ce.Code != tt.wantCode {
				t.Fatalf("Poll code=%d, want %d", ce.Code, tt.wantCode)
			}
			pending := strings.Contains(ce.Msg, "登录尚未完成")
			if pending != tt.wantPending {
				t.Fatalf("Poll pending=%v msg=%q, want %v", pending, ce.Msg, tt.wantPending)
			}
		})
	}
}

func TestSetDNSSystemUsesSystemMode(t *testing.T) {
	SetDNSSystem()
	if got := ActiveDNS(); len(got) != 0 {
		t.Fatalf("system DNS mode should have no fixed servers: %v", got)
	}
	SetDNSConfig(nil)
}

func TestClassifyBusinessDoesNotTreatChatSessionAsAuthFailure(t *testing.T) {
	if IsKind(classifyBusiness(http.StatusBadRequest, 10009, "conversation session not found"), KindAuthDead) {
		t.Fatal("chat conversation lookup failure must not invalidate account auth")
	}
	if !IsKind(classifyBusiness(http.StatusBadRequest, 10010, "请求频繁，请稍后重试"), KindRate) {
		t.Fatal("business rate-limit message should be classified as rate limited")
	}
}

func TestHTTPBusinessCodeIsPreservedOnErrorStatus(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusBadRequest, `{"code":14018,"msg":"quota"}`), nil
	})
	err := c.DailyCheckin(&model.Account{AccessToken: "at", UID: "u1"})
	if !IsKind(err, KindCredit) {
		t.Fatalf("expected credit error, got %v", err)
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
	if directSentinel, err := NewWithProxy("direct"); err != nil || directSentinel == nil {
		t.Fatalf("direct sentinel should select direct transport: %v", err)
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

package client

import (
	"errors"
	"io"
	"net/http"
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

package model

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func testJWT(t *testing.T, payload map[string]any, padded bool) string {
	t.Helper()
	encode := base64.RawURLEncoding
	if padded {
		encode = base64.URLEncoding
	}
	header, err := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return encode.EncodeToString(header) + "." + encode.EncodeToString(body) + ".signature"
}

func TestJWTExpiryUnix(t *testing.T) {
	const want int64 = 1790000123
	tests := []struct {
		name   string
		token  string
		want   int64
		wantOK bool
	}{
		{name: "numeric", token: testJWT(t, map[string]any{"exp": want}, false), want: want, wantOK: true},
		{name: "quoted", token: testJWT(t, map[string]any{"exp": "1790000123"}, false), want: want, wantOK: true},
		{name: "padded", token: testJWT(t, map[string]any{"exp": want}, true), want: want, wantOK: true},
		{name: "missing", token: testJWT(t, map[string]any{"sub": "u1"}, false), wantOK: false},
		{name: "malformed", token: "a.b.c", wantOK: false},
		{name: "not-jwt", token: "opaque-token", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := JWTExpiryUnix(tc.token)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("JWTExpiryUnix() = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestResolveTokenExpiresAtPrefersJWTClaim(t *testing.T) {
	now := time.Unix(1700000000, 0)
	token := testJWT(t, map[string]any{"exp": 1790000123}, false)
	if got := ResolveTokenExpiresAt(token, 60, now); got != 1790000123 {
		t.Fatalf("ResolveTokenExpiresAt() = %d, want JWT exp", got)
	}
	if got := ResolveTokenExpiresAt("opaque-token", 60, now); got != now.Add(time.Minute).Unix() {
		t.Fatalf("ResolveTokenExpiresAt() fallback = %d, want expiresIn fallback", got)
	}
	if got := ResolveTokenExpiresAt("opaque-token", 0, now); got != 0 {
		t.Fatalf("ResolveTokenExpiresAt() unknown expiry = %d, want 0", got)
	}
}

func TestPublicAccountIncludesOnlyTokenExpiry(t *testing.T) {
	const want int64 = 1790000123
	token := testJWT(t, map[string]any{"exp": want}, false)
	account := Account{
		ID:          "a1",
		UID:         "u1",
		AccessToken: token,
		ExpiresAt:   1700000060,
		Enabled:     true,
	}
	public := account.Public()
	if public.TokenExpiresAt != want {
		t.Fatalf("TokenExpiresAt = %d, want %d", public.TokenExpiresAt, want)
	}
	raw, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, secretField := range []string{"accessToken", "refreshToken"} {
		if containsJSONField(body, secretField) {
			t.Fatalf("public account exposed %q: %s", secretField, body)
		}
	}
}

func containsJSONField(raw, field string) bool {
	needle := `"` + field + `"`
	return len(raw) >= len(needle) && stringContains(raw, needle)
}

func stringContains(raw, needle string) bool {
	for i := 0; i+len(needle) <= len(raw); i++ {
		if raw[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

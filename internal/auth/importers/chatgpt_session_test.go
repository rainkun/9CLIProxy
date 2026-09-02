package importers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// makeDummyJWT builds an unsigned JWT with the given claims. The
// signature is left empty, matching real-world data exports and
// exercising the parser's signature-agnostic path.
func makeDummyJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("failed to marshal claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + body + "."
}

func TestConvertSessionRejectsMissingTokens(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err := ConvertSession(&ChatGPTSession{Email: "demo@example.com"}, now)
	if !errors.Is(err, ErrChatGPTSessionMissingTokens) {
		t.Fatalf("err = %v, want ErrChatGPTSessionMissingTokens", err)
	}
}

func TestConvertSessionRejectsWhitespaceOnlyTokens(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err := ConvertSession(&ChatGPTSession{
		AccessToken:  "   ",
		RefreshToken: "\t\n",
		Email:        "demo@example.com",
	}, now)
	if !errors.Is(err, ErrChatGPTSessionMissingTokens) {
		t.Fatalf("err = %v, want ErrChatGPTSessionMissingTokens", err)
	}
}

func TestConvertSessionBuildsCanonicalCodexAuth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	in := &ChatGPTSession{
		Phone:            "15555550100",
		Email:            "demo@example.com",
		Password:         "example-pw",
		ChatgptAccountID: "user-83cb8187-2d27-4d58-a1e5-f6f7a3722649",
		AccessToken:      "header.payload.sig",
		RefreshToken:     "rt.1.AABV_dummy",
		IDToken: makeDummyJWT(t, map[string]any{
			"email": "demo@example.com",
			"sub":   "user-83cb8187-2d27-4d58-a1e5-f6f7a3722649",
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": "user-83cb8187-2d27-4d58-a1e5-f6f7a3722649",
				"chatgpt_plan_type":  "plus",
			},
		}),
		OAIDid:       "8713e286-c158-4b7a-889f-e6c85f2fcfb2",
		PlusOneMFree: false,
		Promo:        []any{},
		UA:           "Mozilla/5.0 test",
	}

	res, err := ConvertSession(in, now)
	if err != nil {
		t.Fatalf("ConvertSession() error = %v", err)
	}

	if got := res.Auth["type"]; got != "codex" {
		t.Fatalf("auth.type = %v, want codex", got)
	}
	if got := res.Auth["access_token"]; got != "header.payload.sig" {
		t.Fatalf("auth.access_token = %v", got)
	}
	if got := res.Auth["refresh_token"]; got != "rt.1.AABV_dummy" {
		t.Fatalf("auth.refresh_token = %v", got)
	}
	if got := res.Auth["email"]; got != "demo@example.com" {
		t.Fatalf("auth.email = %v", got)
	}
	if got := res.Auth["account_id"]; got != "user-83cb8187-2d27-4d58-a1e5-f6f7a3722649" {
		t.Fatalf("auth.account_id = %v", got)
	}
	if got := res.Auth["last_refresh"]; got != "2026-01-02T03:04:05Z" {
		t.Fatalf("auth.last_refresh = %v, want 2026-01-02T03:04:05Z", got)
	}
	if got, _ := res.Auth["expired"].(string); !strings.HasPrefix(got, "2026-01-02T04:04:05Z") {
		t.Fatalf("auth.expired = %v, want prefix 2026-01-02T04:04:05Z", got)
	}

	if res.PlanType != "plus" {
		t.Fatalf("PlanType = %q, want plus", res.PlanType)
	}
	if len(res.HashAccountID) != 8 {
		t.Fatalf("HashAccountID = %q, want 8 hex chars", res.HashAccountID)
	}
	if res.Email != "demo@example.com" {
		t.Fatalf("Email = %q", res.Email)
	}
	if res.AccountID != "user-83cb8187-2d27-4d58-a1e5-f6f7a3722649" {
		t.Fatalf("AccountID = %q", res.AccountID)
	}

	meta, ok := res.Auth["meta"].(map[string]any)
	if !ok {
		t.Fatalf("auth.meta missing or wrong type: %#v", res.Auth["meta"])
	}
	if got := meta["label"]; got != "demo@example.com" {
		t.Fatalf("meta.label = %v", got)
	}
	if got := meta["chatgpt_account_id"]; got != "user-83cb8187-2d27-4d58-a1e5-f6f7a3722649" {
		t.Fatalf("meta.chatgpt_account_id = %v", got)
	}
	if got := meta["note"]; got != "Imported from ChatGPT session" {
		t.Fatalf("meta.note = %v", got)
	}
	if got := meta["password"]; got != "example-pw" {
		t.Fatalf("meta.password = %v", got)
	}
	if got := meta["phone"]; got != "15555550100" {
		t.Fatalf("meta.phone = %v", got)
	}
	session, ok := meta["session"].(map[string]any)
	if !ok {
		t.Fatalf("meta.session missing: %#v", meta["session"])
	}
	if got := session["oai_did"]; got != "8713e286-c158-4b7a-889f-e6c85f2fcfb2" {
		t.Fatalf("meta.session.oai_did = %v", got)
	}
	if got, ok := session["plus_1m_free"].(bool); !ok || got != false {
		t.Fatalf("meta.session.plus_1m_free = %v (%T)", session["plus_1m_free"], session["plus_1m_free"])
	}
	if got := session["ua"]; got != "Mozilla/5.0 test" {
		t.Fatalf("meta.session.ua = %v", got)
	}
	if got, ok := session["promo"].([]any); !ok || len(got) != 0 {
		t.Fatalf("meta.session.promo = %#v", session["promo"])
	}
}

func TestConvertSessionFallsBackToJWTClaims(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	in := &ChatGPTSession{
		AccessToken:  "a",
		RefreshToken: "b",
		IDToken: makeDummyJWT(t, map[string]any{
			"email": "jwt-only@example.com",
			"sub":   "user-jwt-only",
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": "user-jwt-only",
			},
		}),
	}

	res, err := ConvertSession(in, now)
	if err != nil {
		t.Fatalf("ConvertSession() error = %v", err)
	}
	if res.Email != "jwt-only@example.com" {
		t.Fatalf("Email = %q, want jwt-only@example.com", res.Email)
	}
	if res.AccountID != "user-jwt-only" {
		t.Fatalf("AccountID = %q, want user-jwt-only", res.AccountID)
	}
	if res.PlanType != "" {
		t.Fatalf("PlanType = %q, want empty", res.PlanType)
	}
	if len(res.HashAccountID) != 8 {
		t.Fatalf("HashAccountID = %q", res.HashAccountID)
	}
}

func TestConvertSessionAcceptsAccessTokenOnly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	in := &ChatGPTSession{
		AccessToken: "only-access",
		Email:       "demo@example.com",
	}
	if _, err := ConvertSession(in, now); err != nil {
		t.Fatalf("ConvertSession() error = %v", err)
	}
}

func TestConvertSessionPreservesProvidedLastRefreshAndExpire(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	in := &ChatGPTSession{
		AccessToken: "a",
		Email:       "demo@example.com",
		LastRefresh: "2025-12-31T00:00:00Z",
		Expire:      "2025-12-31T01:00:00Z",
	}
	res, err := ConvertSession(in, now)
	if err != nil {
		t.Fatalf("ConvertSession() error = %v", err)
	}
	if got := res.Auth["last_refresh"]; got != "2025-12-31T00:00:00Z" {
		t.Fatalf("auth.last_refresh = %v", got)
	}
	if got := res.Auth["expired"]; got != "2025-12-31T01:00:00Z" {
		t.Fatalf("auth.expired = %v", got)
	}
}

func TestConvertSessionDefaultsLabelToPhoneWhenNoEmail(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	in := &ChatGPTSession{
		AccessToken: "a",
		Phone:       "15555550100",
	}
	res, err := ConvertSession(in, now)
	if err != nil {
		t.Fatalf("ConvertSession() error = %v", err)
	}
	meta, _ := res.Auth["meta"].(map[string]any)
	if got := meta["label"]; got != "15555550100" {
		t.Fatalf("meta.label = %v, want phone fallback", got)
	}
}

func TestConvertSessionNilInput(t *testing.T) {
	t.Parallel()
	_, err := ConvertSession(nil, time.Now())
	if !errors.Is(err, ErrChatGPTSessionMissingTokens) {
		t.Fatalf("err = %v, want ErrChatGPTSessionMissingTokens", err)
	}
}

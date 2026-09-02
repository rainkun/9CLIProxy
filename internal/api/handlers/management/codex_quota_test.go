package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexQuotaFetchesUsageAndResetCredits(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("usage authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-1" {
			t.Errorf("usage account header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type":"plus",
			"rate_limit":{"limit_reached":false,"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_at":1785900000},"secondary_window":{"used_percent":31,"limit_window_seconds":604800,"reset_at":1786000000}},
			"rate_limit_reset_credits":{"available_count":1,"applicable_available_count":1}
		}`))
	}))
	defer usageServer.Close()

	creditsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("OpenAI-Beta") != "codex-1" {
			t.Errorf("credits OpenAI-Beta = %q", r.Header.Get("OpenAI-Beta"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"available_count":1,
			"applicable_available_count":1,
			"credits":[{"id":"credit-1","status":"available","reset_type":"codex_rate_limits","expires_at":"2026-09-01T00:00:00Z"}]
		}`))
	}))
	defer creditsServer.Close()

	oldUsage, oldCredits := codexUsageEndpoint, codexQuotaCreditsEndpoint
	codexUsageEndpoint, codexQuotaCreditsEndpoint = usageServer.URL, creditsServer.URL
	t.Cleanup(func() {
		codexUsageEndpoint, codexQuotaCreditsEndpoint = oldUsage, oldCredits
	})

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-id",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "access-token",
			"account_id":   "acct-1",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/codex/quota",
		strings.NewReader(`{"auth_id":"codex-id"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.CodexQuota(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("CodexQuota status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Results []struct {
			AuthIndex string         `json:"auth_index"`
			Quota     map[string]any `json:"quota"`
		} `json:"results"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(response.Results) != 1 {
		t.Fatalf("results = %#v, want one result", response.Results)
	}
	if response.Results[0].AuthIndex == "" {
		t.Fatal("result auth_index is empty")
	}
	if response.Results[0].Quota["plan_type"] != "plus" {
		t.Fatalf("quota plan_type = %#v", response.Results[0].Quota["plan_type"])
	}
	rateLimit, ok := response.Results[0].Quota["rate_limit"].(map[string]any)
	if !ok {
		t.Fatalf("quota rate_limit = %#v", response.Results[0].Quota["rate_limit"])
	}
	if _, ok := rateLimit["primary_window"].(map[string]any); !ok {
		t.Fatalf("quota primary_window = %#v", rateLimit["primary_window"])
	}
	credits, ok := response.Results[0].Quota["rate_limit_reset_credits"].(map[string]any)
	if !ok {
		t.Fatalf("quota reset credits = %#v", response.Results[0].Quota["rate_limit_reset_credits"])
	}
	if credits["available_count"] != float64(1) {
		t.Fatalf("available_count = %#v", credits["available_count"])
	}
	items, ok := credits["credits"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("credits = %#v, want one item", credits["credits"])
	}
	credit, ok := items[0].(map[string]any)
	if !ok || credit["reset_type"] != "codex_rate_limits" {
		t.Fatalf("credit reset_type = %#v, want codex_rate_limits", items[0])
	}
}

func TestCodexQuotaRejectsAPIKeyCodexAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-api-key",
		FileName: "codex-api-key.json",
		Provider: "codex",
		Attributes: map[string]string{
			coreauth.AttributeAPIKey: "sk-test",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/codex/quota",
		strings.NewReader(`{"all":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.CodexQuota(ctx)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("CodexQuota status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCodexQuotaReportsUsageFailureAsFailedResult(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider failure with sensitive details", http.StatusBadGateway)
	}))
	defer usageServer.Close()

	oldUsage, oldCredits := codexUsageEndpoint, codexQuotaCreditsEndpoint
	codexUsageEndpoint, codexQuotaCreditsEndpoint = usageServer.URL, usageServer.URL
	t.Cleanup(func() {
		codexUsageEndpoint, codexQuotaCreditsEndpoint = oldUsage, oldCredits
	})

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-failing",
		FileName: "codex-failing.json",
		Provider: "codex",
		Metadata: map[string]any{"access_token": "access-token"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/codex/quota",
		strings.NewReader(`{"all":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.CodexQuota(ctx)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("CodexQuota status = %d, want 207; body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Results []map[string]any `json:"results"`
		Failed  []struct {
			Error string `json:"error"`
		} `json:"failed"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(response.Results) != 0 || len(response.Failed) != 1 {
		t.Fatalf("results=%#v failed=%#v, want one failed result", response.Results, response.Failed)
	}
	if strings.Contains(response.Failed[0].Error, "sensitive details") {
		t.Fatalf("failure leaked upstream body: %q", response.Failed[0].Error)
	}
}

func TestCodexQuotaUsesBoundedConcurrencyAndStableOrdering(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	var active int32
	var maxActive int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := atomic.AddInt32(&active, 1)
		for {
			observed := atomic.LoadInt32(&maxActive)
			if current <= observed || atomic.CompareAndSwapInt32(&maxActive, observed, current) {
				break
			}
		}
		defer atomic.AddInt32(&active, -1)

		// Reverse completion order to ensure response aggregation is based on
		// stable auth identity rather than worker completion order.
		if account := r.Header.Get("ChatGPT-Account-ID"); account != "" {
			if strings.HasSuffix(account, "0") {
				time.Sleep(35 * time.Millisecond)
			} else {
				time.Sleep(5 * time.Millisecond)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("OpenAI-Beta") == "codex-1" {
			_, _ = w.Write([]byte(`{"available_count":1,"applicable_available_count":1,"credits":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"plan_type":"plus",
			"rate_limit":{"limit_reached":false,"primary_window":{"used_percent":1,"limit_window_seconds":60,"reset_at":1785900000}}
		}`))
	}))
	defer server.Close()

	oldUsage, oldCredits := codexUsageEndpoint, codexQuotaCreditsEndpoint
	codexUsageEndpoint, codexQuotaCreditsEndpoint = server.URL, server.URL
	t.Cleanup(func() {
		codexUsageEndpoint, codexQuotaCreditsEndpoint = oldUsage, oldCredits
	})

	manager := coreauth.NewManager(nil, nil, nil)
	for index := 0; index < 12; index++ {
		auth := &coreauth.Auth{
			ID:       fmt.Sprintf("codex-%02d", index),
			FileName: fmt.Sprintf("codex-%02d.json", index),
			Provider: "codex",
			Metadata: map[string]any{
				"type":         "codex",
				"access_token": fmt.Sprintf("access-%02d", index),
				"account_id":   fmt.Sprintf("acct-%02d", index),
			},
		}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%d) error = %v", index, errRegister)
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/codex/quota",
		strings.NewReader(`{"all":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.CodexQuota(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("CodexQuota status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Results []struct {
			AuthIndex string `json:"auth_index"`
		} `json:"results"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(response.Results) != 12 {
		t.Fatalf("results = %d, want 12; body=%s", len(response.Results), rec.Body.String())
	}
	for index := 1; index < len(response.Results); index++ {
		if response.Results[index-1].AuthIndex > response.Results[index].AuthIndex {
			t.Fatalf("results are not stably ordered: %#v", response.Results)
		}
	}
	if got := atomic.LoadInt32(&maxActive); got < 2 {
		t.Fatalf("max concurrent requests = %d, want >1", got)
	} else if got > 6 {
		t.Fatalf("max concurrent requests = %d, want <=6", got)
	}
}

func TestResolveCodexOAuthTokenRefreshesAndPersists(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	oldRefresh := refreshCodexOAuthToken
	refreshCodexOAuthToken = func(context.Context, *config.Config, string, string) (*codexauth.CodexTokenData, error) {
		return &codexauth.CodexTokenData{
			AccessToken:  "fresh-access",
			RefreshToken: "fresh-refresh",
			IDToken:      "fresh-id",
			AccountID:    "acct-fresh",
			Email:        "fresh@example.com",
			Expire:       time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}, nil
	}
	t.Cleanup(func() { refreshCodexOAuthToken = oldRefresh })

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-refresh",
		FileName: "codex-refresh.json",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token":  "stale-access",
			"refresh_token": "old-refresh",
			"expired":       time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	updated, token, errRefresh := h.resolveCodexOAuthToken(context.Background(), auth, false)
	if errRefresh != nil {
		t.Fatalf("resolveCodexOAuthToken() error = %v", errRefresh)
	}
	if token != "fresh-access" || updated.Metadata["refresh_token"] != "fresh-refresh" {
		t.Fatalf("updated token state = %#v, token=%q", updated.Metadata, token)
	}
	persisted, ok := manager.GetByID("codex-refresh")
	if !ok || persisted == nil || persisted.Metadata["access_token"] != "fresh-access" {
		t.Fatalf("manager did not persist refreshed auth: %#v", persisted)
	}
}

func TestResolveCodexOAuthTokenRefreshFailureIsSanitized(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	oldRefresh := refreshCodexOAuthToken
	refreshCodexOAuthToken = func(context.Context, *config.Config, string, string) (*codexauth.CodexTokenData, error) {
		return nil, errors.New(`upstream body contained "Bearer secret-token"`)
	}
	t.Cleanup(func() { refreshCodexOAuthToken = oldRefresh })

	auth := &coreauth.Auth{
		ID:       "codex-refresh-fail",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token":  "stale-access",
			"refresh_token": "old-refresh",
			"expired":       time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		},
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	_, _, errRefresh := h.resolveCodexOAuthToken(context.Background(), auth, false)
	if errRefresh == nil || errRefresh.Error() != "Codex token refresh failed" {
		t.Fatalf("refresh error = %v, want sanitized error", errRefresh)
	}
}

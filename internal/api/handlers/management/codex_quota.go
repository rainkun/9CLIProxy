package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"
	// Quota responses are provider metadata, not credentials. Keep the bound
	// independent from the larger import limit so a malformed upstream cannot
	// retain an unexpectedly large response in a management request.
	maxCodexQuotaResponseBody = 1 << 20
)

// These are variables rather than constants so tests can exercise the complete
// request/normalization path with an httptest server.
var (
	codexUsageEndpoint        = codexUsageURL
	codexQuotaCreditsEndpoint = codexResetCreditsURL
	refreshCodexOAuthToken    = func(ctx context.Context, cfg *config.Config, proxyURL, refreshToken string) (*codexauth.CodexTokenData, error) {
		return codexauth.NewCodexAuthWithProxyURL(cfg, proxyURL).RefreshTokens(ctx, refreshToken)
	}
)

// CodexQuota fetches current usage windows and reset-credit metadata for the
// selected Codex OAuth accounts. It is deliberately read-only: consuming a
// reset credit remains an explicit CodexBolt operation.
//
// POST /v0/management/codex/quota
//
// Selection fields mirror CodexBolt (auth_indices/authIndexes/auth_index,
// auth_id, names/name, and all). A successful result contains a normalized
// Codex usage payload under result.quota; reset-credit lookup failures are
// reported per result without discarding otherwise usable usage data.
func (h *Handler) CodexQuota(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req codexBoltRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	indices := uniqueAuthFileNames(append(append([]string{}, req.AuthIndices...), req.AuthIndexes...))
	if strings.TrimSpace(req.AuthIndex) != "" {
		indices = uniqueAuthFileNames(append(indices, req.AuthIndex))
	}
	if strings.TrimSpace(req.AuthID) != "" {
		indices = uniqueAuthFileNames(append(indices, req.AuthID))
	}
	names := uniqueAuthFileNames(req.Names)
	if strings.TrimSpace(req.Name) != "" {
		names = uniqueAuthFileNames(append(names, req.Name))
	}
	if len(indices) == 0 && len(names) == 0 && !req.All {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_indices, names, or all is required"})
		return
	}

	indexSet := make(map[string]struct{}, len(indices))
	for _, value := range indices {
		indexSet[value] = struct{}{}
	}
	nameSet := make(map[string]struct{}, len(names))
	for _, value := range names {
		nameSet[value] = struct{}{}
	}

	type quotaWork struct {
		auth              *coreauth.Auth
		quota             map[string]any
		resetCreditsError error
		err               error
	}

	works := make([]quotaWork, 0)
	for _, auth := range h.authManager.List() {
		if auth == nil ||
			!strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") ||
			auth.AuthKind() != coreauth.AuthKindOAuth {
			continue
		}
		auth.EnsureIndex()
		if !req.All {
			_, byIndex := indexSet[auth.Index]
			_, byID := indexSet[auth.ID]
			_, byName := nameSet[auth.FileName]
			if !byIndex && !byID && !byName {
				continue
			}
		}
		work := quotaWork{auth: auth}
		if auth.Disabled || auth.Status == coreauth.StatusDisabled {
			work.err = errors.New("Codex auth is disabled")
		}
		works = append(works, work)
	}

	if len(works) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no matching Codex OAuth auth files"})
		return
	}

	// Manager storage is map-backed, so sort by the stable auth index before
	// dispatching work. This keeps bulk responses deterministic even though
	// requests complete out of order.
	sort.SliceStable(works, func(i, j int) bool {
		left := strings.TrimSpace(works[i].auth.Index)
		right := strings.TrimSpace(works[j].auth.Index)
		if left == right {
			return strings.TrimSpace(works[i].auth.ID) < strings.TrimSpace(works[j].auth.ID)
		}
		return left < right
	})

	const quotaWorkerLimit = 6
	jobs := make(chan int, len(works))
	workerCount := quotaWorkerLimit
	if len(works) < workerCount {
		workerCount = len(works)
	}
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for worker := 0; worker < workerCount; worker++ {
		go func() {
			defer workers.Done()
			for index := range jobs {
				work := &works[index]
				if work.err != nil {
					continue
				}
				work.quota, work.resetCreditsError, work.err = h.fetchCodexQuota(c.Request.Context(), work.auth)
			}
		}()
	}
	for index := range works {
		if works[index].err == nil {
			jobs <- index
		}
	}
	close(jobs)
	workers.Wait()

	results := make([]gin.H, 0, len(works))
	failed := make([]gin.H, 0)
	for _, work := range works {
		auth := work.auth
		if work.err != nil {
			failed = append(failed, gin.H{
				"auth_index": auth.Index,
				"id":         auth.ID,
				"name":       auth.FileName,
				"error":      work.err.Error(),
			})
			continue
		}
		entry := gin.H{
			"auth_index": auth.Index,
			"id":         auth.ID,
			"name":       auth.FileName,
			"provider":   auth.Provider,
			"status":     "ok",
			"quota":      work.quota,
		}
		if work.resetCreditsError != nil {
			// Usage is still useful when the optional reset-credit endpoint is
			// unavailable (for example, during a provider rollout).
			entry["reset_credits_error"] = work.resetCreditsError.Error()
		}
		results = append(results, entry)
	}

	status := "ok"
	code := http.StatusOK
	if len(failed) > 0 {
		status = "partial"
		code = http.StatusMultiStatus
	}
	c.JSON(code, gin.H{
		"status":        status,
		"results":       results,
		"failed":        failed,
		"updated_count": len(results),
		"failed_count":  len(failed),
	})
}

func (h *Handler) fetchCodexQuota(ctx context.Context, auth *coreauth.Auth) (map[string]any, error, error) {
	if auth == nil {
		return nil, nil, errors.New("auth is missing")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	tokenAuth, token, errToken := h.resolveCodexOAuthToken(ctx, auth, false)
	if errToken != nil {
		return nil, nil, errToken
	}

	usage, errUsage := h.requestCodexQuotaEndpoint(ctx, tokenAuth, token, codexUsageEndpoint, false)
	if errUsage != nil && isCodexQuotaAuthError(errUsage) {
		tokenAuth, token, errToken = h.resolveCodexOAuthToken(ctx, tokenAuth, true)
		if errToken == nil {
			usage, errUsage = h.requestCodexQuotaEndpoint(ctx, tokenAuth, token, codexUsageEndpoint, false)
		}
	}
	if errUsage != nil {
		if errToken != nil {
			return nil, nil, errToken
		}
		return nil, nil, errUsage
	}

	quota, errNormalize := normalizeCodexUsagePayload(usage)
	if errNormalize != nil {
		return nil, nil, errNormalize
	}
	if plan := codexPlanType(tokenAuth); plan != "" {
		if _, exists := quota["plan_type"]; !exists {
			quota["plan_type"] = plan
		}
	}

	// The usage endpoint contains counts on current providers, while the
	// reset-credit endpoint carries the detailed credit records. A failure here
	// is intentionally non-fatal and is surfaced to the caller separately.
	credits, errCredits := h.requestCodexQuotaEndpoint(ctx, tokenAuth, token, codexQuotaCreditsEndpoint, true)
	if errCredits != nil && isCodexQuotaAuthError(errCredits) {
		tokenAuth, token, errToken = h.resolveCodexOAuthToken(ctx, tokenAuth, true)
		if errToken == nil {
			credits, errCredits = h.requestCodexQuotaEndpoint(ctx, tokenAuth, token, codexQuotaCreditsEndpoint, true)
		} else {
			errCredits = errToken
		}
	}

	usageCredits, _ := quota["rate_limit_reset_credits"].(map[string]any)
	if usageCredits == nil {
		usageCredits = make(map[string]any)
		quota["rate_limit_reset_credits"] = usageCredits
	}
	if errCredits == nil {
		for key, value := range credits {
			usageCredits[key] = value
		}
	}
	return quota, errCredits, nil
}

func (h *Handler) resolveCodexOAuthToken(ctx context.Context, auth *coreauth.Auth, forceRefresh bool) (*coreauth.Auth, string, error) {
	if auth == nil {
		return nil, "", errors.New("auth is missing")
	}
	token := tokenValueForAuth(auth)
	if token == "" {
		return nil, "", errors.New("Codex access token is missing")
	}
	refreshToken := stringValue(auth.Metadata, "refresh_token")
	if !forceRefresh && !codexTokenNeedsRefresh(auth.Metadata) {
		return auth, token, nil
	}
	if refreshToken == "" {
		if forceRefresh {
			return nil, "", errors.New("Codex access token expired and refresh token is missing")
		}
		return auth, token, nil
	}

	var cfg *config.Config
	if h != nil {
		h.mu.Lock()
		if h.cfg != nil {
			cfg = h.cfg.CloneForRuntime()
		}
		h.mu.Unlock()
	}
	tokenData, errRefresh := refreshCodexOAuthToken(ctx, cfg, strings.TrimSpace(auth.ProxyURL), refreshToken)
	if errRefresh != nil || tokenData == nil || strings.TrimSpace(tokenData.AccessToken) == "" {
		return nil, "", errors.New("Codex token refresh failed")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["type"] = "codex"
	auth.Metadata["access_token"] = strings.TrimSpace(tokenData.AccessToken)
	if strings.TrimSpace(tokenData.RefreshToken) != "" {
		auth.Metadata["refresh_token"] = strings.TrimSpace(tokenData.RefreshToken)
	}
	if strings.TrimSpace(tokenData.IDToken) != "" {
		auth.Metadata["id_token"] = strings.TrimSpace(tokenData.IDToken)
	}
	if strings.TrimSpace(tokenData.AccountID) != "" {
		auth.Metadata["account_id"] = strings.TrimSpace(tokenData.AccountID)
	}
	if strings.TrimSpace(tokenData.Email) != "" {
		auth.Metadata["email"] = strings.TrimSpace(tokenData.Email)
	}
	if strings.TrimSpace(tokenData.Expire) != "" {
		auth.Metadata["expired"] = strings.TrimSpace(tokenData.Expire)
	}
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	auth.LastRefreshedAt = time.Now()
	auth.UpdatedAt = time.Now()

	if h != nil && h.authManager != nil {
		if saved, errUpdate := h.authManager.Update(ctx, auth); errUpdate == nil && saved != nil {
			auth = saved
		}
	}
	return auth, strings.TrimSpace(tokenData.AccessToken), nil
}

func codexTokenNeedsRefresh(metadata map[string]any) bool {
	if metadata == nil {
		return false
	}
	const skew = 30 * time.Second
	for _, key := range []string{"expired", "expires_at", "expiresAt"} {
		if raw := strings.TrimSpace(stringValue(metadata, key)); raw != "" {
			if ts, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
				return !ts.After(time.Now().Add(skew))
			}
		}
	}
	expiresIn := int64Value(metadata["expires_in"])
	timestamp := int64Value(metadata["timestamp"])
	if expiresIn > 0 && timestamp > 0 {
		expiry := time.UnixMilli(timestamp).Add(time.Duration(expiresIn) * time.Second)
		return !expiry.After(time.Now().Add(skew))
	}
	return false
}

func (h *Handler) requestCodexQuotaEndpoint(ctx context.Context, auth *coreauth.Auth, token, endpoint string, resetCredits bool) (map[string]any, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("Codex quota endpoint is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, errors.New("failed to build Codex quota request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "codex_cli_rs/0.136.0")
	if accountID := codexAccountID(auth); accountID != "" {
		request.Header.Set("ChatGPT-Account-ID", accountID)
	}
	if resetCredits {
		request.Header.Set("OpenAI-Beta", "codex-1")
		request.Header.Set("Originator", "codex_cli_rs")
	}

	client := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth, ""),
	}
	response, errDo := client.Do(request)
	if errDo != nil {
		return nil, fmt.Errorf("Codex quota request failed: %w", errDo)
	}
	body, errRead := readBoundedCodexQuotaResponse(response)
	if errRead != nil {
		return nil, errRead
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, codexQuotaHTTPError(response.StatusCode, body)
	}
	payload, errDecode := decodeCodexQuotaObject(body)
	if errDecode != nil {
		return nil, fmt.Errorf("invalid Codex quota response: %w", errDecode)
	}
	if resetCredits {
		return normalizeCodexResetCreditsPayload(payload), nil
	}
	return payload, nil
}

func readBoundedCodexQuotaResponse(response *http.Response) ([]byte, error) {
	if response == nil {
		return nil, errors.New("empty Codex quota response")
	}
	defer response.Body.Close()
	body, errRead := io.ReadAll(io.LimitReader(response.Body, maxCodexQuotaResponseBody+1))
	if errRead != nil {
		return nil, fmt.Errorf("failed to read Codex quota response: %w", errRead)
	}
	if len(body) > maxCodexQuotaResponseBody {
		return nil, errors.New("Codex quota response body is too large")
	}
	return body, nil
}

func decodeCodexQuotaObject(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var payload map[string]any
	if errDecode := decoder.Decode(&payload); errDecode != nil {
		return nil, errDecode
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); errTrailing != io.EOF {
		if errTrailing == nil {
			return nil, errors.New("trailing JSON data")
		}
		return nil, errTrailing
	}
	if payload == nil {
		return nil, errors.New("response is not an object")
	}
	return payload, nil
}

func codexQuotaHTTPError(status int, body []byte) error {
	_ = body // never surface provider response bodies through management errors
	return fmt.Errorf("Codex quota request returned HTTP %d", status)
}

func isCodexQuotaAuthError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"http 401", "http 403", "unauthorized", "authentication", "token expired", "access token expired"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func codexPlanType(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	for _, key := range []string{"plan_type", "chatgpt_plan_type", "chatgptPlanType"} {
		if value := strings.TrimSpace(stringValue(auth.Metadata, key)); value != "" {
			return value
		}
		if auth.Attributes != nil {
			if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

func normalizeCodexUsagePayload(payload map[string]any) (map[string]any, error) {
	if payload == nil {
		return nil, errors.New("Codex usage response is empty")
	}
	out := make(map[string]any)
	if plan := strings.TrimSpace(stringValueAny(payload, "plan_type", "planType")); plan != "" {
		out["plan_type"] = plan
	}
	if rate := normalizeCodexRateLimit(payloadValue(payload, "rate_limit", "rateLimit", "rate_limits")); rate != nil {
		out["rate_limit"] = rate
	}
	if review := normalizeCodexRateLimit(payloadValue(payload, "code_review_rate_limit", "codeReviewRateLimit")); review != nil {
		out["code_review_rate_limit"] = review
	}
	if additional := normalizeCodexAdditionalRateLimits(payloadValue(payload, "additional_rate_limits", "additionalRateLimits")); len(additional) > 0 {
		out["additional_rate_limits"] = additional
	}
	if credits := normalizeCodexResetCreditsPayload(payloadValue(payload, "rate_limit_reset_credits", "rateLimitResetCredits")); credits != nil {
		out["rate_limit_reset_credits"] = credits
	}
	if len(out) == 0 {
		return nil, errors.New("Codex usage response did not contain quota fields")
	}
	return out, nil
}

func normalizeCodexRateLimit(raw any) map[string]any {
	record, ok := raw.(map[string]any)
	if !ok || record == nil {
		return nil
	}
	out := make(map[string]any)
	copyAnyField(out, record, "allowed", "allowed")
	copyAnyField(out, record, "limit_reached", "limit_reached", "limitReached")
	if primary := normalizeCodexQuotaWindow(payloadValue(record, "primary_window", "primaryWindow", "primary")); primary != nil {
		out["primary_window"] = primary
	}
	if secondary := normalizeCodexQuotaWindow(payloadValue(record, "secondary_window", "secondaryWindow", "secondary")); secondary != nil {
		out["secondary_window"] = secondary
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeCodexQuotaWindow(raw any) map[string]any {
	record, ok := raw.(map[string]any)
	if !ok || record == nil {
		return nil
	}
	out := make(map[string]any)
	copyAnyField(out, record, "used_percent", "used_percent", "usedPercent", "percent_used")
	copyAnyField(out, record, "limit_window_seconds", "limit_window_seconds", "limitWindowSeconds")
	copyAnyField(out, record, "reset_after_seconds", "reset_after_seconds", "resetAfterSeconds")
	copyAnyField(out, record, "reset_at", "reset_at", "resetAt")
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeCodexAdditionalRateLimits(raw any) []map[string]any {
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(values))
	for _, value := range values {
		record, ok := value.(map[string]any)
		if !ok {
			continue
		}
		item := make(map[string]any)
		copyAnyField(item, record, "limit_name", "limit_name", "limitName")
		copyAnyField(item, record, "metered_feature", "metered_feature", "meteredFeature")
		if rate := normalizeCodexRateLimit(payloadValue(record, "rate_limit", "rateLimit")); rate != nil {
			item["rate_limit"] = rate
		}
		if len(item) > 0 {
			out = append(out, item)
		}
	}
	return out
}

func normalizeCodexResetCreditsPayload(raw any) map[string]any {
	record, ok := raw.(map[string]any)
	if !ok || record == nil {
		return nil
	}
	out := make(map[string]any)
	copyAnyField(out, record, "available_count", "available_count", "availableCount")
	copyAnyField(out, record, "applicable_available_count", "applicable_available_count", "applicableAvailableCount")
	if values, ok := record["credits"].([]any); ok {
		credits := make([]map[string]any, 0, len(values))
		for _, value := range values {
			item, ok := value.(map[string]any)
			if !ok {
				continue
			}
			credit := make(map[string]any)
			copyAnyField(credit, item, "id", "id")
			copyAnyField(credit, item, "status", "status")
			copyAnyField(credit, item, "reset_type", "reset_type", "resetType")
			copyAnyField(credit, item, "granted_at", "granted_at", "grantedAt")
			copyAnyField(credit, item, "expires_at", "expires_at", "expiresAt")
			if len(credit) > 0 {
				credits = append(credits, credit)
			}
		}
		if len(credits) > 0 {
			out["credits"] = credits
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func payloadValue(payload map[string]any, keys ...string) any {
	if payload == nil {
		return nil
	}
	for _, key := range keys {
		if value, ok := payload[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func copyAnyField(target, source map[string]any, targetKey string, sourceKeys ...string) {
	if _, exists := target[targetKey]; exists {
		return
	}
	for _, key := range sourceKeys {
		if value, ok := source[key]; ok && value != nil {
			target[targetKey] = value
			return
		}
	}
}

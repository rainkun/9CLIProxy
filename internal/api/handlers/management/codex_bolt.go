package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	codexResetCreditsURL        = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	codexResetCreditsConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	maxCodexBoltResponseBody    = 1 << 20
)

type codexBoltRequest struct {
	AuthIndices []string `json:"auth_indices"`
	AuthIndexes []string `json:"authIndexes"`
	AuthIndex   string   `json:"auth_index"`
	AuthID      string   `json:"auth_id"`
	Names       []string `json:"names"`
	Name        string   `json:"name"`
	All         bool     `json:"all"`
}

// CodexBolt consumes one available Codex rate-limit reset credit for every
// selected Codex auth. The reset-credit list is fetched first so the response
// includes the provider's current reset/expiry time and accounts without a
// credit are not charged.
func (h *Handler) CodexBolt(c *gin.Context) {
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

	applied := make([]gin.H, 0)
	failed := make([]gin.H, 0)
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
			if !byIndex && !byName && !byID {
				continue
			}
		}
		if auth.Disabled || auth.Status == coreauth.StatusDisabled {
			failed = append(failed, gin.H{
				"auth_index": auth.Index,
				"id":         auth.ID,
				"name":       auth.FileName,
				"error":      "Codex auth is disabled",
			})
			continue
		}
		result, errBolt := h.consumeCodexResetCredit(c.Request.Context(), auth)
		if errBolt != nil {
			failed = append(failed, gin.H{
				"auth_index": auth.Index,
				"name":       auth.FileName,
				"error":      errBolt.Error(),
			})
			continue
		}
		entry := gin.H{
			"auth_index": auth.Index,
			"id":         auth.ID,
			"name":       auth.FileName,
			"provider":   auth.Provider,
			"status":     "ok",
		}
		for key, value := range result {
			entry[key] = value
		}
		applied = append(applied, entry)
	}

	if len(applied) == 0 && len(failed) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no matching Codex auth files"})
		return
	}
	status := "ok"
	code := http.StatusOK
	if len(failed) > 0 {
		status = "partial"
		code = http.StatusMultiStatus
	}
	c.JSON(code, gin.H{
		"status":        status,
		"applied":       len(applied),
		"updated_count": len(applied),
		"failed":        failed,
		"failed_count":  len(failed),
		"results":       applied,
	})
}

func (h *Handler) consumeCodexResetCredit(ctx context.Context, auth *coreauth.Auth) (map[string]any, error) {
	if auth == nil {
		return nil, errors.New("auth is missing")
	}
	requestAuth, token, errToken := h.resolveCodexOAuthToken(ctx, auth, false)
	if errToken != nil {
		return nil, errToken
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("Codex access token is missing")
	}

	summary, errLookup := h.lookupCodexResetCredits(ctx, requestAuth, token)
	if errLookup != nil && isCodexQuotaAuthError(errLookup) {
		refreshedAuth, refreshedToken, errRefresh := h.resolveCodexOAuthToken(ctx, requestAuth, true)
		if errRefresh == nil {
			requestAuth, token = refreshedAuth, refreshedToken
			summary, errLookup = h.lookupCodexResetCredits(ctx, requestAuth, token)
		} else {
			errLookup = errRefresh
		}
	}
	if errLookup != nil {
		return nil, errLookup
	}
	if summary.availableCount != nil && *summary.availableCount <= 0 {
		return map[string]any{
			"available_count":            *summary.availableCount,
			"applicable_available_count": summary.applicableAvailableCount,
			"reset_at":                   summary.resetAt,
			"consumed":                   false,
		}, errors.New("no Codex reset credit is available")
	}

	redeemRequestID := uuid.NewString()
	payload, errMarshal := json.Marshal(map[string]string{"redeem_request_id": redeemRequestID})
	if errMarshal != nil {
		return nil, errors.New("failed to build Codex redeem request")
	}
	consume, errConsume := h.consumeCodexResetCreditRequest(ctx, requestAuth, token, payload)
	if errConsume != nil && isCodexQuotaAuthError(errConsume) {
		refreshedAuth, refreshedToken, errRefresh := h.resolveCodexOAuthToken(ctx, requestAuth, true)
		if errRefresh == nil {
			requestAuth, token = refreshedAuth, refreshedToken
			consume, errConsume = h.consumeCodexResetCreditRequest(ctx, requestAuth, token, payload)
		} else {
			errConsume = errRefresh
		}
	}
	if errConsume != nil {
		return nil, errConsume
	}
	if consume.noCredit {
		return map[string]any{
			"available_count":            summary.availableCount,
			"applicable_available_count": summary.applicableAvailableCount,
			"reset_at":                   summary.resetAt,
			"consumed":                   false,
			"code":                       consume.code,
			"message":                    consume.message,
		}, errors.New("no Codex reset credit is available")
	}
	if (consume.code == "" || !strings.EqualFold(consume.code, "reset")) && consume.windowsReset <= 0 {
		return nil, fmt.Errorf("Codex redeem returned code %s", consume.code)
	}
	return map[string]any{
		"available_count":            summary.availableCount,
		"applicable_available_count": summary.applicableAvailableCount,
		"reset_at":                   summary.resetAt,
		"consumed":                   true,
		"code":                       consume.code,
		"windows_reset":              consume.windowsReset,
		"redeem_request_id":          redeemRequestID,
	}, nil
}

func (h *Handler) codexResetHeaders(auth *coreauth.Auth, token string) map[string]string {
	headers := map[string]string{
		"Accept":        "application/json",
		"Authorization": "Bearer " + token,
		"OpenAI-Beta":   "codex-1",
		"Originator":    "codex_cli_rs",
	}
	if accountID := codexAccountID(auth); accountID != "" {
		headers["ChatGPT-Account-ID"] = accountID
	}
	return headers
}

func (h *Handler) lookupCodexResetCredits(ctx context.Context, auth *coreauth.Auth, token string) (codexResetSummary, error) {
	var summary codexResetSummary
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, codexResetCreditsURL, nil)
	if errRequest != nil {
		return summary, errors.New("failed to build Codex reset request")
	}
	for key, value := range h.codexResetHeaders(auth, token) {
		request.Header.Set(key, value)
	}
	client := &http.Client{Timeout: defaultAPICallTimeout, Transport: h.apiCallTransport(auth, "")}
	response, errDo := client.Do(request)
	if errDo != nil {
		return summary, errors.New("Codex reset lookup failed")
	}
	body, errRead := readBoundedResponse(response)
	if errRead != nil {
		return summary, errors.New("failed to read Codex reset response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return summary, fmt.Errorf("Codex reset lookup returned HTTP %d", response.StatusCode)
	}
	return parseCodexResetSummary(body), nil
}

func (h *Handler) consumeCodexResetCreditRequest(ctx context.Context, auth *coreauth.Auth, token string, payload []byte) (codexConsumeSummary, error) {
	var summary codexConsumeSummary
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, codexResetCreditsConsumeURL, bytes.NewReader(payload))
	if errRequest != nil {
		return summary, errors.New("failed to build Codex redeem request")
	}
	for key, value := range h.codexResetHeaders(auth, token) {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: defaultAPICallTimeout, Transport: h.apiCallTransport(auth, "")}
	response, errDo := client.Do(request)
	if errDo != nil {
		return summary, errors.New("Codex reset credit consumption failed")
	}
	body, errRead := readBoundedResponse(response)
	if errRead != nil {
		return summary, errors.New("failed to read Codex redeem response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return summary, fmt.Errorf("Codex redeem returned HTTP %d", response.StatusCode)
	}
	return parseCodexConsumeResponse(body), nil
}

type codexConsumeSummary struct {
	code         string
	message      string
	windowsReset int
	noCredit     bool
}

func parseCodexConsumeResponse(body []byte) codexConsumeSummary {
	var payload map[string]any
	if len(body) == 0 || json.Unmarshal(body, &payload) != nil {
		return codexConsumeSummary{}
	}
	summary := codexConsumeSummary{
		code:    strings.TrimSpace(stringValueAny(payload, "code")),
		message: strings.TrimSpace(stringValueAny(payload, "message", "error", "detail")),
	}
	summary.windowsReset = normalizeCodexCountValue(payload["windows_reset"])
	if summary.windowsReset == 0 {
		summary.windowsReset = normalizeCodexCountValue(payload["windowsReset"])
	}
	summary.noCredit = strings.EqualFold(summary.code, "no_credit") ||
		strings.Contains(strings.ToLower(summary.message), "no credit")
	return summary
}

func stringValueAny(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := payload[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return value
			}
		case json.Number:
			return value.String()
		}
	}
	return ""
}

func normalizeCodexCountValue(value any) int {
	switch typed := value.(type) {
	case float64:
		if typed > 0 {
			return int(typed)
		}
	case json.Number:
		if count, err := typed.Int64(); err == nil && count > 0 {
			return int(count)
		}
	case int:
		if typed > 0 {
			return typed
		}
	case int64:
		if typed > 0 {
			return int(typed)
		}
	}
	return 0
}

type codexResetSummary struct {
	availableCount           *int
	applicableAvailableCount *int
	resetAt                  string
}

func parseCodexResetSummary(body []byte) codexResetSummary {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return codexResetSummary{}
	}
	summary := codexResetSummary{
		availableCount:           normalizeCodexCount(payload["available_count"]),
		applicableAvailableCount: normalizeCodexCount(payload["applicable_available_count"]),
	}
	if summary.availableCount == nil {
		summary.availableCount = normalizeCodexCount(payload["availableCount"])
	}
	if summary.applicableAvailableCount == nil {
		summary.applicableAvailableCount = normalizeCodexCount(payload["applicableAvailableCount"])
	}
	if credits, ok := payload["credits"].([]any); ok {
		for _, value := range credits {
			record, ok := value.(map[string]any)
			if !ok {
				continue
			}
			status, _ := record["status"].(string)
			if !strings.EqualFold(strings.TrimSpace(status), "available") {
				continue
			}
			for _, key := range []string{"expires_at", "expiresAt", "reset_at", "resetAt"} {
				if value, ok := record[key].(string); ok && strings.TrimSpace(value) != "" {
					summary.resetAt = strings.TrimSpace(value)
					return summary
				}
			}
		}
	}
	for _, key := range []string{"reset_at", "resetAt", "expires_at", "expiresAt"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			summary.resetAt = strings.TrimSpace(value)
			break
		}
	}
	return summary
}

func normalizeCodexCount(value any) *int {
	switch typed := value.(type) {
	case float64:
		count := int(typed)
		if typed == float64(count) {
			return &count
		}
	case json.Number:
		if count, err := typed.Int64(); err == nil {
			value := int(count)
			return &value
		}
	case int:
		return &typed
	case int64:
		value := int(typed)
		return &value
	}
	return nil
}

func readBoundedResponse(resp *http.Response) ([]byte, error) {
	if resp == nil {
		return nil, errors.New("empty response")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCodexBoltResponseBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCodexBoltResponseBody {
		return nil, errors.New("response body is too large")
	}
	return data, nil
}

func codexAccountID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	for _, key := range []string{"account_id", "chatgpt_account_id", "chatgptAccountId"} {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	for _, key := range []string{"provider_specific_data", "providerSpecificData"} {
		if nested, ok := auth.Metadata[key].(map[string]any); ok {
			for _, nestedKey := range []string{"account_id", "chatgpt_account_id", "chatgptAccountId", "workspace_id", "workspaceId"} {
				if value, ok := nested[nestedKey].(string); ok && strings.TrimSpace(value) != "" {
					return strings.TrimSpace(value)
				}
			}
		}
	}
	for _, key := range []string{"account_id", "chatgpt_account_id", "chatgptAccountId"} {
		if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
			return value
		}
	}
	return ""
}

package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

const (
	defaultProxyTestURL = "https://api.ipify.org?format=json"
	maxProxyTestBody    = 1 << 20
	maxImportBody       = 64 << 20
)

type proxyTestRequest struct {
	ProxyURL       string `json:"proxy_url"`
	ProxyURLCamel  string `json:"proxyUrl"`
	Proxy          string `json:"proxy"`
	URL            string `json:"url"`
	AuthIndex      string `json:"auth_index"`
	AuthIndexCamel string `json:"authIndex"`
	AuthID         string `json:"auth_id"`
	Name           string `json:"name"`
}

// TestProxyURL performs a small outbound request through the selected proxy.
// It is intentionally separate from the generic API-call endpoint so the UI can
// validate a proxy without constructing provider-specific headers.
func (h *Handler) TestProxyURL(c *gin.Context) {
	var req proxyTestRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	proxyURL := strings.TrimSpace(req.ProxyURL)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(req.ProxyURLCamel)
	}
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(req.Proxy)
	}
	if proxyURL != "" {
		if _, errParse := proxyutil.Parse(proxyURL); errParse != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid proxy_url"})
			return
		}
	}

	targetURL := strings.TrimSpace(req.URL)
	if targetURL == "" {
		targetURL = defaultProxyTestURL
	}
	parsedURL, errParseURL := url.Parse(targetURL)
	if errParseURL != nil || parsedURL.Scheme == "" || parsedURL.Host == "" ||
		(parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url"})
		return
	}

	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		authIndex = strings.TrimSpace(req.AuthIndexCamel)
	}
	if authIndex == "" {
		authIndex = strings.TrimSpace(req.AuthID)
	}
	if authIndex == "" {
		authIndex = strings.TrimSpace(req.Name)
	}
	var auth *coreauth.Auth
	if authIndex != "" {
		auth = h.authByIndex(authIndex)
		if auth == nil {
			auth, _ = h.lookupAuthFile(authIndex, "")
		}
		if auth == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultAPICallTimeout)
	defer cancel()
	request, errNewRequest := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if errNewRequest != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to build request"})
		return
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CLIProxyAPI proxy test")

	startedAt := time.Now()
	client := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth, proxyURL),
	}
	response, errDo := client.Do(request)
	if errDo != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"status":    "error",
			"proxy_url": redactProxySetting(proxyURL),
			"url":       targetURL,
			"error":     "request failed",
		})
		return
	}
	defer func() {
		_ = response.Body.Close()
	}()

	body, errRead := io.ReadAll(io.LimitReader(response.Body, maxProxyTestBody+1))
	if errRead != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response"})
		return
	}
	if len(body) > maxProxyTestBody {
		c.JSON(http.StatusBadGateway, gin.H{"error": "proxy test response is too large"})
		return
	}

	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	ip, _ := decoded["ip"].(string)
	statusOK := response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
	c.JSON(http.StatusOK, gin.H{
		"status":       map[bool]string{true: "ok", false: "error"}[statusOK],
		"ok":           statusOK,
		"proxy_url":    redactProxySetting(proxyURL),
		"url":          targetURL,
		"status_code":  response.StatusCode,
		"ip":           strings.TrimSpace(ip),
		"body":         string(body),
		"latency_ms":   time.Since(startedAt).Milliseconds(),
		"content_type": response.Header.Get("Content-Type"),
	})
}

type applyProxyRequest struct {
	ProxyURL      string   `json:"proxy_url"`
	ProxyURLCamel string   `json:"proxyUrl"`
	Proxy         string   `json:"proxy"`
	AuthIndices   []string `json:"auth_indices"`
	AuthIndexes   []string `json:"authIndexes"`
	AuthIndex     string   `json:"auth_index"`
	AuthID        string   `json:"auth_id"`
	Names         []string `json:"names"`
	Name          string   `json:"name"`
	All           bool     `json:"all"`
}

// ApplyProxyToAuthFiles applies one proxy setting to selected credentials.
// Empty proxy_url removes the per-account override; "direct"/"none" explicitly
// bypasses the global proxy.
func (h *Handler) ApplyProxyToAuthFiles(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var req applyProxyRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	proxyURL := strings.TrimSpace(req.ProxyURL)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(req.ProxyURLCamel)
	}
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(req.Proxy)
	}
	if proxyURL != "" {
		if _, errParse := proxyutil.Parse(proxyURL); errParse != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid proxy_url"})
			return
		}
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
	for _, index := range indices {
		indexSet[index] = struct{}{}
	}
	nameSet := make(map[string]struct{}, len(names))
	for _, name := range names {
		nameSet[name] = struct{}{}
	}

	updated := make([]gin.H, 0)
	failed := make([]gin.H, 0)
	for _, current := range h.authManager.List() {
		if current == nil {
			continue
		}
		current.EnsureIndex()
		matches := req.All
		if _, ok := indexSet[current.Index]; ok {
			matches = true
		}
		// auth_id is accepted as an alias of auth_indices/auth_index. Keep
		// matching it against the stable runtime ID as well as the generated
		// index; otherwise a request that supplies only auth_id silently
		// selects nothing.
		if _, ok := indexSet[current.ID]; ok {
			matches = true
		}
		if _, ok := nameSet[current.FileName]; ok {
			matches = true
		}
		if _, ok := nameSet[current.ID]; ok {
			matches = true
		}
		if !matches {
			continue
		}
		if coreauth.IsPluginVirtualAuth(current) {
			failed = append(failed, gin.H{
				"auth_index": current.Index,
				"name":       current.FileName,
				"error":      "plugin virtual auth must be changed at its source file",
			})
			continue
		}

		auth := current.Clone()
		auth.ProxyURL = proxyURL
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		if proxyURL == "" {
			delete(auth.Metadata, "proxy_url")
		} else {
			auth.Metadata["proxy_url"] = proxyURL
		}
		auth.UpdatedAt = time.Now()
		saved, errUpdate := h.authManager.Update(c.Request.Context(), auth)
		if errUpdate != nil {
			failed = append(failed, gin.H{"auth_index": current.Index, "name": current.FileName, "error": errUpdate.Error()})
			continue
		}
		if saved == nil {
			failed = append(failed, gin.H{"auth_index": current.Index, "name": current.FileName, "error": "auth not found"})
			continue
		}
		saved.EnsureIndex()
		updated = append(updated, gin.H{
			"auth_index": saved.Index,
			"name":       saved.FileName,
			"provider":   saved.Provider,
			"proxy_url":  redactProxySetting(proxyURL),
		})
	}

	if len(updated) == 0 && len(failed) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no matching auth files"})
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
		"proxy_url":     redactProxySetting(proxyURL),
		"updated":       updated,
		"failed":        failed,
		"updated_count": len(updated),
		"failed_count":  len(failed),
	})
}

type importedAuthSummary struct {
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Email     string `json:"email,omitempty"`
	AuthIndex string `json:"auth_index,omitempty"`
	// Storage identifies whether the credential was written as an auth file or
	// as a provider configuration entry. Gemini API keys use config storage
	// because the runtime intentionally ignores type=gemini auth files.
	Storage     string `json:"storage,omitempty"`
	ConfigIndex *int   `json:"config_index,omitempty"`
}

type nineRouterImportResult struct {
	Imported []importedAuthSummary `json:"imported"`
	Skipped  []gin.H               `json:"skipped"`
	Failed   []gin.H               `json:"failed"`
}

type pendingGeminiConfigImport struct {
	index  int
	record map[string]any
	entry  config.GeminiKey
}

// ImportNineRouterAuthFile imports providerConnections/accounts exports from
// 9router and writes one normal CLIProxyAPI auth JSON per account.
func (h *Handler) ImportNineRouterAuthFile(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	files, errMultipart := h.multipartAuthFileHeaders(c)
	if errMultipart != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid multipart form: %v", errMultipart)})
		return
	}

	combined := nineRouterImportResult{
		Imported: make([]importedAuthSummary, 0),
		Skipped:  make([]gin.H, 0),
		Failed:   make([]gin.H, 0),
	}
	if len(files) > 0 {
		for _, file := range files {
			if file == nil {
				continue
			}
			name := filepath.Base(strings.TrimSpace(file.Filename))
			if !strings.HasSuffix(strings.ToLower(name), ".json") {
				combined.Failed = append(combined.Failed, gin.H{"name": name, "error": "file must be .json"})
				continue
			}
			src, errOpen := file.Open()
			if errOpen != nil {
				combined.Failed = append(combined.Failed, gin.H{"name": name, "error": errOpen.Error()})
				continue
			}
			data, errRead := io.ReadAll(io.LimitReader(src, maxImportBody+1))
			_ = src.Close()
			if errRead != nil {
				combined.Failed = append(combined.Failed, gin.H{"name": name, "error": errRead.Error()})
				continue
			}
			if len(data) > maxImportBody {
				combined.Failed = append(combined.Failed, gin.H{"name": name, "error": "file is too large"})
				continue
			}
			result, errImport := h.importNineRouterData(c.Request.Context(), name, data)
			if errImport != nil {
				combined.Failed = append(combined.Failed, gin.H{"name": name, "error": errImport.Error()})
				continue
			}
			combined.Imported = append(combined.Imported, result.Imported...)
			combined.Skipped = append(combined.Skipped, result.Skipped...)
			combined.Failed = append(combined.Failed, result.Failed...)
		}
	} else {
		data, errRead := io.ReadAll(io.LimitReader(c.Request.Body, maxImportBody+1))
		if errRead != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
			return
		}
		if len(data) > maxImportBody {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file is too large"})
			return
		}
		result, errImport := h.importNineRouterData(c.Request.Context(), "9router-backup.json", data)
		if errImport != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errImport.Error()})
			return
		}
		combined = *result
	}

	status := "ok"
	code := http.StatusOK
	if len(combined.Failed) > 0 {
		status = "partial"
		code = http.StatusMultiStatus
	}
	c.JSON(code, gin.H{
		"status":   status,
		"imported": combined.Imported,
		"skipped":  combined.Skipped,
		"failed":   combined.Failed,
		"counts": gin.H{
			"imported": len(combined.Imported),
			"skipped":  len(combined.Skipped),
			"failed":   len(combined.Failed),
		},
	})
}

func (h *Handler) importNineRouterData(ctx context.Context, sourceName string, data []byte) (*nineRouterImportResult, error) {
	backup, recognized, errDecode := decodeNineRouterBackup(data)
	if errDecode != nil {
		return nil, errDecode
	}
	if !recognized {
		return nil, fmt.Errorf("not a recognized 9router backup")
	}
	records := backup.Connections
	result := &nineRouterImportResult{
		Imported: make([]importedAuthSummary, 0, len(records)),
		Skipped:  make([]gin.H, 0),
		Failed:   make([]gin.H, 0),
	}
	compatEntries, compatPending, compatHandled := planImportedOpenAICompat(backup, result)
	usedNames := make(map[string]struct{}, len(records))
	pendingGemini := make([]pendingGeminiConfigImport, 0)
	for index, record := range records {
		if _, handled := compatHandled[index]; handled {
			continue
		}
		provider := importedProvider(record)
		if provider == "" {
			result.Skipped = append(result.Skipped, gin.H{"index": index, "reason": "provider could not be detected"})
			continue
		}
		if !hasImportedCredential(record) {
			result.Skipped = append(result.Skipped, gin.H{"index": index, "provider": provider, "reason": "credential token is missing"})
			continue
		}
		if provider == "gemini" {
			if !isGeminiAPIKeyRecord(record) {
				// A type=gemini auth JSON is deliberately ignored by the file
				// synthesizer; do not create a file that appears imported but
				// can never become a runtime auth.
				result.Skipped = append(result.Skipped, gin.H{
					"index":    index,
					"provider": provider,
					"reason":   "Gemini OAuth records are not supported as auth files; import a Gemini API key or Gemini CLI credential",
				})
				continue
			}
			entry, errEntry := importedGeminiConfigEntry(record)
			if errEntry != nil {
				result.Failed = append(result.Failed, gin.H{"index": index, "provider": provider, "error": errEntry.Error()})
				continue
			}
			pendingGemini = append(pendingGemini, pendingGeminiConfigImport{
				index:  index,
				record: record,
				entry:  entry,
			})
			continue
		}
		metadata := normalizeImportedCredential(record, provider)
		name := importedCredentialFileName(record, provider, index)
		for {
			if _, exists := usedNames[name]; !exists {
				break
			}
			name = strings.TrimSuffix(name, ".json") + "-dup.json"
		}
		usedNames[name] = struct{}{}
		raw, errMarshal := json.Marshal(metadata)
		if errMarshal != nil {
			result.Failed = append(result.Failed, gin.H{"index": index, "provider": provider, "error": errMarshal.Error()})
			continue
		}
		if errWrite := h.writeAuthFile(ctx, name, raw); errWrite != nil {
			result.Failed = append(result.Failed, gin.H{"index": index, "name": name, "provider": provider, "error": errWrite.Error()})
			continue
		}
		auth := h.authByFileName(name)
		summary := importedAuthSummary{Name: name, Provider: provider}
		if email, ok := metadata["email"].(string); ok {
			summary.Email = email
		}
		if auth != nil {
			auth.EnsureIndex()
			summary.AuthIndex = auth.Index
		}
		result.Imported = append(result.Imported, summary)
	}
	h.persistImportedOpenAICompatConfig(ctx, compatEntries, compatPending, result)
	h.persistImportedGeminiConfig(ctx, pendingGemini, result)
	_ = sourceName // retained for future audit metadata without exposing secrets
	return result, nil
}

func isGeminiAPIKeyRecord(record map[string]any) bool {
	if record == nil {
		return false
	}
	authType := strings.ToLower(strings.TrimSpace(importStringValue(record, "authType", "auth_type", "authMethod", "auth_method")))
	if isOAuthLikeAuthType(authType) {
		return false
	}
	return importedAPIKeyValue(record) != ""
}

func importedGeminiConfigEntry(record map[string]any) (config.GeminiKey, error) {
	var entry config.GeminiKey
	if record == nil {
		return entry, fmt.Errorf("Gemini record is empty")
	}

	nested := importedProviderSpecificData(record)
	entry.APIKey = importedAPIKeyValue(record)
	if entry.APIKey == "" {
		return entry, fmt.Errorf("Gemini API key is missing")
	}
	entry.BaseURL = importedFirstString(record, nested,
		[]string{"baseUrl", "base_url", "baseURL"},
		[]string{"baseUrl", "base_url", "baseURL"})
	entry.Prefix = importedFirstString(record, nested,
		[]string{"prefix"},
		[]string{"prefix"})
	entry.ProxyURL = importedGeminiProxyURL(record, nested)
	if entry.ProxyURL != "" {
		if _, errParse := proxyutil.Parse(entry.ProxyURL); errParse != nil {
			return entry, fmt.Errorf("invalid Gemini proxy URL: %w", errParse)
		}
	}
	entry.Priority = importedFirstInt(record, nested,
		[]string{"priority"},
		[]string{"priority"})
	entry.Weight = importedFirstOptionalInt(record, nested,
		[]string{"weight"},
		[]string{"weight"})
	if errWeight := config.ValidateCredentialWeight(entry.Weight); errWeight != nil {
		return entry, fmt.Errorf("invalid Gemini weight: %w", errWeight)
	}
	entry.Headers = importedFirstStringMap(record, nested,
		[]string{"headers"},
		[]string{"headers"})
	entry.ExcludedModels = importedFirstStringSlice(record, nested,
		[]string{"excludedModels", "excluded_models", "excluded-models"},
		[]string{"excludedModels", "excluded_models", "excluded-models"})
	if importedRecordDisabled(record) {
		entry.ExcludedModels = append(entry.ExcludedModels, "*")
	}
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
	return entry, nil
}

func importedAPIKeyValue(record map[string]any) string {
	value := strings.TrimSpace(importStringValue(record, "apiKey", "api_key", "key"))
	if value != "" {
		return value
	}
	if nested := importedProviderSpecificData(record); nested != nil {
		return strings.TrimSpace(importStringValue(nested, "apiKey", "api_key", "key"))
	}
	return ""
}

func importedGeminiProxyURL(record, nested map[string]any) string {
	if value := strings.TrimSpace(importStringValue(record, "proxyUrl", "proxy_url")); value != "" {
		return value
	}
	if nested == nil {
		return ""
	}
	enabled := true
	if rawEnabled, ok := nested["connectionProxyEnabled"]; ok {
		if parsed, okParsed := importedBoolValue(rawEnabled); okParsed {
			enabled = parsed
		}
	}
	if !enabled {
		return ""
	}
	return strings.TrimSpace(importStringValue(nested, "connectionProxyUrl", "connection_proxy_url", "proxyUrl", "proxy_url"))
}

func importedFirstString(record, nested map[string]any, recordKeys, nestedKeys []string) string {
	if value := strings.TrimSpace(importStringValue(record, recordKeys...)); value != "" {
		return value
	}
	if nested != nil {
		return strings.TrimSpace(importStringValue(nested, nestedKeys...))
	}
	return ""
}

func importedFirstInt(record, nested map[string]any, recordKeys, nestedKeys []string) int {
	if value, ok := importedOptionalIntValue(record, recordKeys...); ok {
		return value
	}
	if nested != nil {
		if value, ok := importedOptionalIntValue(nested, nestedKeys...); ok {
			return value
		}
	}
	return 0
}

func importedFirstOptionalInt(record, nested map[string]any, recordKeys, nestedKeys []string) *int {
	if value, ok := importedOptionalIntValue(record, recordKeys...); ok {
		return &value
	}
	if nested != nil {
		if value, ok := importedOptionalIntValue(nested, nestedKeys...); ok {
			return &value
		}
	}
	return nil
}

func importedOptionalIntValue(object map[string]any, keys ...string) (int, bool) {
	if object == nil {
		return 0, false
	}
	for _, key := range keys {
		raw, ok := object[key]
		if !ok || raw == nil {
			continue
		}
		switch typed := raw.(type) {
		case int:
			return typed, true
		case int64:
			if int64(int(typed)) == typed {
				return int(typed), true
			}
		case json.Number:
			parsed, errParse := strconv.ParseInt(typed.String(), 10, 64)
			if errParse == nil && int64(int(parsed)) == parsed {
				return int(parsed), true
			}
		case float64:
			parsed := int64(typed)
			if float64(parsed) == typed && int64(int(parsed)) == parsed {
				return int(parsed), true
			}
		case string:
			parsed, errParse := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
			if errParse == nil && int64(int(parsed)) == parsed {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

func importedFirstStringMap(record, nested map[string]any, recordKeys, nestedKeys []string) map[string]string {
	if out := importedStringMapValue(record, recordKeys...); len(out) > 0 {
		return out
	}
	if nested != nil {
		return importedStringMapValue(nested, nestedKeys...)
	}
	return nil
}

func importedStringMapValue(object map[string]any, keys ...string) map[string]string {
	if object == nil {
		return nil
	}
	for _, key := range keys {
		raw, ok := object[key]
		if !ok || raw == nil {
			continue
		}
		source, okMap := raw.(map[string]any)
		if !okMap {
			if typed, okTyped := raw.(map[string]string); okTyped {
				out := make(map[string]string, len(typed))
				for name, value := range typed {
					if strings.TrimSpace(name) != "" {
						out[name] = value
					}
				}
				return out
			}
			continue
		}
		out := make(map[string]string, len(source))
		for name, value := range source {
			if strings.TrimSpace(name) == "" || value == nil {
				continue
			}
			switch typed := value.(type) {
			case string:
				out[name] = typed
			default:
				out[name] = fmt.Sprint(typed)
			}
		}
		return out
	}
	return nil
}

func importedFirstStringSlice(record, nested map[string]any, recordKeys, nestedKeys []string) []string {
	if out := importedStringSliceValue(record, recordKeys...); len(out) > 0 {
		return out
	}
	if nested != nil {
		return importedStringSliceValue(nested, nestedKeys...)
	}
	return nil
}

func importedStringSliceValue(object map[string]any, keys ...string) []string {
	if object == nil {
		return nil
	}
	for _, key := range keys {
		raw, ok := object[key]
		if !ok || raw == nil {
			continue
		}
		values, okSlice := raw.([]any)
		if !okSlice {
			if typed, okTyped := raw.([]string); okTyped {
				values = make([]any, 0, len(typed))
				for _, value := range typed {
					values = append(values, value)
				}
			} else {
				continue
			}
		}
		out := make([]string, 0, len(values))
		for _, value := range values {
			if text, okText := value.(string); okText && strings.TrimSpace(text) != "" {
				out = append(out, strings.TrimSpace(text))
			}
		}
		return out
	}
	return nil
}

func importedRecordDisabled(record map[string]any) bool {
	if record == nil {
		return false
	}
	if raw, ok := record["disabled"]; ok {
		if disabled, parsed := importedBoolValue(raw); parsed {
			return disabled
		}
	}
	for _, key := range []string{"isActive", "active"} {
		if raw, ok := record[key]; ok {
			if active, parsed := importedBoolValue(raw); parsed {
				return !active
			}
		}
	}
	return false
}

func (h *Handler) persistImportedGeminiConfig(ctx context.Context, pending []pendingGeminiConfigImport, result *nineRouterImportResult) {
	if h == nil || h.cfg == nil || len(pending) == 0 || result == nil {
		return
	}

	h.mu.Lock()
	previous := append([]config.GeminiKey(nil), h.cfg.GeminiKey...)
	h.cfg.GeminiKey = append(h.cfg.GeminiKey, make([]config.GeminiKey, 0, len(pending))...)
	for _, item := range pending {
		h.cfg.GeminiKey = append(h.cfg.GeminiKey, item.entry)
	}
	h.cfg.SanitizeGeminiKeys()
	if strings.TrimSpace(h.configFilePath) == "" {
		h.cfg.GeminiKey = previous
		h.mu.Unlock()
		for _, item := range pending {
			result.Failed = append(result.Failed, gin.H{
				"index":    item.index,
				"provider": "gemini",
				"error":    "configuration file path is empty; Gemini API key was not persisted",
			})
		}
		return
	}
	if errSave := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); errSave != nil {
		h.cfg.GeminiKey = previous
		h.mu.Unlock()
		for _, item := range pending {
			result.Failed = append(result.Failed, gin.H{
				"index":    item.index,
				"provider": "gemini",
				"error":    fmt.Sprintf("failed to persist Gemini API key: %v", errSave),
			})
		}
		return
	}
	snapshot := h.reloadSnapshotConfigLocked()
	h.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	// Apply synchronously so the response can truthfully report only runtime
	// credentials that are visible to the auth manager.
	h.reloadConfigAfterManagementSave(ctx, snapshot)
	for _, item := range pending {
		auth, errAuth := h.ensureGeminiConfigAuth(ctx, item.entry)
		if errAuth != nil {
			result.Failed = append(result.Failed, gin.H{
				"index":    item.index,
				"provider": "gemini",
				"error":    errAuth.Error(),
			})
			continue
		}
		auth.EnsureIndex()
		configIndex := h.geminiConfigIndex(item.entry)
		summary := importedAuthSummary{
			Name:      fmt.Sprintf("config:gemini-api-key[%d]", configIndex),
			Provider:  "gemini",
			AuthIndex: auth.Index,
			Storage:   "config",
		}
		if configIndex >= 0 {
			summary.ConfigIndex = &configIndex
		}
		if email := importStringValue(item.record, "email", "name", "displayName", "display_name"); email != "" {
			summary.Email = email
		}
		result.Imported = append(result.Imported, summary)
	}
}

func (h *Handler) ensureGeminiConfigAuth(ctx context.Context, entry config.GeminiKey) (*coreauth.Auth, error) {
	if h == nil || h.authManager == nil {
		return nil, fmt.Errorf("core auth manager unavailable")
	}
	cfg := &config.Config{GeminiKey: []config.GeminiKey{entry}}
	generated, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		return nil, fmt.Errorf("failed to synthesize Gemini runtime auth: %w", errSynthesize)
	}
	if len(generated) == 0 || generated[0] == nil {
		return nil, fmt.Errorf("Gemini API key did not produce a runtime auth")
	}
	auth := generated[0]
	if existing, ok := h.authManager.GetByID(auth.ID); ok && existing != nil {
		return existing, nil
	}
	registered, errRegister := h.authManager.Register(coreauth.WithSkipPersist(ctx), auth)
	if errRegister != nil || registered == nil {
		if errRegister != nil {
			return nil, fmt.Errorf("failed to register Gemini runtime auth: %w", errRegister)
		}
		return nil, fmt.Errorf("failed to register Gemini runtime auth")
	}
	return registered, nil
}

func (h *Handler) geminiConfigIndex(entry config.GeminiKey) int {
	if h == nil || h.cfg == nil {
		return -1
	}
	for index, current := range h.cfg.GeminiKey {
		if strings.TrimSpace(current.APIKey) != strings.TrimSpace(entry.APIKey) ||
			strings.TrimSpace(current.BaseURL) != strings.TrimSpace(entry.BaseURL) ||
			strings.TrimSpace(current.ProxyURL) != strings.TrimSpace(entry.ProxyURL) ||
			strings.TrimSpace(current.Prefix) != strings.TrimSpace(entry.Prefix) ||
			config.FormatSortedHeaders(current.Headers) != config.FormatSortedHeaders(entry.Headers) {
			continue
		}
		return index
	}
	return -1
}

func (h *Handler) authByFileName(name string) *coreauth.Auth {
	name = filepath.Base(strings.TrimSpace(name))
	if h == nil || h.authManager == nil || name == "" {
		return nil
	}
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if filepath.Base(strings.TrimSpace(auth.FileName)) == name {
			return auth
		}
	}
	return nil
}

func decodeNineRouterConnections(data []byte) ([]map[string]any, bool, error) {
	backup, recognized, errDecode := decodeNineRouterBackup(data)
	if errDecode != nil || backup == nil {
		return nil, recognized, errDecode
	}
	return backup.Connections, recognized, nil
}

func mapsFromAny(values []any) []map[string]any {
	out := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if object, ok := value.(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out
}

func importedProvider(record map[string]any) string {
	provider := strings.ToLower(strings.TrimSpace(importStringValue(record, "provider", "type", "provider_id", "providerId")))
	authType := strings.ToLower(strings.TrimSpace(importStringValue(record, "authType", "auth_type", "authMethod", "auth_method")))
	switch provider {
	case "anthropic":
		provider = "claude"
	case "google":
		provider = "gemini"
	case "x-ai", "x_ai", "x.ai":
		provider = "xai"
	case "chatgpt", "chatgpt-oauth", "codex-cli", "codex-oauth":
		provider = "codex"
	case "openai":
		// 9router stores Codex OAuth connections under the OpenAI family in
		// some older exports. An API-key record should remain OpenAI-like,
		// while OAuth/access-token records are Codex connections.
		if looksLikeCodex(record) || isOAuthLikeAuthType(authType) ||
			importStringValue(record, "idToken", "id_token") != "" {
			provider = "codex"
		}
	}
	if provider == "" && looksLikeCodex(record) {
		provider = "codex"
	}
	return provider
}

func isOAuthLikeAuthType(authType string) bool {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(authType)), "-", "_") {
	case "oauth", "access_token", "accesstoken", "oauth2", "authorization_code", "authorization_code_pkce":
		return true
	default:
		return false
	}
}

func looksLikeCodex(record map[string]any) bool {
	if nested := importedProviderSpecificData(record); nested != nil {
		if strings.TrimSpace(importStringValue(nested, "chatgptAccountId", "chatgpt_account_id", "account_id", "workspaceId", "workspace_id")) != "" {
			return true
		}
	}
	for _, token := range importedTokenCandidates(record) {
		if claims, errParse := codexauth.ParseJWTToken(strings.TrimSpace(token)); errParse == nil && claims != nil {
			if strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID) != "" ||
				strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType) != "" {
				return true
			}
		}
	}
	return false
}

func hasImportedCredential(record map[string]any) bool {
	for _, key := range []string{"accessToken", "access_token", "refreshToken", "refresh_token", "idToken", "id_token", "apiKey", "api_key", "token"} {
		if value := strings.TrimSpace(importStringValue(record, key)); value != "" {
			return true
		}
	}
	if nested := importedProviderSpecificData(record); nested != nil {
		if hasImportedCredential(nested) {
			return true
		}
	}
	for _, key := range []string{"tokens", "tokenData", "token_data", "credentials", "credential"} {
		if nested, ok := record[key].(map[string]any); ok && hasImportedCredential(nested) {
			return true
		}
	}
	return false
}

func importedTokenCandidates(record map[string]any) []string {
	if record == nil {
		return nil
	}
	out := make([]string, 0, 6)
	appendIf := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range out {
			if existing == value {
				return
			}
		}
		out = append(out, value)
	}
	appendIf(importStringValue(record, "idToken", "id_token"))
	appendIf(importStringValue(record, "accessToken", "access_token"))
	for _, key := range []string{"tokens", "tokenData", "token_data", "credentials", "credential"} {
		if nested, ok := record[key].(map[string]any); ok {
			appendIf(importStringValue(nested, "idToken", "id_token"))
			appendIf(importStringValue(nested, "accessToken", "access_token"))
		}
	}
	if nested := importedProviderSpecificData(record); nested != nil {
		appendIf(importStringValue(nested, "idToken", "id_token"))
		appendIf(importStringValue(nested, "accessToken", "access_token"))
	}
	return out
}

func normalizeImportedCredential(record map[string]any, provider string) map[string]any {
	out := make(map[string]any, len(record)+12)
	for key, value := range record {
		if strings.HasPrefix(strings.ToLower(key), "modellock_") {
			// 9router backups can contain the same model lock under casing
			// variants (for example, modelLock_GPT-5.6-sol and
			// modelLock_gpt-5.6-sol). JSON itself permits this, but several
			// Windows JSON consumers reject case-insensitive duplicate keys.
			// Canonicalize the model portion and retain the most recent value.
			canonicalKey := "modelLock_" + strings.ToLower(key[len("modelLock_"):])
			if previous, exists := out[canonicalKey]; exists {
				out[canonicalKey] = preferredImportedModelLockValue(previous, value)
			} else {
				out[canonicalKey] = value
			}
			continue
		}
		out[key] = value
	}
	out["type"] = provider
	// 9router exports `isActive` while CLIProxyAPI persists `disabled`.
	// Preserve an explicitly inactive connection instead of silently enabling it.
	if _, hasDisabled := out["disabled"]; !hasDisabled {
		if rawActive, ok := record["isActive"]; ok {
			if active, parsed := importedBoolValue(rawActive); parsed {
				out["disabled"] = !active
			}
		} else if rawActive, ok := record["active"]; ok {
			if active, parsed := importedBoolValue(rawActive); parsed {
				out["disabled"] = !active
			}
		}
	}
	copyStringAlias(out, record, "access_token", "accessToken", "access_token")
	copyStringAlias(out, record, "refresh_token", "refreshToken", "refresh_token")
	copyStringAlias(out, record, "id_token", "idToken", "id_token")
	copyStringAlias(out, record, "token_type", "tokenType", "token_type")
	copyStringAlias(out, record, "email", "email")
	copyStringAlias(out, record, "expired", "expiresAt", "expires_at", "expired")
	copyStringAlias(out, record, "last_refresh", "lastRefreshAt", "last_refresh", "lastRefreshedAt")
	copyStringAlias(out, record, "project_id", "projectId", "project_id")
	copyStringAlias(out, record, "api_key", "apiKey", "api_key")
	copyStringAlias(out, record, "scope", "scope")
	copyStringAlias(out, record, "device_id", "deviceId", "device_id")
	copyStringAlias(out, record, "proxy_url", "proxyUrl", "proxy_url")
	copyStringAlias(out, record, "base_url", "baseUrl", "base_url", "baseURL")
	copyStringAlias(out, record, "prefix", "prefix")
	if importStringValue(out, "label") == "" {
		if label := importStringValue(record, "displayName", "display_name", "name"); label != "" {
			out["label"] = label
		}
	}
	if nested := importedProviderSpecificData(record); nested != nil {
		out["provider_specific_data"] = nested
		copyStringAlias(out, nested, "base_url", "baseUrl", "base_url", "baseURL")
		copyStringAlias(out, nested, "prefix", "prefix")
		if accountID := importStringValue(nested, "chatgptAccountId", "chatgpt_account_id", "account_id", "workspaceId", "workspace_id"); accountID != "" {
			out["account_id"] = accountID
		}
		if plan := importStringValue(nested, "chatgptPlanType", "chatgpt_plan_type", "planType", "plan_type"); plan != "" {
			out["plan_type"] = plan
		}
		if email := importStringValue(nested, "email"); email != "" && importStringValue(out, "email") == "" {
			out["email"] = email
		}
		// 9router stores per-connection proxy settings in providerSpecificData.
		// Carry a concrete URL over when enabled; proxy-pool IDs remain in the
		// preserved nested object for manual resolution by the UI.
		if importStringValue(out, "proxy_url") == "" {
			proxyEnabled := true
			if rawEnabled, ok := nested["connectionProxyEnabled"]; ok {
				switch enabled := rawEnabled.(type) {
				case bool:
					proxyEnabled = enabled
				case string:
					if parsed, errParse := strconv.ParseBool(strings.TrimSpace(enabled)); errParse == nil {
						proxyEnabled = parsed
					}
				}
			}
			if proxyEnabled {
				if proxy := importStringValue(nested, "connectionProxyUrl", "connection_proxy_url"); proxy != "" {
					out["proxy_url"] = proxy
				}
			}
		}
	}
	// Some backup producers nest token fields under a `tokens`/`credentials`
	// object. Flatten those aliases without discarding the original payload.
	for _, key := range []string{"tokens", "tokenData", "token_data", "credentials", "credential"} {
		if nested, ok := record[key].(map[string]any); ok {
			copyStringAlias(out, nested, "access_token", "accessToken", "access_token")
			copyStringAlias(out, nested, "refresh_token", "refreshToken", "refresh_token")
			copyStringAlias(out, nested, "id_token", "idToken", "id_token")
			copyStringAlias(out, nested, "api_key", "apiKey", "api_key")
		}
	}
	if provider == "codex" {
		for _, token := range importedTokenCandidates(out) {
			claims, errParse := codexauth.ParseJWTToken(token)
			if errParse != nil || claims == nil {
				continue
			}
			if importStringValue(out, "email") == "" {
				if email := strings.TrimSpace(claims.GetUserEmail()); email != "" {
					out["email"] = email
				}
			}
			if importStringValue(out, "account_id") == "" {
				if accountID := strings.TrimSpace(claims.GetAccountID()); accountID != "" {
					out["account_id"] = accountID
				}
			}
			if plan := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); plan != "" {
				out["plan_type"] = plan
			}
			// The first valid JWT is enough; subsequent tokens are usually the
			// same identity and should not overwrite an explicit field.
			break
		}
	}
	delete(out, "providerConnections")
	delete(out, "provider_connections")
	return out
}

func preferredImportedModelLockValue(current, incoming any) any {
	if current == nil {
		return incoming
	}
	if incoming == nil {
		return current
	}

	currentText, currentIsString := current.(string)
	incomingText, incomingIsString := incoming.(string)
	if currentIsString && incomingIsString {
		currentTime, currentErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(currentText))
		incomingTime, incomingErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(incomingText))
		if currentErr == nil && incomingErr == nil {
			if incomingTime.After(currentTime) {
				return incoming
			}
			return current
		}
		if incomingText > currentText {
			return incoming
		}
		return current
	}

	// Model locks are normally timestamps, but keep a deterministic fallback
	// for older exports that used another scalar representation.
	if fmt.Sprintf("%T:%v", incoming, incoming) > fmt.Sprintf("%T:%v", current, current) {
		return incoming
	}
	return current
}

func importedBoolValue(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return parsed, err == nil
	case json.Number:
		switch typed.String() {
		case "1":
			return true, true
		case "0":
			return false, true
		}
	}
	return false, false
}

func importedProviderSpecificData(record map[string]any) map[string]any {
	if record == nil {
		return nil
	}
	for _, key := range []string{"providerSpecificData", "provider_specific_data", "providerData", "provider_data"} {
		if nested, ok := record[key].(map[string]any); ok {
			return nested
		}
	}
	return nil
}

func copyStringAlias(out, record map[string]any, target string, aliases ...string) {
	if strings.TrimSpace(importStringValue(out, target)) != "" {
		return
	}
	if value := strings.TrimSpace(importStringValue(record, aliases...)); value != "" {
		out[target] = value
	}
}

func importStringValue(object map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := object[key]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return typed
			}
		case json.Number:
			return typed.String()
		case float64:
			return fmt.Sprintf("%v", typed)
		}
	}
	return ""
}

func importedCredentialFileName(record map[string]any, provider string, ordinal int) string {
	email := strings.TrimSpace(importStringValue(record, "email", "displayName", "name"))
	id := strings.TrimSpace(importStringValue(record, "id"))
	seed := email
	if seed == "" {
		seed = id
	}
	if seed == "" {
		seed = "account"
	}
	var digestInput = []byte(provider + "|" + seed + "|" + importStringValue(record, "accessToken", "access_token"))
	digest := sha256.Sum256(digestInput)
	hash := hex.EncodeToString(digest[:])[:8]
	segment := sanitizeImportedFileSegment(seed)
	if segment == "" {
		segment = "account"
	}
	safeProvider := sanitizeImportedFileSegment(provider)
	if safeProvider == "" {
		safeProvider = "provider"
	}
	const maxName = 180
	suffix := fmt.Sprintf("-%d-%s", ordinal+1, hash)
	prefix := fmt.Sprintf("9router-%s-", safeProvider)
	maxSegmentLength := maxName - len(".json") - len(prefix) - len(suffix)
	if maxSegmentLength < 1 {
		// Provider names are normally short, but retain a valid bounded name
		// even if a future provider identifier is unexpectedly long.
		prefix = "9router-provider-"
		maxSegmentLength = maxName - len(".json") - len(prefix) - len(suffix)
	}
	if len(segment) > maxSegmentLength {
		segment = strings.TrimRight(segment[:maxSegmentLength], "-.")
		if segment == "" {
			segment = "account"
		}
	}
	return prefix + segment + suffix + ".json"
}

func sanitizeImportedFileSegment(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '@' || char == '.' || char == '_' || char == '-':
			builder.WriteRune(char)
		default:
			builder.WriteByte('-')
		}
	}
	return strings.Trim(builder.String(), "-.")
}

func redactProxySetting(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.EqualFold(trimmed, "direct") || strings.EqualFold(trimmed, "none") {
		return strings.ToLower(trimmed)
	}
	return proxyutil.Redact(trimmed)
}

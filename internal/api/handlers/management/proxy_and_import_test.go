package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestApplyProxyToAuthFilesMatchesStableAuthID(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	auth := &coreauth.Auth{
		ID:       "stable-auth-id",
		FileName: "account.json",
		Provider: "codex",
		Metadata: map[string]any{"access_token": "oauth-token"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/auth-files/proxy",
		strings.NewReader(`{"auth_id":"stable-auth-id","proxy_url":"direct"}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.ApplyProxyToAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("ApplyProxyToAuthFiles status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	updated, ok := manager.GetByID("stable-auth-id")
	if !ok || updated == nil {
		t.Fatal("updated auth is missing")
	}
	if updated.ProxyURL != "direct" {
		t.Fatalf("ProxyURL = %q, want direct", updated.ProxyURL)
	}
}

func TestDecodeNineRouterConnectionsRecognizesExportShape(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"settings": {"theme": "dark"},
		"providerConnections": [
			{"provider": "openai", "authType": "oauth", "accessToken": "codex-token",
				"providerSpecificData": {"chatgptAccountId": "acct_test"}},
			{"provider": "anthropic", "apiKey": "sk-ant-test"}
		],
		"proxyPools": [{"id": "pool-1"}]
	}`)

	records, recognized, errDecode := decodeNineRouterConnections(raw)
	if errDecode != nil {
		t.Fatalf("decodeNineRouterConnections() error = %v", errDecode)
	}
	if !recognized {
		t.Fatal("decodeNineRouterConnections() did not recognize providerConnections export")
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if got := importedProvider(records[0]); got != "codex" {
		t.Fatalf("first provider = %q, want codex", got)
	}
	if got := importedProvider(records[1]); got != "claude" {
		t.Fatalf("second provider = %q, want claude", got)
	}
}

func TestDecodeNineRouterConnectionsRejectsUnrelatedJSON(t *testing.T) {
	t.Parallel()

	records, recognized, errDecode := decodeNineRouterConnections([]byte(`{"foo":"bar"}`))
	if errDecode != nil {
		t.Fatalf("decodeNineRouterConnections() error = %v", errDecode)
	}
	if recognized || len(records) != 0 {
		t.Fatalf("unexpected recognition: recognized=%t records=%#v", recognized, records)
	}
}

func TestNormalizeImportedCredentialAddsCanonicalCodexFields(t *testing.T) {
	t.Parallel()

	record := map[string]any{
		"provider":     "openai",
		"accessToken":  "header.payload.signature",
		"refreshToken": "refresh",
		"email":        "user@example.com",
		"providerSpecificData": map[string]any{
			"chatgptAccountId": "acct_test",
		},
	}

	normalized := normalizeImportedCredential(record, "codex")
	if got := stringValue(normalized, "type"); got != "codex" {
		t.Fatalf("type = %q, want codex", got)
	}
	if got := stringValue(normalized, "access_token"); got != "header.payload.signature" {
		t.Fatalf("access_token = %q", got)
	}
	if got := stringValue(normalized, "refresh_token"); got != "refresh" {
		t.Fatalf("refresh_token = %q", got)
	}
	if got := stringValue(normalized, "account_id"); got != "acct_test" {
		t.Fatalf("account_id = %q, want acct_test", got)
	}
}

func TestNormalizeImportedCredentialPreservesInactiveNineRouterConnection(t *testing.T) {
	t.Parallel()

	normalized := normalizeImportedCredential(map[string]any{
		"provider": "anthropic",
		"apiKey":   "sk-ant-test",
		"isActive": false,
	}, "claude")
	if disabled, ok := normalized["disabled"].(bool); !ok || !disabled {
		t.Fatalf("disabled = %#v, want true for isActive=false", normalized["disabled"])
	}
}

func TestImportedCredentialFileNameIsSafeForLongIdentity(t *testing.T) {
	t.Parallel()

	longEmail := strings.Repeat("very-long-user-", 40) + "@example.com"
	name := importedCredentialFileName(map[string]any{
		"email":        longEmail,
		"access_token": "token",
	}, "codex", 0)
	if len(name) > 180 {
		t.Fatalf("filename length = %d, want <= 180: %q", len(name), name)
	}
	if !strings.HasSuffix(name, ".json") {
		t.Fatalf("filename = %q, want .json suffix", name)
	}
	if _, errParse := url.PathUnescape(name); errParse != nil {
		t.Fatalf("filename is not path-safe: %v", errParse)
	}
}

func TestNormalizeImportedCredentialRemainsJSONSerializable(t *testing.T) {
	t.Parallel()

	record := map[string]any{
		"provider": "gemini",
		"apiKey":   "key",
		"metadata": map[string]any{"region": "global"},
	}
	normalized := normalizeImportedCredential(record, "gemini")
	if _, errMarshal := json.Marshal(normalized); errMarshal != nil {
		t.Fatalf("normalized credential is not JSON serializable: %v", errMarshal)
	}
}

func TestNormalizeImportedCredentialCanonicalizesCaseInsensitiveModelLocks(t *testing.T) {
	t.Parallel()

	normalized := normalizeImportedCredential(map[string]any{
		"provider":                "codex",
		"accessToken":             "token",
		"modelLock_GPT-5.6-sol":   "2026-08-26T13:14:45.000Z",
		"modelLock_gpt-5.6-sol":   "2026-08-26T13:14:51.000Z",
		"modelLock_GPT-5.6-Terra": nil,
		"modelLock_gpt-5.6-terra": "2026-08-26T13:14:52.000Z",
	}, "codex")

	if got := normalized["modelLock_gpt-5.6-sol"]; got != "2026-08-26T13:14:51.000Z" {
		t.Fatalf("canonical model lock = %#v, want latest timestamp", got)
	}
	if got := normalized["modelLock_gpt-5.6-terra"]; got != "2026-08-26T13:14:52.000Z" {
		t.Fatalf("nil/non-nil model lock = %#v, want non-nil value", got)
	}

	seen := make(map[string]string)
	for key := range normalized {
		folded := strings.ToLower(key)
		if previous, exists := seen[folded]; exists {
			t.Fatalf("case-insensitive duplicate keys %q and %q remain", previous, key)
		}
		seen[folded] = key
	}
}

func TestImportedGeminiConfigEntryMapsNineRouterFields(t *testing.T) {
	t.Parallel()

	entry, errEntry := importedGeminiConfigEntry(map[string]any{
		"provider": "gemini",
		"authType": "apikey",
		"apiKey":   "gemini-key",
		"priority": json.Number("7"),
		"isActive": false,
		"providerSpecificData": map[string]any{
			"baseUrl":                "https://generativelanguage.example.test",
			"prefix":                 "team-a",
			"connectionProxyEnabled": true,
			"connectionProxyUrl":     "socks5://127.0.0.1:1080",
		},
	})
	if errEntry != nil {
		t.Fatalf("importedGeminiConfigEntry() error = %v", errEntry)
	}
	if entry.APIKey != "gemini-key" {
		t.Fatalf("APIKey = %q, want gemini-key", entry.APIKey)
	}
	if entry.BaseURL != "https://generativelanguage.example.test" {
		t.Fatalf("BaseURL = %q", entry.BaseURL)
	}
	if entry.Prefix != "team-a" {
		t.Fatalf("Prefix = %q, want team-a", entry.Prefix)
	}
	if entry.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("ProxyURL = %q", entry.ProxyURL)
	}
	if entry.Priority != 7 {
		t.Fatalf("Priority = %d, want 7", entry.Priority)
	}
	if len(entry.ExcludedModels) != 1 || entry.ExcludedModels[0] != "*" {
		t.Fatalf("ExcludedModels = %#v, want [*] for inactive record", entry.ExcludedModels)
	}
}

func TestImportNineRouterDataPersistsGeminiConfigWithoutPhantomFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("auth-dir: "+authDir+"\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	cfg := &config.Config{AuthDir: authDir}
	h := NewHandler(cfg, configPath, manager)
	raw := []byte(`{
		"providerConnections": [
			{
				"provider": "gemini",
				"authType": "apikey",
				"name": "gemini-primary",
				"apiKey": "gemini-key",
				"priority": 9,
				"providerSpecificData": {
					"connectionProxyEnabled": true,
					"connectionProxyUrl": "http://127.0.0.1:3128"
				}
			},
			{
				"provider": "openai",
				"authType": "oauth",
				"name": "codex-user",
				"email": "codex@example.test",
				"accessToken": "codex-access-token",
				"refreshToken": "codex-refresh-token",
				"providerSpecificData": {"chatgptAccountId": "acct-test"}
			}
		]
	}`)

	result, errImport := h.importNineRouterData(context.Background(), "mixed-backup.json", raw)
	if errImport != nil {
		t.Fatalf("importNineRouterData() error = %v", errImport)
	}
	if len(result.Failed) != 0 || len(result.Skipped) != 0 {
		t.Fatalf("unexpected import failures: failed=%#v skipped=%#v", result.Failed, result.Skipped)
	}
	if len(result.Imported) != 2 {
		t.Fatalf("imported = %d, want 2: %#v", len(result.Imported), result.Imported)
	}
	if len(cfg.GeminiKey) != 1 {
		t.Fatalf("GeminiKey len = %d, want 1", len(cfg.GeminiKey))
	}
	if cfg.GeminiKey[0].APIKey != "gemini-key" ||
		cfg.GeminiKey[0].Priority != 9 ||
		cfg.GeminiKey[0].ProxyURL != "http://127.0.0.1:3128" {
		t.Fatalf("GeminiKey = %#v", cfg.GeminiKey[0])
	}

	var geminiSummary *importedAuthSummary
	for index := range result.Imported {
		if result.Imported[index].Provider == "gemini" {
			geminiSummary = &result.Imported[index]
			break
		}
	}
	if geminiSummary == nil {
		t.Fatal("Gemini import summary is missing")
	}
	if geminiSummary.Storage != "config" ||
		geminiSummary.ConfigIndex == nil ||
		*geminiSummary.ConfigIndex != 0 ||
		strings.TrimSpace(geminiSummary.AuthIndex) == "" {
		t.Fatalf("Gemini summary = %#v", *geminiSummary)
	}

	entries, errReadDir := os.ReadDir(authDir)
	if errReadDir != nil {
		t.Fatalf("read auth dir: %v", errReadDir)
	}
	if len(entries) != 1 {
		t.Fatalf("auth files = %d, want only the Codex auth file", len(entries))
	}
	if strings.Contains(strings.ToLower(entries[0].Name()), "gemini") {
		t.Fatalf("phantom Gemini auth file was written: %s", entries[0].Name())
	}

	auths := manager.List()
	providers := map[string]int{}
	for _, auth := range auths {
		if auth != nil {
			providers[auth.Provider]++
		}
	}
	if providers["gemini"] != 1 || providers["codex"] != 1 {
		t.Fatalf("runtime providers = %#v, want one Gemini and one Codex", providers)
	}

	persisted, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("read persisted config: %v", errRead)
	}
	if !strings.Contains(string(persisted), "gemini-api-key:") ||
		!strings.Contains(string(persisted), "gemini-key") {
		t.Fatalf("persisted config does not contain Gemini key:\n%s", persisted)
	}
}

func TestImportNineRouterDataRollsBackGeminiConfigWhenPersistFails(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	cfg := &config.Config{
		AuthDir: authDir,
		GeminiKey: []config.GeminiKey{{
			APIKey: "existing-key",
		}},
	}
	h := NewHandler(cfg, filepath.Join(t.TempDir(), "missing-config.yaml"), manager)

	result, errImport := h.importNineRouterData(context.Background(), "gemini.json", []byte(`{
		"providerConnections": [{
			"provider": "gemini",
			"authType": "apikey",
			"apiKey": "new-key"
		}]
	}`))
	if errImport != nil {
		t.Fatalf("importNineRouterData() error = %v", errImport)
	}
	if len(result.Imported) != 0 || len(result.Failed) != 1 {
		t.Fatalf("result = %#v, want one failure and no imported entries", result)
	}
	if len(cfg.GeminiKey) != 1 || cfg.GeminiKey[0].APIKey != "existing-key" {
		t.Fatalf("Gemini config was not rolled back: %#v", cfg.GeminiKey)
	}
	if len(manager.List()) != 0 {
		t.Fatalf("runtime auth was registered after persistence failure: %#v", manager.List())
	}
}

func TestImportNineRouterDataSkipsGeminiOAuthInsteadOfWritingPhantomFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("auth-dir: "+authDir+"\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	cfg := &config.Config{AuthDir: authDir}
	h := NewHandler(cfg, configPath, manager)

	result, errImport := h.importNineRouterData(context.Background(), "gemini-oauth.json", []byte(`{
		"providerConnections": [{
			"provider": "gemini",
			"authType": "oauth",
			"accessToken": "oauth-token"
		}]
	}`))
	if errImport != nil {
		t.Fatalf("importNineRouterData() error = %v", errImport)
	}
	if len(result.Imported) != 0 || len(result.Failed) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("result = %#v, want one skipped entry", result)
	}
	entries, errReadDir := os.ReadDir(authDir)
	if errReadDir != nil {
		t.Fatalf("read auth dir: %v", errReadDir)
	}
	if len(entries) != 0 {
		t.Fatalf("unexpected Gemini auth file(s): %#v", entries)
	}
}

func TestImportNineRouterDataMapsOpenAICompatNodeModelsAndAliases(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("auth-dir: "+authDir+"\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	cfg := &config.Config{AuthDir: authDir}
	h := NewHandler(cfg, configPath, manager)
	raw := []byte(`{
		"providerConnections": [{
			"provider": "openai-compatible-chat-node-1",
			"authType": "apikey",
			"name": "node-key",
			"apiKey": "compat-secret",
			"isActive": true,
			"providerSpecificData": {
				"connectionProxyEnabled": true,
				"connectionProxyUrl": "http://127.0.0.1:3128"
			}
		}],
		"providerNodes": [{
			"id": "openai-compatible-chat-node-1",
			"type": "openai-compatible",
			"name": "Node One",
			"prefix": "node",
			"apiType": "chat",
			"baseUrl": "https://compat.example.test/v1"
		}],
		"customModels": [
			{"providerAlias": "openai-compatible-chat-node-1", "id": "model-a", "type": "llm", "name": "Model A"},
			{"providerAlias": "node", "id": "model-b", "type": "llm", "name": "Model B"},
			{"providerAlias": "other", "id": "ignored", "type": "llm"}
		],
		"modelAliases": {
			"public-a": "node/model-a",
			"public-b": "openai-compatible-chat-node-1/model-b",
			"foreign": "other/model-a"
		}
	}`)

	result, errImport := h.importNineRouterData(context.Background(), "compat-backup.json", raw)
	if errImport != nil {
		t.Fatalf("importNineRouterData() error = %v", errImport)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("unexpected import failures: %#v", result.Failed)
	}
	if len(result.Imported) != 1 {
		t.Fatalf("imported = %d, want one config credential: %#v", len(result.Imported), result.Imported)
	}
	if result.Imported[0].Storage != "config" || result.Imported[0].ConfigIndex == nil {
		t.Fatalf("import summary = %#v, want config storage/index", result.Imported[0])
	}
	if len(cfg.OpenAICompatibility) != 1 {
		t.Fatalf("OpenAICompatibility len = %d, want 1", len(cfg.OpenAICompatibility))
	}
	entry := cfg.OpenAICompatibility[0]
	if entry.Name != "Node One" || entry.Prefix != "node" || entry.BaseURL != "https://compat.example.test/v1" {
		t.Fatalf("compat entry identity = %#v", entry)
	}
	if len(entry.APIKeyEntries) != 1 ||
		entry.APIKeyEntries[0].APIKey != "compat-secret" ||
		entry.APIKeyEntries[0].ProxyURL != "http://127.0.0.1:3128" {
		t.Fatalf("compat key entries = %#v", entry.APIKeyEntries)
	}

	models := make(map[string]string, len(entry.Models))
	for _, model := range entry.Models {
		models[model.Alias] = model.Name
	}
	for alias, wantName := range map[string]string{
		"model-a":  "model-a",
		"model-b":  "model-b",
		"public-a": "model-a",
		"public-b": "model-b",
	} {
		if got := models[alias]; got != wantName {
			t.Fatalf("model alias %q = %q, want %q; all=%#v", alias, got, wantName, entry.Models)
		}
	}
	if _, exists := models["foreign"]; exists {
		t.Fatalf("foreign provider alias was imported: %#v", entry.Models)
	}

	files, errReadDir := os.ReadDir(authDir)
	if errReadDir != nil {
		t.Fatalf("read auth dir: %v", errReadDir)
	}
	if len(files) != 0 {
		t.Fatalf("OpenAI-compatible config import wrote duplicate auth files: %#v", files)
	}

	auths := manager.List()
	if len(auths) != 1 || auths[0] == nil || !strings.HasPrefix(auths[0].Provider, "openai-compatible-") {
		t.Fatalf("runtime auths = %#v, want one OpenAI-compatible config auth", auths)
	}
}

func TestImportNineRouterDataReportsUnsupportedCompatNodeKinds(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("auth-dir: "+authDir+"\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	h := NewHandler(&config.Config{AuthDir: authDir}, configPath, manager)
	raw := []byte(`{
		"providerConnections": [
			{"provider": "openai-compatible-responses-node", "authType": "apikey", "apiKey": "responses-key"},
			{"provider": "anthropic-compatible-node", "authType": "apikey", "apiKey": "anthropic-key"}
		],
		"providerNodes": [
			{"id": "openai-compatible-responses-node", "type": "openai-compatible", "name": "Responses", "prefix": "resp", "apiType": "responses", "baseUrl": "https://compat.example.test/v1"},
			{"id": "anthropic-compatible-node", "type": "anthropic-compatible", "name": "Anthropic", "prefix": "anth", "baseUrl": "https://anthropic.example.test/v1"}
		]
	}`)

	result, errImport := h.importNineRouterData(context.Background(), "unsupported.json", raw)
	if errImport != nil {
		t.Fatalf("importNineRouterData() error = %v", errImport)
	}
	if len(result.Imported) != 0 || len(result.Failed) != 0 || len(result.Skipped) != 2 {
		t.Fatalf("result = %#v, want two skipped unsupported nodes", result)
	}
	if len(h.cfg.OpenAICompatibility) != 0 {
		t.Fatalf("unsupported nodes unexpectedly produced config: %#v", h.cfg.OpenAICompatibility)
	}
}

func TestImportNineRouterDataOpenAICompatIsIdempotent(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("auth-dir: "+authDir+"\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	cfg := &config.Config{AuthDir: authDir}
	h := NewHandler(cfg, configPath, manager)
	raw := []byte(`{
		"providerConnections": [{
			"provider": "openai-compatible-chat-node-1",
			"authType": "apikey",
			"apiKey": "compat-secret"
		}],
		"providerNodes": [{
			"id": "openai-compatible-chat-node-1",
			"type": "openai-compatible",
			"name": "Node One",
			"prefix": "node",
			"apiType": "chat",
			"baseUrl": "https://compat.example.test/v1"
		}],
		"customModels": [{"providerAlias": "node", "id": "model-a", "type": "llm"}]
	}`)

	for attempt := 0; attempt < 2; attempt++ {
		result, errImport := h.importNineRouterData(context.Background(), "compat-backup.json", raw)
		if errImport != nil {
			t.Fatalf("attempt %d import error = %v", attempt+1, errImport)
		}
		if len(result.Failed) != 0 {
			t.Fatalf("attempt %d failures = %#v", attempt+1, result.Failed)
		}
	}
	if len(cfg.OpenAICompatibility) != 1 {
		t.Fatalf("compat entries = %d, want 1: %#v", len(cfg.OpenAICompatibility), cfg.OpenAICompatibility)
	}
	if got := len(cfg.OpenAICompatibility[0].APIKeyEntries); got != 1 {
		t.Fatalf("API key entries = %d, want 1", got)
	}
	if got := len(manager.List()); got != 1 {
		t.Fatalf("runtime auth count = %d, want 1", got)
	}
}

func TestImportNineRouterDataDoesNotCaptureNativeOpenAIByNodeAlias(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("auth-dir: "+authDir+"\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	cfg := &config.Config{AuthDir: authDir}
	h := NewHandler(cfg, configPath, manager)
	raw := []byte(`{
		"providerConnections": [{
			"provider": "openai",
			"authType": "apikey",
			"apiKey": "native-openai-key"
		}],
		"providerNodes": [{
			"id": "openai-compatible-chat-node-1",
			"type": "openai-compatible",
			"name": "openai",
			"prefix": "openai",
			"apiType": "chat",
			"baseUrl": "https://compat.example.test/v1"
		}]
	}`)

	result, errImport := h.importNineRouterData(context.Background(), "native-openai.json", raw)
	if errImport != nil {
		t.Fatalf("importNineRouterData() error = %v", errImport)
	}
	if len(result.Failed) != 0 || len(result.Skipped) != 0 || len(result.Imported) != 1 {
		t.Fatalf("result = %#v, want native OpenAI file import only", result)
	}
	if len(cfg.OpenAICompatibility) != 0 {
		t.Fatalf("native OpenAI connection was captured as compatibility config: %#v", cfg.OpenAICompatibility)
	}
	files, errReadDir := os.ReadDir(authDir)
	if errReadDir != nil {
		t.Fatalf("read auth dir: %v", errReadDir)
	}
	if len(files) != 1 {
		t.Fatalf("auth files = %d, want one native OpenAI file", len(files))
	}
}

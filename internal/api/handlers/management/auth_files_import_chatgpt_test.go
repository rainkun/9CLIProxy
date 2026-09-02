package management

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func makeChatGPTSessionDummyJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + body + "."
}

func TestImportChatGPTSession_WritesCodexAuthFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	session := map[string]any{
		"phone":              "15555550100",
		"email":              "demo@example.com",
		"password":           "example-pw",
		"chatgpt_account_id": "user-83cb8187-2d27-4d58-a1e5-f6f7a3722649",
		"access_token":       "header.payload.sig",
		"refresh_token":      "rt.1.AABV_dummy",
		"id_token": makeChatGPTSessionDummyJWT(t, map[string]any{
			"email": "demo@example.com",
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": "user-83cb8187-2d27-4d58-a1e5-f6f7a3722649",
				"chatgpt_plan_type":  "plus",
			},
		}),
		"oai_did":      "8713e286-c158-4b7a-889f-e6c85f2fcfb2",
		"plus_1m_free": false,
		"promo":        []any{},
		"ua":           "Mozilla/5.0 test",
	}
	payload, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "session.json")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err = part.Write(payload); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/import-chatgpt-session", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.ImportChatGPTSession(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status   string                `json:"status"`
		Imported []importedAuthSummary `json:"imported"`
		Skipped  []map[string]any      `json:"skipped"`
		Failed   []map[string]any      `json:"failed"`
		Counts   map[string]int        `json:"counts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Counts["imported"] != 1 || len(resp.Imported) != 1 {
		t.Fatalf("imported count = %d, want 1; body=%s", resp.Counts["imported"], rec.Body.String())
	}
	entry := resp.Imported[0]
	if entry.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", entry.Provider)
	}
	if entry.Email != "demo@example.com" {
		t.Fatalf("email = %q, want demo@example.com", entry.Email)
	}
	if !strings.HasPrefix(entry.Name, "codex-") {
		t.Fatalf("name = %q, want codex- prefix", entry.Name)
	}

	stored, err := os.ReadFile(filepath.Join(authDir, entry.Name))
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(stored, &parsed); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	if parsed["type"] != "codex" {
		t.Fatalf("stored.type = %v, want codex", parsed["type"])
	}
	meta, ok := parsed["meta"].(map[string]any)
	if !ok {
		t.Fatalf("stored.meta missing: %#v", parsed["meta"])
	}
	if meta["chatgpt_account_id"] != "user-83cb8187-2d27-4d58-a1e5-f6f7a3722649" {
		t.Fatalf("meta.chatgpt_account_id = %v", meta["chatgpt_account_id"])
	}
	if meta["phone"] != "15555550100" {
		t.Fatalf("meta.phone = %v", meta["phone"])
	}
	if meta["password"] != "example-pw" {
		t.Fatalf("meta.password = %v", meta["password"])
	}
	if meta["note"] != "Imported from ChatGPT session" {
		t.Fatalf("meta.note = %v", meta["note"])
	}
}

func TestImportChatGPTSession_RejectsPayloadWithoutTokens(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	payload := []byte(`{"email":"demo@example.com"}`)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "session.json")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err = part.Write(payload); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/import-chatgpt-session", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.ImportChatGPTSession(ctx)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Imported []importedAuthSummary `json:"imported"`
		Failed   []map[string]any      `json:"failed"`
		Counts   map[string]int        `json:"counts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Counts["imported"] != 0 {
		t.Fatalf("imported count = %d, want 0", resp.Counts["imported"])
	}
	if resp.Counts["failed"] != 1 {
		t.Fatalf("failed count = %d, want 1", resp.Counts["failed"])
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("read auth dir: %v", err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("auth dir should be empty, found: %v", names)
	}
}

func TestImportChatGPTSession_RawBody(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	session := map[string]any{
		"email":         "demo@example.com",
		"access_token":  "access",
		"refresh_token": "refresh",
	}
	payload, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/import-chatgpt-session", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.ImportChatGPTSession(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("read auth dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("auth dir entries = %d, want 1", len(entries))
	}
	if !strings.HasPrefix(entries[0].Name(), "codex-") {
		t.Fatalf("file = %q, want codex- prefix", entries[0].Name())
	}
	if !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("file = %q, want .json suffix", entries[0].Name())
	}
}

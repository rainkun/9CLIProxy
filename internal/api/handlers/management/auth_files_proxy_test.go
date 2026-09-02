package management

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFiles_IncludesRedactedProxyFromManager(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "codex-proxy.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		ProxyURL: "http://user:secret@proxy.example:8080/private/path?token=hidden",
		Attributes: map[string]string{
			"path": filePath,
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	entry := firstAuthFileEntry(t, h)
	assertRedactedAuthProxy(t, entry, "http://redacted@proxy.example:8080")
}

func TestListAuthFiles_IncludesRedactedProxyFromManagerMetadata(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "codex-metadata-proxy.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{
			"proxy_url": "socks5://user:secret@127.0.0.1:1080/private",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	entry := firstAuthFileEntry(t, h)
	assertRedactedAuthProxy(t, entry, "socks5://redacted@127.0.0.1:1080")
}

func TestListAuthFilesFromDisk_IncludesRedactedProxy(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	filePath := filepath.Join(authDir, "codex-proxy.json")
	body := `{"type":"codex","proxy_url":"https://user:secret@proxy.example:8443/private/path?token=hidden"}`
	if errWrite := os.WriteFile(filePath, []byte(body), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	entry := firstAuthFileEntry(t, h)
	assertRedactedAuthProxy(t, entry, "https://redacted@proxy.example:8443")
}

func TestListAuthFiles_IncludesDirectProxyModes(t *testing.T) {
	for _, mode := range []string{"direct", "none"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("MANAGEMENT_PASSWORD", "")

			authDir := t.TempDir()
			fileName := "codex-" + mode + ".json"
			filePath := filepath.Join(authDir, fileName)
			if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
				t.Fatalf("failed to write auth file: %v", errWrite)
			}

			manager := coreauth.NewManager(nil, nil, nil)
			record := &coreauth.Auth{
				ID:         fileName,
				FileName:   fileName,
				Provider:   "codex",
				Status:     coreauth.StatusActive,
				ProxyURL:   strings.ToUpper(mode),
				Attributes: map[string]string{"path": filePath},
			}
			if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
				t.Fatalf("failed to register auth record: %v", errRegister)
			}

			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
			entry := firstAuthFileEntry(t, h)
			assertRedactedAuthProxy(t, entry, mode)
		})
	}
}

func TestListAuthFiles_OmitsInvalidProxy(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "codex-invalid-proxy.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:         fileName,
		FileName:   fileName,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		ProxyURL:   "user:secret@missing-scheme",
		Attributes: map[string]string{"path": filePath},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	entry := firstAuthFileEntry(t, h)
	if _, exists := entry["proxy_url"]; exists {
		t.Fatalf("invalid proxy_url should be omitted, got %#v", entry["proxy_url"])
	}
	if _, exists := entry["proxyUrl"]; exists {
		t.Fatalf("invalid proxyUrl should be omitted, got %#v", entry["proxyUrl"])
	}
}

func assertRedactedAuthProxy(t *testing.T, entry map[string]any, want string) {
	t.Helper()
	if got := entry["proxy_url"]; got != want {
		t.Fatalf("proxy_url = %#v, want %q", got, want)
	}
	if got := entry["proxyUrl"]; got != want {
		t.Fatalf("proxyUrl = %#v, want %q", got, want)
	}
	encoded := strings.ToLower(strings.TrimSpace(entry["proxy_url"].(string)))
	for _, secret := range []string{"secret", "hidden", "private/path"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("proxy response leaked %q: %q", secret, encoded)
		}
	}
}

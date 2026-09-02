package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestExtractCloudflareQuickTunnelURLSkipsAPIHost(t *testing.T) {
	t.Parallel()

	line := "info https://api.trycloudflare.com and https://w2dy6w.trycloudflare.com"
	if got := extractCloudflareQuickTunnelURL(line); got != "https://w2dy6w.trycloudflare.com" {
		t.Fatalf("extractCloudflareQuickTunnelURL() = %q", got)
	}
}

func TestBuildTunnelPublicURL(t *testing.T) {
	t.Parallel()

	got, errBuild := buildTunnelPublicURL("https://abc-tunnel.us/", "w2dy6w")
	if errBuild != nil {
		t.Fatalf("buildTunnelPublicURL() error = %v", errBuild)
	}
	if got != "https://rw2dy6w.abc-tunnel.us" {
		t.Fatalf("buildTunnelPublicURL() = %q", got)
	}

	if _, errBuild = buildTunnelPublicURL("https://abc-tunnel.us", "invalid!"); errBuild == nil {
		t.Fatal("buildTunnelPublicURL() accepted an invalid short ID")
	}
}

func TestTunnelStateRoundTrip(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "runtime", "tunnel", "state.json")
	want := tunnelState{ShortID: "w2dy6w", TunnelURL: "https://w2dy6w.trycloudflare.com"}
	if errSave := saveTunnelState(statePath, want); errSave != nil {
		t.Fatalf("saveTunnelState() error = %v", errSave)
	}
	got, errLoad := loadTunnelState(statePath)
	if errLoad != nil {
		t.Fatalf("loadTunnelState() error = %v", errLoad)
	}
	if got != want {
		t.Fatalf("loadTunnelState() = %#v, want %#v", got, want)
	}
}

func TestRegisterTunnelURLUses9RouterContract(t *testing.T) {
	t.Parallel()

	var requestPayload struct {
		ShortID   string `json:"shortId"`
		TunnelURL string `json:"tunnelUrl"`
	}
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/tunnel/register" {
			t.Errorf("path = %s, want /api/tunnel/register", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if errDecode := json.NewDecoder(r.Body).Decode(&requestPayload); errDecode != nil {
			t.Errorf("decode request: %v", errDecode)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if errRegister := registerTunnelURL(ctx, relay.URL, "w2dy6w", "https://new.trycloudflare.com"); errRegister != nil {
		t.Fatalf("registerTunnelURL() error = %v", errRegister)
	}
	if requestPayload.ShortID != "w2dy6w" || requestPayload.TunnelURL != "https://new.trycloudflare.com" {
		t.Fatalf("request payload = %#v", requestPayload)
	}
}

func TestRegisterTunnelURLReportsRelayError(t *testing.T) {
	t.Parallel()

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "registration rejected", http.StatusBadRequest)
	}))
	defer relay.Close()

	errRegister := registerTunnelURL(context.Background(), relay.URL, "w2dy6w", "https://new.trycloudflare.com")
	if errRegister == nil || !strings.Contains(errRegister.Error(), "HTTP 400") {
		t.Fatalf("registerTunnelURL() error = %v, want HTTP 400", errRegister)
	}
}

func TestResolveTunnelIdentityPersistsShortID(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	handler := &Handler{configFilePath: configPath}
	cfg := config.TunnelConfig{RelayURL: "https://abc-tunnel.us"}
	first, errFirst := handler.resolveTunnelIdentity(cfg)
	if errFirst != nil {
		t.Fatalf("first resolveTunnelIdentity() error = %v", errFirst)
	}
	second, errSecond := handler.resolveTunnelIdentity(cfg)
	if errSecond != nil {
		t.Fatalf("second resolveTunnelIdentity() error = %v", errSecond)
	}
	if first.ShortID == "" || first.ShortID != second.ShortID {
		t.Fatalf("short IDs changed: first=%q second=%q", first.ShortID, second.ShortID)
	}
	wantURL := "https://r" + first.ShortID + ".abc-tunnel.us"
	if first.PublicURL != wantURL {
		t.Fatalf("public URL = %q, want %q", first.PublicURL, wantURL)
	}
}

func TestNormalizeTunnelConfigPreservesDirectRelayMode(t *testing.T) {
	t.Parallel()

	cfg := config.TunnelConfig{
		Provider:  "cloudflare",
		RelayURL:  "direct",
		TargetURL: "http://127.0.0.1:8317",
	}
	normalizeTunnelConfig(&cfg, 8317)
	if cfg.RelayURL != "direct" {
		t.Fatalf("normalizeTunnelConfig() relay URL = %q, want direct", cfg.RelayURL)
	}

	handler := &Handler{configFilePath: filepath.Join(t.TempDir(), "config.yaml")}
	identity, errIdentity := handler.currentTunnelIdentity(cfg)
	if errIdentity != nil {
		t.Fatalf("currentTunnelIdentity() error = %v", errIdentity)
	}
	if identity.RelayURL != "" || identity.PublicURL != "" {
		t.Fatalf("direct identity = %#v, want no relay/public URL", identity)
	}
}

func TestClearPersistedTunnelURLKeepsStableIdentity(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	handler := &Handler{configFilePath: configPath}
	statePath := managedTunnelStatePath(configPath)
	if errSave := saveTunnelState(statePath, tunnelState{
		ShortID:   "w2dy6w",
		TunnelURL: "https://old.trycloudflare.com",
	}); errSave != nil {
		t.Fatalf("saveTunnelState() error = %v", errSave)
	}

	if errClear := handler.clearPersistedTunnelURL(""); errClear != nil {
		t.Fatalf("clearPersistedTunnelURL() error = %v", errClear)
	}
	state, errLoad := loadTunnelState(statePath)
	if errLoad != nil {
		t.Fatalf("loadTunnelState() error = %v", errLoad)
	}
	if state.ShortID != "w2dy6w" {
		t.Fatalf("short ID changed after clear: %q", state.ShortID)
	}
	if state.TunnelURL != "" {
		t.Fatalf("tunnel URL = %q, want empty", state.TunnelURL)
	}
}

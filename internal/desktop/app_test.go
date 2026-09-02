package desktop

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	management "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestInjectDesktopSessionUsesWebViewOrigin(t *testing.T) {
	body := []byte("<!doctype html><html><head></head><body>ok</body></html>")

	got := string(injectDesktopSession(body, "desktop-secret"))

	if !strings.Contains(got, "window.location.origin") {
		t.Fatalf("desktop session bootstrap must derive the API base from the WebView origin")
	}
	if strings.Contains(got, "127.0.0.1") || strings.Contains(got, "localhost:8317") {
		t.Fatalf("desktop session bootstrap must not hard-code the embedded server address: %s", got)
	}
	if !strings.Contains(got, "desktop-secret") || !strings.Contains(got, "isLoggedIn") {
		t.Fatalf("desktop session bootstrap did not include the local session")
	}
}

func TestAssetHandlerProxiesSameOriginManagementRequest(t *testing.T) {
	const localPassword = "desktop-secret"
	var gotAuthorization string
	var gotForwardedFor string
	upstreamHandler := management.NewHandler(&config.Config{}, "", nil)
	upstreamHandler.SetLocalPassword(localPassword)
	upstreamEngine := gin.New()
	upstreamEngine.Use(upstreamHandler.Middleware())
	upstreamEngine.GET("/v0/management/config", func(c *gin.Context) {
		gotAuthorization = c.GetHeader("Authorization")
		gotForwardedFor = c.GetHeader("X-Forwarded-For")
		c.Header("Content-Type", "application/json")
		c.String(http.StatusOK, `{"ok":true}`)
	})
	upstream := httptest.NewServer(upstreamEngine)
	defer upstream.Close()

	serverURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	app := &App{serverURL: serverURL}
	request := httptest.NewRequest(http.MethodGet, "http://wails.localhost/v0/management/config", nil)
	request.Header.Set("Authorization", "Bearer "+localPassword)
	response := httptest.NewRecorder()

	app.assetHandler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if gotAuthorization != "Bearer "+localPassword {
		t.Fatalf("upstream authorization = %q, want local session", gotAuthorization)
	}
	if gotForwardedFor != "" {
		t.Fatalf("desktop proxy must not forward the synthetic WebView client IP, got %q", gotForwardedFor)
	}
	if response.Body.String() != `{"ok":true}` {
		t.Fatalf("proxy body = %q, want upstream response", response.Body.String())
	}
}

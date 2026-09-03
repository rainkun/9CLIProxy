package desktop

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	configaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cmd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/safemode"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	windowsOptions "github.com/wailsapp/wails/v2/pkg/options/windows"
)

const (
	defaultDesktopWidth  = 1280
	defaultDesktopHeight = 860
)

type RuntimeInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	BuiltAt string `json:"builtAt"`
}

type App struct {
	configPath      string
	localModel      bool
	noBrowser       bool
	callbackPort    int
	wailsCtx        context.Context
	cancelService   context.CancelFunc
	serviceDone     <-chan struct{}
	localPassword   string
	serverBaseURL   string
	serverURL       *url.URL
	startupError    error
	startupErrorMsg string
}

func Run() error {
	app := &App{}
	if errPrepare := app.prepare(); errPrepare != nil {
		return errPrepare
	}

	return wails.Run(&options.App{
		Title:            "CLIProxyAPI Management",
		Width:            defaultDesktopWidth,
		Height:           defaultDesktopHeight,
		MinWidth:         960,
		MinHeight:        640,
		WindowStartState: options.Maximised,
		BackgroundColour: options.NewRGB(18, 18, 18),
		AssetServer: &assetserver.Options{
			Handler: app.assetHandler(),
		},
		Bind: []interface{}{app},
		OnStartup: func(ctx context.Context) {
			app.wailsCtx = ctx
		},
		OnDomReady: func(ctx context.Context) {
			app.wailsCtx = ctx
		},
		OnShutdown: func(ctx context.Context) {
			app.shutdownService()
		},
		Windows: &windowsOptions.Options{
			DisablePinchZoom:     true,
			IsZoomControlEnabled: false,
			Theme:                windowsOptions.SystemDefault,
		},
		LogLevelProduction:       logger.ERROR,
		EnableDefaultContextMenu: true,
	})
}

func (a *App) RuntimeInfo() RuntimeInfo {
	return RuntimeInfo{
		Version: buildinfo.Version,
		Commit:  buildinfo.Commit,
		BuiltAt: buildinfo.BuildDate,
	}
}

func (a *App) prepare() error {
	flag.StringVar(&a.configPath, "config", "", "Configure File Path")
	flag.BoolVar(&a.localModel, "local-model", false, "Use embedded models.json and codex_client_models.json only, skip remote model catalog fetching")
	flag.BoolVar(&a.noBrowser, "no-browser", false, "Don't open browser automatically for OAuth")
	flag.IntVar(&a.callbackPort, "oauth-callback-port", 0, "Override OAuth callback port")
	flag.Parse()

	wd, errGetwd := os.Getwd()
	if errGetwd != nil {
		return fmt.Errorf("failed to get working directory: %w", errGetwd)
	}

	if errLoad := godotenv.Load(filepath.Join(wd, ".env")); errLoad != nil && !errors.Is(errLoad, os.ErrNotExist) {
		log.WithError(errLoad).Warn("failed to load .env file")
	}

	pluginHost := pluginhost.New()
	configPath := a.resolveConfigPath(wd)
	if bootstrapCfg := loadPluginBootstrapConfig(configPath); bootstrapCfg != nil {
		pluginHost.ApplyConfig(context.Background(), bootstrapCfg)
	}

	cfg, configFilePath, errLoadConfig := loadDesktopConfig(wd, configPath)
	if errLoadConfig != nil {
		return errLoadConfig
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	if cfg.Port == 0 {
		cfg.Port = 8317
	}
	if strings.TrimSpace(cfg.Host) == "" {
		cfg.Host = "127.0.0.1"
	}
	cfg.RemoteManagement.DisableControlPanel = false
	if !isLoopbackHost(cfg.Host) {
		log.WithField("host", cfg.Host).Warn("desktop mode requires a loopback host; overriding server host to 127.0.0.1")
		cfg.Host = "127.0.0.1"
	}
	if cfg.TLS.Enable {
		a.serverBaseURL = fmt.Sprintf("https://%s:%d", cfg.Host, cfg.Port)
	} else {
		a.serverBaseURL = fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)
	}
	serverURL, errParseServerURL := url.Parse(a.serverBaseURL)
	if errParseServerURL != nil {
		return fmt.Errorf("failed to parse desktop server URL: %w", errParseServerURL)
	}
	a.serverURL = serverURL

	redisqueue.SetUsageStatisticsEnabled(cfg.UsageStatisticsEnabled)
	redisqueue.SetRetentionSeconds(cfg.RedisUsageQueueRetentionSeconds)
	coreauth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	coreauth.SetTransientErrorCooldownSeconds(cfg.TransientErrorCooldownSeconds)

	if errLog := logging.ConfigureLogOutput(cfg); errLog != nil {
		return fmt.Errorf("failed to configure log output: %w", errLog)
	}
	util.SetLogLevel(cfg)

	resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(cfg.AuthDir)
	if errResolveAuthDir != nil {
		return fmt.Errorf("failed to resolve auth directory: %w", errResolveAuthDir)
	}
	cfg.AuthDir = resolvedAuthDir
	managementasset.SetCurrentConfig(cfg)
	configaccess.Register(&cfg.SDKConfig)
	pluginHost.ApplyConfig(context.Background(), cfg)
	sdkAuth.RegisterTokenStore(sdkAuth.NewFileTokenStore())

	serverOptions := []api.ServerOption(nil)
	if safemode.HasExampleAPIKeys(cfg.APIKeys) {
		matches := safemode.ExampleAPIKeys(cfg.APIKeys)
		log.WithField("api_keys", strings.Join(matches, ",")).Error("unsafe example API key configured; proxy API endpoints disabled until api-keys is updated")
		serverOptions = append(serverOptions, api.WithExampleAPIKeySafeMode())
	}

	misc.StartAntigravityVersionUpdater(context.Background())
	startModelCatalogUpdaters(a.localModel, cfg.Home.Enabled)

	a.localPassword = fmt.Sprintf("desktop-%d-%d", os.Getpid(), time.Now().UnixNano())
	a.cancelService, a.serviceDone = cmd.StartServiceBackgroundWithPluginHost(cfg, configFilePath, a.localPassword, pluginHost, serverOptions...)

	if errReady := waitForManagementReady(a.serverBaseURL, a.localPassword); errReady != nil {
		a.shutdownService()
		return errReady
	}

	return nil
}

func (a *App) resolveConfigPath(wd string) string {
	if strings.TrimSpace(a.configPath) != "" {
		return a.configPath
	}
	return filepath.Join(wd, "config.yaml")
}

func loadDesktopConfig(wd, configPath string) (*config.Config, string, error) {
	if strings.TrimSpace(configPath) == "" {
		configPath = filepath.Join(wd, "config.yaml")
	}
	if _, errStat := os.Stat(configPath); errors.Is(errStat, fs.ErrNotExist) {
		examplePath := filepath.Join(wd, "config.example.yaml")
		if _, errExample := os.Stat(examplePath); errExample == nil {
			if errCopy := misc.CopyConfigTemplate(examplePath, configPath); errCopy != nil {
				return nil, configPath, fmt.Errorf("failed to bootstrap config: %w", errCopy)
			}
		}
	} else if errStat != nil {
		return nil, configPath, fmt.Errorf("failed to inspect config file: %w", errStat)
	}

	cfg, errLoad := config.LoadConfigOptional(configPath, false)
	if errLoad != nil {
		return nil, configPath, fmt.Errorf("failed to load config: %w", errLoad)
	}
	return cfg, configPath, nil
}

func loadPluginBootstrapConfig(path string) *config.Config {
	raw, errReadFile := os.ReadFile(path)
	if errReadFile != nil {
		if !errors.Is(errReadFile, os.ErrNotExist) {
			log.Warnf("failed to read plugin bootstrap config: %v", errReadFile)
		}
		cfg := &config.Config{}
		cfg.NormalizePluginsConfig()
		return cfg
	}
	parsed, errParseConfig := config.ParseConfigBytes(raw)
	if errParseConfig != nil {
		log.Warnf("failed to parse plugin bootstrap config: %v", errParseConfig)
		cfg := &config.Config{}
		cfg.NormalizePluginsConfig()
		return cfg
	}
	if parsed == nil {
		parsed = &config.Config{}
	}
	parsed.NormalizePluginsConfig()
	return parsed
}

func startModelCatalogUpdaters(localModel, homeEnabled bool) {
	if localModel {
		log.Info("Local model mode: using embedded model catalogs, remote model updates disabled")
		return
	}
	registry.StartCodexClientModelsUpdater(context.Background())
	if homeEnabled {
		log.Info("Home mode: remote models.json updates disabled; Codex client model list follows Home model IDs")
		return
	}
	registry.StartModelsUpdater(context.Background())
}

func waitForManagementReady(baseURL, localPassword string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	backoff := 100 * time.Millisecond
	for i := 0; i < 30; i++ {
		req, errReq := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/v0/management/config", nil)
		if errReq != nil {
			return errReq
		}
		req.Header.Set("Authorization", "Bearer "+localPassword)
		resp, errDo := client.Do(req)
		if errDo == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		time.Sleep(backoff)
		if backoff < time.Second {
			backoff = time.Duration(float64(backoff) * 1.5)
		}
	}
	return fmt.Errorf("desktop error: embedded server is not ready")
}

func (a *App) shutdownService() {
	if a.cancelService != nil {
		a.cancelService()
		a.cancelService = nil
	}
	if a.serviceDone != nil {
		<-a.serviceDone
		a.serviceDone = nil
	}
}

func (a *App) assetHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.startupError != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(errorPage(a.startupErrorMsg)))
			return
		}
		if isEmbeddedManagementRoute(r.URL.Path) {
			body := injectDesktopSession(embeddedManagementHTML, a.localPassword)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
		proxy := newDesktopReverseProxy(a.serverURL)
		proxy.ModifyResponse = func(resp *http.Response) error {
			if resp == nil || resp.Request == nil || resp.Request.URL == nil || resp.Request.URL.Path != "/management.html" {
				return nil
			}
			contentType := resp.Header.Get("Content-Type")
			if !strings.Contains(strings.ToLower(contentType), "text/html") {
				return nil
			}
			body, errRead := io.ReadAll(resp.Body)
			if errRead != nil {
				return errRead
			}
			_ = resp.Body.Close()
			body = injectDesktopSession(body, a.localPassword)
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
			return nil
		}
		proxy.ServeHTTP(w, r)
	})
}

func isEmbeddedManagementRoute(path string) bool {
	switch strings.TrimSpace(path) {
	case "", "/", "/index.html", "/management.html", "/dashboard", "/combos",
		"/ai-providers", "/auth-files", "/oauth", "/quota", "/config", "/logs", "/system":
		return true
	default:
		return false
	}
}

func newDesktopReverseProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			// The WebView2 asset server uses a synthetic TEST-NET RemoteAddr when
			// it hands requests to the Go handler. Do not forward that address as
			// X-Forwarded-For: the management middleware would then treat the
			// request as remote and reject the desktop-only local password.
			proxyRequest.SetURL(target)
			proxyRequest.Out.Header.Del("Forwarded")
			proxyRequest.Out.Header.Del("X-Forwarded-For")
			proxyRequest.Out.Header.Del("X-Forwarded-Host")
			proxyRequest.Out.Header.Del("X-Forwarded-Proto")
		},
	}
}

func injectDesktopSession(body []byte, localPassword string) []byte {
	passwordJS := jsString(localPassword)
	script := []byte(`<script>(function(){try{var desktopOrigin=window.location&&window.location.origin||'';if(!desktopOrigin||desktopOrigin==='null'){desktopOrigin=window.location.protocol+'//'+window.location.host;}if(!desktopOrigin||desktopOrigin==='null'){throw new Error('desktop origin unavailable');}localStorage.setItem('apiBase',desktopOrigin);localStorage.setItem('apiUrl',desktopOrigin);localStorage.setItem('managementKey',` + passwordJS + `);localStorage.setItem('isLoggedIn','true');}catch(e){console.warn('CLIProxyAPI desktop autologin failed',e);}})();</script>`)
	lower := bytes.ToLower(body)
	if idx := bytes.Index(lower, []byte("<head>")); idx >= 0 {
		insertAt := idx + len("<head>")
		out := make([]byte, 0, len(body)+len(script))
		out = append(out, body[:insertAt]...)
		out = append(out, script...)
		out = append(out, body[insertAt:]...)
		return out
	}
	return append(script, body...)
}

func jsString(value string) string {
	return fmt.Sprintf("%q", value)
}

func errorPage(message string) string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>CLIProxyAPI Desktop</title><style>body{font-family:system-ui,sans-serif;margin:3rem;background:#111;color:#eee}pre{white-space:pre-wrap;color:#fca5a5}</style></head><body><h1>CLIProxyAPI Desktop failed to start</h1><pre>` + htmlEscape(message) + `</pre></body></html>`
}

func htmlEscape(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	value = strings.ReplaceAll(value, `"`, "&quot;")
	value = strings.ReplaceAll(value, "'", "&#39;")
	return value
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func init() {
	gin.SetMode(gin.ReleaseMode)
}

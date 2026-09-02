package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestValidateTunnelTargetAllowsLoopbackOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "localhost", raw: "http://localhost:8317", ok: true},
		{name: "ipv4 loopback", raw: "http://127.0.0.1:8317", ok: true},
		{name: "ipv6 loopback", raw: "http://[::1]:8317", ok: true},
		{name: "remote host", raw: "https://example.com:8317", ok: false},
		{name: "wildcard ipv4", raw: "http://0.0.0.0:8317", ok: false},
		{name: "wildcard ipv6", raw: "http://[::]:8317", ok: false},
		{name: "credentials", raw: "http://user:pass@127.0.0.1:8317", ok: false},
		{name: "wrong scheme", raw: "ftp://127.0.0.1:8317", ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, errValidate := validateTunnelTarget(test.raw)
			if (errValidate == nil) != test.ok {
				t.Fatalf("validateTunnelTarget(%q) error = %v, want ok=%t", test.raw, errValidate, test.ok)
			}
		})
	}
}

func TestCloudflaredAssetForSupportedArchitectures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		goos   string
		goarch string
		want   string
		ok     bool
	}{
		{goos: "windows", goarch: "amd64", want: "cloudflared-windows-amd64.exe", ok: true},
		{goos: "windows", goarch: "386", want: "cloudflared-windows-386.exe", ok: true},
		{goos: "windows", goarch: "arm64", want: "cloudflared-windows-arm64.exe", ok: true},
		{goos: "linux", goarch: "amd64", want: "cloudflared-linux-amd64", ok: true},
		{goos: "linux", goarch: "arm64", want: "cloudflared-linux-arm64", ok: true},
		{goos: "freebsd", goarch: "amd64", ok: false},
	}
	for _, test := range tests {
		t.Run(test.goos+"/"+test.goarch, func(t *testing.T) {
			got, errAsset := cloudflaredAsset(test.goos, test.goarch)
			if (errAsset == nil) != test.ok {
				t.Fatalf("cloudflaredAsset() error = %v, want ok=%t", errAsset, test.ok)
			}
			if test.ok && got != test.want {
				t.Fatalf("cloudflaredAsset() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDownloadCloudflaredValidatesAndInstallsBinary(t *testing.T) {
	t.Parallel()

	payload := make([]byte, tunnelMinBinarySize+4)
	switch runtime.GOOS {
	case "windows":
		payload[0], payload[1] = 'M', 'Z'
	case "darwin":
		payload[0], payload[1], payload[2], payload[3] = 0xcf, 0xfa, 0xed, 0xfe
	default:
		payload[0], payload[1], payload[2], payload[3] = 0x7f, 'E', 'L', 'F'
	}

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer source.Close()

	destination := filepath.Join(t.TempDir(), "runtime", "cloudflared", cloudflaredBinaryName())
	if errDownload := downloadCloudflared(context.Background(), source.URL, destination, nil); errDownload != nil {
		t.Fatalf("downloadCloudflared() error = %v", errDownload)
	}
	if !isValidCloudflaredBinary(destination) {
		t.Fatalf("downloaded binary %q did not pass validation", destination)
	}
	if _, errStat := os.Stat(destination); errStat != nil {
		t.Fatalf("downloaded binary missing: %v", errStat)
	}
}

func TestCreateQuickTunnelConfigIsolatedAndCleanup(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	tempDir, isolatedPath, errCreate := createQuickTunnelConfig(configPath)
	if errCreate != nil {
		t.Fatalf("createQuickTunnelConfig() error = %v", errCreate)
	}
	if !strings.HasPrefix(filepath.Base(tempDir), "cloudflared-quick-") {
		t.Fatalf("temp directory = %q, want cloudflared-quick-*", tempDir)
	}
	wantParent := filepath.Join(filepath.Dir(configPath), "runtime", "tunnel")
	if filepath.Dir(tempDir) != wantParent {
		t.Fatalf("temp directory parent = %q, want %q", filepath.Dir(tempDir), wantParent)
	}
	data, errRead := os.ReadFile(isolatedPath)
	if errRead != nil {
		t.Fatalf("read isolated config: %v", errRead)
	}
	if !strings.Contains(string(data), "isolated Quick Tunnel") {
		t.Fatalf("isolated config = %q, want explanatory marker", string(data))
	}

	cleanupTunnelTempDir(tempDir)
	if _, errStat := os.Stat(tempDir); !os.IsNotExist(errStat) {
		t.Fatalf("cleanupTunnelTempDir() left %q behind, stat error = %v", tempDir, errStat)
	}
}

func TestProbeTunnelURLUsesRelayHealthEndpoint(t *testing.T) {
	t.Parallel()

	requested := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested <- r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	if !probeTunnelURL(context.Background(), server.URL) {
		t.Fatal("probeTunnelURL() = false, want true")
	}
	select {
	case path := <-requested:
		if path != "/api/health" {
			t.Fatalf("health probe path = %q, want /api/health", path)
		}
	default:
		t.Fatal("health probe did not reach test server")
	}
}

func TestWaitForTunnelRegistrationAcceptsDirectMode(t *testing.T) {
	t.Parallel()

	cmd := &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
	runtimeState := newTunnelRuntime()
	runtimeState.lifecycleGeneration = 7
	runtimeState.cmd = cmd
	runtimeState.quickTunnelURL = "https://quick.trycloudflare.com"
	runtimeState.registeredQuickURL = runtimeState.quickTunnelURL

	if errWait := waitForTunnelRegistration(
		context.Background(),
		runtimeState,
		7,
		cmd,
		runtimeState.quickTunnelURL,
	); errWait != nil {
		t.Fatalf("waitForTunnelRegistration() error = %v", errWait)
	}
}

func TestDetachTunnelRuntimeInvalidatesPendingStart(t *testing.T) {
	t.Parallel()

	cancelled := make(chan struct{})
	runtimeState := newTunnelRuntime()
	generation, claimed := runtimeState.beginStart(func() {
		close(cancelled)
	})
	if !claimed {
		t.Fatal("beginStart() did not claim lifecycle")
	}
	nextGeneration, termination, detached := detachTunnelRuntime(runtimeState, generation)
	if !detached {
		t.Fatal("detachTunnelRuntime() did not detach expected generation")
	}
	if nextGeneration <= generation {
		t.Fatalf("lifecycle generation = %d, want > %d", nextGeneration, generation)
	}
	runtimeState.mu.Lock()
	starting := runtimeState.starting
	runtimeState.mu.Unlock()
	if starting {
		t.Fatal("detached runtime still reports starting")
	}
	terminateTunnelProcess(termination)
	select {
	case <-cancelled:
	default:
		t.Fatal("detaching pending start did not cancel its context")
	}
}

func TestTunnelProcessMatchingRequiresCloudflaredTunnelAndTargetPort(t *testing.T) {
	t.Parallel()

	if !tunnelProcessMatches(
		"cloudflared.exe",
		`cloudflared.exe tunnel --url http://127.0.0.1:18317 --config C:\runtime\config.yml`,
		"http://127.0.0.1:18317",
		18317,
	) {
		t.Fatal("tunnelProcessMatches() rejected a matching cloudflared command")
	}
	tests := []struct {
		name string
		raw  string
	}{
		{name: "wrong process", raw: `other.exe tunnel --url http://127.0.0.1:18317`},
		{name: "wrong port", raw: `cloudflared.exe tunnel --url http://127.0.0.1:18318`},
		{name: "not a tunnel", raw: `cloudflared.exe version`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name := "cloudflared.exe"
			if test.name == "wrong process" {
				name = "other.exe"
			}
			if tunnelProcessMatches(name, test.raw, "http://127.0.0.1:18317", 18317) {
				t.Fatalf("tunnelProcessMatches() accepted %q", test.raw)
			}
		})
	}
}

func TestTunnelCommandEnvironmentDefaultsToHTTP2(t *testing.T) {
	t.Setenv("TUNNEL_TRANSPORT_PROTOCOL", "")
	t.Setenv("CLOUDFLARED_PROTOCOL", "")
	environment := tunnelCommandEnvironment()
	found := false
	for _, entry := range environment {
		if entry == "TUNNEL_TRANSPORT_PROTOCOL=http2" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("tunnelCommandEnvironment() did not set the http2 default: %v", environment)
	}
}

func TestTunnelRespawnDelayIsBounded(t *testing.T) {
	t.Parallel()

	if got := tunnelRespawnDelay(1); got != tunnelRespawnInitialGap {
		t.Fatalf("first respawn delay = %s, want %s", got, tunnelRespawnInitialGap)
	}
	if got := tunnelRespawnDelay(100); got != tunnelRespawnMaxGap {
		t.Fatalf("large respawn delay = %s, want %s", got, tunnelRespawnMaxGap)
	}
}

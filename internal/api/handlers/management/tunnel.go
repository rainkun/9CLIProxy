package management

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	defaultTunnelProvider   = "cloudflare"
	tunnelLogLines          = 32
	tunnelDownloadTimeout   = 5 * time.Minute
	tunnelDownloadChunk     = 32 * 1024
	tunnelMinBinarySize     = 1 * 1024 * 1024
	tunnelQuickURLTimeout   = 90 * time.Second
	tunnelHealthTimeout     = 60 * time.Second
	tunnelHealthInterval    = 2 * time.Second
	tunnelHealthRequestTTL  = 5 * time.Second
	tunnelReadyPollInterval = 100 * time.Millisecond
	tunnelReuseProbeTimeout = 8 * time.Second
	tunnelWatchdogInterval  = 5 * time.Second
	tunnelRespawnInitialGap = 2 * time.Second
	tunnelRespawnMaxGap     = 30 * time.Second
)

var cloudflareQuickTunnelURL = regexp.MustCompile(`https://[A-Za-z0-9.-]+\.trycloudflare\.com(?:/[^\s"'<>]*)?`)
var cloudflaredDownloadBaseURL = "https://github.com/cloudflare/cloudflared/releases/latest/download"

type tunnelRuntime struct {
	mu                  sync.Mutex
	intentMu            sync.Mutex
	relayMu             sync.Mutex
	lifecycleGeneration uint64
	cmd                 *exec.Cmd
	cancel              context.CancelFunc
	downloadCancel      context.CancelFunc
	tempDir             string
	targetURL           string
	externalPID         int
	starting            bool
	stopping            bool
	downloading         bool
	downloadProgress    int
	downloadError       string
	publicURL           string
	stablePublicURL     string
	quickTunnelURL      string
	shortID             string
	relayURL            string
	relayRegistered     bool
	relayError          string
	lastError           string
	logs                []string
	startedAt           time.Time
	registeredQuickURL  string
	respawnScheduled    bool
	respawnAttempts     int
	respawnGeneration   uint64
}

func newTunnelRuntime() *tunnelRuntime {
	return &tunnelRuntime{}
}

// beginStart claims the next tunnel lifecycle generation. The generation is
// invalidated by stopTunnelRuntime so a start that is still downloading or
// between cmd.Start and runtime publication cannot resurrect a tunnel after a
// concurrent stop request.
func (r *tunnelRuntime) beginStart(cancel context.CancelFunc) (uint64, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil && r.cmd.Process != nil {
		return r.lifecycleGeneration, false
	}
	if r.externalPID > 0 {
		return r.lifecycleGeneration, false
	}
	if r.starting {
		return r.lifecycleGeneration, false
	}
	r.lifecycleGeneration++
	r.stopping = false
	r.starting = true
	r.respawnScheduled = false
	r.respawnGeneration = 0
	r.downloadCancel = cancel
	r.downloading = false
	r.downloadProgress = 0
	r.downloadError = ""
	return r.lifecycleGeneration, true
}

func (r *tunnelRuntime) startIsCurrent(generation uint64) bool {
	if r == nil || generation == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lifecycleGeneration == generation && r.starting
}

func (r *tunnelRuntime) finishStart(generation uint64) {
	if r == nil || generation == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lifecycleGeneration != generation {
		return
	}
	r.starting = false
	r.downloadCancel = nil
}

func (r *tunnelRuntime) attachExternalForGeneration(
	generation uint64,
	pid int,
	targetURL string,
	identity tunnelIdentity,
	quickURL string,
) bool {
	if r == nil || generation == 0 || pid <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lifecycleGeneration != generation {
		return false
	}
	r.cmd = nil
	r.cancel = nil
	r.downloadCancel = nil
	r.tempDir = ""
	r.targetURL = targetURL
	r.externalPID = pid
	r.starting = false
	r.stopping = false
	r.downloading = false
	r.downloadProgress = 100
	r.downloadError = ""
	r.publicURL = identity.PublicURL
	if r.publicURL == "" {
		r.publicURL = quickURL
	}
	r.stablePublicURL = identity.PublicURL
	r.quickTunnelURL = quickURL
	r.shortID = identity.ShortID
	r.relayURL = identity.RelayURL
	r.relayRegistered = identity.RelayURL != ""
	r.relayError = ""
	r.lastError = ""
	r.registeredQuickURL = quickURL
	r.startedAt = time.Now()
	r.respawnScheduled = false
	r.respawnAttempts = 0
	r.respawnGeneration = 0
	return true
}

func (r *tunnelRuntime) attachTempDirForGeneration(generation uint64, tempDir string) bool {
	if r == nil || generation == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lifecycleGeneration != generation || !r.starting {
		return false
	}
	r.tempDir = tempDir
	return true
}

func (r *tunnelRuntime) markReadyForGeneration(generation uint64, cmd *exec.Cmd) bool {
	if r == nil || generation == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lifecycleGeneration != generation {
		return false
	}
	if cmd != nil {
		if r.cmd != cmd || cmd.Process == nil {
			return false
		}
	} else if r.externalPID <= 0 {
		return false
	}
	r.respawnScheduled = false
	r.respawnAttempts = 0
	r.respawnGeneration = 0
	r.lastError = ""
	return true
}

func (r *tunnelRuntime) setTunnelIdentityForGeneration(generation uint64, identity tunnelIdentity) bool {
	if r == nil || generation == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lifecycleGeneration != generation || !r.starting {
		return false
	}
	r.shortID = identity.ShortID
	r.relayURL = identity.RelayURL
	r.stablePublicURL = identity.PublicURL
	r.relayRegistered = false
	r.relayError = ""
	r.registeredQuickURL = ""
	if identity.PublicURL != "" {
		r.publicURL = identity.PublicURL
	} else {
		r.publicURL = r.quickTunnelURL
	}
	return true
}

func (r *tunnelRuntime) setDownloadStateForGeneration(
	generation uint64,
	downloading bool,
	progress int,
	err error,
) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != 0 && r.lifecycleGeneration != generation {
		return
	}
	r.downloading = downloading
	r.downloadProgress = progress
	if err != nil {
		r.downloadError = err.Error()
	} else if downloading || progress >= 100 {
		r.downloadError = ""
	}
}

func (r *tunnelRuntime) snapshot() map[string]any {
	if r == nil {
		return map[string]any{"running": false}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	running := (r.cmd != nil && r.cmd.Process != nil) || r.externalPID > 0
	pid := 0
	if r.cmd != nil && r.cmd.Process != nil {
		pid = r.cmd.Process.Pid
	} else if r.externalPID > 0 {
		pid = r.externalPID
	}
	logs := append([]string(nil), r.logs...)
	publicAPIURL := ""
	if r.publicURL != "" {
		publicAPIURL = strings.TrimRight(r.publicURL, "/") + "/v1"
	}
	return map[string]any{
		"lifecycle_generation": r.lifecycleGeneration,
		"running":              running,
		"starting":             r.starting,
		"downloading":          r.downloading,
		"download_progress":    r.downloadProgress,
		"download_error":       r.downloadError,
		"public_url":           r.publicURL,
		"public_api_url":       publicAPIURL,
		"stable_public_url":    r.stablePublicURL,
		"quick_tunnel_url":     r.quickTunnelURL,
		"tunnel_url":           r.quickTunnelURL,
		"short_id":             r.shortID,
		"relay_url":            r.relayURL,
		"relay_registered":     r.relayRegistered,
		"relay_error":          r.relayError,
		"error":                r.lastError,
		"pid":                  pid,
		"started_at":           r.startedAt,
		"stopping":             r.stopping,
		"external_process":     r.externalPID > 0 && r.cmd == nil,
		"registered_quick_url": r.registeredQuickURL,
		"respawn_scheduled":    r.respawnScheduled,
		"respawn_attempt":      r.respawnAttempts,
		"logs":                 logs,
	}
}

func (r *tunnelRuntime) appendLog(line string) string {
	return r.appendLogForGeneration(0, nil, line)
}

func (r *tunnelRuntime) appendLogForGeneration(
	generation uint64,
	cmd *exec.Cmd,
	line string,
) string {
	line = strings.TrimSpace(line)
	if line == "" || r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != 0 &&
		(r.lifecycleGeneration != generation || r.cmd != cmd) {
		return ""
	}
	r.logs = append(r.logs, line)
	if len(r.logs) > tunnelLogLines {
		r.logs = r.logs[len(r.logs)-tunnelLogLines:]
	}
	match := extractCloudflareQuickTunnelURL(line)
	if match == "" || match == r.quickTunnelURL {
		return ""
	}
	r.quickTunnelURL = match
	if r.publicURL == "" {
		r.publicURL = match
	}
	return match
}

func (r *tunnelRuntime) setDownloadState(downloading bool, progress int, err error) {
	r.setDownloadStateForGeneration(0, downloading, progress, err)
}

func (h *Handler) tunnelConfigSnapshot() (config.TunnelConfig, int, string, bool) {
	if h == nil {
		return config.TunnelConfig{}, 0, "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return config.TunnelConfig{}, 0, "", false
	}
	return h.cfg.Tunnel, h.cfg.Port, h.configFilePath, true
}

type tunnelPatchRequest struct {
	Provider        *string `json:"provider"`
	Enabled         *bool   `json:"enabled"`
	TargetURL       *string `json:"target_url"`
	TargetURLHyphen *string `json:"target-url"`
	CloudflaredPath *string `json:"cloudflared_path"`
	CloudflaredDash *string `json:"cloudflared-path"`
	RelayURL        *string `json:"relay_url"`
	RelayURLHyphen  *string `json:"relay-url"`
	ShortID         *string `json:"short_id"`
	ShortIDHyphen   *string `json:"short-id"`
}

// GetTunnel returns configuration and current Cloudflare Quick Tunnel state.
func (h *Handler) GetTunnel(c *gin.Context) {
	cfg, port, _, ok := h.tunnelConfigSnapshot()
	if !ok {
		c.JSON(200, gin.H{"provider": defaultTunnelProvider, "running": false})
		return
	}
	normalizeTunnelConfig(&cfg, port)
	status := h.tunnelStatus(cfg)
	c.JSON(200, status)
}

// PutTunnel updates tunnel configuration. Starting/stopping remains explicit
// through the dedicated endpoints so a config save never launches a process.
func (h *Handler) PutTunnel(c *gin.Context) {
	if h == nil {
		c.JSON(503, gin.H{"error": "configuration unavailable"})
		return
	}
	var req tunnelPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "invalid request body"})
		return
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(503, gin.H{"error": "configuration unavailable"})
		return
	}
	cfg := &h.cfg.Tunnel
	if req.Provider != nil {
		cfg.Provider = strings.ToLower(strings.TrimSpace(*req.Provider))
	}
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.TargetURL != nil {
		cfg.TargetURL = strings.TrimSpace(*req.TargetURL)
	} else if req.TargetURLHyphen != nil {
		cfg.TargetURL = strings.TrimSpace(*req.TargetURLHyphen)
	}
	if req.CloudflaredPath != nil {
		cfg.CloudflaredPath = strings.TrimSpace(*req.CloudflaredPath)
	} else if req.CloudflaredDash != nil {
		cfg.CloudflaredPath = strings.TrimSpace(*req.CloudflaredDash)
	}
	if req.RelayURL != nil {
		cfg.RelayURL = strings.TrimSpace(*req.RelayURL)
	} else if req.RelayURLHyphen != nil {
		cfg.RelayURL = strings.TrimSpace(*req.RelayURLHyphen)
	}
	if req.ShortID != nil {
		cfg.ShortID = strings.TrimSpace(*req.ShortID)
	} else if req.ShortIDHyphen != nil {
		cfg.ShortID = strings.TrimSpace(*req.ShortIDHyphen)
	}
	normalizeTunnelConfig(cfg, h.cfg.Port)
	if cfg.Provider != defaultTunnelProvider {
		h.mu.Unlock()
		c.JSON(400, gin.H{"error": "unsupported tunnel provider"})
		return
	}
	if err := validateTunnelRelayConfig(*cfg); err != nil {
		h.mu.Unlock()
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
		h.mu.Unlock()
		c.JSON(500, gin.H{"error": fmt.Sprintf("failed to save tunnel config: %v", err)})
		return
	}
	updatedCfg := *cfg
	snapshot := h.reloadSnapshotConfigLocked()
	h.mu.Unlock()
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)
	c.JSON(200, h.tunnelStatus(updatedCfg))
}

type tunnelStartFailure struct {
	status int
	body   gin.H
}

func (e *tunnelStartFailure) Error() string {
	if e == nil {
		return "tunnel start failed"
	}
	if message, ok := e.body["error"].(string); ok && strings.TrimSpace(message) != "" {
		return message
	}
	return "tunnel start failed"
}

func newTunnelStartFailure(status int, message string, fields gin.H) error {
	body := gin.H{"error": message}
	for key, value := range fields {
		body[key] = value
	}
	return &tunnelStartFailure{status: status, body: body}
}

func writeTunnelStartFailure(c *gin.Context, err error) {
	if c == nil {
		return
	}
	var failure *tunnelStartFailure
	if errors.As(err, &failure) && failure != nil {
		c.JSON(failure.status, failure.body)
		return
	}
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
}

type tunnelTermination struct {
	cmd            *exec.Cmd
	cancel         context.CancelFunc
	downloadCancel context.CancelFunc
	tempDir        string
	shortID        string
	quickURL       string
	pid            int
}

// detachTunnelRuntime invalidates a lifecycle and removes its process handles
// while holding only the runtime lock. Callers terminate the detached process
// after releasing all locks so cloudflared cannot block status or config work.
func detachTunnelRuntime(runtimeState *tunnelRuntime, expectedGeneration uint64) (uint64, tunnelTermination, bool) {
	if runtimeState == nil {
		return 0, tunnelTermination{}, false
	}
	runtimeState.mu.Lock()
	defer runtimeState.mu.Unlock()
	if expectedGeneration != 0 && runtimeState.lifecycleGeneration != expectedGeneration {
		return runtimeState.lifecycleGeneration, tunnelTermination{}, false
	}

	runtimeState.lifecycleGeneration++
	generation := runtimeState.lifecycleGeneration
	termination := tunnelTermination{
		cmd:            runtimeState.cmd,
		cancel:         runtimeState.cancel,
		downloadCancel: runtimeState.downloadCancel,
		tempDir:        runtimeState.tempDir,
		shortID:        runtimeState.shortID,
		quickURL:       runtimeState.quickTunnelURL,
		pid:            runtimeState.externalPID,
	}
	runtimeState.cmd = nil
	runtimeState.cancel = nil
	runtimeState.downloadCancel = nil
	runtimeState.tempDir = ""
	runtimeState.targetURL = ""
	runtimeState.externalPID = 0
	runtimeState.starting = false
	runtimeState.downloading = false
	runtimeState.quickTunnelURL = ""
	runtimeState.registeredQuickURL = ""
	runtimeState.relayRegistered = false
	runtimeState.relayError = ""
	runtimeState.respawnScheduled = false
	runtimeState.respawnGeneration = 0
	runtimeState.stopping = termination.cmd != nil ||
		termination.cancel != nil ||
		termination.downloadCancel != nil
	if runtimeState.stablePublicURL == "" {
		runtimeState.publicURL = ""
	} else {
		runtimeState.publicURL = runtimeState.stablePublicURL
	}
	runtimeState.startedAt = time.Time{}
	return generation, termination, true
}

func terminateTunnelProcess(termination tunnelTermination) {
	if termination.cancel != nil {
		termination.cancel()
	}
	if termination.downloadCancel != nil {
		termination.downloadCancel()
	}
	if termination.cmd != nil && termination.cmd.Process != nil {
		_ = termination.cmd.Process.Kill()
	} else if termination.pid > 0 {
		// A tunnel adopted from a previous server process has no *exec.Cmd
		// handle. It still must be terminated on an explicit stop/shutdown;
		// otherwise the next server instance would leave an orphaned
		// cloudflared process behind.
		_ = killTunnelProcessPID(termination.pid)
	}
	if termination.cmd == nil && termination.tempDir != "" {
		cleanupTunnelTempDir(termination.tempDir)
	}
}

func (h *Handler) abortTunnelStart(runtimeState *tunnelRuntime, generation uint64) {
	if h == nil || runtimeState == nil || generation == 0 {
		return
	}
	runtimeState.intentMu.Lock()
	_, termination, detached := detachTunnelRuntime(runtimeState, generation)
	if detached {
		_ = h.clearPersistedTunnelURLIfMatches(
			termination.shortID,
			termination.quickURL,
			termination.pid,
		)
	}
	runtimeState.intentMu.Unlock()
	if detached {
		terminateTunnelProcess(termination)
	}
}

func (h *Handler) persistTunnelEnabled(enabled bool) (config.TunnelConfig, error) {
	if h == nil {
		return config.TunnelConfig{}, errors.New("configuration unavailable")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return config.TunnelConfig{}, errors.New("configuration unavailable")
	}
	h.cfg.Tunnel.Enabled = enabled
	if errSave := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); errSave != nil {
		return h.cfg.Tunnel, errSave
	}
	return h.cfg.Tunnel, nil
}

func tunnelProcessMatches(name, commandLine, targetURL string, port int) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	commandLine = strings.ToLower(strings.TrimSpace(commandLine))
	if !strings.Contains(name, "cloudflared") || !strings.Contains(commandLine, "tunnel") {
		return false
	}
	if port > 0 && !strings.Contains(commandLine, ":"+strconv.Itoa(port)) {
		return false
	}
	if targetURL != "" {
		parsed, errParse := url.Parse(targetURL)
		if errParse == nil && parsed.Host != "" {
			hostPort := parsed.Host
			if !strings.Contains(commandLine, strings.ToLower(hostPort)) &&
				!strings.Contains(commandLine, strings.ToLower(parsed.Hostname())) {
				return false
			}
		}
	}
	return true
}

func tunnelTargetPort(targetURL string) int {
	parsed, errParse := url.Parse(strings.TrimSpace(targetURL))
	if errParse != nil {
		return 0
	}
	port, errPort := strconv.Atoi(parsed.Port())
	if errPort != nil || port <= 0 {
		return 0
	}
	return port
}

func tunnelCommandEnvironment() []string {
	const defaultProtocol = "http2"
	protocol := strings.ToLower(strings.TrimSpace(os.Getenv("TUNNEL_TRANSPORT_PROTOCOL")))
	if protocol == "" {
		protocol = strings.ToLower(strings.TrimSpace(os.Getenv("CLOUDFLARED_PROTOCOL")))
	}
	switch protocol {
	case "http2", "quic", "auto":
	default:
		protocol = defaultProtocol
	}

	environment := make([]string, 0, len(os.Environ())+1)
	prefixes := []string{"TUNNEL_TRANSPORT_PROTOCOL=", "CLOUDFLARED_PROTOCOL="}
	for _, entry := range os.Environ() {
		skip := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(entry, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			environment = append(environment, entry)
		}
	}
	environment = append(environment, "TUNNEL_TRANSPORT_PROTOCOL="+protocol)
	return environment
}

func tunnelCandidatePIDs(state tunnelState, targetURL string, port int) []int {
	ids := make([]int, 0, 4)
	seen := make(map[int]struct{})
	add := func(pid int) {
		if pid <= 0 {
			return
		}
		if _, exists := seen[pid]; exists {
			return
		}
		seen[pid] = struct{}{}
		ids = append(ids, pid)
	}
	add(state.PID)
	for _, pid := range findTunnelProcessIDs(port) {
		add(pid)
	}
	return ids
}

func probeExistingTunnel(ctx context.Context, identity tunnelIdentity, quickURL string) bool {
	if strings.TrimSpace(quickURL) == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	quickCtx, cancelQuick := context.WithTimeout(ctx, tunnelReuseProbeTimeout)
	defer cancelQuick()
	quickOK := probeDirectTunnelURL(quickCtx, quickURL)
	if !quickOK {
		return false
	}
	if identity.PublicURL == "" {
		return true
	}
	stableCtx, cancelStable := context.WithTimeout(ctx, tunnelReuseProbeTimeout)
	defer cancelStable()
	return probeTunnelURL(stableCtx, identity.PublicURL)
}

// tryReuseExistingTunnel follows 9router's restart behavior. A persisted
// cloudflared process is reused only when its command line, temporary URL and
// stable relay URL all still describe a healthy tunnel; stale candidates are
// terminated before a new process is spawned.
func (h *Handler) tryReuseExistingTunnel(
	cfg config.TunnelConfig,
	port int,
	configPath string,
	runtimeState *tunnelRuntime,
) (bool, error) {
	if h == nil || runtimeState == nil {
		return false, nil
	}
	runtimeState.mu.Lock()
	if runtimeState.cmd != nil || runtimeState.starting || runtimeState.externalPID > 0 {
		runtimeState.mu.Unlock()
		return false, nil
	}
	runtimeState.mu.Unlock()
	state, errState := loadTunnelStateSafe(managedTunnelStatePath(configPath))
	if errState != nil {
		return false, nil
	}
	if state.ShortID == "" {
		return false, nil
	}
	identity, errIdentity := h.currentTunnelIdentity(cfg)
	if errIdentity != nil {
		return false, errIdentity
	}
	if identity.ShortID == "" || identity.ShortID != normalizeTunnelShortID(state.ShortID) {
		return false, nil
	}
	targetURL := cfg.TargetURL
	if state.TunnelURL == "" && state.PID > 0 {
		name, commandLine, alive := inspectTunnelProcess(state.PID)
		if alive && tunnelProcessMatches(name, commandLine, targetURL, port) {
			// The previous process was interrupted before it published a
			// Quick Tunnel URL. It cannot be safely reused, so remove it
			// before launching a replacement.
			_ = killTunnelProcessPID(state.PID)
		}
		_ = h.clearPersistedTunnelURL(identity.ShortID)
		return false, nil
	}
	if state.TunnelURL == "" {
		return false, nil
	}
	if state.TargetURL != "" && state.TargetURL != targetURL {
		// A process forwarding another local listener must not be adopted.
		for _, pid := range tunnelCandidatePIDs(
			state,
			state.TargetURL,
			tunnelTargetPort(state.TargetURL),
		) {
			name, commandLine, alive := inspectTunnelProcess(pid)
			if alive && tunnelProcessMatches(
				name,
				commandLine,
				state.TargetURL,
				tunnelTargetPort(state.TargetURL),
			) {
				_ = killTunnelProcessPID(pid)
			}
		}
		_ = h.clearPersistedTunnelURL(identity.ShortID)
		return false, nil
	}

	var reusablePID int
	var candidateCommand string
	for _, pid := range tunnelCandidatePIDs(state, targetURL, port) {
		name, commandLine, alive := inspectTunnelProcess(pid)
		if !alive || !tunnelProcessMatches(name, commandLine, targetURL, port) {
			continue
		}
		if probeExistingTunnel(context.Background(), identity, state.TunnelURL) {
			reusablePID = pid
			candidateCommand = commandLine
			break
		}
		_ = killTunnelProcessPID(pid)
	}
	if reusablePID == 0 {
		if state.PID > 0 {
			if _, _, alive := inspectTunnelProcess(state.PID); !alive {
				_ = h.clearPersistedTunnelURL(identity.ShortID)
			}
		}
		return false, nil
	}

	runtimeState.mu.Lock()
	runtimeState.lifecycleGeneration++
	generation := runtimeState.lifecycleGeneration
	runtimeState.mu.Unlock()
	if !runtimeState.attachExternalForGeneration(
		generation,
		reusablePID,
		targetURL,
		identity,
		state.TunnelURL,
	) {
		return false, nil
	}
	if errPersist := h.persistTunnelURLWithProcess(
		identity.ShortID,
		state.TunnelURL,
		reusablePID,
		targetURL,
	); errPersist != nil {
		runtimeState.setRelayErrorForGeneration(generation, errPersist)
	}
	if _, errPersist := h.persistTunnelEnabled(true); errPersist != nil {
		_, termination, _ := detachTunnelRuntime(runtimeState, generation)
		terminateTunnelProcess(termination)
		_ = h.clearPersistedTunnelURL(identity.ShortID)
		return false, fmt.Errorf("persist tunnel enabled state: %w", errPersist)
	}
	runtimeState.appendLog(fmt.Sprintf(
		"reused healthy cloudflared process pid=%d command=%s",
		reusablePID,
		candidateCommand,
	))
	h.startExternalTunnelWatchdog(reusablePID, generation, identity.ShortID)
	return true, nil
}

func (h *Handler) startExternalTunnelWatchdog(pid int, generation uint64, shortID string) {
	if h == nil || pid <= 0 || generation == 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(tunnelWatchdogInterval)
		defer ticker.Stop()
		for range ticker.C {
			runtimeState := h.ensureTunnelRuntime()
			if runtimeState == nil {
				return
			}
			runtimeState.mu.Lock()
			if runtimeState.lifecycleGeneration != generation ||
				runtimeState.externalPID != pid ||
				runtimeState.cmd != nil {
				runtimeState.mu.Unlock()
				return
			}
			runtimeState.mu.Unlock()
			name, commandLine, alive := inspectTunnelProcess(pid)
			if alive && tunnelProcessMatches(name, commandLine, "", 0) {
				continue
			}
			quickURL := ""
			runtimeState.mu.Lock()
			if runtimeState.lifecycleGeneration == generation &&
				runtimeState.externalPID == pid {
				quickURL = runtimeState.quickTunnelURL
				runtimeState.lifecycleGeneration++
				runtimeState.externalPID = 0
				runtimeState.targetURL = ""
				runtimeState.quickTunnelURL = ""
				runtimeState.registeredQuickURL = ""
				runtimeState.relayRegistered = false
				runtimeState.lastError = "cloudflared exited unexpectedly"
				runtimeState.logs = append(runtimeState.logs, runtimeState.lastError)
				runtimeState.publicURL = runtimeState.stablePublicURL
			}
			runtimeState.mu.Unlock()
			runtimeState.intentMu.Lock()
			_ = h.clearPersistedTunnelURLIfMatches(shortID, quickURL, pid)
			runtimeState.intentMu.Unlock()
			h.scheduleTunnelRespawn()
			return
		}
	}()
}

// startTunnelRuntime starts a local Cloudflare Quick Tunnel and returns the
// management status payload and HTTP status code that should be exposed.
func (h *Handler) startTunnelRuntime(parent context.Context) (map[string]any, int, error) {
	cfg, port, configPath, ok := h.tunnelConfigSnapshot()
	if !ok {
		return nil, http.StatusServiceUnavailable, newTunnelStartFailure(
			http.StatusServiceUnavailable,
			"configuration unavailable",
			nil,
		)
	}
	if parent == nil {
		parent = context.Background()
	}

	normalizeTunnelConfig(&cfg, port)
	if cfg.Provider != "" && cfg.Provider != defaultTunnelProvider {
		return nil, http.StatusBadRequest, newTunnelStartFailure(
			http.StatusBadRequest,
			"unsupported tunnel provider",
			nil,
		)
	}
	if errValidate := validateTunnelRelayConfig(cfg); errValidate != nil {
		return nil, http.StatusBadRequest, newTunnelStartFailure(
			http.StatusBadRequest,
			errValidate.Error(),
			nil,
		)
	}
	target, errTarget := validateTunnelTarget(cfg.TargetURL)
	if errTarget != nil {
		return nil, http.StatusBadRequest, newTunnelStartFailure(
			http.StatusBadRequest,
			errTarget.Error(),
			nil,
		)
	}

	runtimeState := h.ensureTunnelRuntime()
	runtimeState.intentMu.Lock()
	reused, errReuse := h.tryReuseExistingTunnel(cfg, port, configPath, runtimeState)
	runtimeState.intentMu.Unlock()
	if errReuse != nil {
		return nil, http.StatusServiceUnavailable, newTunnelStartFailure(
			http.StatusServiceUnavailable,
			fmt.Sprintf("failed to inspect an existing tunnel: %v", errReuse),
			nil,
		)
	}
	if reused {
		cfg.Enabled = true
		return h.tunnelStatus(cfg), http.StatusOK, nil
	}

	ctx, cancel := context.WithCancel(parent)
	runtimeState.intentMu.Lock()
	startGeneration, claimed := runtimeState.beginStart(cancel)
	runtimeState.intentMu.Unlock()
	if !claimed {
		runtimeState.mu.Lock()
		starting := runtimeState.starting
		runtimeState.mu.Unlock()
		if starting {
			cancel()
			return h.tunnelStatus(cfg), http.StatusAccepted, nil
		}
		cancel()
		return h.tunnelStatus(cfg), http.StatusOK, nil
	}
	defer func() {
		runtimeState.intentMu.Lock()
		defer runtimeState.intentMu.Unlock()
		runtimeState.finishStart(startGeneration)
	}()
	startSucceeded := false
	defer func() {
		if !startSucceeded {
			h.abortTunnelStart(runtimeState, startGeneration)
		}
	}()

	identity, errIdentity := h.resolveTunnelIdentity(cfg)
	if errIdentity != nil {
		return nil, http.StatusInternalServerError, newTunnelStartFailure(
			http.StatusInternalServerError,
			fmt.Sprintf("failed to prepare stable tunnel identity: %v", errIdentity),
			nil,
		)
	}
	if !runtimeState.setTunnelIdentityForGeneration(startGeneration, identity) {
		return h.tunnelStatus(cfg), http.StatusOK, nil
	}

	binary, errBinary := ensureCloudflaredForGeneration(
		ctx,
		cfg.CloudflaredPath,
		h.configFilePath,
		runtimeState,
		startGeneration,
	)
	if errBinary != nil {
		return nil, http.StatusServiceUnavailable, newTunnelStartFailure(
			http.StatusServiceUnavailable,
			errBinary.Error(),
			gin.H{
				"binary_found": false,
				"target_url":   cfg.TargetURL,
				"downloaded":   false,
			},
		)
	}
	if !runtimeState.startIsCurrent(startGeneration) || ctx.Err() != nil {
		return h.tunnelStatus(cfg), http.StatusOK, nil
	}

	tempDir, isolatedConfigPath, errTempConfig := createQuickTunnelConfig(configPath)
	if errTempConfig != nil {
		return nil, http.StatusInternalServerError, newTunnelStartFailure(
			http.StatusInternalServerError,
			fmt.Sprintf("failed to prepare isolated cloudflared config: %v", errTempConfig),
			nil,
		)
	}
	if !runtimeState.attachTempDirForGeneration(startGeneration, tempDir) {
		cleanupTunnelTempDir(tempDir)
		return h.tunnelStatus(cfg), http.StatusOK, nil
	}

	cmd := exec.CommandContext(
		ctx,
		binary,
		"tunnel",
		"--url",
		target,
		"--config",
		isolatedConfigPath,
		"--no-autoupdate",
		"--retries",
		"99",
	)
	cmd.Dir = tempDir
	configureTunnelCommand(cmd)
	cmd.Env = tunnelCommandEnvironment()
	stdout, errStdout := cmd.StdoutPipe()
	if errStdout != nil {
		return nil, http.StatusInternalServerError, newTunnelStartFailure(
			http.StatusInternalServerError,
			"failed to prepare tunnel output",
			nil,
		)
	}
	stderr, errStderr := cmd.StderrPipe()
	if errStderr != nil {
		return nil, http.StatusInternalServerError, newTunnelStartFailure(
			http.StatusInternalServerError,
			"failed to prepare tunnel error output",
			nil,
		)
	}
	if errStart := cmd.Start(); errStart != nil {
		if !runtimeState.startIsCurrent(startGeneration) || ctx.Err() != nil {
			return h.tunnelStatus(cfg), http.StatusOK, nil
		}
		return nil, http.StatusServiceUnavailable, newTunnelStartFailure(
			http.StatusServiceUnavailable,
			"failed to start cloudflared",
			gin.H{"detail": errStart.Error()},
		)
	}
	runtimeState.mu.Lock()
	if runtimeState.lifecycleGeneration != startGeneration ||
		!runtimeState.starting ||
		ctx.Err() != nil {
		runtimeState.mu.Unlock()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cleanupTunnelTempDir(tempDir)
		return h.tunnelStatus(cfg), http.StatusOK, nil
	}
	runtimeState.cmd = cmd
	runtimeState.cancel = cancel
	runtimeState.downloadCancel = nil
	runtimeState.downloading = false
	runtimeState.downloadProgress = 100
	runtimeState.targetURL = target
	runtimeState.externalPID = cmd.Process.Pid
	runtimeState.publicURL = identity.PublicURL
	runtimeState.stablePublicURL = identity.PublicURL
	runtimeState.quickTunnelURL = ""
	runtimeState.shortID = identity.ShortID
	runtimeState.relayURL = identity.RelayURL
	runtimeState.relayRegistered = false
	runtimeState.relayError = ""
	runtimeState.lastError = ""
	runtimeState.logs = nil
	runtimeState.startedAt = time.Now()
	runtimeState.mu.Unlock()

	// Start the readers and waiter before the first state persistence. If that
	// persistence fails, the deferred abort path can kill the process while
	// waitTunnelProcess still reaps it and removes the isolated config dir.
	go h.readTunnelOutput(stdout, cmd, startGeneration)
	go h.readTunnelOutput(stderr, cmd, startGeneration)
	go h.waitTunnelProcess(cmd, cancel, startGeneration, tempDir)

	// Persist the process identity before waiting for cloudflared's first URL.
	// If the application is restarted during this short window, the next
	// instance can find and terminate the orphan instead of spawning a second
	// tunnel on the same local listener.
	if errPersist := h.persistTunnelURLForGeneration(
		runtimeState,
		startGeneration,
		identity.ShortID,
		"",
	); errPersist != nil && !errors.Is(errPersist, errTunnelLifecycleStale) {
		return nil, http.StatusInternalServerError, newTunnelStartFailure(
			http.StatusInternalServerError,
			fmt.Sprintf("failed to persist tunnel process state: %v", errPersist),
			nil,
		)
	}

	quickURL, errQuickURL := waitForQuickTunnelURL(
		ctx,
		runtimeState,
		startGeneration,
		cmd,
	)
	if errQuickURL != nil {
		return nil, http.StatusServiceUnavailable, newTunnelStartFailure(
			http.StatusServiceUnavailable,
			fmt.Sprintf("quick tunnel did not become ready: %v", errQuickURL),
			gin.H{"target_url": target},
		)
	}
	if errRegistration := waitForTunnelRegistration(
		ctx,
		runtimeState,
		startGeneration,
		cmd,
		quickURL,
	); errRegistration != nil {
		return nil, http.StatusServiceUnavailable, newTunnelStartFailure(
			http.StatusServiceUnavailable,
			fmt.Sprintf("tunnel relay registration failed: %v", errRegistration),
			gin.H{
				"quick_tunnel_url":  quickURL,
				"stable_public_url": identity.PublicURL,
			},
		)
	}

	if identity.PublicURL != "" {
		if errHealth := waitForTunnelHealth(ctx, identity.PublicURL); errHealth != nil {
			return nil, http.StatusServiceUnavailable, newTunnelStartFailure(
				http.StatusServiceUnavailable,
				fmt.Sprintf("stable tunnel URL did not become healthy: %v", errHealth),
				gin.H{
					"quick_tunnel_url":  quickURL,
					"stable_public_url": identity.PublicURL,
				},
			)
		}
	} else if !probeDirectTunnelURL(ctx, quickURL) {
		// Direct mode follows 9router: a temporary URL probe is best effort
		// because TryCloudflare DNS can lag or be blocked independently.
		runtimeState.appendLogForGeneration(
			startGeneration,
			cmd,
			"direct tunnel URL probe was not reachable yet; continuing",
		)
	}

	if errPersist := h.persistTunnelEnabledForGeneration(runtimeState, startGeneration); errPersist != nil {
		if errors.Is(errPersist, errTunnelLifecycleStale) {
			return h.tunnelStatus(cfg), http.StatusOK, nil
		}
		return nil, http.StatusInternalServerError, newTunnelStartFailure(
			http.StatusInternalServerError,
			fmt.Sprintf("failed to persist tunnel enabled state: %v", errPersist),
			nil,
		)
	}
	if !runtimeState.markReadyForGeneration(startGeneration, cmd) {
		return h.tunnelStatus(cfg), http.StatusOK, nil
	}
	startSucceeded = true
	// Publish a fully settled status to the caller. The deferred cleanup still
	// invokes finishStart defensively, but the successful response should not
	// report starting=true after readiness/health checks have passed.
	runtimeState.finishStart(startGeneration)
	cfg.Enabled = true
	return h.tunnelStatus(cfg), http.StatusOK, nil
}

// persistTunnelEnabledForGeneration commits the enabled intent only while the
// process publication still belongs to the same lifecycle generation. The
// intent lock serializes this commit with StopTunnel, while runtime and config
// locks are acquired separately to avoid lock-order inversions.
func (h *Handler) persistTunnelEnabledForGeneration(
	runtimeState *tunnelRuntime,
	generation uint64,
) error {
	if h == nil || runtimeState == nil || generation == 0 {
		return errTunnelLifecycleStale
	}
	runtimeState.intentMu.Lock()
	defer runtimeState.intentMu.Unlock()

	runtimeState.mu.Lock()
	valid := false
	if runtimeState.lifecycleGeneration == generation &&
		runtimeState.starting &&
		runtimeState.cmd != nil &&
		runtimeState.cmd.Process != nil {
		valid = true
	}
	runtimeState.mu.Unlock()
	if !valid {
		return errTunnelLifecycleStale
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		return errors.New("configuration unavailable")
	}
	h.cfg.Tunnel.Enabled = true
	errSave := config.SaveConfigPreserveComments(h.configFilePath, h.cfg)
	h.mu.Unlock()
	if errSave != nil {
		return fmt.Errorf("save config: %w", errSave)
	}
	return nil
}

func createQuickTunnelConfig(configFilePath string) (string, string, error) {
	baseDir := strings.TrimSpace(configFilePath)
	if baseDir != "" {
		baseDir = filepath.Join(filepath.Dir(baseDir), "runtime", "tunnel")
	} else {
		baseDir = os.TempDir()
	}
	if errMkdir := os.MkdirAll(baseDir, 0o755); errMkdir != nil {
		return "", "", errMkdir
	}
	tempDir, errTempDir := os.MkdirTemp(baseDir, "cloudflared-quick-")
	if errTempDir != nil {
		return "", "", errTempDir
	}
	configPath := filepath.Join(tempDir, "config.yml")
	const isolatedConfig = "# CLIProxyAPI isolated Quick Tunnel configuration.\n"
	if errWrite := os.WriteFile(configPath, []byte(isolatedConfig), 0o600); errWrite != nil {
		cleanupTunnelTempDir(tempDir)
		return "", "", errWrite
	}
	return tempDir, configPath, nil
}

func cleanupTunnelTempDir(tempDir string) {
	rawPath := strings.TrimSpace(tempDir)
	if rawPath == "" {
		return
	}
	absolutePath, errAbs := filepath.Abs(rawPath)
	if errAbs != nil {
		return
	}
	absolutePath = filepath.Clean(absolutePath)
	if absolutePath == filepath.VolumeName(absolutePath)+string(filepath.Separator) {
		return
	}
	if !strings.HasPrefix(strings.ToLower(filepath.Base(absolutePath)), "cloudflared-quick-") {
		return
	}
	parent := filepath.Clean(filepath.Dir(absolutePath))
	expectedTempParent := filepath.Clean(os.TempDir())
	// Runtime tunnel directories are named "tunnel"; os.TempDir is used when
	// no config path is available. Keep the recursive removal constrained to
	// these two roots and never accept an arbitrary prefixed path.
	runtimeParentAllowed := strings.EqualFold(filepath.Base(parent), "tunnel") &&
		strings.EqualFold(filepath.Base(filepath.Dir(parent)), "runtime")
	tempParentAllowed := strings.EqualFold(parent, expectedTempParent)
	if !runtimeParentAllowed && !tempParentAllowed {
		return
	}
	info, errStat := os.Stat(absolutePath)
	if errStat != nil || !info.IsDir() {
		return
	}
	_ = os.RemoveAll(absolutePath)
}

func waitForQuickTunnelURL(
	ctx context.Context,
	runtimeState *tunnelRuntime,
	generation uint64,
	cmd *exec.Cmd,
) (string, error) {
	if runtimeState == nil || generation == 0 || cmd == nil {
		return "", errTunnelLifecycleStale
	}
	timeout := time.NewTimer(tunnelQuickURLTimeout)
	defer timeout.Stop()
	ticker := time.NewTicker(tunnelReadyPollInterval)
	defer ticker.Stop()

	for {
		runtimeState.mu.Lock()
		if runtimeState.lifecycleGeneration != generation || runtimeState.cmd != cmd {
			runtimeState.mu.Unlock()
			return "", errTunnelLifecycleStale
		}
		quickURL := runtimeState.quickTunnelURL
		processAlive := runtimeState.cmd != nil && runtimeState.cmd.Process != nil
		lastError := runtimeState.lastError
		logs := append([]string(nil), runtimeState.logs...)
		runtimeState.mu.Unlock()
		if quickURL != "" {
			return quickURL, nil
		}
		if !processAlive {
			if lastError != "" {
				return "", errors.New(lastError)
			}
			return "", errors.New("cloudflared exited before publishing a Quick Tunnel URL")
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeout.C:
			detail := ""
			if len(logs) > 0 {
				detail = strings.TrimSpace(logs[len(logs)-1])
			}
			if detail == "" {
				detail = "no Quick Tunnel URL was reported by cloudflared"
			}
			return "", fmt.Errorf("timeout after %s: %s", tunnelQuickURLTimeout, detail)
		case <-ticker.C:
		}
	}
}

func waitForTunnelRegistration(
	ctx context.Context,
	runtimeState *tunnelRuntime,
	generation uint64,
	cmd *exec.Cmd,
	quickURL string,
) error {
	if runtimeState == nil || generation == 0 || cmd == nil {
		return errTunnelLifecycleStale
	}
	timeout := time.NewTimer(tunnelHealthTimeout)
	defer timeout.Stop()
	ticker := time.NewTicker(tunnelReadyPollInterval)
	defer ticker.Stop()
	for {
		runtimeState.mu.Lock()
		if runtimeState.lifecycleGeneration != generation || runtimeState.cmd != cmd {
			runtimeState.mu.Unlock()
			return errTunnelLifecycleStale
		}
		if runtimeState.quickTunnelURL != "" {
			quickURL = runtimeState.quickTunnelURL
		}
		registeredQuickURL := runtimeState.registeredQuickURL
		relayURL := runtimeState.relayURL
		relayRegistered := runtimeState.relayRegistered
		relayError := runtimeState.relayError
		processAlive := runtimeState.cmd != nil && runtimeState.cmd.Process != nil
		runtimeState.mu.Unlock()

		if relayError != "" {
			return errors.New(relayError)
		}
		if registeredQuickURL == quickURL &&
			quickURL != "" &&
			(relayURL == "" || relayRegistered) {
			return nil
		}
		if !processAlive {
			return errors.New("cloudflared exited while registering the tunnel URL")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("timeout after %s waiting for relay registration", tunnelHealthTimeout)
		case <-ticker.C:
		}
	}
}

func tunnelHealthURL(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/api/health"
}

func probeTunnelURL(ctx context.Context, baseURL string) bool {
	if strings.TrimSpace(baseURL) == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, tunnelHealthRequestTTL)
	defer cancel()
	request, errRequest := http.NewRequestWithContext(
		requestCtx,
		http.MethodGet,
		tunnelHealthURL(baseURL),
		nil,
	)
	if errRequest != nil {
		return false
	}
	request.Header.Set("User-Agent", "CLIProxyAPI tunnel health")
	response, errDo := http.DefaultClient.Do(request)
	if errDo != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
}

func probeDirectTunnelURL(ctx context.Context, baseURL string) bool {
	if strings.TrimSpace(baseURL) == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, tunnelHealthRequestTTL)
	defer cancel()
	request, errRequest := http.NewRequestWithContext(
		requestCtx,
		http.MethodGet,
		strings.TrimRight(strings.TrimSpace(baseURL), "/")+"/healthz",
		nil,
	)
	if errRequest != nil {
		return false
	}
	request.Header.Set("User-Agent", "CLIProxyAPI tunnel health")
	response, errDo := http.DefaultClient.Do(request)
	if errDo != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
}

func waitForTunnelHealth(ctx context.Context, baseURL string) error {
	if strings.TrimSpace(baseURL) == "" {
		return errors.New("stable tunnel URL is empty")
	}
	timeout := time.NewTimer(tunnelHealthTimeout)
	defer timeout.Stop()
	ticker := time.NewTicker(tunnelHealthInterval)
	defer ticker.Stop()
	for {
		if probeTunnelURL(ctx, baseURL) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("timeout after %s probing %s", tunnelHealthTimeout, tunnelHealthURL(baseURL))
		case <-ticker.C:
		}
	}
}

func tunnelEnabled(h *Handler) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg != nil && h.cfg.Tunnel.Enabled
}

func tunnelRespawnDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := tunnelRespawnInitialGap
	for index := 1; index < attempt; index++ {
		if delay >= tunnelRespawnMaxGap/2 {
			return tunnelRespawnMaxGap
		}
		delay *= 2
	}
	if delay > tunnelRespawnMaxGap {
		return tunnelRespawnMaxGap
	}
	return delay
}

func (h *Handler) scheduleTunnelRespawn() {
	if h == nil {
		return
	}
	runtimeState := h.ensureTunnelRuntime()
	if runtimeState == nil || !tunnelEnabled(h) {
		return
	}
	runtimeState.mu.Lock()
	if runtimeState.cmd != nil || runtimeState.starting || runtimeState.respawnScheduled {
		runtimeState.mu.Unlock()
		return
	}
	runtimeState.respawnAttempts++
	attempt := runtimeState.respawnAttempts
	generation := runtimeState.lifecycleGeneration
	runtimeState.respawnScheduled = true
	runtimeState.respawnGeneration = generation
	runtimeState.mu.Unlock()

	delay := tunnelRespawnDelay(attempt)
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C

		runtimeState.mu.Lock()
		if !runtimeState.respawnScheduled ||
			runtimeState.respawnGeneration != generation ||
			runtimeState.lifecycleGeneration != generation {
			runtimeState.mu.Unlock()
			return
		}
		runtimeState.respawnScheduled = false
		runtimeState.respawnGeneration = 0
		runtimeState.mu.Unlock()
		if !tunnelEnabled(h) {
			return
		}
		if _, _, errStart := h.startTunnelRuntime(context.Background()); errStart != nil {
			log.WithError(errStart).Warn("tunnel respawn attempt failed")
			h.scheduleTunnelRespawn()
		}
	}()
}

// StartTunnel starts a local Cloudflare Quick Tunnel using cloudflared.
func (h *Handler) StartTunnel(c *gin.Context) {
	status, statusCode, errStart := h.startTunnelRuntime(context.Background())
	if errStart != nil {
		writeTunnelStartFailure(c, errStart)
		return
	}
	c.JSON(statusCode, status)
}

// StartTunnelIfEnabled resumes the configured tunnel after the proxy server is
// ready, matching 9router's startup behavior.
func (h *Handler) StartTunnelIfEnabled(ctx context.Context) {
	if h == nil {
		return
	}
	h.mu.Lock()
	enabled := h.cfg != nil && h.cfg.Tunnel.Enabled
	h.mu.Unlock()
	if !enabled {
		return
	}
	if _, _, errStart := h.startTunnelRuntime(ctx); errStart != nil {
		log.WithError(errStart).Warn("configured tunnel auto-resume failed")
		h.scheduleTunnelRespawn()
	}
}

// reconcileTunnelAfterConfigReload stops a live tunnel when a hot-reloaded
// configuration disables it or changes the local target/relay identity. The
// explicit Management enable button remains responsible for starting a tunnel
// that was previously stopped; startup auto-resume is handled by
// StartTunnelIfEnabled after the local listener is ready.
func (h *Handler) reconcileTunnelAfterConfigReload() {
	if h == nil {
		return
	}
	h.tunnelReconcileMu.Lock()
	defer h.tunnelReconcileMu.Unlock()

	cfg, port, _, ok := h.tunnelConfigSnapshot()
	runtimeState := h.ensureTunnelRuntime()
	if runtimeState == nil {
		return
	}
	if !ok {
		h.stopTunnelRuntimeAndClearState()
		return
	}
	normalizeTunnelConfig(&cfg, port)

	runtimeState.mu.Lock()
	active := (runtimeState.cmd != nil && runtimeState.cmd.Process != nil) ||
		runtimeState.externalPID > 0 ||
		runtimeState.starting
	starting := runtimeState.starting
	currentTarget := strings.TrimSpace(runtimeState.targetURL)
	currentRelay := strings.TrimSpace(runtimeState.relayURL)
	currentShortID := normalizeTunnelShortID(runtimeState.shortID)
	runtimeState.mu.Unlock()
	if !active {
		return
	}

	configuredRelay := normalizeTunnelRelayURL(cfg.RelayURL)
	configuredShortID := normalizeTunnelShortID(cfg.ShortID)
	// During the download/initialization window the runtime has not published
	// its target yet. A routine config reload should not cancel that start
	// merely because the target field is still empty.
	if cfg.Enabled && starting && currentTarget == "" {
		return
	}
	identityChanged := cfg.Provider != defaultTunnelProvider ||
		currentTarget != strings.TrimSpace(cfg.TargetURL) ||
		currentRelay != configuredRelay ||
		(configuredShortID != "" && currentShortID != configuredShortID)
	if cfg.Enabled && !identityChanged {
		return
	}

	h.stopTunnelRuntimeAndClearState()
	if !cfg.Enabled || !identityChanged {
		return
	}

	// Retargeting an enabled tunnel is an intentional restart. Re-read the
	// config before launching so a newer reload that disabled it wins.
	latest, latestPort, _, latestOK := h.tunnelConfigSnapshot()
	if !latestOK {
		return
	}
	normalizeTunnelConfig(&latest, latestPort)
	if !latest.Enabled {
		return
	}
	go func() {
		if _, _, errStart := h.startTunnelRuntime(context.Background()); errStart != nil {
			log.WithError(errStart).Warn("tunnel restart after config reload failed")
		}
	}()
}

// stopTunnelRuntimeAndClearState is used for a config-driven stop/restart.
// Unlike server shutdown, a reload that changes the desired tunnel must also
// remove the ephemeral Quick Tunnel URL/PID so a later enable cannot mistake
// the old process for a reusable one.
func (h *Handler) stopTunnelRuntimeAndClearState() {
	if h == nil {
		return
	}
	runtimeState := h.ensureTunnelRuntime()
	if runtimeState == nil {
		return
	}
	runtimeState.intentMu.Lock()
	_, termination, detached := detachTunnelRuntime(runtimeState, 0)
	if detached {
		_ = h.clearPersistedTunnelURLIfMatches(
			termination.shortID,
			termination.quickURL,
			termination.pid,
		)
	}
	runtimeState.intentMu.Unlock()
	if detached {
		terminateTunnelProcess(termination)
	}
}

// stopTunnelRuntime terminates the running cloudflared process, if any.
// It deliberately does not touch persisted configuration so it can also be
// used by the API server shutdown path.
func (h *Handler) stopTunnelRuntime() uint64 {
	if h == nil {
		return 0
	}
	runtimeState := h.ensureTunnelRuntime()
	if runtimeState == nil {
		return 0
	}
	runtimeState.intentMu.Lock()
	generation, termination, detached := detachTunnelRuntime(runtimeState, 0)
	runtimeState.intentMu.Unlock()
	if detached {
		terminateTunnelProcess(termination)
	}
	return generation
}

// StopTunnelRuntime stops a running tunnel without changing the configured
// enabled flag. API server shutdown uses this to avoid orphaning cloudflared.
func (h *Handler) StopTunnelRuntime() {
	h.stopTunnelRuntime()
}

// StopTunnel stops the running cloudflared process and persists disabled state.
func (h *Handler) StopTunnel(c *gin.Context) {
	if h == nil {
		c.JSON(503, gin.H{"error": "configuration unavailable"})
		return
	}
	runtimeState := h.ensureTunnelRuntime()
	if runtimeState == nil {
		c.JSON(503, gin.H{"error": "configuration unavailable"})
		return
	}

	// Invalidate the runtime before persisting disabled intent. This ordering
	// prevents a concurrent start from writing enabled=true after this stop.
	runtimeState.intentMu.Lock()
	stopGeneration, termination, _ := detachTunnelRuntime(runtimeState, 0)
	cfg, errPersist := h.persistTunnelEnabled(false)
	shortID := normalizeTunnelShortID(cfg.ShortID)
	if shortID == "" {
		if state, errState := loadTunnelStateSafe(managedTunnelStatePath(h.configFilePath)); errState == nil {
			shortID = state.ShortID
		}
	}
	errClear := h.clearPersistedTunnelURL(shortID)
	runtimeState.intentMu.Unlock()
	terminateTunnelProcess(termination)
	if errPersist != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":  fmt.Sprintf("tunnel stopped but failed to persist disabled state: %v", errPersist),
			"status": h.tunnelStatus(cfg),
		})
		return
	}
	if errClear != nil && !errors.Is(errClear, errTunnelLifecycleStale) {
		runtimeState.setRelayErrorForGeneration(
			stopGeneration,
			fmt.Errorf("clear tunnel state: %w", errClear),
		)
	}
	c.JSON(200, h.tunnelStatus(cfg))
}

func (h *Handler) ensureTunnelRuntime() *tunnelRuntime {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tunnelRuntime == nil {
		h.tunnelRuntime = newTunnelRuntime()
	}
	return h.tunnelRuntime
}

func (h *Handler) tunnelPort() int {
	if h == nil {
		return 8317
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg != nil && h.cfg.Port > 0 {
		return h.cfg.Port
	}
	return 8317
}

func (h *Handler) tunnelStatus(cfg config.TunnelConfig) map[string]any {
	normalizeTunnelConfig(&cfg, h.tunnelPort())
	runtimeState := h.ensureTunnelRuntime()
	status := runtimeState.snapshot()
	if identity, errIdentity := h.currentTunnelIdentity(cfg); errIdentity == nil {
		if identity.ShortID != "" && status["short_id"] == "" {
			status["short_id"] = identity.ShortID
		}
		if identity.PublicURL != "" {
			status["stable_public_url"] = identity.PublicURL
			if status["public_url"] == "" {
				status["public_url"] = identity.PublicURL
			}
		}
		status["relay_url"] = identity.RelayURL
	}
	if state, errState := loadTunnelStateSafe(managedTunnelStatePath(h.configFilePath)); errState == nil {
		if status["short_id"] == "" {
			status["short_id"] = state.ShortID
		}
		if status["quick_tunnel_url"] == "" && state.TunnelURL != "" {
			status["quick_tunnel_url"] = state.TunnelURL
			status["tunnel_url"] = state.TunnelURL
		}
	}
	status["provider"] = cfg.Provider
	status["enabled"] = cfg.Enabled
	status["target_url"] = cfg.TargetURL
	status["cloudflared_path"] = cfg.CloudflaredPath
	status["relay_url"] = cfg.RelayURL
	status["publicUrl"] = status["public_url"]
	status["stablePublicUrl"] = status["stable_public_url"]
	status["quickTunnelUrl"] = status["quick_tunnel_url"]
	status["shortId"] = status["short_id"]
	status["relayUrl"] = status["relay_url"]
	status["relayRegistered"] = status["relay_registered"]
	status["relayError"] = status["relay_error"]
	if generation, ok := status["lifecycle_generation"]; ok {
		status["lifecycleGeneration"] = generation
	}
	if publicURL, ok := status["public_url"].(string); ok && publicURL != "" {
		status["public_api_url"] = strings.TrimRight(publicURL, "/") + "/v1"
	}
	if binary, err := resolveCloudflared(cfg.CloudflaredPath, h.configFilePath); err == nil {
		status["binary_found"] = true
		status["binary"] = binary
	} else {
		status["binary_found"] = false
		status["binary_error"] = err.Error()
	}
	return status
}

func normalizeTunnelConfig(cfg *config.TunnelConfig, port int) {
	if cfg == nil {
		return
	}
	if strings.TrimSpace(cfg.Provider) == "" {
		cfg.Provider = defaultTunnelProvider
	}
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.Provider == "cloudflared" || cfg.Provider == "cloudflare-quick" {
		cfg.Provider = defaultTunnelProvider
	}
	// Preserve an explicit direct/disabled sentinel. An empty relay URL means
	// "use the default relay", so collapsing "direct" to empty here would
	// accidentally re-enable the relay on the next normalization pass.
	if !isTunnelRelayDisabled(cfg.RelayURL) {
		cfg.RelayURL = normalizeTunnelRelayURL(cfg.RelayURL)
	} else {
		cfg.RelayURL = strings.ToLower(strings.TrimSpace(cfg.RelayURL))
	}
	cfg.ShortID = normalizeTunnelShortID(cfg.ShortID)
	if strings.TrimSpace(cfg.TargetURL) == "" {
		if port <= 0 {
			port = 8317
		}
		cfg.TargetURL = "http://127.0.0.1:" + strconv.Itoa(port)
	}
}

func resolveCloudflared(configured, configFilePath string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		info, err := os.Stat(configured)
		if err != nil {
			return "", fmt.Errorf("cloudflared not found at configured path")
		}
		if info.IsDir() {
			return "", errors.New("configured cloudflared path is a directory")
		}
		return filepath.Clean(configured), nil
	}
	if binary, err := exec.LookPath("cloudflared"); err == nil {
		return binary, nil
	}
	if managed := managedCloudflaredPath(configFilePath); managed != "" &&
		isValidCloudflaredBinary(managed) {
		return managed, nil
	}
	return "", errors.New("cloudflared executable was not found; it will be downloaded when the tunnel is enabled")
}

var cloudflaredDownloadMu sync.Mutex

func ensureCloudflared(ctx context.Context, configured, configFilePath string, runtimeState *tunnelRuntime) (string, error) {
	return ensureCloudflaredForGeneration(ctx, configured, configFilePath, runtimeState, 0)
}

func ensureCloudflaredForGeneration(
	ctx context.Context,
	configured,
	configFilePath string,
	runtimeState *tunnelRuntime,
	generation uint64,
) (string, error) {
	if binary, err := resolveCloudflared(configured, configFilePath); err == nil {
		return binary, nil
	} else if strings.TrimSpace(configured) != "" {
		return "", err
	}

	cloudflaredDownloadMu.Lock()
	defer cloudflaredDownloadMu.Unlock()

	if binary, err := resolveCloudflared(configured, configFilePath); err == nil {
		return binary, nil
	}

	asset, errAsset := cloudflaredAsset(runtime.GOOS, runtime.GOARCH)
	if errAsset != nil {
		return "", errAsset
	}
	destination := managedCloudflaredPath(configFilePath)
	if destination == "" {
		return "", errors.New("unable to determine a writable cloudflared directory")
	}
	if _, errStat := os.Stat(destination); errStat == nil && !isValidCloudflaredBinary(destination) {
		if errRemove := os.Remove(destination); errRemove != nil {
			return "", fmt.Errorf("remove invalid cloudflared binary: %w", errRemove)
		}
	}

	if runtimeState != nil {
		runtimeState.setDownloadStateForGeneration(generation, true, 0, nil)
	}
	downloadCtx, cancelDownload := context.WithTimeout(ctx, tunnelDownloadTimeout)
	defer cancelDownload()
	errDownload := downloadCloudflaredForGeneration(
		downloadCtx,
		cloudflaredDownloadBaseURL+"/"+asset,
		destination,
		runtimeState,
		generation,
	)
	if runtimeState != nil {
		if errDownload != nil {
			runtimeState.setDownloadStateForGeneration(generation, false, 0, errDownload)
		} else {
			runtimeState.setDownloadStateForGeneration(generation, false, 100, nil)
		}
	}
	if errDownload != nil {
		return "", fmt.Errorf("automatic cloudflared download failed: %w", errDownload)
	}
	return destination, nil
}

func managedCloudflaredPath(configFilePath string) string {
	base := strings.TrimSpace(configFilePath)
	if base != "" {
		base = filepath.Dir(base)
	} else if executable, errExecutable := os.Executable(); errExecutable == nil {
		base = filepath.Dir(executable)
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "runtime", "cloudflared", cloudflaredBinaryName())
}

func cloudflaredBinaryName() string {
	if runtime.GOOS == "windows" {
		return "cloudflared.exe"
	}
	return "cloudflared"
}

func cloudflaredAsset(goos, goarch string) (string, error) {
	switch goos {
	case "windows":
		switch goarch {
		case "amd64":
			return "cloudflared-windows-amd64.exe", nil
		case "386":
			return "cloudflared-windows-386.exe", nil
		case "arm64":
			return "cloudflared-windows-arm64.exe", nil
		}
	case "linux":
		switch goarch {
		case "amd64":
			return "cloudflared-linux-amd64", nil
		case "386":
			return "cloudflared-linux-386", nil
		case "arm64":
			return "cloudflared-linux-arm64", nil
		}
	case "darwin":
		return "", fmt.Errorf("automatic cloudflared install is not available for macOS; set tunnel.cloudflared-path")
	}
	return "", fmt.Errorf("automatic cloudflared install is not available for %s/%s; set tunnel.cloudflared-path", goos, goarch)
}

func isValidCloudflaredBinary(path string) bool {
	info, errStat := os.Stat(path)
	if errStat != nil || info.IsDir() || info.Size() < tunnelMinBinarySize {
		return false
	}
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	magic := make([]byte, 4)
	if _, errRead := io.ReadFull(file, magic); errRead != nil {
		return false
	}
	switch runtime.GOOS {
	case "windows":
		return magic[0] == 'M' && magic[1] == 'Z'
	case "darwin":
		return string(magic) == "\xcf\xfa\xed\xfe" ||
			string(magic) == "\xfe\xed\xfa\xcf" ||
			string(magic) == "\xca\xfe\xba\xbe" ||
			string(magic) == "\xbe\xba\xfe\xca"
	default:
		return string(magic) == "\x7fELF"
	}
}

func downloadCloudflared(ctx context.Context, sourceURL, destination string, runtimeState *tunnelRuntime) error {
	return downloadCloudflaredForGeneration(ctx, sourceURL, destination, runtimeState, 0)
}

func downloadCloudflaredForGeneration(
	ctx context.Context,
	sourceURL,
	destination string,
	runtimeState *tunnelRuntime,
	generation uint64,
) error {
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if errRequest != nil {
		return errRequest
	}
	request.Header.Set("User-Agent", "CLIProxyAPI cloudflared bootstrap")
	response, errDo := http.DefaultClient.Do(request)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("download returned HTTP %d", response.StatusCode)
	}

	directory := filepath.Dir(destination)
	if errMkdir := os.MkdirAll(directory, 0o755); errMkdir != nil {
		return errMkdir
	}
	tempFile, errCreate := os.CreateTemp(directory, ".cloudflared-download-*")
	if errCreate != nil {
		return errCreate
	}
	tempPath := tempFile.Name()
	keepTemp := false
	defer func() {
		_ = tempFile.Close()
		if !keepTemp {
			_ = os.Remove(tempPath)
		}
	}()

	buffer := make([]byte, tunnelDownloadChunk)
	var written int64
	total := response.ContentLength
	for {
		readCount, errRead := response.Body.Read(buffer)
		if readCount > 0 {
			writtenNow, errWrite := tempFile.Write(buffer[:readCount])
			if errWrite != nil {
				return errWrite
			}
			if writtenNow != readCount {
				return io.ErrShortWrite
			}
			written += int64(writtenNow)
			if runtimeState != nil && total > 0 {
				runtimeState.setDownloadStateForGeneration(
					generation,
					true,
					int((written*100)/total),
					nil,
				)
			}
		}
		if errRead != nil {
			if errors.Is(errRead, io.EOF) {
				break
			}
			return errRead
		}
	}
	if errClose := tempFile.Close(); errClose != nil {
		return errClose
	}
	if !isValidCloudflaredBinary(tempPath) {
		return errors.New("downloaded cloudflared binary failed validation")
	}
	if runtime.GOOS != "windows" {
		if errChmod := os.Chmod(tempPath, 0o755); errChmod != nil {
			return errChmod
		}
	}
	if errRename := os.Rename(tempPath, destination); errRename != nil {
		return errRename
	}
	keepTemp = true
	return nil
}

func validateTunnelTarget(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("target_url must be an http or https URL")
	}
	if parsed.User != nil {
		return "", errors.New("target_url must not include credentials")
	}
	host := strings.TrimSpace(parsed.Hostname())
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", errors.New("target_url must use localhost or a loopback IP address")
		}
	}
	return raw, nil
}

func (h *Handler) readTunnelOutput(
	reader interface{ Read([]byte) (int, error) },
	cmd *exec.Cmd,
	generation uint64,
) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		runtimeState := h.ensureTunnelRuntime()
		if tunnelURL := runtimeState.appendLogForGeneration(generation, cmd, scanner.Text()); tunnelURL != "" {
			go h.handleTunnelURL(cmd, generation, tunnelURL)
		}
	}
}

func (h *Handler) waitTunnelProcess(
	cmd *exec.Cmd,
	cancel context.CancelFunc,
	generation uint64,
	tempDir string,
) {
	errWait := cmd.Wait()
	runtime := h.ensureTunnelRuntime()
	unexpected := false
	shortID := ""
	quickURL := ""
	pid := 0
	runtime.mu.Lock()
	if runtime.cmd == cmd && runtime.lifecycleGeneration == generation {
		shortID = runtime.shortID
		quickURL = runtime.quickTunnelURL
		pid = runtime.externalPID
		runtime.lifecycleGeneration++
		runtime.cmd = nil
		runtime.cancel = nil
		runtime.externalPID = 0
		runtime.targetURL = ""
		runtime.starting = false
		runtime.downloadCancel = nil
		runtime.quickTunnelURL = ""
		runtime.registeredQuickURL = ""
		runtime.relayRegistered = false
		runtime.relayError = ""
		if runtime.stablePublicURL == "" {
			runtime.publicURL = ""
		} else {
			runtime.publicURL = runtime.stablePublicURL
		}
		runtime.stopping = false
		runtime.lastError = "cloudflared exited unexpectedly"
		if errWait != nil && !errors.Is(errWait, context.Canceled) {
			runtime.lastError = fmt.Sprintf("cloudflared exited unexpectedly: %v", errWait)
		}
		runtime.logs = append(runtime.logs, runtime.lastError)
		unexpected = true
	} else if runtime.cmd == nil && runtime.stopping {
		// A deliberate stop invalidates the generation before Wait returns.
		runtime.stopping = false
	}
	runtime.mu.Unlock()
	cleanupTunnelTempDir(tempDir)
	cancel()
	if unexpected {
		runtime.intentMu.Lock()
		_ = h.clearPersistedTunnelURLIfMatches(shortID, quickURL, pid)
		runtime.intentMu.Unlock()
		h.scheduleTunnelRespawn()
	}
}

// MarshalTunnelConfig is a small helper used by tests and plugins to validate
// that tunnel configuration never serializes executable runtime state.
func MarshalTunnelConfig(cfg config.TunnelConfig) ([]byte, error) {
	return json.Marshal(cfg)
}

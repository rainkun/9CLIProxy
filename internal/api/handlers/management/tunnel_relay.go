package management

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const (
	defaultTunnelRelayURL = "https://abc-tunnel.us"
	tunnelRelayTimeout    = 15 * time.Second
	tunnelShortIDLength   = 6
)

var (
	tunnelShortIDPattern    = regexp.MustCompile(`^[abcdefghijklmnpqrstuvwxyz23456789]{6}$`)
	tunnelRelayHTTPClient   = &http.Client{Timeout: tunnelRelayTimeout}
	tunnelStateMu           sync.Mutex
	errTunnelLifecycleStale = errors.New("tunnel lifecycle is no longer current")
)

type tunnelState struct {
	ShortID   string `json:"shortId"`
	TunnelURL string `json:"tunnelUrl,omitempty"`
	PID       int    `json:"pid,omitempty"`
	TargetURL string `json:"targetUrl,omitempty"`
}

type tunnelIdentity struct {
	ShortID   string
	RelayURL  string
	PublicURL string
}

func extractCloudflareQuickTunnelURL(line string) string {
	matches := cloudflareQuickTunnelURL.FindAllString(line, -1)
	for index := len(matches) - 1; index >= 0; index-- {
		candidate := strings.TrimRight(matches[index], ".,)")
		parsed, errParse := url.Parse(candidate)
		if errParse != nil || parsed.Hostname() == "" {
			continue
		}
		if strings.EqualFold(parsed.Hostname(), "api.trycloudflare.com") {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".trycloudflare.com") {
			continue
		}
		return candidate
	}
	return ""
}

func normalizeTunnelRelayURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if isTunnelRelayDisabled(raw) {
		return ""
	}
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("TUNNEL_WORKER_URL"))
	}
	if raw == "" {
		raw = defaultTunnelRelayURL
	}
	return strings.TrimRight(raw, "/")
}

func isTunnelRelayDisabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "direct", "disabled", "none", "off":
		return true
	default:
		return false
	}
}

func normalizeTunnelShortID(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if len(value) == tunnelShortIDLength+1 && strings.HasPrefix(value, "r") {
		value = value[1:]
	}
	return value
}

func validateTunnelRelayConfig(cfg config.TunnelConfig) error {
	if shortID := normalizeTunnelShortID(cfg.ShortID); shortID != "" && !tunnelShortIDPattern.MatchString(shortID) {
		return fmt.Errorf("short-id must use six characters from %q", "abcdefghijklmnpqrstuvwxyz23456789")
	}
	relayURL := normalizeTunnelRelayURL(cfg.RelayURL)
	if relayURL == "" {
		return nil
	}

	parsed, errParse := url.Parse(relayURL)
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("relay-url must be an absolute http or https URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("relay-url must use http or https")
	}
	if parsed.User != nil {
		return errors.New("relay-url must not include credentials")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("relay-url must not include a path, query, or fragment")
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" || strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil {
		return errors.New("relay-url must use a domain name with wildcard subdomains")
	}
	return nil
}

func buildTunnelPublicURL(relayURL, shortID string) (string, error) {
	relayURL = strings.TrimSpace(relayURL)
	if relayURL == "" {
		return "", nil
	}
	shortID = normalizeTunnelShortID(shortID)
	if !tunnelShortIDPattern.MatchString(shortID) {
		return "", errors.New("invalid tunnel short ID")
	}

	parsed, errParse := url.Parse(relayURL)
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid relay URL")
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" || strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil {
		return "", errors.New("relay URL must use a domain name with wildcard subdomains")
	}
	publicHost := "r" + shortID + "." + host
	if port := parsed.Port(); port != "" {
		publicHost += ":" + port
	}
	return parsed.Scheme + "://" + publicHost, nil
}

func generateTunnelShortID() (string, error) {
	randomBytes := make([]byte, tunnelShortIDLength)
	if _, errRead := rand.Read(randomBytes); errRead != nil {
		return "", fmt.Errorf("generate tunnel short ID: %w", errRead)
	}
	const chars = "abcdefghijklmnpqrstuvwxyz23456789"
	var builder strings.Builder
	builder.Grow(tunnelShortIDLength)
	for _, value := range randomBytes {
		builder.WriteByte(chars[int(value)%len(chars)])
	}
	return builder.String(), nil
}

func managedTunnelStatePath(configFilePath string) string {
	base := strings.TrimSpace(configFilePath)
	if base != "" {
		base = filepath.Dir(base)
	} else if executable, errExecutable := os.Executable(); errExecutable == nil {
		base = filepath.Dir(executable)
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "runtime", "tunnel", "state.json")
}

func loadTunnelState(path string) (tunnelState, error) {
	if strings.TrimSpace(path) == "" {
		return tunnelState{}, errors.New("unable to determine tunnel state path")
	}
	data, errRead := os.ReadFile(path)
	if errors.Is(errRead, os.ErrNotExist) {
		return tunnelState{}, nil
	}
	if errRead != nil {
		return tunnelState{}, fmt.Errorf("read tunnel state: %w", errRead)
	}
	var state tunnelState
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		return tunnelState{}, fmt.Errorf("parse tunnel state: %w", errUnmarshal)
	}
	state.ShortID = normalizeTunnelShortID(state.ShortID)
	return state, nil
}

func saveTunnelState(path string, state tunnelState) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("unable to determine tunnel state path")
	}
	state.ShortID = normalizeTunnelShortID(state.ShortID)
	if !tunnelShortIDPattern.MatchString(state.ShortID) {
		return errors.New("refusing to save an invalid tunnel short ID")
	}
	data, errMarshal := json.MarshalIndent(state, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("encode tunnel state: %w", errMarshal)
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o755); errMkdir != nil {
		return fmt.Errorf("create tunnel state directory: %w", errMkdir)
	}
	if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
		return fmt.Errorf("write tunnel state: %w", errWrite)
	}
	return nil
}

func loadTunnelStateSafe(path string) (tunnelState, error) {
	tunnelStateMu.Lock()
	defer tunnelStateMu.Unlock()
	return loadTunnelState(path)
}

func (r *tunnelRuntime) setTunnelIdentity(identity tunnelIdentity) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
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
}

func (r *tunnelRuntime) setRelayError(err error) {
	r.setRelayErrorForGeneration(0, err)
}

func (r *tunnelRuntime) setRelayErrorForGeneration(generation uint64, err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != 0 && r.lifecycleGeneration != generation {
		return
	}
	r.relayRegistered = false
	r.relayError = err.Error()
	r.lastError = err.Error()
}

func (r *tunnelRuntime) setRelayRegistration(publicURL string, err error) {
	r.setRelayRegistrationForGeneration(0, publicURL, err)
}

func (r *tunnelRuntime) setRelayRegistrationForGeneration(
	generation uint64,
	publicURL string,
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
	if err != nil {
		r.relayRegistered = false
		r.relayError = err.Error()
		r.lastError = err.Error()
		return
	}
	r.relayError = ""
	if r.relayURL == "" {
		r.relayRegistered = false
		if r.quickTunnelURL != "" {
			r.publicURL = r.quickTunnelURL
		}
		r.registeredQuickURL = r.quickTunnelURL
		return
	}
	r.relayRegistered = true
	r.registeredQuickURL = r.quickTunnelURL
	if publicURL != "" {
		r.stablePublicURL = publicURL
		r.publicURL = publicURL
	}
	if strings.HasPrefix(r.lastError, "tunnel relay") {
		r.lastError = ""
	}
}

func (h *Handler) currentTunnelIdentity(cfg config.TunnelConfig) (tunnelIdentity, error) {
	relayURL := ""
	if !isTunnelRelayDisabled(cfg.RelayURL) {
		relayURL = normalizeTunnelRelayURL(cfg.RelayURL)
	}
	shortID := normalizeTunnelShortID(cfg.ShortID)
	if shortID == "" {
		state, errState := loadTunnelStateSafe(managedTunnelStatePath(h.configFilePath))
		if errState == nil {
			shortID = state.ShortID
		}
	}
	if shortID == "" {
		return tunnelIdentity{RelayURL: relayURL}, nil
	}
	if !tunnelShortIDPattern.MatchString(shortID) {
		return tunnelIdentity{}, errors.New("invalid tunnel short ID")
	}
	publicURL, errPublicURL := buildTunnelPublicURL(relayURL, shortID)
	if errPublicURL != nil {
		return tunnelIdentity{}, errPublicURL
	}
	return tunnelIdentity{
		ShortID:   shortID,
		RelayURL:  relayURL,
		PublicURL: publicURL,
	}, nil
}

func (h *Handler) resolveTunnelIdentity(cfg config.TunnelConfig) (tunnelIdentity, error) {
	tunnelStateMu.Lock()
	defer tunnelStateMu.Unlock()

	cfg.RelayURL = normalizeTunnelRelayURL(cfg.RelayURL)
	cfg.ShortID = normalizeTunnelShortID(cfg.ShortID)
	if errValidate := validateTunnelRelayConfig(cfg); errValidate != nil {
		return tunnelIdentity{}, errValidate
	}

	statePath := managedTunnelStatePath(h.configFilePath)
	state, errState := loadTunnelState(statePath)
	if errState != nil {
		// A corrupt or stale state file must not prevent the user from creating a
		// new public tunnel. The new state will replace it below.
		state = tunnelState{}
	}
	if state.ShortID != "" && !tunnelShortIDPattern.MatchString(state.ShortID) {
		state = tunnelState{}
	}
	shortID := cfg.ShortID
	if shortID == "" {
		shortID = state.ShortID
	}
	if shortID == "" {
		var errGenerate error
		shortID, errGenerate = generateTunnelShortID()
		if errGenerate != nil {
			return tunnelIdentity{}, errGenerate
		}
	}
	if !tunnelShortIDPattern.MatchString(shortID) {
		return tunnelIdentity{}, errors.New("invalid tunnel short ID")
	}

	publicURL, errPublicURL := buildTunnelPublicURL(cfg.RelayURL, shortID)
	if errPublicURL != nil {
		return tunnelIdentity{}, errPublicURL
	}
	if state.ShortID != shortID {
		state.ShortID = shortID
		if errSave := saveTunnelState(statePath, state); errSave != nil {
			return tunnelIdentity{}, errSave
		}
	}
	return tunnelIdentity{
		ShortID:   shortID,
		RelayURL:  cfg.RelayURL,
		PublicURL: publicURL,
	}, nil
}

func (h *Handler) persistTunnelURL(shortID, tunnelURL string) error {
	return h.persistTunnelURLWithProcess(shortID, tunnelURL, 0, "")
}

func (h *Handler) persistTunnelURLWithProcess(
	shortID,
	tunnelURL string,
	pid int,
	targetURL string,
) error {
	tunnelStateMu.Lock()
	defer tunnelStateMu.Unlock()

	statePath := managedTunnelStatePath(h.configFilePath)
	state, errState := loadTunnelState(statePath)
	if errState != nil {
		state = tunnelState{}
	}
	state.ShortID = shortID
	state.TunnelURL = strings.TrimSpace(tunnelURL)
	state.PID = pid
	state.TargetURL = strings.TrimSpace(targetURL)
	return saveTunnelState(statePath, state)
}

func (h *Handler) persistTunnelURLForGeneration(
	runtimeState *tunnelRuntime,
	generation uint64,
	shortID,
	tunnelURL string,
) error {
	if h == nil || runtimeState == nil || generation == 0 {
		return errTunnelLifecycleStale
	}
	// Serialize state writes with stop/start intent transitions, but do not
	// hold the runtime mutex while performing filesystem I/O.
	runtimeState.intentMu.Lock()
	defer runtimeState.intentMu.Unlock()

	runtimeState.mu.Lock()
	if runtimeState.lifecycleGeneration != generation ||
		runtimeState.cmd == nil ||
		runtimeState.cmd.Process == nil ||
		runtimeState.quickTunnelURL != tunnelURL {
		runtimeState.mu.Unlock()
		return errTunnelLifecycleStale
	}
	pid := runtimeState.externalPID
	targetURL := runtimeState.targetURL
	runtimeState.mu.Unlock()

	if errPersist := h.persistTunnelURLWithProcess(shortID, tunnelURL, pid, targetURL); errPersist != nil {
		return errPersist
	}

	runtimeState.mu.Lock()
	stillCurrent := runtimeState.lifecycleGeneration == generation &&
		runtimeState.cmd != nil &&
		runtimeState.cmd.Process != nil &&
		runtimeState.quickTunnelURL == tunnelURL
	runtimeState.mu.Unlock()
	if !stillCurrent {
		return errTunnelLifecycleStale
	}
	return nil
}

func (h *Handler) clearPersistedTunnelURL(shortID string) error {
	return h.clearPersistedTunnelURLIfMatches(shortID, "", 0)
}

// clearPersistedTunnelURLIfMatches removes the ephemeral Quick Tunnel state
// only when it still belongs to the supplied tunnel. This prevents an older
// cloudflared wait goroutine from clearing a freshly registered URL after a
// restart or respawn.
func (h *Handler) clearPersistedTunnelURLIfMatches(
	shortID,
	tunnelURL string,
	pid int,
) error {
	tunnelStateMu.Lock()
	defer tunnelStateMu.Unlock()

	statePath := managedTunnelStatePath(h.configFilePath)
	state, errState := loadTunnelState(statePath)
	if errState != nil {
		state = tunnelState{}
	}
	if shortID = normalizeTunnelShortID(shortID); shortID != "" {
		if state.ShortID != "" && normalizeTunnelShortID(state.ShortID) != shortID {
			return nil
		}
		state.ShortID = shortID
	}
	if state.ShortID == "" {
		return nil
	}
	if tunnelURL != "" && strings.TrimSpace(state.TunnelURL) != strings.TrimSpace(tunnelURL) {
		return nil
	}
	if pid > 0 && state.PID > 0 && state.PID != pid {
		return nil
	}
	state.TunnelURL = ""
	state.PID = 0
	state.TargetURL = ""
	return saveTunnelState(statePath, state)
}

func (h *Handler) clearPersistedTunnelURLForGeneration(
	runtimeState *tunnelRuntime,
	generation uint64,
	shortID string,
) error {
	if h == nil || runtimeState == nil || generation == 0 {
		return errTunnelLifecycleStale
	}
	runtimeState.intentMu.Lock()
	defer runtimeState.intentMu.Unlock()

	runtimeState.mu.Lock()
	if runtimeState.lifecycleGeneration != generation ||
		runtimeState.cmd != nil ||
		runtimeState.starting {
		runtimeState.mu.Unlock()
		return errTunnelLifecycleStale
	}
	runtimeState.mu.Unlock()
	return h.clearPersistedTunnelURL(shortID)
}

func registerTunnelURL(ctx context.Context, relayURL, shortID, tunnelURL string) error {
	relayURL = strings.TrimRight(strings.TrimSpace(relayURL), "/")
	if relayURL == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tunnelRelayTimeout)
		defer cancel()
	}
	shortID = normalizeTunnelShortID(shortID)
	if !tunnelShortIDPattern.MatchString(shortID) {
		return errors.New("invalid tunnel short ID")
	}
	if strings.TrimSpace(tunnelURL) == "" {
		return errors.New("tunnel URL is empty")
	}
	payload, errMarshal := json.Marshal(struct {
		ShortID   string `json:"shortId"`
		TunnelURL string `json:"tunnelUrl"`
	}{
		ShortID:   shortID,
		TunnelURL: tunnelURL,
	})
	if errMarshal != nil {
		return fmt.Errorf("encode tunnel relay registration: %w", errMarshal)
	}
	request, errRequest := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		relayURL+"/api/tunnel/register",
		bytes.NewReader(payload),
	)
	if errRequest != nil {
		return fmt.Errorf("create tunnel relay registration request: %w", errRequest)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "CLIProxyAPI tunnel relay")
	response, errDo := tunnelRelayHTTPClient.Do(request)
	if errDo != nil {
		return fmt.Errorf("register tunnel relay: %w", errDo)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4*1024))
	message := strings.TrimSpace(string(body))
	if message == "" {
		return fmt.Errorf("tunnel relay registration returned HTTP %d", response.StatusCode)
	}
	return fmt.Errorf("tunnel relay registration returned HTTP %d: %s", response.StatusCode, message)
}

func (h *Handler) handleTunnelURL(cmd *exec.Cmd, generation uint64, tunnelURL string) {
	tunnelURL = strings.TrimSpace(tunnelURL)
	if h == nil || cmd == nil || generation == 0 || tunnelURL == "" {
		return
	}
	runtimeState := h.ensureTunnelRuntime()
	if runtimeState == nil {
		return
	}
	runtimeState.mu.Lock()
	if runtimeState.lifecycleGeneration != generation ||
		runtimeState.cmd != cmd ||
		runtimeState.cmd.Process == nil ||
		runtimeState.quickTunnelURL != tunnelURL {
		runtimeState.mu.Unlock()
		return
	}
	identity := tunnelIdentity{
		ShortID:   runtimeState.shortID,
		RelayURL:  runtimeState.relayURL,
		PublicURL: runtimeState.stablePublicURL,
	}
	runtimeState.mu.Unlock()
	if identity.ShortID == "" {
		return
	}

	runtimeState.relayMu.Lock()
	defer runtimeState.relayMu.Unlock()

	runtimeState.mu.Lock()
	if runtimeState.lifecycleGeneration != generation ||
		runtimeState.cmd != cmd ||
		runtimeState.cmd.Process == nil ||
		runtimeState.quickTunnelURL != tunnelURL {
		runtimeState.mu.Unlock()
		return
	}
	runtimeState.mu.Unlock()

	if errPersist := h.persistTunnelURLForGeneration(
		runtimeState,
		generation,
		identity.ShortID,
		tunnelURL,
	); errPersist != nil {
		if errors.Is(errPersist, errTunnelLifecycleStale) {
			return
		}
		runtimeState.setRelayErrorForGeneration(
			generation,
			fmt.Errorf("persist tunnel state: %w", errPersist),
		)
		return
	}
	if identity.RelayURL == "" {
		runtimeState.setRelayRegistrationForGeneration(generation, tunnelURL, nil)
		return
	}

	errRegister := registerTunnelURL(context.Background(), identity.RelayURL, identity.ShortID, tunnelURL)
	runtimeState.setRelayRegistrationForGeneration(generation, identity.PublicURL, errRegister)
}

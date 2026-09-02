package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/context"
)

type comboExecutionContextKey struct{}

type comboRotation struct {
	mu    sync.Mutex
	state map[string]comboRotationState
}

type comboRotationState struct {
	index int
	used  int
}

var globalComboRotation = comboRotation{state: make(map[string]comboRotationState)}

func comboExecutionContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, comboExecutionContextKey{}, true)
}

func comboExecutionActive(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	active, _ := ctx.Value(comboExecutionContextKey{}).(bool)
	return active
}

func (h *BaseAPIHandler) comboForModel(model string) (config.ComboConfig, bool) {
	if h == nil || h.Cfg == nil {
		return config.ComboConfig{}, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return config.ComboConfig{}, false
	}
	for _, combo := range h.Cfg.Combos {
		if combo.Disabled || !strings.EqualFold(strings.TrimSpace(combo.Name), model) {
			continue
		}
		if len(combo.Models) == 0 {
			continue
		}
		return combo, true
	}
	return config.ComboConfig{}, false
}

func comboMemberOrder(combo config.ComboConfig) []string {
	models := append([]string(nil), combo.Models...)
	if len(models) <= 1 || combo.Strategy != config.ComboStrategyRoundRobin {
		return models
	}
	key := strings.ToLower(strings.TrimSpace(combo.Name))
	globalComboRotation.mu.Lock()
	state := globalComboRotation.state[key]
	index := state.index % len(models)
	limit := combo.StickyRoundRobinLimit
	if limit < 1 {
		limit = 1
	}
	state.used++
	if state.used >= limit {
		state.index = (index + 1) % len(models)
		state.used = 0
	}
	globalComboRotation.state[key] = state
	globalComboRotation.mu.Unlock()
	return append(models[index:], models[:index]...)
}

func comboFallbackEligible(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil {
		return false
	}
	if errMsg.StatusCode == http.StatusBadRequest && errMsg.Error != nil {
		message := strings.ToLower(errMsg.Error.Error())
		return strings.Contains(message, "model_not_found") || strings.Contains(message, "unknown provider")
	}
	switch errMsg.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusNotFound, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	default:
		return errMsg.StatusCode >= http.StatusInternalServerError || errMsg.StatusCode == 0
	}
}

func rewriteComboRequestModel(raw []byte, model string) []byte {
	if len(raw) == 0 || strings.TrimSpace(model) == "" || !gjson.GetBytes(raw, "model").Exists() {
		return raw
	}
	updated, err := sjson.SetBytes(raw, "model", strings.TrimSpace(model))
	if err != nil {
		return raw
	}
	return updated
}

func rewriteComboResponseModel(raw []byte, model string) []byte {
	if len(raw) == 0 || strings.TrimSpace(model) == "" {
		return raw
	}
	for _, path := range []string{"model", "modelVersion", "response.model", "response.modelVersion", "message.model"} {
		if gjson.GetBytes(raw, path).Exists() {
			if updated, err := sjson.SetBytes(raw, path, model); err == nil {
				raw = updated
			}
		}
	}
	return raw
}

func rewriteComboStreamChunk(raw []byte, model string) []byte {
	if len(raw) == 0 {
		return raw
	}
	lines := bytes.Split(raw, []byte("\n"))
	for i, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
			if len(payload) > 0 && payload[0] == '{' && json.Valid(payload) {
				rewritten := rewriteComboResponseModel(payload, model)
				lines[i] = append([]byte("data: "), rewritten...)
			}
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

func (h *BaseAPIHandler) executeComboStream(ctx context.Context, entryProtocol, exitProtocol string, combo config.ComboConfig, rawJSON []byte, alt string, allowImageModel bool, execOptions modelExecutionOptions) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	var lastErr *interfaces.ErrorMessage
	for _, member := range comboMemberOrder(combo) {
		attemptCtx, cancel := context.WithCancel(comboExecutionContext(ctx))
		data, headers, errs := h.executeStreamWithAuthManagerFormats(attemptCtx, entryProtocol, exitProtocol, member, rewriteComboRequestModel(rawJSON, member), alt, allowImageModel, execOptions)
		first, firstErr, dataOpen, errOpen := readComboStreamStart(attemptCtx, data, errs)
		if firstErr != nil {
			cancel()
			lastErr = firstErr
			if comboFallbackEligible(firstErr) {
				continue
			}
			return comboErrorStream(firstErr)
		}
		return forwardComboStream(attemptCtx, cancel, data, errs, first, dataOpen, errOpen, combo.Name, headers)
	}
	if lastErr != nil {
		return comboErrorStream(lastErr)
	}
	return comboErrorStream(&interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable})
}

func readComboStreamStart(ctx context.Context, data <-chan []byte, errs <-chan *interfaces.ErrorMessage) ([]byte, *interfaces.ErrorMessage, bool, bool) {
	var first []byte
	for data != nil || errs != nil {
		select {
		case <-ctx.Done():
			return nil, &interfaces.ErrorMessage{StatusCode: http.StatusRequestTimeout, Error: ctx.Err()}, false, false
		case chunk, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			first = chunk
			return first, nil, true, errs != nil
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg != nil {
				return nil, errMsg, data != nil, true
			}
		}
	}
	return first, nil, false, false
}

func forwardComboStream(ctx context.Context, cancel context.CancelFunc, data <-chan []byte, errs <-chan *interfaces.ErrorMessage, first []byte, dataOpen, errOpen bool, comboName string, headers http.Header) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	outData := make(chan []byte)
	outErr := make(chan *interfaces.ErrorMessage, 1)
	if !dataOpen {
		data = nil
	}
	if !errOpen {
		errs = nil
	}
	go func() {
		defer close(outData)
		defer close(outErr)
		defer cancel()
		sendData := func(payload []byte) bool {
			select {
			case <-ctx.Done():
				return false
			case outData <- rewriteComboStreamChunk(payload, comboName):
				return true
			}
		}
		if first != nil && !sendData(first) {
			return
		}
		for dataOpen || errOpen {
			select {
			case <-ctx.Done():
				return
			case payload, ok := <-data:
				if !ok {
					dataOpen = false
					continue
				}
				if !sendData(payload) {
					return
				}
			case errMsg, ok := <-errs:
				if !ok {
					errOpen = false
					continue
				}
				if errMsg != nil {
					select {
					case <-ctx.Done():
					case outErr <- errMsg:
					}
					return
				}
			}
		}
	}()
	return outData, headers, outErr
}

func comboErrorStream(errMsg *interfaces.ErrorMessage) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- errMsg
	close(errs)
	close(data)
	return data, nil, errs
}

// ComboCatalogModels returns protocol-shaped model metadata for enabled combos.
func (h *BaseAPIHandler) ComboCatalogModels(format string) []map[string]any {
	if h == nil || h.Cfg == nil || len(h.Cfg.Combos) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(h.Cfg.Combos))
	for _, combo := range h.Cfg.Combos {
		name := strings.TrimSpace(combo.Name)
		if combo.Disabled || name == "" || len(combo.Models) == 0 {
			continue
		}
		displayName := strings.TrimSpace(combo.DisplayName)
		if displayName == "" {
			displayName = name
		}
		if format == "gemini" {
			out = append(out, map[string]any{
				"name":                       name,
				"displayName":                displayName,
				"description":                "CLIProxyAPI model combo",
				"supportedGenerationMethods": []string{"generateContent"},
			})
			continue
		}
		out = append(out, map[string]any{
			"id":           name,
			"object":       "model",
			"created":      int64(0),
			"owned_by":     "combo",
			"display_name": displayName,
		})
	}
	return out
}

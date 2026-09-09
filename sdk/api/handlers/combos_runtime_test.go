package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func newComboHarness(t *testing.T, combo config.ComboConfig, executor *modelExecutionCaptureExecutor) *BaseAPIHandler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: t.Name(), Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	var models []*registry.ModelInfo
	for _, member := range append(append([]string(nil), combo.Models...), combo.JudgeModel) {
		if member != "" {
			model, _ := comboMemberExecution(member, modelExecutionOptions{})
			models = append(models, &registry.ModelInfo{ID: model})
		}
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, models)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	return NewBaseAPIHandlers(&sdkconfig.SDKConfig{Combos: []config.ComboConfig{combo}}, manager)
}

func comboTextResponse(model, text string) coreexecutor.Response {
	body, _ := json.Marshal(map[string]any{
		"model": model, "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": text}}},
	})
	return coreexecutor.Response{Payload: body}
}

func TestComboFallbackExecutesInOrder(t *testing.T) {
	combo := config.ComboConfig{Name: t.Name(), Models: []string{"fallback-bad", "fallback-good"}, Strategy: config.ComboStrategyFallback}
	var calls []string
	executor := &modelExecutionCaptureExecutor{execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
		calls = append(calls, req.Model)
		if req.Model == "fallback-bad" {
			return coreexecutor.Response{}, modelExecutionStatusHeaderError{statusCode: 503, message: "unavailable"}
		}
		return comboTextResponse(req.Model, "ok"), nil
	}}
	h := newComboHarness(t, combo, executor)
	body, _, err := h.ExecuteWithAuthManager(context.Background(), "openai", combo.Name, []byte(`{"model":"combo","messages":[{"role":"user","content":"Hi"}]}`), "")
	if err != nil || gjson.GetBytes(body, "model").String() != combo.Name {
		t.Fatalf("response=%s error=%+v", body, err)
	}
	if strings.Join(calls, ",") != strings.Join(combo.Models, ",") {
		t.Fatalf("calls=%v", calls)
	}
}

func TestComboRoundRobinConcurrentAndSticky(t *testing.T) {
	for _, sticky := range []int{1, 3} {
		combo := config.ComboConfig{Name: fmt.Sprintf("%s-%d", t.Name(), sticky), Models: []string{"a", "b", "c"}, Strategy: config.ComboStrategyRoundRobin, StickyRoundRobinLimit: sticky}
		counts := make(map[string]int)
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < 90; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				first := comboMemberOrder(combo)[0]
				mu.Lock()
				counts[first]++
				mu.Unlock()
			}()
		}
		wg.Wait()
		for _, model := range combo.Models {
			if counts[model] != 30 {
				t.Fatalf("sticky=%d counts=%v", sticky, counts)
			}
		}
	}
}

func TestComboCountDoesNotAdvanceRotation(t *testing.T) {
	combo := config.ComboConfig{Name: t.Name(), Models: []string{"count-first", "count-second"}, Strategy: config.ComboStrategyRoundRobin}
	h := newComboHarness(t, combo, &modelExecutionCaptureExecutor{})
	_, _, err := h.ExecuteCountWithAuthManager(context.Background(), "openai", combo.Name, []byte(`{"model":"combo"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := comboMemberOrder(combo)[0]; got != combo.Models[0] {
		t.Fatalf("count advanced rotation to %s", got)
	}
}

func TestFusionRunsPanelInParallelThenJudge(t *testing.T) {
	combo := config.ComboConfig{Name: t.Name(), Models: []string{"fusion-a", "fusion-b"}, Strategy: config.ComboStrategyFusion, JudgeModel: "fusion-judge"}
	started := make(chan string, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	executor := &modelExecutionCaptureExecutor{execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
		calls.Add(1)
		if opts.Stream || gjson.GetBytes(req.Payload, "stream").Bool() {
			return coreexecutor.Response{}, fmt.Errorf("panel must not stream")
		}
		if req.Model == combo.JudgeModel {
			if calls.Load() != 3 || !strings.Contains(string(req.Payload), "answer-fusion-a") || !strings.Contains(string(req.Payload), "answer-fusion-b") {
				return coreexecutor.Response{}, fmt.Errorf("judge called before both answers")
			}
			if gjson.GetBytes(req.Payload, "messages.0.content").String() != "Keep the user format" {
				return coreexecutor.Response{}, fmt.Errorf("lost system instruction")
			}
			return comboTextResponse(req.Model, "synthesized"), nil
		}
		started <- req.Model
		select {
		case <-release:
			return comboTextResponse(req.Model, "answer-"+req.Model), nil
		case <-ctx.Done():
			return coreexecutor.Response{}, ctx.Err()
		}
	}}
	h := newComboHarness(t, combo, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx = context.WithValue(ctx, "gin", ginCtx)
	done := make(chan struct{})
	var body []byte
	var headers http.Header
	var errMsg *interfaces.ErrorMessage
	raw := []byte(`{"model":"combo","messages":[{"role":"system","content":"Keep the user format"},{"role":"user","content":"Question"}]}`)
	original := string(raw)
	go func() {
		defer close(done)
		body, headers, errMsg = h.ExecuteWithAuthManager(ctx, "openai", combo.Name, raw, "")
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("panel did not run concurrently")
		}
	}
	close(release)
	<-done
	if errMsg != nil || gjson.GetBytes(body, "choices.0.message.content").String() != "synthesized" {
		t.Fatalf("body=%s err=%+v", body, errMsg)
	}
	if calls.Load() != 3 || headers.Get("X-CLIProxy-Fusion-Calls") != "3" || gjson.GetBytes(body, "model").String() != combo.Name {
		t.Fatalf("calls=%d headers=%v body=%s", calls.Load(), headers, body)
	}
	if string(raw) != original {
		t.Fatal("request was mutated")
	}
}

func TestComboProviderSelectionExecutesCorrectProvider(t *testing.T) {
	combo := config.ComboConfig{Name: t.Name(), Models: []string{"choice-a::same-model", "choice-b::same-model"}, Strategy: config.ComboStrategyRoundRobin}
	manager := coreauth.NewManager(nil, nil, nil)
	var calls []string
	for _, provider := range []string{"choice-a", "choice-b"} {
		executor := &modelExecutionCaptureExecutor{provider: provider, execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
			calls = append(calls, auth.Provider)
			if req.Model != "same-model" || gjson.GetBytes(req.Payload, "model").String() != "same-model" {
				return coreexecutor.Response{}, fmt.Errorf("provider qualification leaked into upstream model")
			}
			return comboTextResponse(req.Model, auth.Provider), nil
		}}
		manager.RegisterExecutor(executor)
		auth := &coreauth.Auth{ID: t.Name() + provider, Provider: provider, Status: coreauth.StatusActive}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: "same-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{Combos: []config.ComboConfig{combo}}, manager)
	for i := 0; i < 4; i++ {
		_, _, err := h.ExecuteWithAuthManager(context.Background(), "openai", combo.Name, []byte(`{"model":"combo","messages":[{"role":"user","content":"Hi"}]}`), "")
		if err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(calls, ",") != "choice-a,choice-b,choice-a,choice-b" {
		t.Fatalf("providers=%v", calls)
	}
}

func TestComboBadRequestStopsWithoutFallback(t *testing.T) {
	combo := config.ComboConfig{Name: t.Name(), Models: []string{"bad-request-a", "bad-request-b"}, Strategy: config.ComboStrategyFallback}
	calls := 0
	executor := &modelExecutionCaptureExecutor{execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
		calls++
		return coreexecutor.Response{}, modelExecutionStatusHeaderError{statusCode: 400, message: "invalid request payload"}
	}}
	h := newComboHarness(t, combo, executor)
	_, _, err := h.ExecuteWithAuthManager(context.Background(), "openai", combo.Name, []byte(`{"model":"combo","messages":[]}`), "")
	if err == nil || err.StatusCode != 400 || calls != 1 {
		t.Fatalf("calls=%d err=%+v", calls, err)
	}
}

func TestFusionJudgeFailureNeverReturnsPanelAnswer(t *testing.T) {
	combo := config.ComboConfig{Name: t.Name(), Models: []string{"judge-failure-panel"}, Strategy: config.ComboStrategyFusion, JudgeModel: "judge-failure-judge"}
	executor := &modelExecutionCaptureExecutor{execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
		if req.Model == combo.JudgeModel {
			return coreexecutor.Response{}, modelExecutionStatusHeaderError{statusCode: 503, message: "judge unavailable"}
		}
		return comboTextResponse(req.Model, "panel-only"), nil
	}}
	h := newComboHarness(t, combo, executor)
	body, _, err := h.ExecuteWithAuthManager(context.Background(), "openai", combo.Name, []byte(`{"model":"combo","messages":[{"role":"user","content":"Hi"}]}`), "")
	if err == nil || len(body) > 0 {
		t.Fatalf("body=%s err=%+v", body, err)
	}
}

func TestFusionPartialFailuresAndThreshold(t *testing.T) {
	for _, minimum := range []int{1, 2} {
		t.Run(fmt.Sprint(minimum), func(t *testing.T) {
			combo := config.ComboConfig{Name: t.Name(), Models: []string{"partial-bad", "partial-good"}, Strategy: config.ComboStrategyFusion, JudgeModel: "partial-judge", MinSuccessfulModels: minimum}
			var judgeCalls atomic.Int32
			executor := &modelExecutionCaptureExecutor{execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
				if req.Model == "partial-bad" {
					return coreexecutor.Response{}, modelExecutionStatusHeaderError{statusCode: 503, message: "failed"}
				}
				if req.Model == combo.JudgeModel {
					judgeCalls.Add(1)
				}
				return comboTextResponse(req.Model, "answer"), nil
			}}
			h := newComboHarness(t, combo, executor)
			_, headers, err := h.ExecuteWithAuthManager(context.Background(), "openai", combo.Name, []byte(`{"messages":[{"role":"user","content":"Hi"}]}`), "")
			if minimum == 1 && (err != nil || judgeCalls.Load() != 1 || headers.Get("X-CLIProxy-Fusion-Panel-Successes") != "1") {
				t.Fatalf("expected partial synthesis, calls=%d err=%+v", judgeCalls.Load(), err)
			}
			if minimum == 2 && (err == nil || err.StatusCode != 502 || judgeCalls.Load() != 0) {
				t.Fatalf("threshold not enforced, calls=%d err=%+v", judgeCalls.Load(), err)
			}
		})
	}
}

func TestFusionCancellationStopsPanelAndSkipsJudge(t *testing.T) {
	combo := config.ComboConfig{Name: t.Name(), Models: []string{"cancel-a", "cancel-b"}, Strategy: config.ComboStrategyFusion, JudgeModel: "cancel-judge"}
	started := make(chan struct{}, 2)
	stopped := make(chan struct{}, 2)
	executor := &modelExecutionCaptureExecutor{execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
		if req.Model == combo.JudgeModel {
			return coreexecutor.Response{}, fmt.Errorf("judge must not run")
		}
		started <- struct{}{}
		<-ctx.Done()
		stopped <- struct{}{}
		return coreexecutor.Response{}, ctx.Err()
	}}
	h := newComboHarness(t, combo, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan *interfaces.ErrorMessage, 1)
	go func() {
		_, _, err := h.ExecuteWithAuthManager(ctx, "openai", combo.Name, []byte(`{"messages":[{"role":"user","content":"Hi"}]}`), "")
		done <- err
	}()
	<-started
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled request succeeded")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Fatal("panel did not stop")
		}
	}
}

func TestFusionJudgeRequestAndAnswerProtocols(t *testing.T) {
	tests := []struct{ protocol, raw, response, path string }{
		{"openai", `{"messages":[{"role":"user","content":"question"}]}`, `{"choices":[{"message":{"content":"answer"}}]}`, "messages.1.content"},
		{"openai-response", `{"input":"question","instructions":"keep"}`, `{"output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`, "input.1.content"},
		{"claude", `{"messages":[{"role":"user","content":"question"}],"system":"keep"}`, `{"content":[{"type":"thinking","thinking":"private"},{"type":"text","text":"answer"}]}`, "messages.1.content"},
		{"gemini", `{"contents":[{"role":"user","parts":[{"text":"question"}]}]}`, `{"candidates":[{"content":{"parts":[{"thought":true,"text":"private"},{"text":"answer"}]}}]}`, "contents.1.parts.0.text"},
	}
	for _, tt := range tests {
		t.Run(tt.protocol, func(t *testing.T) {
			if err := validateFusionRequest(tt.protocol, []byte(tt.raw), false); err != nil {
				t.Fatal(err)
			}
			if answer := fusionAnswer([]byte(tt.response), tt.protocol); answer != "answer" {
				t.Fatalf("answer=%q", answer)
			}
			body, err := fusionJudgeRequest([]byte(tt.raw), tt.protocol, config.ComboConfig{JudgeModel: "judge"}, []fusionCandidate{{Model: "a", Answer: "candidate"}})
			if err != nil || !strings.Contains(gjson.GetBytes(body, tt.path).String(), "candidate") {
				t.Fatalf("body=%s err=%v", body, err)
			}
		})
	}
}

func TestFusionRejectsUnsupportedBeforeExecution(t *testing.T) {
	for _, raw := range []string{
		`{"messages":[]}`,
		`{"messages":[{"role":"user","content":"Hi"}],"tools":[{"type":"function"}]}`,
		`{"messages":[{"role":"user","content":"Hi"}],"n":2}`,
		`{"messages":[{"role":"user","content":"Hi"}],"previous_response_id":"resp-1"}`,
	} {
		if err := validateFusionRequest("openai", []byte(raw), false); err == nil {
			t.Fatalf("accepted unsupported request %s", raw)
		}
	}
}

func TestComboExplicitProviderAndSlashModel(t *testing.T) {
	model, options := comboMemberExecution("custom::org/model", modelExecutionOptions{})
	if model != "org/model" || options.ForcedProvider != "custom" {
		t.Fatalf("model=%s options=%+v", model, options)
	}
	model, options = comboMemberExecution("org/model", modelExecutionOptions{})
	if model != "org/model" || options.ForcedProvider != "" {
		t.Fatal("bare slash model was misinterpreted as a provider")
	}
}

func TestComboStreamClosedChannelsAndRawJSON(t *testing.T) {
	data := make(chan []byte, 1)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte(`{"model":"member","choices":[]}`)
	close(data)
	close(errs)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, _, failures := forwardComboStream(ctx, cancel, data, errs, nil, true, true, "combo", nil)
	if body := <-out; gjson.GetBytes(body, "model").String() != "combo" {
		t.Fatalf("model not rewritten: %s", body)
	}
	select {
	case _, open := <-out:
		if open {
			t.Fatal("unexpected extra chunk")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close")
	}
	if err := <-failures; err != nil {
		t.Fatal(err)
	}
}

func TestFusionStreamsOnlyJudge(t *testing.T) {
	combo := config.ComboConfig{Name: t.Name(), Models: []string{"stream-panel"}, Strategy: config.ComboStrategyFusion, JudgeModel: "stream-judge"}
	var panelCalls atomic.Int32
	var judgeCalls atomic.Int32
	executor := &modelExecutionCaptureExecutor{
		execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
			panelCalls.Add(1)
			return comboTextResponse(req.Model, "private panel answer"), nil
		},
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			judgeCalls.Add(1)
			if req.Model != combo.JudgeModel || !strings.Contains(string(req.Payload), "private panel answer") {
				return nil, fmt.Errorf("invalid judge request")
			}
			chunks := make(chan coreexecutor.StreamChunk, 2)
			chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {\"model\":\"stream-judge\",\"choices\":[{\"delta\":{\"content\":\"final\"}}]}\n\n")}
			chunks <- coreexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
			close(chunks)
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	h := newComboHarness(t, combo, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	data, headers, errs := h.ExecuteStreamWithAuthManager(ctx, "openai", combo.Name, []byte(`{"model":"combo","stream":true,"messages":[{"role":"user","content":"Hi"}]}`), "")
	var body strings.Builder
	for chunk := range data {
		body.Write(chunk)
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if panelCalls.Load() != 1 || judgeCalls.Load() != 1 || headers.Get("X-CLIProxy-Fusion-Calls") != "2" {
		t.Fatalf("panel=%d judge=%d headers=%v", panelCalls.Load(), judgeCalls.Load(), headers)
	}
	if strings.Contains(body.String(), "private panel answer") || !strings.Contains(body.String(), combo.Name) || !strings.Contains(body.String(), "final") {
		t.Fatalf("stream=%s", body.String())
	}
}

func TestComboEmptyStreamAndCancellationEligibility(t *testing.T) {
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage)
	close(data)
	close(errs)
	_, err, _, _ := readComboStreamStart(context.Background(), data, errs)
	if err == nil || !comboFallbackEligible(err) {
		t.Fatal("empty stream must fail over")
	}
	if comboFallbackEligible(&interfaces.ErrorMessage{StatusCode: 408, Error: context.Canceled}) {
		t.Fatal("cancelled caller must not trigger more calls")
	}
}

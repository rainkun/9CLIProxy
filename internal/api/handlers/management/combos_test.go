package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestComboModelsGroupSameModelByActualProvider(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	for _, provider := range []string{"combo-provider-a", "combo-provider-b"} {
		clientID := t.Name() + provider
		modelRegistry.RegisterClient(clientID, provider, []*registry.ModelInfo{{ID: "combo-shared-model", DisplayName: "Shared model"}})
		t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h := NewHandler(&config.Config{}, "", nil)
	h.GetComboModels(c)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	body := w.Body.String()
	for _, provider := range []string{"combo-provider-a", "combo-provider-b"} {
		if !strings.Contains(body, provider+"::combo-shared-model") {
			t.Fatalf("missing provider-specific model: %s", body)
		}
	}
}

func TestComboInvalidUpdateDoesNotReplaceSavedConfig(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{Combos: []config.ComboConfig{{Name: "saved", Models: []string{"a"}, Strategy: "fallback"}}}}
	h := NewHandler(cfg, filepath.Join(t.TempDir(), "config.yaml"), nil)
	for _, method := range []string{http.MethodPatch, http.MethodPut} {
		payload := `{"name":"quality","models":["a"],"strategy":"fusion"}`
		if method == http.MethodPut {
			payload = "[" + payload + "]"
		}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, "/v0/management/combos", strings.NewReader(payload))
		c.Request.Header.Set("Content-Type", "application/json")
		if method == http.MethodPut {
			h.PutCombos(c)
		} else {
			h.PatchCombo(c)
		}
		if w.Code != http.StatusBadRequest || len(cfg.Combos) != 1 || cfg.Combos[0].Name != "saved" {
			t.Fatalf("method=%s status=%d cfg=%+v", method, w.Code, cfg.Combos)
		}
	}
}

func TestComboFusionSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{}, path, nil)
	payload := `{"name":"quality","models":["codex::a","claude::b"],"strategy":"fusion","judge-model":"codex::judge","judge-prompt":"Check facts","min-successful-models":2}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/combos", strings.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PatchCombo(c)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(cfg.Combos)
	if len(cfg.Combos) != 1 || cfg.Combos[0].Strategy != "fusion" || cfg.Combos[0].JudgeModel != "codex::judge" || cfg.Combos[0].MinSuccessfulModels != 2 {
		t.Fatalf("reloaded=%s", body)
	}
}

func TestComboInvalidReplacementBodyCannotClearCombos(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"combos":null}`, `[{"name":"","models":[]}]`, `{"combos":[{"name":"c","models":["a"]},{"name":"c","models":["b"]}]}`} {
		cfg := &config.Config{SDKConfig: config.SDKConfig{Combos: []config.ComboConfig{{Name: "saved", Models: []string{"a"}}}}}
		h := NewHandler(cfg, filepath.Join(t.TempDir(), "config.yaml"), nil)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/combos", strings.NewReader(body))
		h.PutCombos(c)
		if w.Code != http.StatusBadRequest || len(h.cfg.Combos) != 1 || h.cfg.Combos[0].Name != "saved" {
			t.Fatalf("body=%s response=%d config=%+v", body, w.Code, h.cfg.Combos)
		}
	}
}

func TestComboSaveFailureLeavesOriginalSnapshot(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{Combos: []config.ComboConfig{{Name: "saved", Models: []string{"a"}}}}}
	h := NewHandler(cfg, t.TempDir(), nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, "/combos", strings.NewReader(`{"name":"new","models":["b"]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PatchCombo(c)
	if w.Code != http.StatusInternalServerError || h.cfg != cfg || len(cfg.Combos) != 1 {
		t.Fatalf("status=%d config=%+v", w.Code, h.cfg.Combos)
	}
}

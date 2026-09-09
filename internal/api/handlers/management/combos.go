package management

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

// GetComboModels exposes registered routable models grouped by actual provider.
// No upstream credentials or configuration secrets are returned.
func (h *Handler) GetComboModels(c *gin.Context) {
	type modelOption struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	type providerOption struct {
		ID     string        `json:"id"`
		Name   string        `json:"name"`
		Models []modelOption `json:"models"`
	}
	labels := make(map[string]string)
	h.mu.Lock()
	if h.cfg != nil {
		for _, provider := range h.cfg.OpenAICompatibility {
			labels[util.OpenAICompatibleProviderKey(provider.Name)] = provider.Name
		}
	}
	h.mu.Unlock()
	modelRegistry := registry.GetGlobalRegistry()
	groups := make(map[string][]modelOption)
	for _, model := range modelRegistry.GetAvailableModels("openai") {
		id, _ := model["id"].(string)
		if id == "" {
			continue
		}
		name, _ := model["display_name"].(string)
		if name == "" {
			name = id
		}
		for _, provider := range modelRegistry.GetModelProviders(id) {
			groups[provider] = append(groups[provider], modelOption{
				ID: id, Name: name, Value: provider + "::" + id,
			})
		}
	}
	providers := make([]providerOption, 0, len(groups))
	for provider, models := range groups {
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
		label := labels[provider]
		if label == "" {
			label = provider
		}
		providers = append(providers, providerOption{ID: provider, Name: label, Models: models})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	c.JSON(http.StatusOK, gin.H{"providers": providers})
}

// GetCombos returns the configured named model combos.
func (h *Handler) GetCombos(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{"combos": h.cfg.Combos})
}

// PutCombos replaces all model combos.
func (h *Handler) PutCombos(c *gin.Context) {
	data, errRead := c.GetRawData()
	if errRead != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	var combos []config.ComboConfig
	if errUnmarshal := json.Unmarshal(data, &combos); errUnmarshal != nil {
		var wrapper struct {
			Combos []config.ComboConfig `json:"combos"`
			Items  []config.ComboConfig `json:"items"`
		}
		if errWrapper := json.Unmarshal(data, &wrapper); errWrapper != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		combos = wrapper.Combos
		if combos == nil {
			combos = wrapper.Items
		}
	}
	if combos == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected a combos array; use [] to clear"})
		return
	}
	temp := &config.Config{}
	temp.Combos = combos
	temp.SanitizeCombos()
	if len(temp.Combos) != len(combos) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "combo names must be unique and each combo needs at least one model"})
		return
	}
	if errValidate := config.ValidateCombos(temp.Combos); errValidate != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.persistCombosLocked(c, temp.Combos)
}

// PatchCombo creates or replaces one combo by name.
func (h *Handler) PatchCombo(c *gin.Context) {
	var combo config.ComboConfig
	if errBind := c.ShouldBindJSON(&combo); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	temp := &config.Config{}
	temp.Combos = []config.ComboConfig{combo}
	temp.SanitizeCombos()
	if len(temp.Combos) != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "combo requires a name and at least one model"})
		return
	}
	combo = temp.Combos[0]
	h.mu.Lock()
	defer h.mu.Unlock()
	next := append([]config.ComboConfig(nil), h.cfg.Combos...)
	replaced := false
	for i := range next {
		if strings.EqualFold(strings.TrimSpace(next[i].Name), combo.Name) {
			next[i] = combo
			replaced = true
			break
		}
	}
	if !replaced {
		next = append(next, combo)
	}
	if errValidate := config.ValidateCombos(next); errValidate != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
		return
	}
	h.persistCombosLocked(c, next)
}

// DeleteCombo deletes one combo selected by the name query parameter.
func (h *Handler) DeleteCombo(c *gin.Context) {
	name := strings.TrimSpace(c.Query("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing name"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.cfg.Combos {
		if !strings.EqualFold(strings.TrimSpace(h.cfg.Combos[i].Name), name) {
			continue
		}
		next := append([]config.ComboConfig(nil), h.cfg.Combos[:i]...)
		next = append(next, h.cfg.Combos[i+1:]...)
		h.persistCombosLocked(c, next)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "combo not found"})
}

// Use a replacement snapshot so in-flight requests retain their original combo
// definitions. A failed disk write must not change the active configuration.
func (h *Handler) persistCombosLocked(c *gin.Context, combos []config.ComboConfig) {
	previous := h.cfg
	next := *previous
	next.Combos = combos
	h.cfg = &next
	if !h.persistLocked(c) {
		h.cfg = previous
	}
}

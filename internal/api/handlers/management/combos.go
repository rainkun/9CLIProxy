package management

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// GetCombos returns the configured named model combos.
func (h *Handler) GetCombos(c *gin.Context) {
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
	h.cfg.Combos = combos
	h.cfg.SanitizeCombos()
	h.persist(c)
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
	replaced := false
	for i := range h.cfg.Combos {
		if strings.EqualFold(strings.TrimSpace(h.cfg.Combos[i].Name), combo.Name) {
			h.cfg.Combos[i] = combo
			replaced = true
			break
		}
	}
	if !replaced {
		h.cfg.Combos = append(h.cfg.Combos, combo)
	}
	h.cfg.SanitizeCombos()
	h.persist(c)
}

// DeleteCombo deletes one combo selected by the name query parameter.
func (h *Handler) DeleteCombo(c *gin.Context) {
	name := strings.TrimSpace(c.Query("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing name"})
		return
	}
	for i := range h.cfg.Combos {
		if !strings.EqualFold(strings.TrimSpace(h.cfg.Combos[i].Name), name) {
			continue
		}
		h.cfg.Combos = append(h.cfg.Combos[:i], h.cfg.Combos[i+1:]...)
		h.persist(c)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "combo not found"})
}

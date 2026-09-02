package config

import "strings"

// SanitizeCombos trims combo definitions, removes duplicates, and applies
// conservative defaults so malformed entries cannot shadow real model names.
func (cfg *Config) SanitizeCombos() {
	if cfg == nil || len(cfg.Combos) == 0 {
		return
	}
	out := make([]ComboConfig, 0, len(cfg.Combos))
	seen := make(map[string]struct{}, len(cfg.Combos))
	for _, combo := range cfg.Combos {
		combo.Name = strings.TrimSpace(combo.Name)
		if combo.Name == "" {
			continue
		}
		nameKey := strings.ToLower(combo.Name)
		if _, exists := seen[nameKey]; exists {
			continue
		}
		seen[nameKey] = struct{}{}
		models := make([]string, 0, len(combo.Models))
		modelSeen := make(map[string]struct{}, len(combo.Models))
		for _, rawModel := range combo.Models {
			model := strings.TrimSpace(rawModel)
			if model == "" {
				continue
			}
			key := strings.ToLower(model)
			if _, exists := modelSeen[key]; exists {
				continue
			}
			modelSeen[key] = struct{}{}
			models = append(models, model)
		}
		if len(models) == 0 {
			continue
		}
		combo.Models = models
		combo.Strategy = strings.ToLower(strings.TrimSpace(combo.Strategy))
		if combo.Strategy != ComboStrategyRoundRobin {
			combo.Strategy = ComboStrategyFallback
		}
		if combo.StickyRoundRobinLimit < 1 {
			combo.StickyRoundRobinLimit = 1
		}
		combo.DisplayName = strings.TrimSpace(combo.DisplayName)
		out = append(out, combo)
	}
	cfg.Combos = out
}

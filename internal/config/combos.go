package config

import (
	"fmt"
	"strings"
)

// MaxFusionModels bounds per-request fan-out and synthesis context size.
const MaxFusionModels = 16

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
		if combo.Strategy == "" {
			combo.Strategy = ComboStrategyFallback
		}
		combo.JudgeModel = strings.TrimSpace(combo.JudgeModel)
		combo.JudgePrompt = strings.TrimSpace(combo.JudgePrompt)
		if combo.StickyRoundRobinLimit < 1 {
			combo.StickyRoundRobinLimit = 1
		}
		combo.DisplayName = strings.TrimSpace(combo.DisplayName)
		out = append(out, combo)
	}
	cfg.Combos = out
}

// ValidateCombos rejects unsupported strategies and nested combos before execution.
// Nested groups would hide additional fan-out and make the N+1 cost misleading.
func ValidateCombos(combos []ComboConfig) error {
	names := make(map[string]bool, len(combos))
	for _, combo := range combos {
		key := strings.ToLower(strings.TrimSpace(combo.Name))
		if key == "" || names[key] {
			return fmt.Errorf("combo names must be non-empty and unique: %q", combo.Name)
		}
		names[key] = true
	}
	for _, combo := range combos {
		if len(combo.Models) == 0 {
			return fmt.Errorf("combo %q requires at least one model", combo.Name)
		}
		for _, model := range combo.Models {
			if strings.TrimSpace(model) == "" || names[strings.ToLower(strings.TrimSpace(model))] {
				return fmt.Errorf("combo %q requires concrete member models, not empty or nested combo names", combo.Name)
			}
		}
		switch strings.ToLower(strings.TrimSpace(combo.Strategy)) {
		case "", ComboStrategyFallback, ComboStrategyRoundRobin:
		case ComboStrategyFusion:
			if combo.JudgeModel == "" || names[strings.ToLower(combo.JudgeModel)] {
				return fmt.Errorf("fusion combo %q requires a concrete judge-model", combo.Name)
			}
			if len(combo.Models) > MaxFusionModels {
				return fmt.Errorf("fusion combo %q exceeds the limit of %d panel models", combo.Name, MaxFusionModels)
			}
			if combo.MinSuccessfulModels < 0 || combo.MinSuccessfulModels > len(combo.Models) {
				return fmt.Errorf("fusion combo %q has invalid min-successful-models", combo.Name)
			}
		default:
			return fmt.Errorf("combo %q has unsupported strategy %q", combo.Name, combo.Strategy)
		}
	}
	return nil
}

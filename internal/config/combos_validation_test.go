package config

import (
	"strings"
	"testing"
)

func TestFusionConfigurationValidation(t *testing.T) {
	tests := []struct {
		name   string
		combos []ComboConfig
		valid  bool
	}{
		{"valid", []ComboConfig{{Name: "c", Models: []string{"a", "b"}, Strategy: ComboStrategyFusion, JudgeModel: "j"}}, true},
		{"missing judge", []ComboConfig{{Name: "c", Models: []string{"a"}, Strategy: ComboStrategyFusion}}, false},
		{"unknown strategy", []ComboConfig{{Name: "c", Models: []string{"a"}, Strategy: "random-typo"}}, false},
		{"self reference", []ComboConfig{{Name: "c", Models: []string{"C"}}}, false},
		{"nested judge", []ComboConfig{{Name: "c", Models: []string{"a"}, Strategy: ComboStrategyFusion, JudgeModel: "C"}}, false},
		{"threshold", []ComboConfig{{Name: "c", Models: []string{"a"}, Strategy: ComboStrategyFusion, JudgeModel: "j", MinSuccessfulModels: 2}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateCombos(tt.combos); (err == nil) != tt.valid {
				t.Fatalf("valid=%v err=%v", tt.valid, err)
			}
		})
	}
}

func TestFusionYAMLRoundTripAndInvalidConfig(t *testing.T) {
	raw := `combos:
  - name: quality
    strategy: FUSION
    models: [a, b]
    judge-model: " j "
    judge-prompt: Check correctness
    min-successful-models: 2
`
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Combos) != 1 || cfg.Combos[0].Strategy != ComboStrategyFusion || cfg.Combos[0].JudgeModel != "j" || cfg.Combos[0].MinSuccessfulModels != 2 {
		t.Fatalf("%+v", cfg.Combos)
	}
	if _, err = ParseConfigBytes([]byte(strings.ReplaceAll(raw, "FUSION", "typo"))); err == nil {
		t.Fatal("unknown strategy silently accepted")
	}
}

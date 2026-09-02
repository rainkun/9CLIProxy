package handlers

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestComboCatalogModels(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{Combos: []sdkconfig.ComboConfig{
		{Name: "coding-combo", Models: []string{"gpt-5.4"}, DisplayName: "Coding Combo"},
		{Name: "disabled", Models: []string{"gpt-5.4"}, Disabled: true},
	}}, nil)
	openAI := handler.ComboCatalogModels("openai")
	if len(openAI) != 1 || openAI[0]["id"] != "coding-combo" || openAI[0]["owned_by"] != "combo" {
		t.Fatalf("unexpected OpenAI catalog: %#v", openAI)
	}
	gemini := handler.ComboCatalogModels("gemini")
	if len(gemini) != 1 || gemini[0]["name"] != "coding-combo" {
		t.Fatalf("unexpected Gemini catalog: %#v", gemini)
	}
}

func TestComboFallbackEligible(t *testing.T) {
	if !comboFallbackEligible(&interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable}) {
		t.Fatal("expected 503 to be fallback eligible")
	}
	if comboFallbackEligible(&interfaces.ErrorMessage{StatusCode: http.StatusBadRequest}) {
		t.Fatal("plain 400 must not be fallback eligible")
	}
}

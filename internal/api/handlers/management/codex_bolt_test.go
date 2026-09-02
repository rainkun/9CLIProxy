package management

import (
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestParseCodexResetSummary(t *testing.T) {
	t.Parallel()

	summary := parseCodexResetSummary([]byte(`{
		"available_count": 2,
		"applicable_available_count": 1,
		"credits": [
			{"status":"redeemed","expires_at":"2026-08-01T00:00:00Z"},
			{"status":"available","expires_at":"2026-09-01T00:00:00Z"}
		]
	}`))
	if summary.availableCount == nil || *summary.availableCount != 2 {
		t.Fatalf("available_count = %#v, want 2", summary.availableCount)
	}
	if summary.applicableAvailableCount == nil || *summary.applicableAvailableCount != 1 {
		t.Fatalf("applicable_available_count = %#v, want 1", summary.applicableAvailableCount)
	}
	if summary.resetAt != "2026-09-01T00:00:00Z" {
		t.Fatalf("reset_at = %q, want available credit expiry", summary.resetAt)
	}
}

func TestParseCodexConsumeResponse(t *testing.T) {
	t.Parallel()

	noCredit := parseCodexConsumeResponse([]byte(`{"code":"no_credit","message":"No credits available"}`))
	if !noCredit.noCredit || noCredit.code != "no_credit" {
		t.Fatalf("no-credit response = %#v", noCredit)
	}

	reset := parseCodexConsumeResponse([]byte(`{"code":"reset","windows_reset":2}`))
	if reset.noCredit || reset.code != "reset" || reset.windowsReset != 2 {
		t.Fatalf("reset response = %#v", reset)
	}
}

func TestCodexAccountIDReadsCanonicalAndNestedMetadata(t *testing.T) {
	t.Parallel()

	auth := &coreauth.Auth{
		Metadata: map[string]any{
			"provider_specific_data": map[string]any{"chatgptAccountId": "nested"},
		},
	}
	if got := codexAccountID(auth); got != "nested" {
		t.Fatalf("nested-only account id = %q, want nested", got)
	}

	auth.Metadata["account_id"] = "canonical"
	if got := codexAccountID(auth); got != "canonical" {
		t.Fatalf("account id = %q, want canonical", got)
	}
}

func TestCodexBoltRequestAliasesAreTrimmedBySharedSelector(t *testing.T) {
	t.Parallel()

	req := codexBoltRequest{
		AuthIndex: "  auth-index  ",
		Name:      " account.json ",
	}
	indices := uniqueAuthFileNames([]string{req.AuthIndex, req.AuthID})
	names := uniqueAuthFileNames([]string{req.Name})
	if len(indices) != 1 || strings.TrimSpace(indices[0]) != "auth-index" {
		t.Fatalf("indices = %#v", indices)
	}
	if len(names) != 1 || names[0] != "account.json" {
		t.Fatalf("names = %#v", names)
	}
}

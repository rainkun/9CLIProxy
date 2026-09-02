// Package importers provides pure-data converters that transform
// third-party credential exports into the JSON shape consumed by
// CLIProxyAPI's auth file synthesizer.
package importers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
)

// ErrChatGPTSessionMissingTokens is returned by ConvertSession when the
// supplied payload does not contain any usable access or refresh token.
var ErrChatGPTSessionMissingTokens = errors.New("chatgpt session is missing access_token and refresh_token")

// ChatGPTSession models the JSON shape exported by 9router and similar
// tools as a raw ChatGPT account session. Only the fields actually used
// by the converter are declared; unknown fields are silently ignored.
type ChatGPTSession struct {
	Phone            string `json:"phone"`
	Email            string `json:"email"`
	Password         string `json:"password"`
	ChatgptAccountID string `json:"chatgpt_account_id"`
	// AccountIDAlias is an optional fallback when chatgpt_account_id is
	// absent; some exporters use the bare field name instead.
	AccountIDAlias string `json:"account_id,omitempty"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	IDToken        string `json:"id_token"`
	OAIDid         string `json:"oai_did"`
	PlusOneMFree   bool   `json:"plus_1m_free"`
	Promo          []any  `json:"promo"`
	UA             string `json:"ua"`
	LastRefresh    string `json:"last_refresh,omitempty"`
	Expire         string `json:"expired,omitempty"`
	Type           string `json:"type,omitempty"`
}

// ConvertResult holds the data the handler should write to disk. Auth is
// the codex-shaped top-level JSON object; Meta is a nested object that
// preserves the supplementary session fields so the operator can still
// see the original account information in the management UI.
type ConvertResult struct {
	Auth map[string]any
	Meta map[string]any
	// PlanType is extracted from the id_token (if present) so the caller
	// can build a deterministic filename that matches the OAuth flow.
	PlanType string
	// HashAccountID is the 8-character hex hash of the resolved account
	// id, matching codex.CredentialFileName expectations. Empty when no
	// account id could be resolved.
	HashAccountID string
	// Email is the resolved email (explicit field or JWT claim) used for
	// both the auth record and the filename.
	Email string
	// AccountID is the resolved chatgpt account id (with user- prefix
	// preserved) used for the auth record and filename hash.
	AccountID string
}

// ConvertSession converts a raw ChatGPT session payload into a codex
// auth JSON document. now is the reference time used to seed
// last_refresh/expired when the payload does not provide them. The
// returned auth map is suitable for json.Marshal.
func ConvertSession(in *ChatGPTSession, now time.Time) (*ConvertResult, error) {
	if in == nil {
		return nil, ErrChatGPTSessionMissingTokens
	}

	accessToken := strings.TrimSpace(in.AccessToken)
	refreshToken := strings.TrimSpace(in.RefreshToken)
	if accessToken == "" && refreshToken == "" {
		return nil, ErrChatGPTSessionMissingTokens
	}

	idToken := strings.TrimSpace(in.IDToken)
	planType := ""
	var claims *codexauth.JWTClaims
	if idToken != "" {
		parsed, errParse := codexauth.ParseJWTToken(idToken)
		if errParse == nil && parsed != nil {
			claims = parsed
			planType = strings.TrimSpace(parsed.CodexAuthInfo.ChatgptPlanType)
		}
	}

	email := strings.TrimSpace(in.Email)
	if email == "" && claims != nil {
		email = strings.TrimSpace(claims.Email)
	}

	accountID := strings.TrimSpace(in.ChatgptAccountID)
	if accountID == "" {
		accountID = strings.TrimSpace(in.AccountIDAlias)
	}
	if accountID == "" && claims != nil {
		if fromClaim := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); fromClaim != "" {
			accountID = fromClaim
		} else if sub := strings.TrimSpace(claims.Sub); sub != "" {
			accountID = sub
		}
	}

	lastRefresh := strings.TrimSpace(in.LastRefresh)
	if lastRefresh == "" {
		lastRefresh = now.UTC().Format(time.RFC3339)
	}
	expired := strings.TrimSpace(in.Expire)
	if expired == "" {
		expired = now.UTC().Add(time.Hour).Format(time.RFC3339)
	}

	hashAccountID := ""
	if accountID != "" {
		digest := sha256.Sum256([]byte(accountID))
		hashAccountID = hex.EncodeToString(digest[:])[:8]
	}

	auth := map[string]any{
		"type":          "codex",
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"id_token":      idToken,
		"email":         email,
		"account_id":    accountID,
		"last_refresh":  lastRefresh,
		"expired":       expired,
	}

	meta := map[string]any{
		"label":              defaultLabel(email, in),
		"chatgpt_account_id": strings.TrimSpace(in.ChatgptAccountID),
		"note":               defaultNote(),
		"password":           strings.TrimSpace(in.Password),
		"phone":              strings.TrimSpace(in.Phone),
		"session": map[string]any{
			"oai_did":      strings.TrimSpace(in.OAIDid),
			"plus_1m_free": in.PlusOneMFree,
			"promo":        clonePromo(in.Promo),
			"ua":           strings.TrimSpace(in.UA),
		},
	}
	auth["meta"] = meta

	return &ConvertResult{
		Auth:          auth,
		Meta:          meta,
		PlanType:      planType,
		HashAccountID: hashAccountID,
		Email:         email,
		AccountID:     accountID,
	}, nil
}

func defaultLabel(email string, in *ChatGPTSession) string {
	if email != "" {
		return email
	}
	if in != nil {
		if phone := strings.TrimSpace(in.Phone); phone != "" {
			return phone
		}
	}
	return "Imported ChatGPT session"
}

func defaultNote() string {
	return "Imported from ChatGPT session"
}

// clonePromo returns a deep-ish copy of the promo slice so the handler
// can mutate the outer map without affecting the caller's data.
func clonePromo(promo []any) []any {
	if len(promo) == 0 {
		return []any{}
	}
	out := make([]any, len(promo))
	copy(out, promo)
	return out
}

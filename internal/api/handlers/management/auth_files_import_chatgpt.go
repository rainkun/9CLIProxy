package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/importers"
)

// ImportChatGPTSession accepts one or more raw ChatGPT session JSON files
// and converts each into a codex auth file. The endpoint always produces
// a codex-shaped credential; if the payload is missing the tokens
// required to refresh credentials, the file is not created and the
// response records a per-file failure with the reason.
func (h *Handler) ImportChatGPTSession(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}

	files, errMultipart := h.multipartAuthFileHeaders(c)
	if errMultipart != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid multipart form: %v", errMultipart)})
		return
	}

	combined := chatGPTSessionImportResult{
		Imported: make([]importedAuthSummary, 0),
		Skipped:  make([]gin.H, 0),
		Failed:   make([]gin.H, 0),
	}

	now := time.Now()

	if len(files) > 0 {
		for _, file := range files {
			if file == nil {
				continue
			}
			name := filepath.Base(strings.TrimSpace(file.Filename))
			if !strings.HasSuffix(strings.ToLower(name), ".json") {
				combined.Failed = append(combined.Failed, gin.H{"name": name, "error": "file must be .json"})
				continue
			}
			src, errOpen := file.Open()
			if errOpen != nil {
				combined.Failed = append(combined.Failed, gin.H{"name": name, "error": errOpen.Error()})
				continue
			}
			data, errRead := io.ReadAll(io.LimitReader(src, maxImportBody+1))
			_ = src.Close()
			if errRead != nil {
				combined.Failed = append(combined.Failed, gin.H{"name": name, "error": errRead.Error()})
				continue
			}
			if len(data) > maxImportBody {
				combined.Failed = append(combined.Failed, gin.H{"name": name, "error": "file is too large"})
				continue
			}
			h.importSingleChatGPTSession(c.Request.Context(), name, data, now, &combined)
		}
	} else {
		data, errRead := io.ReadAll(io.LimitReader(c.Request.Body, maxImportBody+1))
		if errRead != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
			return
		}
		if len(data) > maxImportBody {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file is too large"})
			return
		}
		h.importSingleChatGPTSession(c.Request.Context(), "chatgpt-session.json", data, now, &combined)
	}

	status := "ok"
	code := http.StatusOK
	if len(combined.Failed) > 0 {
		status = "partial"
		code = http.StatusMultiStatus
	}
	c.JSON(code, gin.H{
		"status":   status,
		"imported": combined.Imported,
		"skipped":  combined.Skipped,
		"failed":   combined.Failed,
		"counts": gin.H{
			"imported": len(combined.Imported),
			"skipped":  len(combined.Skipped),
			"failed":   len(combined.Failed),
		},
	})
}

type chatGPTSessionImportResult struct {
	Imported []importedAuthSummary `json:"imported"`
	Skipped  []gin.H               `json:"skipped"`
	Failed   []gin.H               `json:"failed"`
}

func (h *Handler) importSingleChatGPTSession(ctx context.Context, sourceName string, data []byte, now time.Time, result *chatGPTSessionImportResult) {
	if result == nil {
		return
	}

	var session importers.ChatGPTSession
	if errUnmarshal := json.Unmarshal(data, &session); errUnmarshal != nil {
		result.Failed = append(result.Failed, gin.H{"name": sourceName, "error": fmt.Sprintf("invalid chatgpt session json: %v", errUnmarshal)})
		return
	}

	converted, errConvert := importers.ConvertSession(&session, now)
	if errConvert != nil {
		result.Failed = append(result.Failed, gin.H{"name": sourceName, "error": errConvert.Error()})
		return
	}

	fileName := codexauth.CredentialFileName(converted.Email, converted.PlanType, converted.HashAccountID, true)
	raw, errMarshal := json.Marshal(converted.Auth)
	if errMarshal != nil {
		result.Failed = append(result.Failed, gin.H{"name": sourceName, "error": errMarshal.Error()})
		return
	}
	if errWrite := h.writeAuthFile(ctx, fileName, raw); errWrite != nil {
		result.Failed = append(result.Failed, gin.H{"name": sourceName, "error": errWrite.Error()})
		return
	}

	summary := importedAuthSummary{
		Name:     fileName,
		Provider: "codex",
		Email:    converted.Email,
	}
	if auth := h.authByFileName(fileName); auth != nil {
		auth.EnsureIndex()
		summary.AuthIndex = auth.Index
	}
	result.Imported = append(result.Imported, summary)
}

package management

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const (
	maxCodexRefreshTokens       = 200
	codexRefreshTokenWorkers    = 8
	codexRefreshTokenTimeout    = 30 * time.Second
	maxRefreshConversionErrSize = 512
)

type codexRefreshService interface {
	RefreshTokensWithClientID(ctx context.Context, refreshToken, clientID string) (*codex.CodexTokenData, error)
}

var newCodexRefreshService = func(cfg *config.Config) codexRefreshService {
	return codex.NewCodexAuth(cfg)
}

type codexRefreshTokenConvertRequest struct {
	RefreshTokens []string `json:"refresh_tokens"`
	ClientID      string   `json:"client_id"`
}

type codexRefreshTokenConvertedFile struct {
	Index   int            `json:"index"`
	Name    string         `json:"name"`
	Content map[string]any `json:"content"`
}

type codexRefreshTokenConvertFailure struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}

type codexRefreshTokenConvertResult struct {
	file    *codexRefreshTokenConvertedFile
	failure *codexRefreshTokenConvertFailure
}

// ConvertCodexRefreshTokens exchanges refresh tokens for CPA-compatible Codex credentials.
func (h *Handler) ConvertCodexRefreshTokens(c *gin.Context) {
	var request codexRefreshTokenConvertRequest
	if errBind := c.ShouldBindJSON(&request); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON request"})
		return
	}

	clientID := strings.TrimSpace(request.ClientID)
	if clientID == "" {
		clientID = codex.ClientID
	}
	refreshTokens := normalizeRefreshTokens(request.RefreshTokens)
	if len(refreshTokens) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one refresh token is required"})
		return
	}
	if len(refreshTokens) > maxCodexRefreshTokens {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("refresh token count exceeds %d", maxCodexRefreshTokens)})
		return
	}

	h.mu.Lock()
	var cfg *config.Config
	if h.cfg != nil {
		cfg = h.cfg.CloneForRuntime()
	}
	h.mu.Unlock()
	service := newCodexRefreshService(cfg)

	results := make([]codexRefreshTokenConvertResult, len(refreshTokens))
	jobs := make(chan int)
	workerCount := min(codexRefreshTokenWorkers, len(refreshTokens))
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				results[index] = convertCodexRefreshToken(c.Request.Context(), service, refreshTokens[index], clientID, index)
			}
		}()
	}
	for index := range refreshTokens {
		jobs <- index
	}
	close(jobs)
	workers.Wait()

	files := make([]codexRefreshTokenConvertedFile, 0, len(results))
	failed := make([]codexRefreshTokenConvertFailure, 0)
	for _, result := range results {
		if result.file != nil {
			files = append(files, *result.file)
		}
		if result.failure != nil {
			failed = append(failed, *result.failure)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"total":        len(refreshTokens),
		"converted":    len(files),
		"failed_count": len(failed),
		"files":        files,
		"failed":       failed,
	})
}

func normalizeRefreshTokens(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func convertCodexRefreshToken(parent context.Context, service codexRefreshService, refreshToken, clientID string, index int) codexRefreshTokenConvertResult {
	ctx, cancel := context.WithTimeout(parent, codexRefreshTokenTimeout)
	defer cancel()

	tokenData, errRefresh := service.RefreshTokensWithClientID(ctx, refreshToken, clientID)
	if errRefresh != nil {
		return refreshTokenFailure(index, refreshToken, errRefresh)
	}
	if tokenData == nil || strings.TrimSpace(tokenData.AccessToken) == "" {
		return refreshTokenFailure(index, refreshToken, fmt.Errorf("upstream response missing access_token"))
	}
	if strings.TrimSpace(tokenData.RefreshToken) == "" {
		return refreshTokenFailure(index, refreshToken, fmt.Errorf("upstream response missing refresh_token"))
	}

	now := time.Now().UTC().Format(time.RFC3339)
	content := map[string]any{
		"type":          "codex",
		"access_token":  tokenData.AccessToken,
		"refresh_token": tokenData.RefreshToken,
		"last_refresh":  now,
		"client_id":     clientID,
	}
	setNonEmptyString(content, "id_token", tokenData.IDToken)
	setNonEmptyString(content, "account_id", tokenData.AccountID)
	setNonEmptyString(content, "chatgpt_account_id", tokenData.AccountID)
	setNonEmptyString(content, "email", tokenData.Email)
	setNonEmptyString(content, "expired", tokenData.Expire)

	if claims, errClaims := codex.ParseJWTToken(tokenData.IDToken); errClaims == nil && claims != nil {
		setNonEmptyString(content, "chatgpt_user_id", claims.CodexAuthInfo.ChatgptUserID)
		setNonEmptyString(content, "plan_type", claims.CodexAuthInfo.ChatgptPlanType)
		setNonEmptyString(content, "chatgpt_plan_type", claims.CodexAuthInfo.ChatgptPlanType)
		for _, organization := range claims.CodexAuthInfo.Organizations {
			if organization.IsDefault && strings.TrimSpace(organization.ID) != "" {
				content["organization_id"] = strings.TrimSpace(organization.ID)
				break
			}
		}
	}

	name := codexConvertedFileName(index, tokenData.Email, tokenData.AccountID)
	content["name"] = strings.TrimSuffix(name, filepath.Ext(name))
	return codexRefreshTokenConvertResult{file: &codexRefreshTokenConvertedFile{
		Index:   index,
		Name:    name,
		Content: content,
	}}
}

func refreshTokenFailure(index int, refreshToken string, err error) codexRefreshTokenConvertResult {
	message := strings.ReplaceAll(err.Error(), refreshToken, "[redacted]")
	if len(message) > maxRefreshConversionErrSize {
		message = message[:maxRefreshConversionErrSize]
	}
	return codexRefreshTokenConvertResult{failure: &codexRefreshTokenConvertFailure{
		Index: index,
		Error: message,
	}}
}

var unsafeCredentialFileName = regexp.MustCompile(`[^a-zA-Z0-9._@-]+`)

func codexConvertedFileName(index int, email, accountID string) string {
	base := strings.TrimSpace(email)
	if base == "" {
		base = strings.TrimSpace(accountID)
	}
	if base == "" {
		base = fmt.Sprintf("codex-%d", index+1)
	}
	base = strings.Trim(unsafeCredentialFileName.ReplaceAllString(base, "_"), "._-")
	if base == "" {
		base = fmt.Sprintf("codex-%d", index+1)
	}
	return fmt.Sprintf("%04d_%s.json", index+1, base)
}

func setNonEmptyString(target map[string]any, key, value string) {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		target[key] = trimmed
	}
}

package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
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
	RefreshTokens codexRefreshTokenInput `json:"refresh_tokens"`
	ClientID      string                 `json:"client_id"`
}

type codexRefreshTokenInput []string

func (input *codexRefreshTokenInput) UnmarshalJSON(data []byte) error {
	var text string
	if errText := json.Unmarshal(data, &text); errText == nil {
		*input = []string{text}
		return nil
	}
	var values []string
	if errValues := json.Unmarshal(data, &values); errValues == nil {
		*input = values
		return nil
	}
	return fmt.Errorf("refresh_tokens must be a string or string array")
}

type codexRefreshTokenConvertedCredential struct {
	index         int
	tokenData     *codex.CodexTokenData
	clientID      string
	planType      string
	hashAccountID string
	metadata      map[string]any
}

type codexRefreshTokenConvertResult struct {
	credential *codexRefreshTokenConvertedCredential
	failure    error
}

// ConvertCodexRefreshTokens exchanges refresh tokens for CPA-compatible Codex credentials.
func (h *Handler) ConvertCodexRefreshTokens(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
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

	saved := 0
	failed := 0
	for _, result := range results {
		if result.failure != nil || result.credential == nil {
			failed++
			continue
		}
		if errSave := h.saveConvertedCodexCredential(c.Request.Context(), result.credential); errSave != nil {
			failed++
			log.WithError(errSave).Warnf("failed to persist converted Codex credential at input index %d", result.credential.index)
			continue
		}
		saved++
	}

	c.JSON(http.StatusOK, gin.H{
		"total":        len(refreshTokens),
		"saved":        saved,
		"failed_count": failed,
	})
}

func normalizeRefreshTokens(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized := strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
		for _, line := range strings.Split(normalized, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if _, exists := seen[trimmed]; exists {
				continue
			}
			seen[trimmed] = struct{}{}
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

	lastRefresh := time.Now().UTC().Format(time.RFC3339)
	metadata := map[string]any{
		"type":          "codex",
		"client_id":     clientID,
		"access_token":  tokenData.AccessToken,
		"refresh_token": tokenData.RefreshToken,
		"last_refresh":  lastRefresh,
	}
	setNonEmptyString(metadata, "id_token", tokenData.IDToken)
	setNonEmptyString(metadata, "account_id", tokenData.AccountID)
	setNonEmptyString(metadata, "chatgpt_account_id", tokenData.AccountID)
	setNonEmptyString(metadata, "email", tokenData.Email)
	setNonEmptyString(metadata, "expired", tokenData.Expire)

	planType := ""
	hashAccountID := ""
	if claims, errClaims := codex.ParseJWTToken(tokenData.IDToken); errClaims == nil && claims != nil {
		planType = strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
		setNonEmptyString(metadata, "chatgpt_user_id", claims.CodexAuthInfo.ChatgptUserID)
		setNonEmptyString(metadata, "plan_type", planType)
		setNonEmptyString(metadata, "chatgpt_plan_type", planType)
		for _, organization := range claims.CodexAuthInfo.Organizations {
			if organization.IsDefault && strings.TrimSpace(organization.ID) != "" {
				metadata["organization_id"] = strings.TrimSpace(organization.ID)
				break
			}
		}
	}
	if accountID := strings.TrimSpace(tokenData.AccountID); accountID != "" {
		digest := sha256.Sum256([]byte(accountID))
		hashAccountID = hex.EncodeToString(digest[:])[:8]
	}

	return codexRefreshTokenConvertResult{credential: &codexRefreshTokenConvertedCredential{
		index:         index,
		tokenData:     tokenData,
		clientID:      clientID,
		planType:      planType,
		hashAccountID: hashAccountID,
		metadata:      metadata,
	}}
}

func refreshTokenFailure(index int, refreshToken string, err error) codexRefreshTokenConvertResult {
	message := strings.ReplaceAll(err.Error(), refreshToken, "[redacted]")
	if len(message) > maxRefreshConversionErrSize {
		message = message[:maxRefreshConversionErrSize]
	}
	log.Warnf("failed to convert Codex refresh token at input index %d: %s", index, message)
	return codexRefreshTokenConvertResult{failure: fmt.Errorf("conversion failed")}
}

func (h *Handler) saveConvertedCodexCredential(ctx context.Context, credential *codexRefreshTokenConvertedCredential) error {
	if credential == nil || credential.tokenData == nil {
		return fmt.Errorf("converted credential is empty")
	}
	tokenData := credential.tokenData
	fileName := codex.CredentialFileName(tokenData.Email, credential.planType, credential.hashAccountID, true)
	if existing := h.findCodexAuthByAccountID(tokenData.AccountID); existing != nil {
		if strings.TrimSpace(existing.FileName) != "" {
			fileName = existing.FileName
		}
	}
	if fileName == "codex-.json" {
		if credential.hashAccountID != "" {
			fileName = "codex-" + credential.hashAccountID + ".json"
		} else {
			fileName = fmt.Sprintf("codex-import-%d.json", credential.index+1)
		}
	}

	storage := &codex.CodexTokenStorage{
		IDToken:      tokenData.IDToken,
		AccessToken:  tokenData.AccessToken,
		RefreshToken: tokenData.RefreshToken,
		AccountID:    tokenData.AccountID,
		LastRefresh:  refreshMetadataString(credential.metadata["last_refresh"]),
		Email:        tokenData.Email,
		Type:         "codex",
		Expire:       tokenData.Expire,
	}
	record := &coreauth.Auth{
		ID:       fileName,
		Provider: "codex",
		FileName: fileName,
		Status:   coreauth.StatusActive,
		Storage:  storage,
		Metadata: credential.metadata,
	}
	if existing := h.findCodexAuthByAccountID(tokenData.AccountID); existing != nil {
		record.ID = existing.ID
		record.CreatedAt = existing.CreatedAt
		record.Attributes = cloneStringMap(existing.Attributes)
	}
	if _, errSave := h.saveTokenRecord(ctx, record); errSave != nil {
		return fmt.Errorf("save converted credential: %w", errSave)
	}
	if errRegister := h.upsertAuthRecord(coreauth.WithSkipPersist(ctx), record); errRegister != nil {
		return fmt.Errorf("register converted credential: %w", errRegister)
	}
	return nil
}

func (h *Handler) findCodexAuthByAccountID(accountID string) *coreauth.Auth {
	accountID = strings.TrimSpace(accountID)
	if h == nil || h.authManager == nil || accountID == "" {
		return nil
	}
	for _, auth := range h.authManager.List() {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			continue
		}
		if auth.Metadata != nil {
			candidate := ""
			if value, ok := auth.Metadata["account_id"].(string); ok {
				candidate = strings.TrimSpace(value)
			}
			if candidate == "" {
				if value, ok := auth.Metadata["chatgpt_account_id"].(string); ok {
					candidate = strings.TrimSpace(value)
				}
			}
			if candidate == accountID {
				return auth
			}
		}
	}
	return nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func refreshMetadataString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func setNonEmptyString(target map[string]any, key, value string) {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		target[key] = trimmed
	}
}

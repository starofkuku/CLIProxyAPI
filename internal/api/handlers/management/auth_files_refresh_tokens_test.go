package management

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type fakeCodexRefreshService struct {
	mu       sync.Mutex
	clientID string
}

func (f *fakeCodexRefreshService) RefreshTokensWithClientID(_ context.Context, refreshToken, clientID string) (*codex.CodexTokenData, error) {
	f.mu.Lock()
	f.clientID = clientID
	f.mu.Unlock()
	if refreshToken == "secret-bad-token" {
		return nil, errors.New("invalid_grant for secret-bad-token")
	}
	claims := map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "account-123",
			"chatgpt_user_id":    "user-123",
			"chatgpt_plan_type":  "plus",
		},
	}
	payload, _ := json.Marshal(claims)
	idToken := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	return &codex.CodexTokenData{
		IDToken:      idToken,
		AccessToken:  "new-access",
		RefreshToken: "rotated-refresh",
		AccountID:    "account-123",
		Email:        "user@example.com",
		Expire:       "2026-07-17T12:00:00Z",
	}, nil
}

func TestConvertCodexRefreshTokensReturnsOrderedCPAResults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeCodexRefreshService{}
	originalFactory := newCodexRefreshService
	newCodexRefreshService = func(*config.Config) codexRefreshService { return fake }
	t.Cleanup(func() { newCodexRefreshService = originalFactory })

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	body := []byte(`{"refresh_tokens":["good-token","secret-bad-token"],"client_id":"custom-client"}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/refresh-tokens/convert", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.ConvertCodexRefreshTokens(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Total       int                               `json:"total"`
		Converted   int                               `json:"converted"`
		FailedCount int                               `json:"failed_count"`
		Files       []codexRefreshTokenConvertedFile  `json:"files"`
		Failed      []codexRefreshTokenConvertFailure `json:"failed"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response.Total != 2 || response.Converted != 1 || response.FailedCount != 1 {
		t.Fatalf("unexpected counts: %+v", response)
	}
	if len(response.Files) != 1 || response.Files[0].Index != 0 {
		t.Fatalf("unexpected files: %+v", response.Files)
	}
	if response.Files[0].Content["client_id"] != "custom-client" || response.Files[0].Content["chatgpt_user_id"] != "user-123" {
		t.Fatalf("unexpected CPA content: %#v", response.Files[0].Content)
	}
	if len(response.Failed) != 1 || response.Failed[0].Index != 1 {
		t.Fatalf("unexpected failures: %+v", response.Failed)
	}
	if strings.Contains(recorder.Body.String(), "secret-bad-token") {
		t.Fatal("response leaked the submitted refresh token")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.clientID != "custom-client" {
		t.Fatalf("client ID = %q", fake.clientID)
	}
}

func TestConvertCodexRefreshTokensUsesDefaultClientID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeCodexRefreshService{}
	originalFactory := newCodexRefreshService
	newCodexRefreshService = func(*config.Config) codexRefreshService { return fake }
	t.Cleanup(func() { newCodexRefreshService = originalFactory })

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"refresh_tokens":["good-token"]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ConvertCodexRefreshTokens(c)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.clientID != codex.ClientID {
		t.Fatalf("client ID = %q, want %q", fake.clientID, codex.ClientID)
	}
}

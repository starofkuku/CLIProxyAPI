package management

import (
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
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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

func TestConvertCodexRefreshTokensPersistsResultsAndReturnsCounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeCodexRefreshService{}
	originalFactory := newCodexRefreshService
	newCodexRefreshService = func(*config.Config) codexRefreshService { return fake }
	t.Cleanup(func() { newCodexRefreshService = originalFactory })

	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	store := &memoryAuthStore{}
	h.tokenStore = store
	body := `{"refresh_tokens":"good-token\r\nsecret-bad-token\ngood-token\n\n","client_id":"custom-client"}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/refresh-tokens/convert", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.ConvertCodexRefreshTokens(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Total       int `json:"total"`
		Saved       int `json:"saved"`
		FailedCount int `json:"failed_count"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response.Total != 2 || response.Saved != 1 || response.FailedCount != 1 {
		t.Fatalf("unexpected counts: %+v", response)
	}
	for _, secret := range []string{"secret-bad-token", "new-access", "rotated-refresh"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("response leaked credential material %q", secret)
		}
	}
	stored, errList := store.List(context.Background())
	if errList != nil || len(stored) != 1 {
		t.Fatalf("stored credentials = %d, err = %v", len(stored), errList)
	}
	if stored[0].Metadata["client_id"] != "custom-client" || stored[0].Metadata["chatgpt_user_id"] != "user-123" {
		t.Fatalf("unexpected stored metadata: %#v", stored[0].Metadata)
	}
	if stored[0].Metadata["access_token"] != "new-access" || stored[0].Metadata["refresh_token"] != "rotated-refresh" {
		t.Fatalf("stored runtime metadata is missing refreshed tokens: %#v", stored[0].Metadata)
	}
	if auths := manager.List(); len(auths) != 1 {
		t.Fatalf("runtime auth count = %d, want 1", len(auths))
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

	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}
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

func TestConvertCodexRefreshTokensUpdatesExistingAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeCodexRefreshService{}
	originalFactory := newCodexRefreshService
	newCodexRefreshService = func(*config.Config) codexRefreshService { return fake }
	t.Cleanup(func() { newCodexRefreshService = originalFactory })

	manager := coreauth.NewManager(nil, nil, nil)
	existing := &coreauth.Auth{
		ID:       "existing-codex.json",
		Provider: "codex",
		FileName: "existing-codex.json",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "codex", "account_id": "account-123"},
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), existing); errRegister != nil {
		t.Fatalf("register existing auth: %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	store := &memoryAuthStore{}
	h.tokenStore = store

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"refresh_tokens":"good-token"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ConvertCodexRefreshTokens(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	stored, errList := store.List(context.Background())
	if errList != nil || len(stored) != 1 {
		t.Fatalf("stored credentials = %d, err = %v", len(stored), errList)
	}
	if stored[0].ID != "existing-codex.json" {
		t.Fatalf("stored auth ID = %q, want existing-codex.json", stored[0].ID)
	}
	if auths := manager.List(); len(auths) != 1 || auths[0].Metadata["refresh_token"] != "rotated-refresh" {
		t.Fatalf("existing runtime auth was not updated: %#v", auths)
	}
}

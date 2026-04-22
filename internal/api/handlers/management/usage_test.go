package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type fakeUsageMigrationStore struct {
	lastTable string
	raw       []byte
}

type fakeUsageSnapshotStore struct {
	raw []byte
}

func (f *fakeUsageMigrationStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (f *fakeUsageMigrationStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", nil
}
func (f *fakeUsageMigrationStore) Delete(context.Context, string) error { return nil }
func (f *fakeUsageMigrationStore) LoadUsageSnapshot(context.Context) ([]byte, error) {
	return f.raw, nil
}
func (f *fakeUsageMigrationStore) MigrateLegacyUsageTable(_ context.Context, tableName string) (store.UsageLegacyMigrationResult, error) {
	f.lastTable = tableName
	return store.UsageLegacyMigrationResult{
		Table:         tableName,
		DetailRows:    1,
		TotalRequests: 1,
	}, nil
}

func (f *fakeUsageSnapshotStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (f *fakeUsageSnapshotStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", nil
}
func (f *fakeUsageSnapshotStore) Delete(context.Context, string) error { return nil }
func (f *fakeUsageSnapshotStore) LoadUsageSnapshot(context.Context) ([]byte, error) {
	return f.raw, nil
}
func (f *fakeUsageSnapshotStore) PersistUsageSnapshot(_ context.Context, snapshot []byte) error {
	f.raw = append(f.raw[:0], snapshot...)
	return nil
}

func TestGetUsageStatistics_UsesStoreSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	snapshot := usage.StatisticsSnapshot{
		TotalRequests: 3,
		SuccessCount:  2,
		FailureCount:  1,
		TotalTokens:   30,
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	handler := &Handler{
		usageStats: usage.NewRequestStatistics(),
		tokenStore: &fakeUsageMigrationStore{raw: raw},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage", nil)

	handler.GetUsageStatistics(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var body struct {
		Usage usage.StatisticsSnapshot `json:"usage"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Usage.TotalRequests != 3 {
		t.Fatalf("TotalRequests = %d, want 3", body.Usage.TotalRequests)
	}
}

func TestImportUsageStatistics_PersistsToStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ts := time.Date(2026, 3, 18, 13, 0, 0, 0, time.UTC)
	payload := usageImportPayload{
		Version: 1,
		Usage: usage.StatisticsSnapshot{
			APIs: map[string]usage.APISnapshot{
				"/v1/chat/completions": {
					Models: map[string]usage.ModelSnapshot{
						"gpt-5": {
							Details: []usage.RequestDetail{{
								Timestamp: ts,
								Source:    "openai",
								AuthIndex: "0",
								Tokens: usage.TokenStats{
									InputTokens:  6,
									OutputTokens: 4,
									TotalTokens:  10,
								},
							}},
						},
					},
				},
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	store := &fakeUsageSnapshotStore{}
	handler := &Handler{
		usageStats: usage.NewRequestStatistics(),
		tokenStore: store,
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/usage/import", bytes.NewReader(data))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ImportUsageStatistics(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var stored usage.StatisticsSnapshot
	if err = json.Unmarshal(store.raw, &stored); err != nil {
		t.Fatalf("unmarshal stored snapshot: %v", err)
	}
	if stored.TotalRequests != 1 {
		t.Fatalf("stored TotalRequests = %d, want 1", stored.TotalRequests)
	}
}

func TestMigrateLegacyUsageStatistics_DefaultTable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ts := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
	snapshot := usage.StatisticsSnapshot{
		TotalRequests: 1,
		SuccessCount:  1,
		TotalTokens:   12,
		APIs: map[string]usage.APISnapshot{
			"/v1/chat/completions": {
				TotalRequests: 1,
				TotalTokens:   12,
				Models: map[string]usage.ModelSnapshot{
					"gpt-5": {
						TotalRequests: 1,
						TotalTokens:   12,
						Details: []usage.RequestDetail{{
							Timestamp: ts,
							Source:    "openai",
							AuthIndex: "0",
							Tokens: usage.TokenStats{
								InputTokens:  8,
								OutputTokens: 4,
								TotalTokens:  12,
							},
						}},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	fakeStore := &fakeUsageMigrationStore{raw: raw}
	handler := &Handler{
		usageStats: usage.NewRequestStatistics(),
		tokenStore: fakeStore,
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/usage/migrate-legacy", bytes.NewReader(nil))

	handler.MigrateLegacyUsageStatistics(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if fakeStore.lastTable != "usage_store" {
		t.Fatalf("lastTable = %q, want %q", fakeStore.lastTable, "usage_store")
	}

	var body map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got := int(body["detail_rows"].(float64)); got != 1 {
		t.Fatalf("detail_rows = %d, want 1", got)
	}
	if got := int(body["added"].(float64)); got != 1 {
		t.Fatalf("added = %d, want 1", got)
	}
	if got := int(body["total_requests"].(float64)); got != 1 {
		t.Fatalf("total_requests = %d, want 1", got)
	}
}

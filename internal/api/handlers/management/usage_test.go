package management

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type fakeUsageMigrationStore struct {
	lastTable string
	raw       []byte
}

type fakeUsageSnapshotStore struct {
	raw []byte
}

type fakeUsageRangeStore struct {
	raw        []byte
	rangeCalls int
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

func (f *fakeUsageRangeStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (f *fakeUsageRangeStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", nil
}
func (f *fakeUsageRangeStore) Delete(context.Context, string) error { return nil }
func (f *fakeUsageRangeStore) LoadUsageSnapshotRange(context.Context, time.Time, time.Time) ([]byte, error) {
	f.rangeCalls++
	return f.raw, nil
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

func TestGetUsageStatistics_UsesRecentCacheForEligibleRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now()
	loadedSnapshot := usage.StatisticsSnapshot{
		APIs: map[string]usage.APISnapshot{
			"codex": {
				Models: map[string]usage.ModelSnapshot{
					"gpt-test": {
						Details: []usage.RequestDetail{{
							Timestamp: now,
							Tokens:    usage.TokenStats{TotalTokens: 9},
						}},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(loadedSnapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	store := &fakeUsageRangeStore{raw: raw}
	cache := usage.NewRecentUsageCache()
	cache.SetLoader(store)
	cache.SetEnabled(true)
	defer cache.SetEnabled(false)
	deadline := time.Now().Add(2 * time.Second)
	for !cache.Ready() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !cache.Ready() {
		t.Fatal("recent cache did not become ready")
	}
	warmupCalls := store.rangeCalls

	handler := &Handler{
		usageStats:       usage.NewRequestStatistics(),
		tokenStore:       store,
		recentUsageCache: cache,
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	start := now.In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02")
	c.Request = httptest.NewRequest(
		http.MethodGet,
		"/v0/management/usage?from="+url.QueryEscape(start)+"&to="+
			url.QueryEscape(now.Add(time.Second).Format(time.RFC3339Nano)),
		nil,
	)
	handler.GetUsageStatistics(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if store.rangeCalls != warmupCalls {
		t.Fatalf("range loader calls = %d, want %d after cache hit", store.rangeCalls, warmupCalls)
	}
	var body struct {
		Usage usage.StatisticsSnapshot `json:"usage"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Usage.TotalRequests != 1 {
		t.Fatalf("TotalRequests = %d, want 1", body.Usage.TotalRequests)
	}
}

func TestGetUsageStatistics_GzipResponse(t *testing.T) {
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
	c.Request.Header.Set("Accept-Encoding", "gzip, deflate, br")

	handler.GetUsageStatistics(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}

	reader, errReader := gzip.NewReader(w.Body)
	if errReader != nil {
		t.Fatalf("create gzip reader: %v", errReader)
	}
	decompressed, errRead := io.ReadAll(reader)
	if errRead != nil {
		t.Fatalf("read gzip response: %v", errRead)
	}
	if errClose := reader.Close(); errClose != nil {
		t.Fatalf("close gzip reader: %v", errClose)
	}

	var body struct {
		Usage usage.StatisticsSnapshot `json:"usage"`
	}
	if err = json.Unmarshal(decompressed, &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Usage.TotalRequests != 3 {
		t.Fatalf("TotalRequests = %d, want 3", body.Usage.TotalRequests)
	}
}

func TestGetUsageStatistics_GzipDisabledByQualityZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{usageStats: usage.NewRequestStatistics()}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage", nil)
	c.Request.Header.Set("Accept-Encoding", "gzip;q=0, *;q=1")

	handler.GetUsageStatistics(c)

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Fatalf("response is not plain JSON: %q", w.Body.String())
	}
}

func TestGetUsageStatistics_GzipDisabledByConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gzipEnabled := false
	handler := &Handler{
		cfg: &config.Config{ManagementPerformance: config.ManagementPerformanceConfig{
			GzipEnabled: &gzipEnabled,
		}},
		usageStats: usage.NewRequestStatistics(),
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage", nil)
	c.Request.Header.Set("Accept-Encoding", "gzip")

	handler.GetUsageStatistics(c)

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Fatalf("response is not plain JSON: %q", w.Body.String())
	}
}

func TestGetUsageStatistics_FiltersByTimeRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	inRange := time.Date(2026, 3, 18, 1, 30, 0, 0, time.UTC)
	outOfRange := time.Date(2026, 3, 19, 1, 30, 0, 0, time.UTC)
	snapshot := usage.StatisticsSnapshot{
		APIs: map[string]usage.APISnapshot{
			"api-key-1": {
				Models: map[string]usage.ModelSnapshot{
					"gpt-5.5": {
						Details: []usage.RequestDetail{
							{
								Timestamp: inRange,
								Source:    "openai",
								Tokens:    usage.TokenStats{InputTokens: 6, OutputTokens: 4, TotalTokens: 10},
							},
							{
								Timestamp: outOfRange,
								Source:    "openai",
								Tokens:    usage.TokenStats{InputTokens: 20, OutputTokens: 5, TotalTokens: 25},
							},
						},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	handler := &Handler{
		usageStats: usage.NewRequestStatistics(),
		tokenStore: &fakeUsageSnapshotStore{raw: raw},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage?start=2026-03-18T00:00:00Z&end=2026-03-19T00:00:00Z", nil)

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
	if body.Usage.TotalRequests != 1 {
		t.Fatalf("TotalRequests = %d, want 1", body.Usage.TotalRequests)
	}
	if body.Usage.TotalTokens != 10 {
		t.Fatalf("TotalTokens = %d, want 10", body.Usage.TotalTokens)
	}
}

func TestGetUsageStatistics_InvalidTimeRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{usageStats: usage.NewRequestStatistics()}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage?start=2026-03-19&end=2026-03-18", nil)

	handler.GetUsageStatistics(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
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

func TestGetUsageQueuePopsRequestedRecords(t *testing.T) {
	withManagementUsageQueue(t, func() {
		redisqueue.Enqueue([]byte(`{"id":1}`))
		redisqueue.Enqueue([]byte(`{"id":2}`))
		redisqueue.Enqueue([]byte(`{"id":3}`))

		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=2", nil)

		h := &Handler{}
		h.GetUsageQueue(ginCtx)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}

		var payload []json.RawMessage
		if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
			t.Fatalf("unmarshal response: %v", errUnmarshal)
		}
		if len(payload) != 2 {
			t.Fatalf("response records = %d, want 2", len(payload))
		}
		requireRecordID(t, payload[0], 1)
		requireRecordID(t, payload[1], 2)

		remaining := redisqueue.PopOldest(10)
		if len(remaining) != 1 || string(remaining[0]) != `{"id":3}` {
			t.Fatalf("remaining queue = %q, want third item only", remaining)
		}
	})
}

func TestGetUsageQueueInvalidCountDoesNotPop(t *testing.T) {
	withManagementUsageQueue(t, func() {
		redisqueue.Enqueue([]byte(`{"id":1}`))

		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=0", nil)

		h := &Handler{}
		h.GetUsageQueue(ginCtx)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}

		remaining := redisqueue.PopOldest(10)
		if len(remaining) != 1 || string(remaining[0]) != `{"id":1}` {
			t.Fatalf("remaining queue = %q, want original item", remaining)
		}
	})
}

func withManagementUsageQueue(t *testing.T, fn func()) {
	t.Helper()

	prevQueueEnabled := redisqueue.Enabled()
	redisqueue.SetEnabled(false)
	redisqueue.SetEnabled(true)

	defer func() {
		redisqueue.SetEnabled(false)
		redisqueue.SetEnabled(prevQueueEnabled)
	}()

	fn()
}

func requireRecordID(t *testing.T, raw json.RawMessage, want int) {
	t.Helper()

	var payload struct {
		ID int `json:"id"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal record: %v", errUnmarshal)
	}
	if payload.ID != want {
		t.Fatalf("record id = %d, want %d", payload.ID, want)
	}
}

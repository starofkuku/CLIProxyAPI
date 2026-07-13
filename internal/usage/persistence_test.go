package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type fakeSnapshotStore struct {
	mu          sync.Mutex
	loadPayload []byte
	writes      [][]byte
	loadErr     error
	writeErr    error
}

type fakeRecordWriter struct {
	mu      sync.Mutex
	records []PersistentRecord
	err     error
}

func (s *fakeSnapshotStore) LoadUsageSnapshot(context.Context) ([]byte, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return s.loadPayload, nil
}

func (s *fakeSnapshotStore) PersistUsageSnapshot(_ context.Context, snapshot []byte) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(snapshot))
	copy(cp, snapshot)
	s.writes = append(s.writes, cp)
	return nil
}

func (s *fakeSnapshotStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func (w *fakeRecordWriter) AppendUsageRecord(_ context.Context, record PersistentRecord) error {
	if w.err != nil {
		return w.err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.records = append(w.records, record)
	return nil
}

func sampleSnapshot(ts time.Time, api, model string, totalTokens int64) StatisticsSnapshot {
	return StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			api: {
				Models: map[string]ModelSnapshot{
					model: {
						Details: []RequestDetail{
							{
								Timestamp: ts,
								Source:    "unit-test",
								AuthIndex: "0",
								Failed:    false,
								Tokens: TokenStats{
									InputTokens:  totalTokens,
									TotalTokens:  totalTokens,
									OutputTokens: 0,
								},
							},
						},
					},
				},
			},
		},
	}
}

func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

func TestRestoreSnapshot(t *testing.T) {
	stats := NewRequestStatistics()
	store := &fakeSnapshotStore{}
	raw, err := json.Marshal(sampleSnapshot(time.Now().UTC(), "api-1", "gpt-5", 10))
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	store.loadPayload = raw

	if err = RestoreSnapshot(context.Background(), stats, store); err != nil {
		t.Fatalf("RestoreSnapshot() error = %v", err)
	}

	got := stats.Snapshot()
	if got.TotalRequests != 1 {
		t.Fatalf("TotalRequests = %d, want 1", got.TotalRequests)
	}
	if got.TotalTokens != 10 {
		t.Fatalf("TotalTokens = %d, want 10", got.TotalTokens)
	}
}

func TestStartSnapshotAutoSync(t *testing.T) {
	stats := NewRequestStatistics()
	store := &fakeSnapshotStore{}

	stats.MergeSnapshot(sampleSnapshot(time.Now().UTC(), "api-1", "gpt-5", 5))
	stop := StartSnapshotAutoSync(context.Background(), stats, store, 20*time.Millisecond)
	defer stop()

	waitFor(t, time.Second, func() bool { return store.writeCount() == 1 })
	time.Sleep(80 * time.Millisecond)
	if got := store.writeCount(); got != 1 {
		t.Fatalf("write count after unchanged intervals = %d, want 1", got)
	}

	stats.MergeSnapshot(sampleSnapshot(time.Now().UTC().Add(time.Second), "api-1", "gpt-5", 7))
	waitFor(t, time.Second, func() bool { return store.writeCount() >= 2 })
}

func TestPersistSnapshot(t *testing.T) {
	stats := NewRequestStatistics()
	store := &fakeSnapshotStore{}
	stats.MergeSnapshot(sampleSnapshot(time.Now().UTC(), "api-2", "gemini-2.5-pro", 12))

	if err := PersistSnapshot(context.Background(), stats, store); err != nil {
		t.Fatalf("PersistSnapshot() error = %v", err)
	}
	if got := store.writeCount(); got != 1 {
		t.Fatalf("write count = %d, want 1", got)
	}
}

func TestLoggerPluginDirectWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stats := NewRequestStatistics()
	plugin := &LoggerPlugin{stats: stats}
	writer := &fakeRecordWriter{}
	SetUsageRecordWriter(writer)
	defer SetUsageRecordWriter(nil)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Set("apiKey", "sk-test-123")
	requestedAt := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
	firstResponseAt := requestedAt.Add(600 * time.Millisecond)
	completedAt := requestedAt.Add(2 * time.Second)

	plugin.HandleUsage(context.WithValue(context.Background(), "gin", ginCtx), coreusage.Record{
		Provider:        "openai",
		Model:           "gpt-5",
		APIKey:          "sk-test-123",
		AuthIndex:       "0",
		Source:          "openai",
		RequestedAt:     requestedAt,
		ClientIP:        "203.0.113.8",
		FirstResponseAt: firstResponseAt,
		CompletedAt:     completedAt,
		Detail: coreusage.Detail{
			InputTokens:  8,
			OutputTokens: 4,
			TotalTokens:  12,
		},
	})

	if got := stats.Snapshot().TotalRequests; got != 1 {
		t.Fatalf("TotalRequests = %d, want 1", got)
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if got := len(writer.records); got != 1 {
		t.Fatalf("direct writes = %d, want 1", got)
	}
	if writer.records[0].APIName != "sk-test-123" {
		t.Fatalf("APIName = %q, want %q", writer.records[0].APIName, "sk-test-123")
	}
	if writer.records[0].APIKeyHash == "" {
		t.Fatalf("APIKeyHash is empty, want hashed key")
	}
	if writer.records[0].Tokens.TotalTokens != 12 {
		t.Fatalf("TotalTokens = %d, want 12", writer.records[0].Tokens.TotalTokens)
	}
	if writer.records[0].ClientIP != "203.0.113.8" {
		t.Fatalf("ClientIP = %q, want %q", writer.records[0].ClientIP, "203.0.113.8")
	}
	if !writer.records[0].FirstResponseAt.Equal(firstResponseAt) {
		t.Fatalf("FirstResponseAt = %v, want %v", writer.records[0].FirstResponseAt, firstResponseAt)
	}
	if !writer.records[0].CompletedAt.Equal(completedAt) {
		t.Fatalf("CompletedAt = %v, want %v", writer.records[0].CompletedAt, completedAt)
	}
}

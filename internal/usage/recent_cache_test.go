package usage

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type fakeRecentUsageLoader struct {
	raw   []byte
	start time.Time
	end   time.Time
}

func (f *fakeRecentUsageLoader) LoadUsageSnapshotRange(_ context.Context, start, end time.Time) ([]byte, error) {
	f.start = start
	f.end = end
	return f.raw, nil
}

func TestRecentUsageCacheServesOnlyYesterdayAndToday(t *testing.T) {
	now := time.Now()
	windowStart := recentUsageWindowStart(now)
	todayStart := windowStart.AddDate(0, 0, 1)
	todayRecordTime := todayStart.Add(now.Sub(todayStart) / 2)

	loaded := NewRequestStatistics()
	loaded.recordPersistent(PersistentRecord{
		APIName:     "client-key",
		ModelName:   "gpt-test",
		RequestedAt: windowStart.Add(time.Hour),
		Tokens:      TokenStats{TotalTokens: 10},
	})
	loaded.recordPersistent(PersistentRecord{
		APIName:     "client-key",
		ModelName:   "gpt-test",
		RequestedAt: todayRecordTime,
		Tokens:      TokenStats{TotalTokens: 20},
	})
	raw, err := json.Marshal(loaded.Snapshot())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	loader := &fakeRecentUsageLoader{raw: raw}
	cache := NewRecentUsageCache()
	cache.SetLoader(loader)
	cache.SetEnabled(true)
	defer cache.SetEnabled(false)
	waitRecentUsageCacheReady(t, cache)

	if !loader.start.Equal(windowStart) {
		t.Fatalf("loader start = %v, want %v", loader.start, windowStart)
	}
	if loader.end.Before(now) {
		t.Fatalf("loader end = %v, want >= %v", loader.end, now)
	}

	today, ok := cache.SnapshotRange(todayStart, time.Now().Add(time.Second))
	if !ok {
		t.Fatal("today range cache miss")
	}
	if today.TotalRequests != 1 || today.TotalTokens != 20 {
		t.Fatalf("today snapshot = %#v, want one request and 20 tokens", today)
	}

	if _, ok = cache.SnapshotRange(windowStart.Add(-time.Second), time.Now()); ok {
		t.Fatal("range before yesterday unexpectedly hit cache")
	}
}

func TestRecentUsageCacheIncludesLiveRecords(t *testing.T) {
	now := time.Now()
	loader := &fakeRecentUsageLoader{raw: []byte(`{"apis":{}}`)}
	cache := NewRecentUsageCache()
	cache.SetLoader(loader)
	cache.SetEnabled(true)
	defer cache.SetEnabled(false)
	waitRecentUsageCacheReady(t, cache)

	cache.Record(PersistentRecord{
		APIName:     "client-key",
		ModelName:   "gpt-test",
		RequestedAt: now,
		Tokens:      TokenStats{TotalTokens: 7},
	})

	snapshot, ok := cache.SnapshotRange(recentUsageWindowStart(now), time.Now().Add(time.Second))
	if !ok {
		t.Fatal("recent range cache miss")
	}
	if snapshot.TotalRequests != 1 || snapshot.TotalTokens != 7 {
		t.Fatalf("snapshot = %#v, want one request and 7 tokens", snapshot)
	}
}

func waitRecentUsageCacheReady(t *testing.T, cache *RecentUsageCache) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cache.Ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for recent usage cache")
}

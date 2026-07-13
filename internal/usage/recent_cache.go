package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

var shanghaiLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// RecentUsageCacheLoader loads usage snapshots for a specific time range.
type RecentUsageCacheLoader interface {
	LoadUsageSnapshotRange(ctx context.Context, start, end time.Time) ([]byte, error)
}

// RecentUsageCache stores yesterday and today usage records in memory.
type RecentUsageCache struct {
	mu          sync.RWMutex
	loader      RecentUsageCacheLoader
	enabled     bool
	ready       bool
	loading     bool
	generation  uint64
	windowStart time.Time
	stats       *RequestStatistics
	pending     *RequestStatistics
}

// NewRecentUsageCache creates an empty disabled recent usage cache.
func NewRecentUsageCache() *RecentUsageCache {
	return &RecentUsageCache{}
}

// SetLoader updates the persistent range loader used to warm the cache.
func (c *RecentUsageCache) SetLoader(loader RecentUsageCacheLoader) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.loader = loader
	enabled := c.enabled
	c.mu.Unlock()
	if enabled && loader != nil {
		c.ReloadAsync()
	}
}

// SetEnabled enables or disables the cache. Enabling starts an asynchronous warmup.
func (c *RecentUsageCache) SetEnabled(enabled bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !enabled {
		c.generation++
		c.enabled = false
		c.ready = false
		c.loading = false
		c.windowStart = time.Time{}
		c.stats = nil
		c.pending = nil
		c.mu.Unlock()
		return
	}
	alreadyEnabled := c.enabled
	c.enabled = true
	ready := c.ready
	c.mu.Unlock()
	if !alreadyEnabled || !ready {
		c.ReloadAsync()
	}
}

// Enabled reports whether the cache feature is configured on.
func (c *RecentUsageCache) Enabled() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	enabled := c.enabled
	c.mu.RUnlock()
	return enabled
}

// Ready reports whether an in-memory snapshot is available.
func (c *RecentUsageCache) Ready() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	ready := c.enabled && c.ready && c.stats != nil
	c.mu.RUnlock()
	return ready
}

// ReloadAsync reloads yesterday and today data without blocking the caller.
func (c *RecentUsageCache) ReloadAsync() {
	if c == nil {
		return
	}
	go func() {
		if err := c.Reload(context.Background()); err != nil {
			log.WithError(err).Warn("recent usage cache reload failed")
		}
	}()
}

// Reload reloads yesterday and today data and atomically replaces the cache.
func (c *RecentUsageCache) Reload(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now()
	windowStart := recentUsageWindowStart(now)

	c.mu.Lock()
	if !c.enabled || c.loader == nil || c.loading {
		c.mu.Unlock()
		return nil
	}
	c.generation++
	generation := c.generation
	loader := c.loader
	c.loading = true
	c.pending = NewRequestStatistics()
	c.mu.Unlock()

	raw, err := loader.LoadUsageSnapshotRange(ctx, windowStart, now)
	if err != nil {
		c.failReload(generation)
		return fmt.Errorf("load recent usage snapshot: %w", err)
	}

	loadedSnapshot := StatisticsSnapshot{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err = json.Unmarshal(raw, &loadedSnapshot); err != nil {
			c.failReload(generation)
			return fmt.Errorf("parse recent usage snapshot: %w", err)
		}
	}
	loadedStats := NewRequestStatistics()
	loadedStats.MergeSnapshot(loadedSnapshot)

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled || generation != c.generation {
		return nil
	}
	if c.pending != nil {
		loadedStats.MergeSnapshot(c.pending.Snapshot())
	}
	c.stats = loadedStats
	c.windowStart = windowStart
	c.ready = true
	c.loading = false
	c.pending = nil
	log.WithField("window_start", windowStart.Format(time.RFC3339)).Info("recent usage cache ready")
	return nil
}

func (c *RecentUsageCache) failReload(generation uint64) {
	c.mu.Lock()
	if generation == c.generation {
		c.loading = false
		c.pending = nil
	}
	c.mu.Unlock()
}

// Record adds a newly completed request to the active cache and any warmup buffer.
func (c *RecentUsageCache) Record(record PersistentRecord) {
	if c == nil {
		return
	}
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
		record.RequestedAt = timestamp
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.enabled {
		return
	}
	windowStart := c.windowStart
	if windowStart.IsZero() {
		windowStart = recentUsageWindowStart(time.Now())
	}
	if timestamp.Before(windowStart) {
		return
	}
	if c.ready && c.stats != nil {
		c.stats.recordPersistent(record)
	}
	if c.loading && c.pending != nil {
		c.pending.recordPersistent(record)
	}
}

// SnapshotRange returns a cached snapshot when the full query range is within yesterday and today.
func (c *RecentUsageCache) SnapshotRange(start, end time.Time) (StatisticsSnapshot, bool) {
	var empty StatisticsSnapshot
	if c == nil || start.IsZero() {
		return empty, false
	}

	now := time.Now()
	windowStart := recentUsageWindowStart(now)
	if start.Before(windowStart) {
		return empty, false
	}
	c.reloadForNewWindow(windowStart)

	c.mu.RLock()
	if !c.enabled || !c.ready || c.stats == nil {
		shouldReload := c.enabled && !c.loading && c.loader != nil
		c.mu.RUnlock()
		if shouldReload {
			c.ReloadAsync()
		}
		return empty, false
	}
	stats := c.stats
	c.mu.RUnlock()

	return stats.SnapshotRange(start, end), true
}

func (c *RecentUsageCache) reloadForNewWindow(windowStart time.Time) {
	c.mu.RLock()
	needsReload := c.enabled && c.ready && !c.loading && c.loader != nil &&
		(c.windowStart.IsZero() || c.windowStart.Before(windowStart))
	c.mu.RUnlock()
	if needsReload {
		c.ReloadAsync()
	}
}

func recentUsageWindowStart(now time.Time) time.Time {
	localNow := now.In(shanghaiLocation)
	todayStart := time.Date(
		localNow.Year(),
		localNow.Month(),
		localNow.Day(),
		0,
		0,
		0,
		0,
		shanghaiLocation,
	)
	return todayStart.AddDate(0, 0, -1)
}

var defaultRecentUsageCache = NewRecentUsageCache()

// GetRecentUsageCache returns the process-wide recent usage cache.
func GetRecentUsageCache() *RecentUsageCache { return defaultRecentUsageCache }

// SetRecentUsageCacheLoader configures the process-wide cache loader.
func SetRecentUsageCacheLoader(loader RecentUsageCacheLoader) {
	defaultRecentUsageCache.SetLoader(loader)
}

// SetRecentUsageCacheEnabled configures and warms the process-wide cache.
func SetRecentUsageCacheEnabled(enabled bool) {
	defaultRecentUsageCache.SetEnabled(enabled)
}

// ReloadRecentUsageCacheAsync refreshes the process-wide cache after bulk data changes.
func ReloadRecentUsageCacheAsync() {
	defaultRecentUsageCache.ReloadAsync()
}

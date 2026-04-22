package usage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// DefaultSnapshotSyncInterval controls how frequently usage snapshots are persisted.
	DefaultSnapshotSyncInterval = 1 * time.Minute
	persistTimeout              = 5 * time.Second
)

// SnapshotStore defines persistence operations required for usage snapshot sync.
type SnapshotStore interface {
	LoadUsageSnapshot(ctx context.Context) ([]byte, error)
	PersistUsageSnapshot(ctx context.Context, snapshot []byte) error
}

// RestoreSnapshot loads a usage snapshot from store and merges it into in-memory statistics.
func RestoreSnapshot(ctx context.Context, stats *RequestStatistics, store SnapshotStore) error {
	if stats == nil || store == nil {
		return nil
	}
	payload, err := store.LoadUsageSnapshot(ctx)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(payload))) == 0 {
		return nil
	}

	var snapshot StatisticsSnapshot
	if err = json.Unmarshal(payload, &snapshot); err != nil {
		return fmt.Errorf("usage: parse snapshot: %w", err)
	}
	result := stats.MergeSnapshot(snapshot)
	log.Infof("usage snapshot restored from persistent store (added=%d skipped=%d)", result.Added, result.Skipped)
	return nil
}

// PersistSnapshot serializes the current usage statistics and writes it to store.
func PersistSnapshot(ctx context.Context, stats *RequestStatistics, store SnapshotStore) error {
	if stats == nil || store == nil {
		return nil
	}
	payload, err := json.Marshal(stats.Snapshot())
	if err != nil {
		return fmt.Errorf("usage: marshal snapshot: %w", err)
	}
	if err = store.PersistUsageSnapshot(ctx, payload); err != nil {
		return err
	}
	return nil
}

// StartSnapshotAutoSync starts a background worker that periodically persists usage snapshots.
// The returned stop function is idempotent and performs a final flush before returning.
func StartSnapshotAutoSync(parent context.Context, stats *RequestStatistics, store SnapshotStore, interval time.Duration) (stop func()) {
	if stats == nil || store == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = DefaultSnapshotSyncInterval
	}
	if parent == nil {
		parent = context.Background()
	}

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})

	var (
		lastHash [sha256.Size]byte
		hasHash  bool
	)

	persistIfChanged := func() {
		payload, err := json.Marshal(stats.Snapshot())
		if err != nil {
			log.WithError(err).Warn("usage snapshot sync skipped: marshal failed")
			return
		}
		hash := sha256.Sum256(payload)
		if hasHash && hash == lastHash {
			return
		}
		persistCtx, persistCancel := context.WithTimeout(context.Background(), persistTimeout)
		err = store.PersistUsageSnapshot(persistCtx, payload)
		persistCancel()
		if err != nil {
			log.WithError(err).Warn("usage snapshot sync failed")
			return
		}
		lastHash = hash
		hasHash = true
	}

	go func() {
		defer close(done)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		persistIfChanged()
		for {
			select {
			case <-ctx.Done():
				persistIfChanged()
				return
			case <-ticker.C:
				persistIfChanged()
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
)

type usageExportPayload struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Usage      usage.StatisticsSnapshot `json:"usage"`
}

type usageImportPayload struct {
	Version int                      `json:"version"`
	Usage   usage.StatisticsSnapshot `json:"usage"`
}

type usageLegacyMigratePayload struct {
	Table string `json:"table"`
}

type usageQueueRecord []byte

type usageLegacyMigrator interface {
	MigrateLegacyUsageTable(ctx context.Context, tableName string) (store.UsageLegacyMigrationResult, error)
}

type usageSnapshotLoader interface {
	LoadUsageSnapshot(ctx context.Context) ([]byte, error)
}

type usageSnapshotStore interface {
	LoadUsageSnapshot(ctx context.Context) ([]byte, error)
	PersistUsageSnapshot(ctx context.Context, snapshot []byte) error
}

func (r usageQueueRecord) MarshalJSON() ([]byte, error) {
	if json.Valid(r) {
		return append([]byte(nil), r...), nil
	}
	return json.Marshal(string(r))
}

func (h *Handler) loadUsageSnapshot(ctx context.Context, flush bool) (usage.StatisticsSnapshot, error) {
	var snapshot usage.StatisticsSnapshot
	if h == nil {
		return snapshot, nil
	}

	if store, ok := h.tokenStore.(usageSnapshotStore); ok {
		if flush && h.usageStats != nil {
			if err := usage.PersistSnapshot(ctx, h.usageStats, store); err != nil {
				return snapshot, err
			}
		}
		raw, err := store.LoadUsageSnapshot(ctx)
		if err != nil {
			return snapshot, err
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			return snapshot, nil
		}
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return snapshot, err
		}
		return snapshot, nil
	}

	if loader, ok := h.tokenStore.(usageSnapshotLoader); ok {
		raw, err := loader.LoadUsageSnapshot(ctx)
		if err != nil {
			return snapshot, err
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			return snapshot, nil
		}
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return snapshot, err
		}
		return snapshot, nil
	}

	if h.usageStats != nil {
		return h.usageStats.Snapshot(), nil
	}
	return snapshot, nil
}

// GetUsageStatistics returns the request statistics snapshot.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	snapshot, err := h.loadUsageSnapshot(c.Request.Context(), false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
	})
}

// ExportUsageStatistics returns a complete usage snapshot for backup/migration.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	snapshot, err := h.loadUsageSnapshot(c.Request.Context(), false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, usageExportPayload{
		Version:    1,
		ExportedAt: time.Now().UTC(),
		Usage:      snapshot,
	})
}

// ImportUsageStatistics merges a previously exported usage snapshot into memory.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	var payload usageImportPayload
	if err = json.Unmarshal(data, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Version != 0 && payload.Version != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version"})
		return
	}

	if store, ok := h.tokenStore.(usageSnapshotStore); ok {
		current, errLoad := h.loadUsageSnapshot(c.Request.Context(), false)
		if errLoad != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": errLoad.Error()})
			return
		}
		stats := usage.NewRequestStatistics()
		_ = stats.MergeSnapshot(current)
		result := stats.MergeSnapshot(payload.Usage)
		if err = usage.PersistSnapshot(c.Request.Context(), stats, store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		snapshot := stats.Snapshot()
		c.JSON(http.StatusOK, gin.H{
			"added":           result.Added,
			"skipped":         result.Skipped,
			"total_requests":  snapshot.TotalRequests,
			"failed_requests": snapshot.FailureCount,
		})
		return
	}

	if h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	result := h.usageStats.MergeSnapshot(payload.Usage)
	snapshot := h.usageStats.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"added":           result.Added,
		"skipped":         result.Skipped,
		"total_requests":  snapshot.TotalRequests,
		"failed_requests": snapshot.FailureCount,
	})
}

// GetUsageQueue pops queued usage records from the usage queue.
func (h *Handler) GetUsageQueue(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	count, errCount := parseUsageQueueCount(c.Query("count"))
	if errCount != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errCount.Error()})
		return
	}

	items := redisqueue.PopOldest(count)
	records := make([]usageQueueRecord, 0, len(items))
	for _, item := range items {
		records = append(records, usageQueueRecord(append([]byte(nil), item...)))
	}

	c.JSON(http.StatusOK, records)
}

func parseUsageQueueCount(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 1, nil
	}
	count, errCount := strconv.Atoi(value)
	if errCount != nil || count <= 0 {
		return 0, errors.New("count must be a positive integer")
	}
	return count, nil
}

// MigrateLegacyUsageStatistics imports a legacy snapshot table into detail-row storage.
func (h *Handler) MigrateLegacyUsageStatistics(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	migrator, ok := h.tokenStore.(usageLegacyMigrator)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "legacy usage migration unsupported by current store"})
		return
	}

	var payload usageLegacyMigratePayload
	if len(c.ContentType()) != 0 {
		if err := c.ShouldBindJSON(&payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
	}
	if payload.Table == "" {
		payload.Table = "usage_store"
	}

	result, err := migrator.MigrateLegacyUsageTable(c.Request.Context(), payload.Table)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	snapshot, errLoad := h.loadUsageSnapshot(c.Request.Context(), false)
	if errLoad != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errLoad.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"table":          result.Table,
		"detail_rows":    result.DetailRows,
		"migrated":       result.TotalRequests,
		"added":          result.DetailRows,
		"skipped":        0,
		"total_requests": snapshot.TotalRequests,
	})
}

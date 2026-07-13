package management

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
)

type managementPanelSyncFunc func(context.Context, string, string, string) (managementasset.ManagementHTMLSyncResult, error)

// SyncManagementPanel immediately refreshes the cached management panel HTML.
func (h *Handler) SyncManagementPanel(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "management handler unavailable"})
		return
	}

	h.mu.Lock()
	cfg := h.cfg
	configFilePath := h.configFilePath
	syncFn := h.managementPanelSync
	if cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	disabled := cfg.RemoteManagement.DisableControlPanel
	proxyURL := strings.TrimSpace(cfg.ProxyURL)
	panelRepository := strings.TrimSpace(cfg.RemoteManagement.PanelGitHubRepository)
	h.mu.Unlock()

	if disabled {
		c.JSON(http.StatusConflict, gin.H{"error": "management panel is disabled"})
		return
	}

	staticDir := managementasset.StaticDir(configFilePath)
	if strings.TrimSpace(staticDir) == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "management panel cache directory unavailable"})
		return
	}
	if syncFn == nil {
		syncFn = managementasset.SyncLatestManagementHTML
	}

	result, err := syncFn(c.Request.Context(), staticDir, proxyURL, panelRepository)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "management panel sync failed", "message": err.Error()})
		return
	}

	status := "up-to-date"
	if result.Updated {
		status = "updated"
	}
	c.JSON(http.StatusOK, gin.H{
		"status":    status,
		"updated":   result.Updated,
		"available": result.Available,
		"sha256":    result.Hash,
	})
}

package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const authRecycleDirectoryName = ".recycle-bin"

type authRecycleInfo struct {
	Version      int       `json:"version"`
	OriginalName string    `json:"original_name"`
	DeletedAt    time.Time `json:"deleted_at"`
	Reason       string    `json:"reason,omitempty"`
}

type authRecycleRecord struct {
	Recycle authRecycleInfo `json:"_recycle_bin"`
	Content json.RawMessage `json:"content"`
}

type authRecycleListItem struct {
	Name         string    `json:"name"`
	OriginalName string    `json:"original_name"`
	DeletedAt    time.Time `json:"deleted_at"`
	Reason       string    `json:"reason,omitempty"`
	Provider     string    `json:"provider,omitempty"`
	Email        string    `json:"email,omitempty"`
	Size         int64     `json:"size"`
}

type authFilePersister interface {
	PersistAuthFiles(ctx context.Context, message string, paths ...string) error
}

// ListRecycledAuthFiles lists soft-deleted auth files.
func (h *Handler) ListRecycledAuthFiles(c *gin.Context) {
	items, errList := h.listRecycleBin()
	if errList != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errList.Error()})
		return
	}
	h.writeCompressedJSON(c, http.StatusOK, gin.H{"files": items, "count": len(items)})
}

// TrashInvalidAuthFiles soft-deletes auth files with permanent authentication errors.
func (h *Handler) TrashInvalidAuthFiles(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	candidates := make(map[string]string)
	for _, auth := range h.authManager.List() {
		reason := permanentInvalidAuthReason(auth)
		if reason == "" || auth == nil || isRuntimeOnlyAuth(auth) || coreauth.IsPluginVirtualAuth(auth) {
			continue
		}
		name := strings.TrimSpace(auth.FileName)
		if path := strings.TrimSpace(authAttribute(auth, "path")); path != "" {
			name = filepath.Base(path)
		}
		if isUnsafeAuthFileName(name) {
			continue
		}
		candidates[name] = reason
	}

	files := make([]string, 0, len(candidates))
	failed := make([]gin.H, 0)
	for name, reason := range candidates {
		trashedName, _, errTrash := h.trashAuthFileByName(c.Request.Context(), name, reason)
		if errTrash != nil {
			failed = append(failed, gin.H{"name": name, "error": errTrash.Error()})
			continue
		}
		files = append(files, trashedName)
	}
	sort.Strings(files)
	status := "ok"
	httpStatus := http.StatusOK
	if len(failed) > 0 {
		status = "partial"
		httpStatus = http.StatusMultiStatus
	}
	c.JSON(httpStatus, gin.H{
		"status":  status,
		"matched": len(candidates),
		"deleted": len(files),
		"files":   files,
		"failed":  failed,
	})
}

// RestoreRecycledAuthFiles restores selected recycle-bin entries.
func (h *Handler) RestoreRecycledAuthFiles(c *gin.Context) {
	names, errNames := requestedAuthFileNamesForDelete(c)
	if errNames != nil || len(names) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "names are required"})
		return
	}

	restored := make([]string, 0, len(names))
	failed := make([]gin.H, 0)
	for _, name := range names {
		originalName, status, errRestore := h.restoreRecycledAuthFile(c.Request.Context(), name)
		if errRestore != nil {
			failed = append(failed, gin.H{"name": name, "status": status, "error": errRestore.Error()})
			continue
		}
		restored = append(restored, originalName)
	}
	writeAuthRecycleMutationResponse(c, restored, failed)
}

// PermanentlyDeleteRecycledAuthFiles permanently removes selected recycle-bin entries.
func (h *Handler) PermanentlyDeleteRecycledAuthFiles(c *gin.Context) {
	names, errNames := requestedAuthFileNamesForDelete(c)
	if errNames != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errNames.Error()})
		return
	}
	if all := c.Query("all"); (all == "true" || all == "1" || all == "*") && len(names) == 0 {
		items, errList := h.listRecycleBin()
		if errList != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": errList.Error()})
			return
		}
		for _, item := range items {
			names = append(names, item.Name)
		}
	}
	if len(names) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "names are required"})
		return
	}

	deleted := make([]string, 0, len(names))
	failed := make([]gin.H, 0)
	for _, name := range names {
		status, errDelete := h.permanentlyDeleteRecycledAuthFile(c.Request.Context(), name)
		if errDelete != nil {
			failed = append(failed, gin.H{"name": name, "status": status, "error": errDelete.Error()})
			continue
		}
		deleted = append(deleted, name)
	}
	writeAuthRecycleMutationResponse(c, deleted, failed)
}

func writeAuthRecycleMutationResponse(c *gin.Context, files []string, failed []gin.H) {
	if len(failed) > 0 {
		c.JSON(http.StatusMultiStatus, gin.H{"status": "partial", "deleted": len(files), "files": files, "failed": failed})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "deleted": len(files), "files": files, "failed": failed})
}

func (h *Handler) trashAuthFileByName(ctx context.Context, name, reason string) (string, int, error) {
	if h == nil || h.cfg == nil {
		return "", http.StatusInternalServerError, fmt.Errorf("handler unavailable")
	}
	name = strings.TrimSpace(name)
	if isUnsafeAuthFileName(name) {
		return "", http.StatusBadRequest, fmt.Errorf("invalid name")
	}

	h.authRecycleMu.Lock()
	defer h.authRecycleMu.Unlock()

	targetPath := filepath.Join(h.cfg.AuthDir, filepath.Base(name))
	targetID := ""
	if targetAuth := h.findAuthForDelete(name); targetAuth != nil {
		if !isPluginVirtualSourceDelete(name, targetAuth) {
			return filepath.Base(name), http.StatusConflict, errPluginVirtualAuth
		}
		targetID = strings.TrimSpace(targetAuth.ID)
		if path := strings.TrimSpace(authAttribute(targetAuth, "path")); path != "" {
			targetPath = path
		}
	}
	if !filepath.IsAbs(targetPath) {
		if absolute, errAbs := filepath.Abs(targetPath); errAbs == nil {
			targetPath = absolute
		}
	}
	raw, errRead := os.ReadFile(targetPath)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return filepath.Base(name), http.StatusNotFound, errAuthFileNotFound
		}
		return filepath.Base(name), http.StatusInternalServerError, fmt.Errorf("failed to read auth file: %w", errRead)
	}
	var object map[string]any
	if errJSON := json.Unmarshal(raw, &object); errJSON != nil || object == nil {
		return filepath.Base(name), http.StatusBadRequest, fmt.Errorf("invalid auth file")
	}

	recycleDir := h.recycleBinDir()
	if errMkdir := os.MkdirAll(recycleDir, 0o700); errMkdir != nil {
		return filepath.Base(name), http.StatusInternalServerError, fmt.Errorf("failed to create recycle bin: %w", errMkdir)
	}
	recyclePath := uniqueRecyclePath(recycleDir, filepath.Base(name))
	record := authRecycleRecord{
		Recycle: authRecycleInfo{
			Version:      1,
			OriginalName: filepath.Base(name),
			DeletedAt:    time.Now().UTC(),
			Reason:       strings.TrimSpace(reason),
		},
		Content: append(json.RawMessage(nil), raw...),
	}
	encoded, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return filepath.Base(name), http.StatusInternalServerError, errMarshal
	}
	if errWrite := os.WriteFile(recyclePath, encoded, 0o600); errWrite != nil {
		return filepath.Base(name), http.StatusInternalServerError, fmt.Errorf("failed to write recycle record: %w", errWrite)
	}
	if errPersist := h.persistAuthFilePaths(ctx, "Move auth file to recycle bin", recyclePath); errPersist != nil {
		_ = os.Remove(recyclePath)
		return filepath.Base(name), http.StatusInternalServerError, errPersist
	}
	if errDelete := h.deleteTokenRecord(ctx, targetPath); errDelete != nil {
		_ = os.Remove(recyclePath)
		_ = h.persistAuthFilePaths(ctx, "Rollback recycle record", recyclePath)
		return filepath.Base(name), http.StatusInternalServerError, errDelete
	}
	if errRemove := os.Remove(targetPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
		return filepath.Base(name), http.StatusInternalServerError, fmt.Errorf("failed to remove active auth file: %w", errRemove)
	}
	h.removeAuthsForPath(ctx, targetPath, targetID)
	return filepath.Base(name), http.StatusOK, nil
}

func (h *Handler) restoreRecycledAuthFile(ctx context.Context, name string) (string, int, error) {
	if isUnsafeAuthFileName(name) {
		return "", http.StatusBadRequest, fmt.Errorf("invalid name")
	}
	h.authRecycleMu.Lock()
	defer h.authRecycleMu.Unlock()

	recyclePath := filepath.Join(h.recycleBinDir(), filepath.Base(name))
	record, errRead := readAuthRecycleRecord(recyclePath)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return "", http.StatusNotFound, errAuthFileNotFound
		}
		return "", http.StatusInternalServerError, errRead
	}
	originalName := filepath.Base(strings.TrimSpace(record.Recycle.OriginalName))
	if isUnsafeAuthFileName(originalName) || !strings.HasSuffix(strings.ToLower(originalName), ".json") {
		return "", http.StatusBadRequest, fmt.Errorf("invalid original file name")
	}
	originalPath := filepath.Join(h.cfg.AuthDir, originalName)
	if _, errStat := os.Stat(originalPath); errStat == nil {
		return originalName, http.StatusConflict, fmt.Errorf("active auth file already exists")
	} else if !errors.Is(errStat, os.ErrNotExist) {
		return originalName, http.StatusInternalServerError, errStat
	}
	if _, errBuild := h.buildAuthFromFileData(originalPath, record.Content); errBuild != nil {
		return originalName, http.StatusBadRequest, errBuild
	}
	if errWrite := os.WriteFile(originalPath, record.Content, 0o600); errWrite != nil {
		return originalName, http.StatusInternalServerError, errWrite
	}
	if errPersist := h.persistAuthFilePaths(ctx, "Restore auth file from recycle bin", originalPath); errPersist != nil {
		_ = os.Remove(originalPath)
		return originalName, http.StatusInternalServerError, errPersist
	}
	if errDelete := h.deleteTokenRecord(ctx, recyclePath); errDelete != nil {
		return originalName, http.StatusInternalServerError, errDelete
	}
	if errRemove := os.Remove(recyclePath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
		return originalName, http.StatusInternalServerError, errRemove
	}
	auth, errBuild := h.buildAuthFromFileData(originalPath, record.Content)
	if errBuild != nil {
		return originalName, http.StatusInternalServerError, errBuild
	}
	if errUpsert := h.upsertAuthRecord(ctx, auth); errUpsert != nil {
		return originalName, http.StatusInternalServerError, errUpsert
	}
	return originalName, http.StatusOK, nil
}

func (h *Handler) permanentlyDeleteRecycledAuthFile(ctx context.Context, name string) (int, error) {
	if isUnsafeAuthFileName(name) {
		return http.StatusBadRequest, fmt.Errorf("invalid name")
	}
	h.authRecycleMu.Lock()
	defer h.authRecycleMu.Unlock()
	path := filepath.Join(h.recycleBinDir(), filepath.Base(name))
	if _, errStat := os.Stat(path); errStat != nil {
		if errors.Is(errStat, os.ErrNotExist) {
			return http.StatusNotFound, errAuthFileNotFound
		}
		return http.StatusInternalServerError, errStat
	}
	if errDelete := h.deleteTokenRecord(ctx, path); errDelete != nil {
		return http.StatusInternalServerError, errDelete
	}
	if errRemove := os.Remove(path); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
		return http.StatusInternalServerError, errRemove
	}
	return http.StatusOK, nil
}

func (h *Handler) listRecycleBin() ([]authRecycleListItem, error) {
	if h == nil || h.cfg == nil {
		return nil, fmt.Errorf("handler unavailable")
	}
	h.authRecycleMu.Lock()
	defer h.authRecycleMu.Unlock()
	entries, errRead := os.ReadDir(h.recycleBinDir())
	if errors.Is(errRead, os.ErrNotExist) {
		return []authRecycleListItem{}, nil
	}
	if errRead != nil {
		return nil, fmt.Errorf("failed to read recycle bin: %w", errRead)
	}
	items := make([]authRecycleListItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		path := filepath.Join(h.recycleBinDir(), entry.Name())
		record, errRecord := readAuthRecycleRecord(path)
		if errRecord != nil {
			continue
		}
		metadata := make(map[string]any)
		_ = json.Unmarshal(record.Content, &metadata)
		item := authRecycleListItem{
			Name:         entry.Name(),
			OriginalName: record.Recycle.OriginalName,
			DeletedAt:    record.Recycle.DeletedAt,
			Reason:       record.Recycle.Reason,
			Provider:     strings.TrimSpace(fmt.Sprint(metadata["type"])),
			Email:        strings.TrimSpace(fmt.Sprint(metadata["email"])),
			Size:         int64(len(record.Content)),
		}
		if item.Provider == "<nil>" {
			item.Provider = ""
		}
		if item.Email == "<nil>" {
			item.Email = ""
		}
		items = append(items, item)
	}
	sort.Slice(items, func(left, right int) bool { return items[left].DeletedAt.After(items[right].DeletedAt) })
	return items, nil
}

func (h *Handler) recycleBinDir() string {
	if h == nil || h.cfg == nil {
		return ""
	}
	return filepath.Join(h.cfg.AuthDir, authRecycleDirectoryName)
}

func (h *Handler) persistAuthFilePaths(ctx context.Context, message string, paths ...string) error {
	store := h.tokenStoreWithBaseDir()
	if persister, ok := store.(authFilePersister); ok {
		return persister.PersistAuthFiles(ctx, message, paths...)
	}
	return nil
}

func readAuthRecycleRecord(path string) (*authRecycleRecord, error) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, errRead
	}
	var record authRecycleRecord
	if errJSON := json.Unmarshal(raw, &record); errJSON != nil {
		return nil, fmt.Errorf("invalid recycle record: %w", errJSON)
	}
	if record.Recycle.Version != 1 || strings.TrimSpace(record.Recycle.OriginalName) == "" || len(record.Content) == 0 {
		return nil, fmt.Errorf("invalid recycle record")
	}
	return &record, nil
}

func uniqueRecyclePath(dir, originalName string) string {
	timestamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	base := timestamp + "-" + filepath.Base(originalName)
	path := filepath.Join(dir, base)
	for index := 2; ; index++ {
		if _, errStat := os.Stat(path); errStat != nil {
			return path
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d-%s", timestamp, index, filepath.Base(originalName)))
	}
}

func permanentInvalidAuthReason(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.LastError != nil && auth.LastError.StatusCode() == http.StatusUnauthorized {
		return "unauthorized"
	}
	message := strings.ToLower(strings.TrimSpace(auth.StatusMessage))
	for _, reason := range []string{"invalid_grant", "refresh_token_reused", "invalid or expired token", "invalid_api_key", "unauthorized"} {
		if strings.Contains(message, reason) {
			return reason
		}
	}
	return ""
}

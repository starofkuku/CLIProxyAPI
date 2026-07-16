package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAuthFileRecycleDeleteRestoreAndPermanentDelete(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = fileauth.NewFileTokenStore()
	fileName := "account.json"
	content := []byte(`{"type":"codex","email":"user@example.com","access_token":"token"}`)
	if errWrite := h.writeAuthFile(context.Background(), fileName, content); errWrite != nil {
		t.Fatal(errWrite)
	}

	deleteRecorder := httptest.NewRecorder()
	deleteContext, _ := gin.CreateTestContext(deleteRecorder)
	deleteContext.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(fileName), nil)
	h.DeleteAuthFile(deleteContext)
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	if _, errStat := os.Stat(filepath.Join(authDir, fileName)); !os.IsNotExist(errStat) {
		t.Fatalf("active file should be absent: %v", errStat)
	}
	items, errList := h.listRecycleBin()
	if errList != nil || len(items) != 1 {
		t.Fatalf("recycle items = %+v, err = %v", items, errList)
	}
	if len(manager.List()) != 0 {
		t.Fatalf("recycled auth should be removed from manager")
	}

	restoreBody, _ := json.Marshal(map[string]any{"names": []string{items[0].Name}})
	restoreRecorder := httptest.NewRecorder()
	restoreContext, _ := gin.CreateTestContext(restoreRecorder)
	restoreContext.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/recycle-bin/restore", bytes.NewReader(restoreBody))
	restoreContext.Request.Header.Set("Content-Type", "application/json")
	h.RestoreRecycledAuthFiles(restoreContext)
	if restoreRecorder.Code != http.StatusOK {
		t.Fatalf("restore status = %d: %s", restoreRecorder.Code, restoreRecorder.Body.String())
	}
	if restored, errRead := os.ReadFile(filepath.Join(authDir, fileName)); errRead != nil || string(restored) != string(content) {
		t.Fatalf("restored content = %q, err = %v", restored, errRead)
	}
	if len(manager.List()) != 1 {
		t.Fatalf("restored auth should return to manager")
	}

	deleteRecorder = httptest.NewRecorder()
	deleteContext, _ = gin.CreateTestContext(deleteRecorder)
	deleteContext.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(fileName), nil)
	h.DeleteAuthFile(deleteContext)
	items, errList = h.listRecycleBin()
	if errList != nil || len(items) != 1 {
		t.Fatalf("recycle after second delete = %+v, err = %v", items, errList)
	}

	permanentBody, _ := json.Marshal(map[string]any{"names": []string{items[0].Name}})
	permanentRecorder := httptest.NewRecorder()
	permanentContext, _ := gin.CreateTestContext(permanentRecorder)
	permanentContext.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files/recycle-bin", bytes.NewReader(permanentBody))
	permanentContext.Request.Header.Set("Content-Type", "application/json")
	h.PermanentlyDeleteRecycledAuthFiles(permanentContext)
	if permanentRecorder.Code != http.StatusOK {
		t.Fatalf("permanent delete status = %d: %s", permanentRecorder.Code, permanentRecorder.Body.String())
	}
	items, errList = h.listRecycleBin()
	if errList != nil || len(items) != 0 {
		t.Fatalf("recycle should be empty: %+v, err = %v", items, errList)
	}
}

func TestTrashInvalidAuthFilesOnlyMovesPermanentAuthErrors(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = fileauth.NewFileTokenStore()
	for _, name := range []string{"invalid.json", "quota.json", "disabled.json"} {
		if errWrite := h.writeAuthFile(context.Background(), name, []byte(`{"type":"codex","access_token":"token"}`)); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	invalid := h.findAuthForDelete("invalid.json")
	invalid.Status = coreauth.StatusError
	invalid.StatusMessage = "unauthorized"
	invalid.Unavailable = true
	if _, errUpdate := manager.Update(context.Background(), invalid); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	quota := h.findAuthForDelete("quota.json")
	quota.Status = coreauth.StatusError
	quota.StatusMessage = "quota exhausted"
	quota.Unavailable = true
	if _, errUpdate := manager.Update(context.Background(), quota); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	disabled := h.findAuthForDelete("disabled.json")
	disabled.Status = coreauth.StatusDisabled
	disabled.Disabled = true
	disabled.StatusMessage = "disabled via management API"
	if _, errUpdate := manager.Update(context.Background(), disabled); errUpdate != nil {
		t.Fatal(errUpdate)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/trash-invalid", nil)
	h.TrashInvalidAuthFiles(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Matched int `json:"matched"`
		Deleted int `json:"deleted"`
	}
	if errJSON := json.Unmarshal(recorder.Body.Bytes(), &response); errJSON != nil {
		t.Fatal(errJSON)
	}
	if response.Matched != 1 || response.Deleted != 1 {
		t.Fatalf("unexpected response: %+v", response)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, "invalid.json")); !os.IsNotExist(errStat) {
		t.Fatalf("invalid auth should be recycled: %v", errStat)
	}
	for _, name := range []string{"quota.json", "disabled.json"} {
		if _, errStat := os.Stat(filepath.Join(authDir, name)); errStat != nil {
			t.Fatalf("%s should remain active: %v", name, errStat)
		}
	}
}

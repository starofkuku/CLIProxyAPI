package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
)

func TestSyncManagementPanel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)

	called := false
	handler := &Handler{
		cfg: &config.Config{
			SDKConfig: config.SDKConfig{ProxyURL: "http://proxy.example"},
			RemoteManagement: config.RemoteManagement{
				PanelGitHubRepository: "https://github.com/example/panel",
			},
		},
		configFilePath: "/tmp/config.yaml",
		managementPanelSync: func(_ context.Context, gotDir, proxyURL, repository string) (managementasset.ManagementHTMLSyncResult, error) {
			called = true
			if gotDir != staticDir {
				t.Fatalf("static dir = %q, want %q", gotDir, staticDir)
			}
			if proxyURL != "http://proxy.example" {
				t.Fatalf("proxy URL = %q, want %q", proxyURL, "http://proxy.example")
			}
			if repository != "https://github.com/example/panel" {
				t.Fatalf("repository = %q, want expected repository", repository)
			}
			return managementasset.ManagementHTMLSyncResult{Available: true, Updated: true, Hash: "abc123"}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/management-panel/sync", nil)
	handler.SyncManagementPanel(c)

	if !called {
		t.Fatal("management panel sync function was not called")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["status"] != "updated" || response["updated"] != true || response["sha256"] != "abc123" {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestSyncManagementPanelRejectsDisabledPanel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{
		cfg: &config.Config{
			RemoteManagement: config.RemoteManagement{DisableControlPanel: true},
		},
		managementPanelSync: func(context.Context, string, string, string) (managementasset.ManagementHTMLSyncResult, error) {
			t.Fatal("sync function should not be called")
			return managementasset.ManagementHTMLSyncResult{}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/management-panel/sync", nil)
	handler.SyncManagementPanel(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d body=%s", w.Code, http.StatusConflict, w.Body.String())
	}
}

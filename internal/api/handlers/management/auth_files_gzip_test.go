package management

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFiles_GzipResponse(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	t.Run("manager", func(t *testing.T) {
		manager := coreauth.NewManager(nil, nil, nil)
		record := &coreauth.Auth{
			ID:       "runtime-only-auth-1",
			Provider: "codex",
			Attributes: map[string]string{
				"runtime_only": "true",
			},
			Metadata: map[string]any{"type": "codex"},
		}
		if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
			t.Fatalf("register auth: %v", errRegister)
		}

		h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
		assertCompressedAuthFiles(t, h, "runtime-only-auth-1")
	})

	t.Run("disk fallback", func(t *testing.T) {
		authDir := t.TempDir()
		name := "codex-user.json"
		if errWrite := os.WriteFile(
			filepath.Join(authDir, name),
			[]byte(`{"type":"codex","email":"user@example.com"}`),
			0o600,
		); errWrite != nil {
			t.Fatalf("write auth file: %v", errWrite)
		}

		h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
		assertCompressedAuthFiles(t, h, name)
	})
}

func assertCompressedAuthFiles(t *testing.T, h *Handler, wantName string) {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	c.Request.Header.Set("Accept-Encoding", "gzip, deflate, br")

	h.ListAuthFiles(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := recorder.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}

	reader, errReader := gzip.NewReader(recorder.Body)
	if errReader != nil {
		t.Fatalf("create gzip reader: %v", errReader)
	}
	body, errRead := io.ReadAll(reader)
	if errRead != nil {
		t.Fatalf("read gzip response: %v", errRead)
	}
	if errClose := reader.Close(); errClose != nil {
		t.Fatalf("close gzip reader: %v", errClose)
	}

	var payload struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if len(payload.Files) != 1 || payload.Files[0].Name != wantName {
		t.Fatalf("files = %#v, want one file named %q", payload.Files, wantName)
	}
}

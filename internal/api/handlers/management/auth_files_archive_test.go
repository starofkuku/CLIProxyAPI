package management

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestUploadAuthFileArchiveNestedZIP(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	inner := buildTestZIP(t, map[string][]byte{
		"deep/beta.json": []byte(`{"type":"claude","email":"beta@example.com"}`),
	})
	outer := buildTestZIP(t, map[string][]byte{
		"accounts/alpha.json": []byte(`{"type":"codex","email":"alpha@example.com"}`),
		"accounts/bad.json":   []byte(`[1,2,3]`),
		"nested/inner.zip":    inner,
		"notes.txt":           []byte("ignored"),
	})

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	recorder := performArchiveUpload(t, h, "outer.zip", outer)

	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}
	var response struct {
		Archives   int                  `json:"archives"`
		JSONFound  int                  `json:"json_found"`
		Uploaded   int                  `json:"uploaded"`
		Failed     int                  `json:"failed_count"`
		Skipped    int                  `json:"skipped"`
		Files      []string             `json:"files"`
		FailureLog []authArchiveFailure `json:"failed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Archives != 2 || response.JSONFound != 3 || response.Uploaded != 2 || response.Failed != 1 || response.Skipped != 1 {
		t.Fatalf("unexpected response: %+v", response)
	}
	if len(response.Files) != 2 || len(response.FailureLog) != 1 {
		t.Fatalf("unexpected files or failures: %+v", response)
	}
	for _, name := range response.Files {
		if filepath.Ext(name) != ".json" {
			t.Fatalf("unexpected output name: %s", name)
		}
		if _, err := os.Stat(filepath.Join(authDir, name)); err != nil {
			t.Fatalf("uploaded file %s missing: %v", name, err)
		}
	}
	if got := len(manager.List()); got != 2 {
		t.Fatalf("manager auth count = %d, want 2", got)
	}
}

func TestUploadAuthFileArchiveTarGzip(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	archive := buildTestTarGzip(t, map[string][]byte{
		"folder/account.json": []byte(`{"type":"codex","email":"tar@example.com"}`),
	})
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	recorder := performArchiveUpload(t, h, "accounts.tar.gz", archive)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response struct {
		Archives  int `json:"archives"`
		JSONFound int `json:"json_found"`
		Uploaded  int `json:"uploaded"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Archives != 2 || response.JSONFound != 1 || response.Uploaded != 1 {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestSafeArchiveEntryNameRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../secret.json", "/absolute.json", "folder/../../secret.json"} {
		if _, ok := safeArchiveEntryName(name); ok {
			t.Fatalf("expected unsafe path to be rejected: %s", name)
		}
	}
	if got, ok := safeArchiveEntryName("folder/account.json"); !ok || got != "folder/account.json" {
		t.Fatalf("safe path = %q, %v", got, ok)
	}
}

func performArchiveUpload(t *testing.T, h *Handler, name string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/archive", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = request
	h.UploadAuthFileArchive(ctx)
	return recorder
}

func buildTestZIP(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, data := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func buildTestTarGzip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, data := range files {
		header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

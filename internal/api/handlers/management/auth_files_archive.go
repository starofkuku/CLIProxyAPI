package management

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	authArchiveMaxUploadBytes   = 100 << 20
	authArchiveMaxExpandedBytes = 512 << 20
	authArchiveMaxJSONBytes     = 10 << 20
	authArchiveMaxEntries       = 10000
	authArchiveMaxDepth         = 5
)

type authArchiveFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

type authArchiveImport struct {
	handler       *Handler
	ctx           context.Context
	usedNames     map[string]int
	expandedBytes int64
	entries       int
	archives      int
	jsonFound     int
	uploaded      []string
	failures      []authArchiveFailure
	skipped       int
}

type authArchiveKind int

const (
	authArchiveUnknown authArchiveKind = iota
	authArchiveZIP
	authArchiveTAR
	authArchiveGZIP
)

// UploadAuthFileArchive imports JSON auth files from an archive, including nested archives.
func (h *Handler) UploadAuthFileArchive(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	file, errFile := c.FormFile("file")
	if errFile != nil || file == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "archive file is required"})
		return
	}
	if file.Size <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "archive file is empty"})
		return
	}
	if file.Size > authArchiveMaxUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "archive exceeds 100 MiB upload limit"})
		return
	}

	source, errOpen := file.Open()
	if errOpen != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("failed to open archive: %v", errOpen)})
		return
	}
	defer func() { _ = source.Close() }()

	data, errRead := readArchiveEntry(source, authArchiveMaxUploadBytes)
	if errRead != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("failed to read archive: %v", errRead)})
		return
	}
	if detectAuthArchiveKind(file.Filename, data) == authArchiveUnknown {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported archive format; use .zip, .tar, .tar.gz, .tgz, or .gz"})
		return
	}

	importer := &authArchiveImport{
		handler:   h,
		ctx:       c.Request.Context(),
		usedNames: make(map[string]int),
	}
	importer.processArchive(filepath.Base(file.Filename), data, 0, "")

	status := "ok"
	httpStatus := http.StatusOK
	if len(importer.failures) > 0 {
		status = "partial"
		httpStatus = http.StatusMultiStatus
	}
	if len(importer.uploaded) == 0 && len(importer.failures) > 0 {
		status = "error"
	}
	c.JSON(httpStatus, gin.H{
		"status":         status,
		"archives":       importer.archives,
		"json_found":     importer.jsonFound,
		"uploaded":       len(importer.uploaded),
		"failed_count":   len(importer.failures),
		"skipped":        importer.skipped,
		"expanded_bytes": importer.expandedBytes,
		"files":          importer.uploaded,
		"failed":         importer.failures,
	})
}

func (i *authArchiveImport) processArchive(name string, data []byte, depth int, prefix string) {
	if depth > authArchiveMaxDepth {
		i.fail(name, fmt.Sprintf("nested archive depth exceeds %d", authArchiveMaxDepth))
		return
	}
	if i.entries > authArchiveMaxEntries {
		i.fail(name, fmt.Sprintf("archive entry limit exceeds %d", authArchiveMaxEntries))
		return
	}

	i.archives++
	switch detectAuthArchiveKind(name, data) {
	case authArchiveZIP:
		i.processZIP(name, data, depth, prefix)
	case authArchiveTAR:
		i.processTAR(name, bytes.NewReader(data), depth, prefix)
	case authArchiveGZIP:
		i.processGZIP(name, data, depth, prefix)
	default:
		i.fail(name, "unsupported or invalid nested archive")
	}
}

func (i *authArchiveImport) processZIP(archiveName string, data []byte, depth int, prefix string) {
	reader, errOpen := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if errOpen != nil {
		i.fail(archiveName, fmt.Sprintf("invalid zip archive: %v", errOpen))
		return
	}
	archivePrefix := joinArchivePath(prefix, trimArchiveExtension(filepath.Base(archiveName)))
	for _, entry := range reader.File {
		if entry == nil || entry.FileInfo().IsDir() {
			continue
		}
		if !i.reserveEntry(entry.Name) {
			return
		}
		entryName, ok := safeArchiveEntryName(entry.Name)
		if !ok {
			i.fail(entry.Name, "unsafe archive path")
			continue
		}
		if !isJSONArchiveEntry(entryName) && !isSupportedArchiveName(entryName) {
			i.skipped++
			continue
		}
		limit := authArchiveMaxUploadBytes
		if isJSONArchiveEntry(entryName) {
			limit = authArchiveMaxJSONBytes
		}
		source, errEntry := entry.Open()
		if errEntry != nil {
			i.fail(entryName, fmt.Sprintf("failed to open entry: %v", errEntry))
			continue
		}
		entryData, errRead := readArchiveEntry(source, int64(limit))
		_ = source.Close()
		if errRead != nil {
			i.fail(entryName, errRead.Error())
			continue
		}
		if !i.consumeExpanded(entryName, int64(len(entryData))) {
			return
		}
		i.processEntry(entryName, entryData, depth, archivePrefix)
	}
}

func (i *authArchiveImport) processTAR(archiveName string, source io.Reader, depth int, prefix string) {
	reader := tar.NewReader(source)
	archivePrefix := joinArchivePath(prefix, trimArchiveExtension(filepath.Base(archiveName)))
	for {
		header, errNext := reader.Next()
		if errors.Is(errNext, io.EOF) {
			return
		}
		if errNext != nil {
			i.fail(archiveName, fmt.Sprintf("invalid tar archive: %v", errNext))
			return
		}
		if header == nil || !header.FileInfo().Mode().IsRegular() {
			continue
		}
		if !i.reserveEntry(header.Name) {
			return
		}
		entryName, ok := safeArchiveEntryName(header.Name)
		if !ok {
			i.fail(header.Name, "unsafe archive path")
			continue
		}
		if !isJSONArchiveEntry(entryName) && !isSupportedArchiveName(entryName) {
			i.skipped++
			continue
		}
		limit := int64(authArchiveMaxUploadBytes)
		if isJSONArchiveEntry(entryName) {
			limit = authArchiveMaxJSONBytes
		}
		entryData, errRead := readArchiveEntry(reader, limit)
		if errRead != nil {
			i.fail(entryName, errRead.Error())
			continue
		}
		if !i.consumeExpanded(entryName, int64(len(entryData))) {
			return
		}
		i.processEntry(entryName, entryData, depth, archivePrefix)
	}
}

func (i *authArchiveImport) processGZIP(archiveName string, data []byte, depth int, prefix string) {
	reader, errOpen := gzip.NewReader(bytes.NewReader(data))
	if errOpen != nil {
		i.fail(archiveName, fmt.Sprintf("invalid gzip archive: %v", errOpen))
		return
	}
	defer func() { _ = reader.Close() }()

	innerName := strings.TrimSuffix(filepath.Base(archiveName), filepath.Ext(archiveName))
	if strings.TrimSpace(reader.Name) != "" {
		innerName = filepath.Base(reader.Name)
	}
	limit := int64(authArchiveMaxUploadBytes)
	if isJSONArchiveEntry(innerName) {
		limit = authArchiveMaxJSONBytes
	}
	innerData, errRead := readArchiveEntry(reader, limit)
	if errRead != nil {
		i.fail(archiveName, errRead.Error())
		return
	}
	if !i.consumeExpanded(innerName, int64(len(innerData))) {
		return
	}
	i.processEntry(innerName, innerData, depth, prefix)
}

func (i *authArchiveImport) processEntry(name string, data []byte, depth int, prefix string) {
	fullName := joinArchivePath(prefix, name)
	if isJSONArchiveEntry(name) {
		i.importJSON(fullName, data)
		return
	}
	if detectAuthArchiveKind(name, data) != authArchiveUnknown {
		i.processArchive(name, data, depth+1, prefix)
		return
	}
	i.fail(fullName, "unsupported nested archive")
}

func (i *authArchiveImport) importJSON(sourceName string, data []byte) {
	i.jsonFound++
	var object map[string]any
	if errJSON := json.Unmarshal(data, &object); errJSON != nil {
		i.fail(sourceName, fmt.Sprintf("invalid JSON object: %v", errJSON))
		return
	}
	if object == nil {
		i.fail(sourceName, "invalid JSON object")
		return
	}

	outputName := i.uniqueOutputName(sourceName)
	if errWrite := i.handler.writeAuthFile(i.ctx, outputName, data); errWrite != nil {
		i.fail(sourceName, errWrite.Error())
		return
	}
	i.uploaded = append(i.uploaded, outputName)
}

func (i *authArchiveImport) reserveEntry(name string) bool {
	i.entries++
	if i.entries <= authArchiveMaxEntries {
		return true
	}
	i.fail(name, fmt.Sprintf("archive entry limit exceeds %d", authArchiveMaxEntries))
	return false
}

func (i *authArchiveImport) consumeExpanded(name string, size int64) bool {
	i.expandedBytes += size
	if i.expandedBytes <= authArchiveMaxExpandedBytes {
		return true
	}
	i.fail(name, "expanded archive data exceeds 512 MiB limit")
	return false
}

func (i *authArchiveImport) fail(name, message string) {
	i.failures = append(i.failures, authArchiveFailure{
		Name:  strings.TrimSpace(name),
		Error: strings.TrimSpace(message),
	})
}

var authArchiveUnsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func (i *authArchiveImport) uniqueOutputName(sourceName string) string {
	clean := strings.TrimSuffix(strings.TrimSpace(sourceName), filepath.Ext(sourceName))
	parts := strings.FieldsFunc(filepath.ToSlash(clean), func(char rune) bool { return char == '/' || char == '\\' })
	for index, part := range parts {
		parts[index] = strings.Trim(authArchiveUnsafeName.ReplaceAllString(part, "_"), "._-")
	}
	base := strings.Trim(strings.Join(parts, "__"), "._-")
	if base == "" {
		base = "auth"
	}
	count := i.usedNames[base]
	i.usedNames[base] = count + 1
	if count > 0 {
		base = fmt.Sprintf("%s_%d", base, count+1)
	}
	return base + ".json"
}

func readArchiveEntry(source io.Reader, limit int64) ([]byte, error) {
	data, errRead := io.ReadAll(io.LimitReader(source, limit+1))
	if errRead != nil {
		return nil, errRead
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("entry exceeds %s limit", formatArchiveByteLimit(limit))
	}
	return data, nil
}

func formatArchiveByteLimit(limit int64) string {
	if limit%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", limit>>20)
	}
	return fmt.Sprintf("%d bytes", limit)
}

func detectAuthArchiveKind(name string, data []byte) authArchiveKind {
	lowerName := strings.ToLower(strings.TrimSpace(name))
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{'P', 'K', 3, 4}) {
		return authArchiveZIP
	}
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		return authArchiveGZIP
	}
	if len(data) >= 262 && string(data[257:262]) == "ustar" {
		return authArchiveTAR
	}
	switch {
	case strings.HasSuffix(lowerName, ".zip"):
		return authArchiveZIP
	case strings.HasSuffix(lowerName, ".tar"):
		return authArchiveTAR
	case strings.HasSuffix(lowerName, ".tar.gz"), strings.HasSuffix(lowerName, ".tgz"), strings.HasSuffix(lowerName, ".gz"):
		return authArchiveGZIP
	default:
		return authArchiveUnknown
	}
}

func isSupportedArchiveName(name string) bool {
	lowerName := strings.ToLower(strings.TrimSpace(name))
	return strings.HasSuffix(lowerName, ".zip") ||
		strings.HasSuffix(lowerName, ".tar") ||
		strings.HasSuffix(lowerName, ".tar.gz") ||
		strings.HasSuffix(lowerName, ".tgz") ||
		strings.HasSuffix(lowerName, ".gz")
}

func isJSONArchiveEntry(name string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), ".json")
}

func safeArchiveEntryName(name string) (string, bool) {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || strings.HasPrefix(name, "/") {
		return "", false
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

func joinArchivePath(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(strings.TrimSpace(filepath.ToSlash(part)), "/")
		if part != "" && part != "." {
			clean = append(clean, part)
		}
	}
	return strings.Join(clean, "/")
}

func trimArchiveExtension(name string) string {
	lowerName := strings.ToLower(name)
	for _, suffix := range []string{".tar.gz", ".tgz", ".zip", ".tar", ".gz"} {
		if strings.HasSuffix(lowerName, suffix) {
			return strings.TrimSuffix(name, name[len(name)-len(suffix):])
		}
	}
	return strings.TrimSuffix(name, filepath.Ext(name))
}

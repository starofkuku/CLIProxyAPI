package management

import (
	"compress/gzip"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

func writeCompressedJSON(c *gin.Context, status int, payload any) {
	c.Header("Vary", "Accept-Encoding")
	if !acceptsGzip(c.GetHeader("Accept-Encoding")) {
		c.JSON(status, payload)
		return
	}

	c.Header("Content-Encoding", "gzip")
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Status(status)

	w, errWriter := gzip.NewWriterLevel(c.Writer, gzip.BestSpeed)
	if errWriter != nil {
		_ = c.Error(errWriter)
		return
	}
	if errEncode := json.NewEncoder(w).Encode(payload); errEncode != nil {
		_ = c.Error(errEncode)
	}
	if errClose := w.Close(); errClose != nil {
		_ = c.Error(errClose)
	}
}

func acceptsGzip(value string) bool {
	var gzipQuality *float64
	var wildcardQuality *float64

	for _, item := range strings.Split(value, ",") {
		parts := strings.Split(item, ";")
		encoding := strings.ToLower(strings.TrimSpace(parts[0]))
		quality := 1.0
		for _, parameter := range parts[1:] {
			name, raw, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
				continue
			}
			parsed, errParse := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if errParse != nil {
				quality = 0
			} else {
				quality = parsed
			}
		}

		switch encoding {
		case "gzip":
			q := quality
			gzipQuality = &q
		case "*":
			q := quality
			wildcardQuality = &q
		}
	}

	if gzipQuality != nil {
		return *gzipQuality > 0
	}
	return wildcardQuality != nil && *wildcardQuality > 0
}

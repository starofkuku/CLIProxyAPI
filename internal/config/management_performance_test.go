package config

import "testing"

func TestParseConfigBytesManagementPerformanceDefaults(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	if !cfg.ManagementPerformance.IsGzipEnabled() {
		t.Fatal("GzipEnabled = false, want true")
	}
	if cfg.ManagementPerformance.UsageRecentCacheEnabled {
		t.Fatal("UsageRecentCacheEnabled = true, want false")
	}
}

func TestParseConfigBytesManagementPerformanceOverrides(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
management-performance:
  gzip-enabled: false
  usage-recent-cache-enabled: true
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	if cfg.ManagementPerformance.IsGzipEnabled() {
		t.Fatal("GzipEnabled = true, want false")
	}
	if !cfg.ManagementPerformance.UsageRecentCacheEnabled {
		t.Fatal("UsageRecentCacheEnabled = false, want true")
	}
}

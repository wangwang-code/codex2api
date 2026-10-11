package database

import (
	"context"
	"path/filepath"
	"testing"
)

// 代理检测目标 URL 必须能跨一次 Update/Get 往返（sqlite 与 postgres 共用同一套 SQL）。
func TestProxyHealthCheckURLPersistenceSQLite(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "proxy-health-check.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	for _, want := range []string{
		"https://auth.openai.com/cdn-cgi/trace",
		"http://127.0.0.1:8080/health",
		"",
	} {
		settings, err := db.GetSystemSettings(ctx)
		if err != nil {
			t.Fatalf("GetSystemSettings: %v", err)
		}
		if settings == nil {
			settings = &SystemSettings{}
		}
		settings.ProxyHealthCheckURL = want
		if err := db.UpdateSystemSettings(ctx, settings); err != nil {
			t.Fatalf("UpdateSystemSettings(%q): %v", want, err)
		}
		got, err := db.GetSystemSettings(ctx)
		if err != nil {
			t.Fatalf("GetSystemSettings after update: %v", err)
		}
		if got == nil || got.ProxyHealthCheckURL != want {
			t.Fatalf("proxy_health_check_url did not persist %q: %#v", want, got)
		}
	}
}

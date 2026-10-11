package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// 代理检测目标 URL 必须前后端打通：PATCH 后同时落库（持久化）并热更新到
// runtime settings（检测逻辑立即读到新值）。
func TestUpdateSettingsPersistsProxyHealthCheckURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	previousRuntime := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previousRuntime) })

	put := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateSettings(ctx)
		return recorder
	}

	const want = "https://auth.openai.com/cdn-cgi/trace"
	recorder := put(`{"proxy_health_check_url":"https://auth.openai.com/cdn-cgi/trace"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response settingsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.ProxyHealthCheckURL != want {
		t.Fatalf("response proxy_health_check_url = %q, want %q", response.ProxyHealthCheckURL, want)
	}
	if got := store.ProxyHealthCheckURL(); got != want {
		t.Fatalf("store value = %q, want %q", got, want)
	}
	if got := proxy.CurrentRuntimeSettings().ProxyHealthCheckURL; got != want {
		t.Fatalf("runtime settings = %q, want %q", got, want)
	}
	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if persisted.ProxyHealthCheckURL != want {
		t.Fatalf("persisted value = %q, want %q", persisted.ProxyHealthCheckURL, want)
	}

	// 非法 scheme 必须整体拒绝，且不能覆盖已保存的值。
	recorder = put(`{"proxy_health_check_url":"socks5://127.0.0.1:1080"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid url status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if got := store.ProxyHealthCheckURL(); got != want {
		t.Fatalf("store value after rejected update = %q, want %q", got, want)
	}

	// 空串表示关闭该功能，回到内置检测目标。
	recorder = put(`{"proxy_health_check_url":""}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("clear status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := proxy.CurrentRuntimeSettings().ProxyHealthCheckURL; got != "" {
		t.Fatalf("runtime settings after clear = %q, want empty", got)
	}
	persisted, err = db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings after clear: %v", err)
	}
	if persisted.ProxyHealthCheckURL != "" {
		t.Fatalf("persisted value after clear = %q, want empty", persisted.ProxyHealthCheckURL)
	}
}

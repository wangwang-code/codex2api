package proxy

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// newAccountlessTestHandler 构造一个「池里一个账号都没有」的 handler。
// 用于验证无账号请求既不会把客户端挂住，也不会从请求记录里消失。
func newAccountlessTestHandler(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "accountless.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Key: modelQuotaTestKey, Name: "accountless",
	}); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4, MaxRetries: 0, MaxRateLimitRetries: 0})
	t.Cleanup(store.Stop)
	// indexed 引擎原本会在「没有任何结构性候选」时也等满调度超时。
	store.SetFastSchedulerEnabled(true)

	h := NewHandler(store, db, &config.Config{}, nil)
	router := gin.New()
	h.RegisterRoutes(router)
	return h, router
}

// TestChatCompletionsWithoutAccountsFailsFastAndIsRecorded 覆盖两个已知偏差：
//  1. 池里没有任何账号时，请求应当立即 503，而不是等满调度超时把客户端挂住；
//  2. 这类失败必须进用量记录（AccountID 为 0），而不是彻底消失。
func TestChatCompletionsWithoutAccountsFailsFastAndIsRecorded(t *testing.T) {
	// 把调度等待设成 5s：修复后根本等不到这个超时，若回归会明显超时。
	previousWait := dispatchAccountWaitTimeout
	dispatchAccountWaitTimeout = 5 * time.Second
	t.Cleanup(func() { dispatchAccountWaitTimeout = previousWait })

	h, router := newAccountlessTestHandler(t)
	started := time.Now()
	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`)
	elapsed := time.Since(started)
	t.Logf("status=%d elapsed=%s body=%q", response.Code, elapsed, response.Body.String())

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("池里没有账号时等待了 %s，应当立即失败", elapsed)
	}

	h.db.FlushUsageLogs()
	logs, err := h.db.ListUsageLogsByFilter(context.Background(), database.UsageLogFilter{
		Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), IncludeCanceled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("usage log rows = %d, want 1（无账号失败也必须进记录）", len(logs))
	}
	entry := logs[0]
	t.Logf("logged: status=%d account=%d kind=%q message=%q", entry.StatusCode, entry.AccountID, entry.UpstreamErrorKind, entry.ErrorMessage)
	if entry.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("logged status = %d, want 503", entry.StatusCode)
	}
	if entry.AccountID != 0 {
		t.Fatalf("没有账号参与，AccountID = %d, want 0", entry.AccountID)
	}
	if entry.Endpoint != "/v1/chat/completions" {
		t.Fatalf("logged endpoint = %q", entry.Endpoint)
	}
	if !strings.Contains(entry.ErrorMessage, "无可用账号") {
		t.Fatalf("logged message = %q, want the client-facing pool message", entry.ErrorMessage)
	}
}

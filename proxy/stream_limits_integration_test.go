package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// newStreamLimitTestHandler 构造一个可指定重试次数的 handler，用于验证预算中止
// 不会被重试链换号重放。
func newStreamLimitTestHandler(t *testing.T, upstreamURL string, maxRetries int) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "stream-limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Key: modelQuotaTestKey, Name: "stream-limits",
	}); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4, MaxRetries: maxRetries, MaxRateLimitRetries: 0})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL: upstreamURL, APIKey: "relay-test", PlanType: "api",
		Models: []string{"gpt-6-astra"},
	})
	h := NewHandler(store, db, &config.Config{}, nil)
	router := gin.New()
	h.RegisterRoutes(router)
	return h, router
}

// withStreamLimitRule 临时安装一条流预算规则。
func withStreamLimitRule(t *testing.T, rule streamLimitRule) {
	t.Helper()
	previousEnabled, previousRules := streamLimitsEnabled, streamLimitRules
	streamLimitsEnabled, streamLimitRules = true, []streamLimitRule{rule}
	t.Cleanup(func() { streamLimitsEnabled, streamLimitRules = previousEnabled, previousRules })
}

// streamLimitUpstream 返回一个按 deltaCount 个内容帧回放的假上游，并统计被调用次数。
func streamLimitUpstream(t *testing.T, calls *atomic.Int32, deltaCount int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_limit"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_item.added","item":{"type":"message"}}`+"\n\n")
		for i := 0; i < deltaCount; i++ {
			_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"A"}`+"\n\n")
		}
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.done"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_limit","status":"completed","usage":{"input_tokens":1,"output_tokens":`+fmt.Sprint(deltaCount)+`}}}`+"\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestChatCompletionsStreamLimitAllowsNormalOutput 是防误杀的回归用例：
// 上游回了 20 个内容帧（每帧 1 字符），预算 22 字符，必须完整放行。
//
// 这条正是 CPA 踩坑的地方——它按下游 SSE 帧字节计预算，每帧 JSON 信封约 281 字节
// 只装 1-3 个答案字符，于是这种「帧多字少」的正常响应会被误判成输出失控。
func TestChatCompletionsStreamLimitAllowsNormalOutput(t *testing.T) {
	withStreamLimitRule(t, streamLimitRule{Name: "translation", BaseChars: 20, CharsPerInputChar: 1, MinChars: 20})

	var calls atomic.Int32
	upstream := streamLimitUpstream(t, &calls, 20)
	_, router := newStreamLimitTestHandler(t, upstream.URL, 3)

	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	body := response.Body.String()
	t.Logf("status=%d calls=%d contentFrames=%d", response.Code, calls.Load(), strings.Count(body, `"content":"A"`))

	if strings.Contains(body, "upstream_response_too_large") {
		t.Fatalf("正常输出被误杀: %q", body)
	}
	if got := strings.Count(body, `"content":"A"`); got != 20 {
		t.Fatalf("放行了 %d 个内容帧, want 20: %q", got, body)
	}
}

// TestChatCompletionsStreamLimitAbortsRunawayOutput 验证输出失控时按固定 payload
// 中止，且不换号重试（上游只被调用一次）。
func TestChatCompletionsStreamLimitAbortsRunawayOutput(t *testing.T) {
	withStreamLimitRule(t, streamLimitRule{Name: "translation", BaseChars: 20, CharsPerInputChar: 1, MinChars: 20})

	var calls atomic.Int32
	upstream := streamLimitUpstream(t, &calls, 200)
	h, router := newStreamLimitTestHandler(t, upstream.URL, 3)

	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	body := response.Body.String()
	t.Logf("status=%d calls=%d contentFrames=%d", response.Code, calls.Load(), strings.Count(body, `"content":"A"`))

	if !strings.Contains(body, "upstream_response_too_large") {
		t.Fatalf("客户端未收到预算中止 payload: %q", body)
	}
	if got := strings.Count(body, `"content":"A"`); got > 30 {
		t.Fatalf("放行了 %d 个内容帧，预算约 22 字符就该中止", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("上游被调用 %d 次，预算中止是请求级终态，不应换号重试", got)
	}

	h.db.FlushUsageLogs()
	logs, err := h.db.ListUsageLogsByFilter(context.Background(), database.UsageLogFilter{
		Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), IncludeCanceled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("usage log rows = %d, want 1", len(logs))
	}
	entry := logs[0]
	t.Logf("logged: status=%d kind=%q message=%q", entry.StatusCode, entry.UpstreamErrorKind, entry.ErrorMessage)
	if entry.UpstreamErrorKind != "stream_budget" {
		t.Fatalf("logged kind = %q, want stream_budget", entry.UpstreamErrorKind)
	}
	if entry.StatusCode != http.StatusBadGateway {
		t.Fatalf("logged status = %d, want 502", entry.StatusCode)
	}
}

// TestChatCompletionsStreamLimitDisabledPassesThrough 验证总开关关闭时完全不影响既有行为。
func TestChatCompletionsStreamLimitDisabledPassesThrough(t *testing.T) {
	previousEnabled := streamLimitsEnabled
	streamLimitsEnabled = false
	t.Cleanup(func() { streamLimitsEnabled = previousEnabled })

	var calls atomic.Int32
	upstream := streamLimitUpstream(t, &calls, 200)
	_, router := newStreamLimitTestHandler(t, upstream.URL, 0)

	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	body := response.Body.String()

	if strings.Contains(body, "upstream_response_too_large") {
		t.Fatalf("关闭时不应中止: %q", body)
	}
	if got := strings.Count(body, `"content":"A"`); got != 200 {
		t.Fatalf("关闭时应当放行全部 %d 个内容帧，实际 %d", 200, got)
	}
}

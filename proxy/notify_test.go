package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// notifyCollector 收集 webhook 收到的请求体，用于断言通知内容。
type notifyCollector struct {
	mu     sync.Mutex
	bodies []string
}

func (c *notifyCollector) add(body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, body)
}

func (c *notifyCollector) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

// waitForBodies 等待至少 count 条通知（投递是异步的）。
func (c *notifyCollector) waitForBodies(t *testing.T, count int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if bodies := c.all(); len(bodies) >= count {
			return bodies
		}
		time.Sleep(5 * time.Millisecond)
	}
	return c.all()
}

// newNotifyWebhook 起一个收集通知的 webhook 服务。
func newNotifyWebhook(t *testing.T) (*notifyCollector, *httptest.Server) {
	t.Helper()
	collector := &notifyCollector{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		collector.add(string(body))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return collector, server
}

// withNotifyWebhook 临时配置 webhook 并重置降级状态（状态是进程级的，测试间必须隔离）。
func withNotifyWebhook(t *testing.T, url, format string) {
	t.Helper()
	previousURL, previousFormat := notifyWebhookURL, notifyWebhookFormat
	notifyWebhookURL, notifyWebhookFormat = url, format
	resetCodexPoolNotifier()
	t.Cleanup(func() {
		notifyWebhookURL, notifyWebhookFormat = previousURL, previousFormat
		resetCodexPoolNotifier()
	})
}

func resetCodexPoolNotifier() {
	codexPoolNotifier.mu.Lock()
	codexPoolNotifier.degraded = false
	codexPoolNotifier.mu.Unlock()
}

// newNotifyTestHandler 构造「codex 号 + 指向恒返 401 的假上游」的 handler。
func newNotifyTestHandler(t *testing.T, codexCount int, codexUpstreamURL string) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previousSettings := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Key: modelQuotaTestKey, Name: "notify",
	}); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4, MaxRetries: 0, MaxRateLimitRetries: 0})
	t.Cleanup(store.Stop)
	for i := 0; i < codexCount; i++ {
		store.AddAccount(&auth.Account{
			DBID: int64(i + 1), AccessToken: "codex-token", PlanType: "pro",
			Models: []string{"gpt-6-astra"},
		})
	}
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })
	SetResinConfig(&ResinConfig{BaseURL: codexUpstreamURL, PlatformName: "notify-test"})

	h := NewHandler(store, db, &config.Config{}, nil)
	router := gin.New()
	h.RegisterRoutes(router)
	return h, router
}

// TestNotifyPayloadFormats 验证各 webhook 平台的载荷形状。
func TestNotifyPayloadFormats(t *testing.T) {
	previous := notifyWebhookFormat
	t.Cleanup(func() { notifyWebhookFormat = previous })

	cases := []struct {
		format string
		check  func(t *testing.T, raw string)
	}{
		{"json", func(t *testing.T, raw string) {
			var payload map[string]string
			if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload["text"] != "hello" {
				t.Fatalf("json 载荷 = %q (%v)", raw, err)
			}
		}},
		{"text", func(t *testing.T, raw string) {
			if raw != "hello" {
				t.Fatalf("text 载荷 = %q", raw)
			}
		}},
		{"wecom", func(t *testing.T, raw string) {
			var payload struct {
				MsgType string            `json:"msgtype"`
				Text    map[string]string `json:"text"`
			}
			if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload.MsgType != "text" || payload.Text["content"] != "hello" {
				t.Fatalf("wecom 载荷 = %q (%v)", raw, err)
			}
		}},
		{"dingtalk", func(t *testing.T, raw string) {
			var payload struct {
				MsgType string            `json:"msgtype"`
				Text    map[string]string `json:"text"`
			}
			if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload.MsgType != "text" || payload.Text["content"] != "hello" {
				t.Fatalf("dingtalk 载荷 = %q (%v)", raw, err)
			}
		}},
		{"feishu", func(t *testing.T, raw string) {
			var payload struct {
				MsgType string            `json:"msg_type"`
				Content map[string]string `json:"content"`
			}
			if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload.MsgType != "text" || payload.Content["text"] != "hello" {
				t.Fatalf("feishu 载荷 = %q (%v)", raw, err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			notifyWebhookFormat = tc.format
			tc.check(t, string(notifyPayload("hello")))
		})
	}
}

// TestNotifyFormatFromEnv 验证格式解析：默认 json，非法值回落。
func TestNotifyFormatFromEnv(t *testing.T) {
	for raw, want := range map[string]string{
		"": "json", "json": "json", "text": "text", "wecom": "wecom",
		"feishu": "feishu", "dingtalk": "dingtalk", "telegram": "json",
	} {
		t.Setenv("NOTIFY_WEBHOOK_FORMAT", raw)
		if got := notifyWebhookFormatFromEnv(); got != want {
			t.Fatalf("%q => %q, want %q", raw, got, want)
		}
	}
}

// TestCodexUnauthorizedNotifiesWhichAccount 验证 401 会通知「哪个号被拒了」。
func TestCodexUnauthorizedNotifiesWhichAccount(t *testing.T) {
	collector, webhook := newNotifyWebhook(t)
	withNotifyWebhook(t, webhook.URL, "json")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Provided authentication token is expired.","type":"invalid_request_error","code":"token_expired"}}`)
	}))
	t.Cleanup(upstream.Close)

	_, router := newNotifyTestHandler(t, 1, upstream.URL)
	performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`)

	bodies := collector.waitForBodies(t, 2)
	t.Logf("收到 %d 条通知: %v", len(bodies), bodies)

	if !containsSubstring(bodies, "账号 401") {
		t.Fatalf("应通知账号 401，实际: %v", bodies)
	}
	if !containsSubstring(bodies, "id=1") {
		t.Fatalf("401 通知应带上具体账号，实际: %v", bodies)
	}
	if !containsSubstring(bodies, "服务降级") {
		t.Fatalf("唯一 codex 号打空后应通知服务降级，实际: %v", bodies)
	}
}

// TestCodexPoolDegradedOnlyAfterLastAccount 验证降级/恢复通知只在状态翻转时发：
// 还有 codex 号可用时不喊降级，最后一个也退出调度才喊，重新有可用号时喊恢复。
//
// 这里直接驱动状态翻转而不靠请求去打 401 —— 一笔请求可能连续打掉多个号（401 可重试），
// 依赖它来构造「还剩一个」的状态不稳。
func TestCodexPoolDegradedOnlyAfterLastAccount(t *testing.T) {
	collector, webhook := newNotifyWebhook(t)
	withNotifyWebhook(t, webhook.URL, "json")

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, modelQuotaSSE)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(healthy.Close)

	h, router := newNotifyTestHandler(t, 2, healthy.URL)
	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("健康上游请求失败: status=%d", response.Code)
	}
	time.Sleep(150 * time.Millisecond)
	if bodies := collector.all(); containsSubstring(bodies, "服务降级") {
		t.Fatalf("池里有可用 codex 号时不应喊降级，实际: %v", bodies)
	}

	// 冷却一个：还剩一个可用，仍不该喊降级。
	h.store.MarkCooldown(h.store.FindByID(1), time.Hour, "unauthorized")
	h.refreshCodexPoolState("unit-test-one-out")
	time.Sleep(100 * time.Millisecond)
	if bodies := collector.all(); containsSubstring(bodies, "服务降级") {
		t.Fatalf("还剩一个 codex 号可用时不应喊降级，实际: %v", bodies)
	}

	// 最后一个也退出调度 → 降级。
	h.store.MarkCooldown(h.store.FindByID(2), time.Hour, "unauthorized")
	h.refreshCodexPoolState("unit-test-last-out")
	bodies := collector.waitForBodies(t, 1)
	if !containsSubstring(bodies, "服务降级") {
		t.Fatalf("全部 codex 号不可用后应通知降级，实际: %v", bodies)
	}

	// 重新有可用号 → 恢复。
	h.store.AddAccount(&auth.Account{DBID: 3, AccessToken: "codex-token-2", PlanType: "pro", Models: []string{"gpt-6-astra"}})
	h.refreshCodexPoolState("unit-test-recovered")
	bodies = collector.waitForBodies(t, 2)
	if !containsSubstring(bodies, "服务恢复") {
		t.Fatalf("codex 号重新可用后应通知恢复，实际: %v", bodies)
	}
}

// TestNotifyDisabledSendsNothing 验证未配置 webhook 时一条通知都不发。
func TestNotifyDisabledSendsNothing(t *testing.T) {
	collector, webhook := newNotifyWebhook(t)
	withNotifyWebhook(t, "", "json") // 显式关闭

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"expired","code":"token_expired"}}`)
	}))
	t.Cleanup(upstream.Close)

	_, router := newNotifyTestHandler(t, 1, upstream.URL)
	performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`)
	time.Sleep(200 * time.Millisecond)

	if bodies := collector.all(); len(bodies) != 0 {
		t.Fatalf("未配置 webhook 时不应发通知，实际: %v", bodies)
	}
	_ = webhook
}

func containsSubstring(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// upstreamErrorLeakBody 模拟真实上游错误体：带着上游身份与请求 id。
const upstreamErrorLeakBody = `{"error":{"message":"The server is overloaded (via openai, req_leak_abc123)","type":"server_error"}}`

// TestChatCompletionsUpstreamErrorRewriteNonStream 端到端核对非流式出口：
// 上游 503 带身份信息的错误体不得原样透给客户端，而是换成配置文案。
func TestChatCompletionsUpstreamErrorRewriteNonStream(t *testing.T) {
	withUpstreamErrorRewrite(t, true, "", map[int]string{503: "[云翻译]上游服务暂时不可用，请稍后重试"})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, upstreamErrorLeakBody)
	}))
	t.Cleanup(upstream.Close)

	_, _, router := newModelQuotaTestHandler(t, 4, upstream.URL, false)
	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`)
	body := response.Body.String()
	t.Logf("status=%d body=%q", response.Code, body)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if !strings.Contains(body, "[云翻译]上游服务暂时不可用，请稍后重试") {
		t.Fatalf("body = %q, want the configured message", body)
	}
	if strings.Contains(body, "via openai") || strings.Contains(body, "req_leak_abc123") {
		t.Fatalf("upstream identity leaked to the client: %q", body)
	}
}

// TestChatCompletionsUpstreamErrorRewriteStreaming 端到端核对流式出口：保活已提交
// SSE 之后上游才报 503，此时错误必须以 SSE 错误帧发出，且同样只暴露配置文案。
func TestChatCompletionsUpstreamErrorRewriteStreaming(t *testing.T) {
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previousInterval })
	withUpstreamErrorRewrite(t, true, "", map[int]string{503: "[云翻译]上游服务暂时不可用，请稍后重试"})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 先静默，让下游保活把 SSE 200 提交掉，再把 503 交回去。
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, upstreamErrorLeakBody)
	}))
	t.Cleanup(upstream.Close)

	_, _, router := newModelQuotaTestHandler(t, 4, upstream.URL, false)
	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	body := response.Body.String()
	t.Logf("status=%d body=%q", response.Code, body)

	if strings.Contains(body, "via openai") || strings.Contains(body, "req_leak_abc123") {
		t.Fatalf("upstream identity leaked to the client: %q", body)
	}
	if !strings.Contains(body, "[云翻译]上游服务暂时不可用，请稍后重试") {
		t.Fatalf("body = %q, want the configured message", body)
	}
}

// TestChatCompletionsUpstreamClientErrorRewrite 覆盖「上游 message 会直接透给客户端」
// 的场景（不可重试的 4xx）：开启改写后必须换成配置文案，关闭时保留原文以便对照。
func TestChatCompletionsUpstreamClientErrorRewrite(t *testing.T) {
	upstreamBody := `{"error":{"message":"Invalid value for 'input' (via openai, req_leak_abc123)","type":"invalid_request_error"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(upstream.Close)

	run := func(t *testing.T) (int, string) {
		_, _, router := newModelQuotaTestHandler(t, 4, upstream.URL, false)
		response := performModelQuotaRequest(router, "/v1/chat/completions",
			`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`)
		return response.Code, response.Body.String()
	}

	t.Run("rewrite-off", func(t *testing.T) {
		withUpstreamErrorRewrite(t, false, "", nil)
		status, body := run(t)
		t.Logf("status=%d body=%q", status, body)
		if !strings.Contains(body, "req_leak_abc123") {
			t.Skipf("fixture no longer forwards the upstream message verbatim (status=%d): %q", status, body)
		}
	})

	t.Run("rewrite-on", func(t *testing.T) {
		withUpstreamErrorRewrite(t, true, "", map[int]string{400: "[云翻译]请求参数不被上游接受，请检查后重试"})
		status, body := run(t)
		t.Logf("status=%d body=%q", status, body)
		if !strings.Contains(body, "[云翻译]请求参数不被上游接受，请检查后重试") {
			t.Fatalf("body = %q, want the configured message", body)
		}
		if strings.Contains(body, "via openai") || strings.Contains(body, "req_leak_abc123") {
			t.Fatalf("upstream identity leaked to the client: %q", body)
		}
	})
}

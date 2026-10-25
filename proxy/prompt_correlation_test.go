package proxy

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// The correlation id is the only thing the calling middle layer stores to point
// back at prompt_filter_logs, so adopting an inbound one has to be exact — and a
// malformed one must fall back to a generated UUID rather than reach the table.
func TestCorrelationIDAdoptsInboundRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Request.Header.Set("X-Request-Id", "f26e0b6f-0e61-49eb-ac44-d03e6bbc337a")

	if got := ensurePromptPolicyRequestCorrelationID(c); got != "f26e0b6f-0e61-49eb-ac44-d03e6bbc337a" {
		t.Fatalf("correlation id = %q, want the inbound value", got)
	}
	// 第二次调用必须复用，不能重新生成。
	if got := ensurePromptPolicyRequestCorrelationID(c); got != "f26e0b6f-0e61-49eb-ac44-d03e6bbc337a" {
		t.Fatalf("second call = %q, want the cached value", got)
	}
}

func TestCorrelationIDRejectsMalformedInbound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, bad := range []string{
		"short",                       // 太短
		"has space inside it",         // 空格
		"<script>alert(1)</script>",   // 非法字符
		"中文的追踪标识不是合法的关联值", // 非 ASCII
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
		c.Request.Header.Set("X-Request-Id", bad)
		got := ensurePromptPolicyRequestCorrelationID(c)
		if got == bad {
			t.Fatalf("malformed inbound %q was adopted verbatim", bad)
		}
		if len(got) != 36 {
			t.Fatalf("fallback for %q = %q, want a generated UUID", bad, got)
		}
	}
}

func TestCorrelationHeaderEchoesTheUsedID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Request.Header.Set("X-Request-Id", "abc12345-6789-abcd-ef01-234567890abc")

	setPromptCorrelationHeader(c)
	if got := w.Header().Get("X-Prompt-Correlation-Id"); got != "abc12345-6789-abcd-ef01-234567890abc" {
		t.Fatalf("echoed = %q", got)
	}
}

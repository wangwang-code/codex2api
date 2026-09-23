package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// withUpstreamErrorRewrite 临时替换改写配置，测试结束后恢复。
func withUpstreamErrorRewrite(t *testing.T, enabled bool, defaultMessage string, byStatus map[int]string) {
	t.Helper()
	prevEnabled, prevDefault, prevStatus := upstreamErrorRewriteEnabled, upstreamErrorRewriteDefault, upstreamErrorRewriteStatusTexts
	upstreamErrorRewriteEnabled, upstreamErrorRewriteDefault, upstreamErrorRewriteStatusTexts = enabled, defaultMessage, byStatus
	t.Cleanup(func() {
		upstreamErrorRewriteEnabled, upstreamErrorRewriteDefault, upstreamErrorRewriteStatusTexts = prevEnabled, prevDefault, prevStatus
	})
}

// TestUpstreamErrorRewriteEnabledFromEnv 验证总开关默认关闭与识别规则。
func TestUpstreamErrorRewriteEnabledFromEnv(t *testing.T) {
	t.Setenv("UPSTREAM_ERROR_REWRITE_ENABLED", "")
	if upstreamErrorRewriteEnabledFromEnv() {
		t.Fatal("rewrite must default to off")
	}
	for raw, want := range map[string]bool{"true": true, "1": true, "on": true, "false": false, "0": false} {
		t.Setenv("UPSTREAM_ERROR_REWRITE_ENABLED", raw)
		if got := upstreamErrorRewriteEnabledFromEnv(); got != want {
			t.Fatalf("%s => %t, want %t", raw, got, want)
		}
	}
}

// TestParseUpstreamErrorRewriteStatusTexts 验证两种映射写法与转义还原。
func TestParseUpstreamErrorRewriteStatusTexts(t *testing.T) {
	t.Run("pipe-form", func(t *testing.T) {
		got := parseUpstreamErrorRewriteStatusTexts("429=[云翻译]被上游限流，请稍微再试|502=[云翻译]上游服务暂时不可用")
		if len(got) != 2 || got[429] != "[云翻译]被上游限流，请稍微再试" || got[502] != "[云翻译]上游服务暂时不可用" {
			t.Fatalf("parsed = %#v", got)
		}
	})

	t.Run("json-form", func(t *testing.T) {
		got := parseUpstreamErrorRewriteStatusTexts(`{"429":"被限流","503":"暂不可用"}`)
		if len(got) != 2 || got[429] != "被限流" || got[503] != "暂不可用" {
			t.Fatalf("parsed = %#v", got)
		}
	})

	t.Run("escapes-decoded", func(t *testing.T) {
		got := parseUpstreamErrorRewriteStatusTexts(`502=第一行\n第二行`)
		if got[502] != "第一行\n第二行" {
			t.Fatalf("message = %q, want a real newline", got[502])
		}
	})

	t.Run("skips-invalid-entries", func(t *testing.T) {
		got := parseUpstreamErrorRewriteStatusTexts("abc=x|429=|502=ok|503=fine=extra")
		if len(got) != 2 {
			t.Fatalf("parsed = %#v, want only valid entries", got)
		}
		if got[502] != "ok" || got[503] != "fine=extra" {
			t.Fatalf("parsed = %#v", got)
		}
	})

	t.Run("empty-and-invalid-json", func(t *testing.T) {
		if got := parseUpstreamErrorRewriteStatusTexts("   "); got != nil {
			t.Fatalf("blank value must yield nil, got %#v", got)
		}
		if got := parseUpstreamErrorRewriteStatusTexts(`{"429"`); got != nil {
			t.Fatalf("invalid JSON must be ignored, got %#v", got)
		}
	})
}

// TestUpstreamErrorRewriteMessage 验证按状态码取文案与默认文案兜底。
func TestUpstreamErrorRewriteMessage(t *testing.T) {
	byStatus := map[int]string{429: "被限流", 502: "暂不可用"}

	withUpstreamErrorRewrite(t, false, "默认", byStatus)
	if _, ok := upstreamErrorRewriteMessage(429); ok {
		t.Fatal("disabled rewrite must not match")
	}

	withUpstreamErrorRewrite(t, true, "默认", byStatus)
	if got, ok := upstreamErrorRewriteMessage(429); !ok || got != "被限流" {
		t.Fatalf("429 => %q %t", got, ok)
	}
	if got, ok := upstreamErrorRewriteMessage(503); !ok || got != "默认" {
		t.Fatalf("unconfigured status must fall back to the default, got %q %t", got, ok)
	}

	// 没有默认文案时，未配置的状态保持原样透出。
	withUpstreamErrorRewrite(t, true, "", byStatus)
	if _, ok := upstreamErrorRewriteMessage(503); ok {
		t.Fatal("without a default message unconfigured statuses must be untouched")
	}
	if got := rewriteUpstreamErrorText(503, "上游原文"); got != "上游原文" {
		t.Fatalf("unmatched status must keep the original text, got %q", got)
	}
	if got := rewriteUpstreamErrorText(429, "上游原文"); got != "被限流" {
		t.Fatalf("matched status must be rewritten, got %q", got)
	}
}

// TestErrorToGinResponseAppliesUpstreamErrorRewrite 验证非流式 JSON 出口的改写，
// 且保留网关自有的 type/code（下游靠 code 判定重试）。
func TestErrorToGinResponseAppliesUpstreamErrorRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withUpstreamErrorRewrite(t, true, "", map[int]string{503: "[云翻译]上游服务暂时不可用，请稍后重试"})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ErrorToGinResponse(c, ErrUpstream(http.StatusServiceUnavailable, "upstream 503 via openai (req_abc123)", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "[云翻译]上游服务暂时不可用，请稍后重试") {
		t.Fatalf("body = %q, want the configured message", body)
	}
	if strings.Contains(body, "via openai") || strings.Contains(body, "req_abc123") {
		t.Fatalf("upstream identity leaked to the client: %q", body)
	}
	if !strings.Contains(body, `"code":"upstream_error"`) || !strings.Contains(body, `"type":"upstream_error"`) {
		t.Fatalf("gateway-owned type/code must be preserved: %q", body)
	}
}

// TestErrorToGinResponseLeavesUnmatchedStatusUntouched 验证未命中状态时行为不变。
func TestErrorToGinResponseLeavesUnmatchedStatusUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withUpstreamErrorRewrite(t, true, "", map[int]string{503: "改写文案"})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ErrorToGinResponse(c, ErrUpstream(http.StatusBadGateway, "upstream 502 raw", nil))

	if body := recorder.Body.String(); !strings.Contains(body, "upstream 502 raw") {
		t.Fatalf("unmatched status must pass through unchanged, got %q", body)
	}
}

// TestErrorToGinResponseDisabledKeepsOriginal 验证关闭时完全不影响既有行为。
func TestErrorToGinResponseDisabledKeepsOriginal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withUpstreamErrorRewrite(t, false, "默认", map[int]string{503: "改写文案"})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ErrorToGinResponse(c, ErrUpstream(http.StatusServiceUnavailable, "upstream 503 raw", nil))

	if body := recorder.Body.String(); !strings.Contains(body, "upstream 503 raw") {
		t.Fatalf("disabled rewrite must not touch the response, got %q", body)
	}
}

// TestWriteContinuousRetryLastFailureSuppressesRawUpstreamBody 验证命中改写时
// 不再把上游原始 body 原样透给客户端（这是上游身份泄露的主路径）。
func TestWriteContinuousRetryLastFailureSuppressesRawUpstreamBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rawUpstream := []byte(`{"error":{"message":"raw upstream 503 via openai","type":"server_error","request_id":"req_leak"}}`)

	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		return c, recorder
	}

	t.Run("rewrite-on", func(t *testing.T) {
		withUpstreamErrorRewrite(t, true, "", map[int]string{503: "[云翻译]上游服务暂时不可用，请稍后重试"})
		c, recorder := newContext()
		writeContinuousRetryLastFailure(c, continuousRetryProtocolChat, continuousRetryFailure{
			status:      http.StatusServiceUnavailable,
			body:        rawUpstream,
			contentType: "application/json",
		})
		body := recorder.Body.String()
		if !strings.Contains(body, "[云翻译]上游服务暂时不可用，请稍后重试") {
			t.Fatalf("body = %q, want the configured message", body)
		}
		if strings.Contains(body, "raw upstream 503") || strings.Contains(body, "req_leak") {
			t.Fatalf("raw upstream body leaked: %q", body)
		}
	})

	t.Run("rewrite-off", func(t *testing.T) {
		withUpstreamErrorRewrite(t, false, "", nil)
		c, recorder := newContext()
		writeContinuousRetryLastFailure(c, continuousRetryProtocolChat, continuousRetryFailure{
			status:      http.StatusServiceUnavailable,
			body:        rawUpstream,
			contentType: "application/json",
		})
		if body := recorder.Body.String(); !strings.Contains(body, "raw upstream 503") {
			t.Fatalf("disabled rewrite must keep the original passthrough, got %q", body)
		}
	})
}

// TestUpstreamClientErrorMessageKeepsLogTextIntact 验证客户端文案被改写的同时，
// 用量日志仍保留真实上游原因（运维诊断不能被改写吃掉）。
func TestUpstreamClientErrorMessageKeepsLogTextIntact(t *testing.T) {
	body := []byte(`{"error":{"message":"upstream 503 via openai","type":"server_error"}}`)

	withUpstreamErrorRewrite(t, false, "", map[int]string{503: "改写文案"})
	if got, want := upstreamClientErrorMessage(503, body), usageLogErrorMessage(503, body); got != want {
		t.Fatalf("rewrite off: client message = %q, want %q", got, want)
	}

	withUpstreamErrorRewrite(t, true, "", map[int]string{503: "改写文案"})
	if got := upstreamClientErrorMessage(503, body); got != "改写文案" {
		t.Fatalf("rewrite on: client message = %q, want the configured text", got)
	}
	if got := usageLogErrorMessage(503, body); !strings.Contains(got, "upstream 503 via openai") {
		t.Fatalf("usage log must keep the real upstream reason, got %q", got)
	}
}

// TestSendFinalUpstreamErrorRewritesPoolUnavailable 验证池级 503 分支同样套用改写，
// 这样操作者按 503 配置文案后，池空/鉴权失效/工作区停用对下游是同一句话。
func TestSendFinalUpstreamErrorRewritesPoolUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	run := func(t *testing.T, status int, body string) *httptest.ResponseRecorder {
		t.Helper()
		h := &Handler{}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		h.sendFinalUpstreamError(c, status, []byte(body))
		return recorder
	}

	t.Run("rewrite-on", func(t *testing.T) {
		withUpstreamErrorRewrite(t, true, "", map[int]string{503: "[云翻译]上游服务暂时不可用，请稍后重试"})
		recorder := run(t, http.StatusForbidden, `{"error":{"message":"codex_access_restricted via openai"}}`)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", recorder.Code)
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "[云翻译]上游服务暂时不可用，请稍后重试") {
			t.Fatalf("body = %q, want the configured message", body)
		}
		if strings.Contains(body, "codex_access_restricted") {
			t.Fatalf("pool detail leaked to the client: %q", body)
		}
	})

	t.Run("rewrite-off", func(t *testing.T) {
		withUpstreamErrorRewrite(t, false, "", nil)
		recorder := run(t, http.StatusForbidden, `{"error":{"message":"codex_access_restricted via openai"}}`)
		if body := recorder.Body.String(); !strings.Contains(body, "账号池暂无可用账号") {
			t.Fatalf("rewrite off must keep the original pool message, got %q", body)
		}
	})
}

// TestRewriteUpstreamErrorTextHandlesInternalStatuses 验证内部状态码不会被按状态码
// 配置误命中；同时固化 CPA 的既有语义：配了默认文案后未知状态也会走默认。
func TestRewriteUpstreamErrorTextHandlesInternalStatuses(t *testing.T) {
	withUpstreamErrorRewrite(t, true, "", map[int]string{503: "改写文案"})
	for _, status := range []int{0, -1, logStatusUpstreamStreamBreak, logStatusClientClosed} {
		if got := rewriteUpstreamErrorText(status, "原样"); got != "原样" {
			t.Fatalf("internal status %d must not be rewritten, got %q", status, got)
		}
	}

	withUpstreamErrorRewrite(t, true, "默认", nil)
	if got := rewriteUpstreamErrorText(0, "原样"); got != "默认" {
		t.Fatalf("with a default message unknown statuses use it, got %q", got)
	}
}

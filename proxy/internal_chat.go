package proxy

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// internalChatReasonAuditReview tags usage_logs.internal_reason for the prompt
// audit model call, so the review traffic stays separable from real users in
// the usage table.
const internalChatReasonAuditReview = "prompt_audit_review"

// internalReasoningEffortKey carries the reviewer's declared effort from the
// internal entry point down to the executor.
//
// It cannot simply ride in the inbound body: the chat body is rebuilt into the
// upstream (Responses-shaped) payload on the way down, and `reasoning` does not
// survive that rebuild. Payload rules work precisely because they run *after*
// it (executor.go, ApplyPayloadRulesToBody) — so the reviewer's effort is
// applied at the same point, which is also why an equivalent payload rule used
// to have the intended effect.
type internalReasoningEffortKey struct{}

// ApplyInternalReasoningEffort stamps the declared effort onto the final
// upstream body. A value already present (from payload rules or the caller's own
// body) wins: an explicit rule stays the most specific instruction.
func ApplyInternalReasoningEffort(ctx context.Context, body []byte) []byte {
	if ctx == nil || len(body) == 0 {
		return body
	}
	effort, _ := ctx.Value(internalReasoningEffortKey{}).(string)
	effort = normalizeInternalReasoningEffort(effort)
	if effort == "" {
		return body
	}
	if strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String()) != "" {
		return body
	}
	updated, err := sjson.SetBytes(body, "reasoning.effort", effort)
	if err != nil {
		return body
	}
	return updated
}

func normalizeInternalReasoningEffort(raw string) string {
	switch value := strings.ToLower(strings.TrimSpace(raw)); value {
	case "none", "minimal", "low", "medium", "high":
		return value
	}
	return ""
}

// ExecuteInternalChatCompletion runs one /v1/chat/completions request inside the
// process and returns the same (status, body) pair the wire would have carried.
//
// It exists because the prompt audit model is served by this same gateway: the
// reviewer used to call back through http://127.0.0.1:<port>/v1/chat/completions
// with its own API key. That loop cost a full HTTP round trip (TLS-less but still
// auth middleware, body decode, JSON re-encode) on the synchronous first-token
// path, and it needed a key bound with scope=off purely so the filter would not
// block its own reviewer.
//
// The child request keeps the whole downstream pipeline — account selection,
// upstream call, retries, usage accounting — and skips only the two steps that
// only make sense for an external caller: inbound API-key auth and the inbound
// prompt filter (which would otherwise recurse).
func (h *Handler) ExecuteInternalChatCompletion(ctx context.Context, body []byte, reason string, reasoningEffort string) (int, []byte) {
	return h.executeInternalChatWithAttribution(ctx, body, nil, reason, reasoningEffort)
}

// ExecuteInternalChatCompletionForAPIKey is the attributed variant: routing and
// usage are charged to the given gateway key while the raw credential is never
// copied into the child request.
func (h *Handler) ExecuteInternalChatCompletionForAPIKey(ctx context.Context, body []byte, row *database.APIKeyRow, reason string, reasoningEffort string) (int, []byte) {
	if row == nil {
		return h.ExecuteInternalChatCompletion(ctx, body, reason, reasoningEffort)
	}
	return h.executeInternalChatWithAttribution(ctx, body, &internalResponseAttribution{
		APIKeyID:     row.ID,
		APIKeyName:   row.Name,
		APIKeyMasked: security.MaskAPIKey(row.Key),
		APIKeyRow:    row,
		Reason:       reason,
	}, reason, reasoningEffort)
}

func (h *Handler) executeInternalChatWithAttribution(ctx context.Context, body []byte, attribution *internalResponseAttribution, reason string, reasoningEffort string) (int, []byte) {
	if h == nil {
		return http.StatusServiceUnavailable, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if effort := normalizeInternalReasoningEffort(reasoningEffort); effort != "" {
		ctx = context.WithValue(ctx, internalReasoningEffortKey{}, effort)
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	// Same bypass the Responses internal path uses: the audit reviewer's own
	// request must not be classified by the filter it is feeding.
	c.Set("prompt_intelligence_internal", true)
	if attribution == nil {
		attribution = &internalResponseAttribution{Reason: reason}
	}
	if strings.TrimSpace(attribution.Reason) == "" {
		attribution.Reason = reason
	}
	applyInternalResponseAttribution(c, req, attribution)
	h.ChatCompletions(c)
	return recorder.Code, recorder.Body.Bytes()
}

// InternalChatExecutor adapts the handler to promptfilter.InternalExecutor. It
// reports Handles() only for base URLs that point back at this very process, so
// a review endpoint pointed at some other host keeps using plain HTTP.
type InternalChatExecutor struct {
	Handler *Handler
	Port    int
}

func (e InternalChatExecutor) Handles(baseURL string) bool {
	if e.Handler == nil {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return false
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	if e.Port > 0 && port != strconv.Itoa(e.Port) {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "127.0.0.1", "localhost", "::1", "0.0.0.0":
		return true
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func (e InternalChatExecutor) ExecuteChatCompletion(ctx context.Context, body []byte, reason string, reasoningEffort string) (int, []byte) {
	if strings.TrimSpace(reason) == "" {
		reason = internalChatReasonAuditReview
	}
	return e.Handler.ExecuteInternalChatCompletion(ctx, body, reason, reasoningEffort)
}

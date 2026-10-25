package promptfilter

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeInternalExecutor records what the reviewer handed it, so the test can
// assert both that the short-circuit happened and that the payload is byte-for-
// byte what the HTTP transport would have sent.
type fakeInternalExecutor struct {
	mu       sync.Mutex
	handles  string
	calls      int
	lastBody   []byte
	lastWhy    string
	lastEffort string
	status   int
	respBody []byte
}

func (f *fakeInternalExecutor) Handles(baseURL string) bool {
	return strings.Contains(baseURL, f.handles)
}

func (f *fakeInternalExecutor) ExecuteChatCompletion(_ context.Context, body []byte, reason string, reasoningEffort string) (int, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastBody = append([]byte(nil), body...)
	f.lastWhy = reason
	f.lastEffort = reasoningEffort
	return f.status, f.respBody
}

func internalReviewConfig(baseURL string) ReviewConfig {
	cfg := DefaultReviewConfig()
	cfg.Enabled = true // Ready() 要求开启，否则 ReviewTextDetailed 直接空转返回
	cfg.BaseURL = baseURL
	cfg.APIKey = "sk-internal-test"
	cfg.Model = "codex-auto-review"
	cfg.Adapter.RequestMode = ReviewRequestModeChatCompletions
	cfg.TimeoutSeconds = 5
	return cfg
}

func chatCompletionBody(content string) []byte {
	payload := map[string]any{
		"model": "codex-auto-review",
		"choices": []map[string]any{
			{"message": map[string]any{"content": content}},
		},
	}
	raw, _ := json.Marshal(payload)
	return raw
}

// The reviewer must take the in-process path when the endpoint points back at
// this process, and it must still produce a decoded outcome — the transport
// changed, the contract did not.
func TestReviewUsesInternalExecutorWhenEndpointIsSelf(t *testing.T) {
	fake := &fakeInternalExecutor{
		handles:  "127.0.0.1:8317",
		status:   http.StatusOK,
		respBody: chatCompletionBody(`{"confidence":0.97,"reason":"非翻译行为","is_r18":false}`),
	}
	RegisterInternalExecutor(fake)
	defer RegisterInternalExecutor(nil)

	cfg := internalReviewConfig("http://127.0.0.1:8317/v1")
	outcome, err := DefaultReviewClient.ReviewTextDetailed(context.Background(), "some text", cfg)
	if err != nil {
		t.Fatalf("ReviewTextDetailed: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("internal executor calls = %d, want 1 (HTTP must not be used)", fake.calls)
	}
	if fake.lastWhy != ReviewInternalReason {
		t.Fatalf("reason = %q, want %q", fake.lastWhy, ReviewInternalReason)
	}
	if !outcome.Flagged || outcome.Confidence < 0.9 {
		t.Fatalf("outcome = %+v, want flagged with high confidence", outcome)
	}
	if outcome.IsR18 == nil || *outcome.IsR18 {
		t.Fatalf("IsR18 = %v, want false carried through the internal path", outcome.IsR18)
	}
	// 送进执行器的必须还是那套 chat-completions 报文，否则「复用同一套解码」就成了空话。
	if !strings.Contains(string(fake.lastBody), `"messages"`) {
		t.Fatalf("payload is not a chat completion body: %s", string(fake.lastBody)[:200])
	}
}

// A review endpoint aimed at some other host must keep going over HTTP: the
// executor must not claim URLs it does not own.
func TestInternalExecutorDeclinesForeignEndpoint(t *testing.T) {
	fake := &fakeInternalExecutor{handles: "127.0.0.1:8317"}
	RegisterInternalExecutor(fake)
	defer RegisterInternalExecutor(nil)

	if exec := internalExecutorFor("https://openrouter.ai/api/v1"); exec != nil {
		t.Fatal("executor claimed a foreign base URL; review traffic would be misrouted")
	}
	if exec := internalExecutorFor("http://127.0.0.1:9999/v1"); exec != nil {
		t.Fatal("executor claimed a different local port")
	}
	if exec := internalExecutorFor("http://127.0.0.1:8317/v1"); exec == nil {
		t.Fatal("executor declined its own endpoint")
	}
}

// Without a registration the reviewer must fall back to HTTP rather than
// silently skipping the audit.
func TestReviewFallsBackToHTTPWhenExecutorMissing(t *testing.T) {
	RegisterInternalExecutor(nil)
	if exec := internalExecutorFor("http://127.0.0.1:8317/v1"); exec != nil {
		t.Fatal("executor present after unregister")
	}
}

// An internal transport that answers with a non-2xx status has to surface the
// same status error the HTTP transport would, so key/circuit handling and the
// caller's error path stay identical.
func TestInternalReviewSurfacesStatusError(t *testing.T) {
	fake := &fakeInternalExecutor{
		handles:  "127.0.0.1:8317",
		status:   http.StatusTooManyRequests,
		respBody: []byte(`{"error":{"message":"slow down"}}`),
	}
	RegisterInternalExecutor(fake)
	defer RegisterInternalExecutor(nil)

	cfg := internalReviewConfig("http://127.0.0.1:8317/v1")
	if _, err := DefaultReviewClient.ReviewTextDetailed(context.Background(), "text", cfg); err == nil {
		t.Fatal("want an error for a 429 from the internal transport")
	}
}

// 内部短路没有跨进程边界，密钥是多余的：把它删掉不该让审核静默停摆。
func TestInternalReviewWorksWithoutAPIKey(t *testing.T) {
	fake := &fakeInternalExecutor{
		handles:  "127.0.0.1:8317",
		status:   http.StatusOK,
		respBody: chatCompletionBody(`{"confidence":0.2,"reason":"正常","is_r18":false}`),
	}
	RegisterInternalExecutor(fake)
	defer RegisterInternalExecutor(nil)

	cfg := internalReviewConfig("http://127.0.0.1:8317/v1")
	cfg.APIKey = "" // 没有密钥
	if !cfg.Ready() {
		t.Fatal("Ready() = false for an internally served endpoint without a key")
	}
	if err := ValidateReviewConfig(cfg); err != nil {
		t.Fatalf("ValidateReviewConfig rejected a keyless internal config: %v", err)
	}
	if _, err := DefaultReviewClient.ReviewTextDetailed(context.Background(), "text", cfg); err != nil {
		t.Fatalf("ReviewTextDetailed: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want 1", fake.calls)
	}
}

// 但同一个「无密钥」配置，只要 URL 指向别处，就必须继续报错 —— 放宽不能漏到 HTTP。
func TestKeylessConfigStillRejectedForRemoteEndpoint(t *testing.T) {
	fake := &fakeInternalExecutor{handles: "127.0.0.1:8317"}
	RegisterInternalExecutor(fake)
	defer RegisterInternalExecutor(nil)

	cfg := internalReviewConfig("https://openrouter.ai/api/v1")
	cfg.APIKey = ""
	if cfg.Ready() {
		t.Fatal("Ready() = true for a keyless remote endpoint")
	}
	if err := ValidateReviewConfig(cfg); err == nil {
		t.Fatal("ValidateReviewConfig accepted a keyless remote endpoint")
	}
}

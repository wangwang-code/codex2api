package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestChatCompletionsFakeThinkingSurvivesReasoningPhase 复现线上现象并守住修复。
//
// 场景：上游先流式输出 reasoning（思考摘要），随后长时间没有输出——这段时间下游只有
// 心跳在写，正是按序假思考文案该出现的地方。
//
// 修复前的行为：reasoning 事件同样带 delta 字段，被 isFirstTokenResult 判成「已出内容」，
// 于是 markFirstContentSeen 立即置位，后续心跳全部退回纯注释——客户端只看到首帧文案，
// STREAM_FAKE_THINKING_TEXTS 里的文案从未被挑选。
func TestChatCompletionsFakeThinkingSurvivesReasoningPhase(t *testing.T) {
	restoreInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 50 * time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = restoreInterval })

	restoreEnabled, restoreImmediate := streamFakeThinkingEnabled, streamFakeThinkingImmediate
	restoreFirst, restoreTexts := streamFakeThinkingText, streamFakeThinkingTexts
	streamFakeThinkingEnabled, streamFakeThinkingImmediate = true, true
	streamFakeThinkingText = "开流首帧"
	streamFakeThinkingTexts = productionFakeThinkingTexts
	t.Cleanup(func() {
		streamFakeThinkingEnabled, streamFakeThinkingImmediate = restoreEnabled, restoreImmediate
		streamFakeThinkingText, streamFakeThinkingTexts = restoreFirst, restoreTexts
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_think"}}`+"\n\n")
		// 上游先思考：reasoning 不是给用户看的译文。
		_, _ = io.WriteString(w, `data: {"type":"response.reasoning_summary_text.delta","delta":"上游在思考"}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// 思考期间上游没有输出：下游只能靠心跳，按序假思考文案应当在这段时间发出。
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"译文"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.done","text":"译文"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_think","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	_, router := newStreamLimitTestHandler(t, upstream.URL, 0)
	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	body := response.Body.String()

	if !strings.Contains(body, "开流首帧") {
		t.Fatalf("首帧假思考缺失: %q", body)
	}
	for _, want := range []string{"再耐心等等", "这有点超出预计耗时了"} {
		if !strings.Contains(body, want) {
			t.Fatalf("按序假思考文案 %q 未发出（reasoning 到达后假思考被提前停掉）: %q", want, body)
		}
	}
	if !strings.Contains(body, "译文") {
		t.Fatalf("真实译文缺失: %q", body)
	}
}

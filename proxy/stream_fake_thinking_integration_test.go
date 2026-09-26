package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeThinkingUpstreamSSE 是上游 Responses 形态的流：内容帧之前先静默一段，
// 用来制造「首字节前」的观察窗口。
const fakeThinkingUpstreamSSE = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fake\"}}\n\n" +
	"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\"}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"REAL-ANSWER\"}\n\n" +
	"data: {\"type\":\"response.output_text.done\"}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fake\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"

// reasoningValuesFromSSE 按顺序取出下游流里所有 chat.completion.chunk 的
// reasoning_content。真实内容帧只带 delta.content，会被这里过滤掉。
func reasoningValuesFromSSE(t *testing.T, body string) []string {
	t.Helper()
	var values []string
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if !strings.HasPrefix(block, "data: ") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(block, "data: ")), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 1 && chunk.Choices[0].Delta.ReasoningContent != "" {
			values = append(values, chunk.Choices[0].Delta.ReasoningContent)
		}
	}
	return values
}

// TestChatCompletionsPreemptiveFakeThinkingReachesDownstream 是抢先思考的端到端核对：
// 走真实 /v1/chat/completions 流式链路，确认
//   - 首字节前的空窗里下游先收到开流假思考帧（且该帧不带 delta.content）
//   - 之后每次心跳按序附加文案，空项只发心跳但下标继续推进
//   - 列表耗尽后退回纯注释心跳
//   - 真实内容到达后不再注入假思考
func TestChatCompletionsPreemptiveFakeThinkingReachesDownstream(t *testing.T) {
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 20 * time.Millisecond
	// 假思考有自己的空窗节奏（STREAM_FAKE_THINKING_INTERVAL，默认 1s），不再跟随全局
	// 保活间隔；本用例靠密集心跳在 250ms 空窗里把三条文案发完，所以两者都要调小。
	previousRhythm := streamFakeThinkingInterval
	streamFakeThinkingInterval = 20 * time.Millisecond
	previousEnabled, previousImmediate := streamFakeThinkingEnabled, streamFakeThinkingImmediate
	previousText, previousTexts := streamFakeThinkingText, streamFakeThinkingTexts
	t.Cleanup(func() {
		continuousRetryKeepaliveInterval = previousInterval
		streamFakeThinkingInterval = previousRhythm
		streamFakeThinkingEnabled, streamFakeThinkingImmediate = previousEnabled, previousImmediate
		streamFakeThinkingText, streamFakeThinkingTexts = previousText, previousTexts
	})
	streamFakeThinkingEnabled = true
	streamFakeThinkingImmediate = true
	streamFakeThinkingText = "开流首帧"
	streamFakeThinkingTexts = []string{"\n第一条", "", "\n第三条"}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 首字节前保持静默：上游响应头在第一次写入时才产生，因此这段就是
		// 「等待上游首字节」的空窗，抢先开流必须在这里把假思考发出去。
		time.Sleep(250 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, fakeThinkingUpstreamSSE)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	_, _, router := newModelQuotaTestHandler(t, 4, upstream.URL, false)
	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	body := response.Body.String()

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, body)
	}
	if !strings.Contains(body, "REAL-ANSWER") {
		t.Fatalf("real upstream content never reached the client: %q", body)
	}
	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("downstream must open with a data frame, got %q", headOf(body, 160))
	}
	firstFrame := body[:strings.Index(body, "\n\n")]
	if !strings.Contains(firstFrame, "开流首帧") {
		t.Fatalf("first frame must carry the preemptive thinking text, got %q", firstFrame)
	}
	if strings.Contains(firstFrame, `"content"`) {
		t.Fatalf("preemptive frame must not carry delta.content, got %q", firstFrame)
	}

	values := reasoningValuesFromSSE(t, body)
	want := []string{"开流首帧", "\n第一条", "\n第三条"}
	if len(values) != len(want) {
		t.Fatalf("reasoning frames = %#v, want %#v", values, want)
	}
	for i := range want {
		if values[i] != want[i] {
			t.Fatalf("reasoning[%d] = %q, want %q", i, values[i], want[i])
		}
	}

	if beats := strings.Count(body, ": keepalive"); beats < 3 {
		t.Fatalf("expected repeated heartbeats across the pre-first-byte window, got %d in %q", beats, body)
	}
	if last := strings.LastIndex(body, "reasoning_content"); last > strings.Index(body, "REAL-ANSWER") {
		t.Fatalf("fake thinking leaked past the first real content frame: %q", body)
	}
}

// TestChatCompletionsFakeThinkingDisabledKeepsOriginalKeepalive 确认总开关关闭时
// 下游只有纯注释心跳，不出现任何假思考帧（即默认行为与改动前一致）。
func TestChatCompletionsFakeThinkingDisabledKeepsOriginalKeepalive(t *testing.T) {
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 20 * time.Millisecond
	previousEnabled := streamFakeThinkingEnabled
	t.Cleanup(func() {
		continuousRetryKeepaliveInterval = previousInterval
		streamFakeThinkingEnabled = previousEnabled
	})
	streamFakeThinkingEnabled = false

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, fakeThinkingUpstreamSSE)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	_, _, router := newModelQuotaTestHandler(t, 4, upstream.URL, false)
	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	body := response.Body.String()

	if response.Code != http.StatusOK || !strings.Contains(body, "REAL-ANSWER") {
		t.Fatalf("status = %d, body = %q", response.Code, body)
	}
	if values := reasoningValuesFromSSE(t, body); len(values) != 0 {
		t.Fatalf("disabled switch must not inject fake thinking, got %#v", values)
	}
}

// headOf 截断过长的响应体，避免断言失败时刷屏。
func headOf(body string, limit int) string {
	if len(body) <= limit {
		return body
	}
	return body[:limit] + "..."
}

// TestPreemptiveFakeThinkingDoesNotWaitForKeepalivePeriod 证明「抢先开流」不依赖保活
// 周期：把周期设成远超上游首字节延迟，首帧仍然立刻发出。若 priming 失效，这个窗口里
// 一次心跳都不会到期，断言就会失败——这是把「抢先」和「按周期心跳」区分开的关键用例。
func TestPreemptiveFakeThinkingDoesNotWaitForKeepalivePeriod(t *testing.T) {
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 5 * time.Second
	previousEnabled, previousImmediate := streamFakeThinkingEnabled, streamFakeThinkingImmediate
	previousText, previousTexts := streamFakeThinkingText, streamFakeThinkingTexts
	t.Cleanup(func() {
		continuousRetryKeepaliveInterval = previousInterval
		streamFakeThinkingEnabled, streamFakeThinkingImmediate = previousEnabled, previousImmediate
		streamFakeThinkingText, streamFakeThinkingTexts = previousText, previousTexts
	})
	streamFakeThinkingEnabled = true
	streamFakeThinkingText = "开流首帧"
	streamFakeThinkingTexts = nil

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, fakeThinkingUpstreamSSE)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	for _, tc := range []struct {
		name      string
		immediate bool
		want      []string
	}{
		{name: "immediate-on", immediate: true, want: []string{"开流首帧"}},
		{name: "immediate-off", immediate: false, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streamFakeThinkingImmediate = tc.immediate
			_, _, router := newModelQuotaTestHandler(t, 4, upstream.URL, false)
			response := performModelQuotaRequest(router, "/v1/chat/completions",
				`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":true}`)
			body := response.Body.String()
			if response.Code != http.StatusOK || !strings.Contains(body, "REAL-ANSWER") {
				t.Fatalf("status = %d, body = %q", response.Code, body)
			}
			values := reasoningValuesFromSSE(t, body)
			if len(values) != len(tc.want) {
				t.Fatalf("reasoning frames = %#v, want %#v", values, tc.want)
			}
			for i := range tc.want {
				if values[i] != tc.want[i] {
					t.Fatalf("reasoning[%d] = %q, want %q", i, values[i], tc.want[i])
				}
			}
		})
	}
}

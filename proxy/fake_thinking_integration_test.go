package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withFakeThinkingForTest 在测试期间打开伪装思考并注入按序文案。
func withFakeThinkingForTest(t *testing.T) {
	t.Helper()
	restoreEnabled, restoreImmediate := streamFakeThinkingEnabled, streamFakeThinkingImmediate
	restoreFirst, restoreTexts := streamFakeThinkingText, streamFakeThinkingTexts
	streamFakeThinkingEnabled, streamFakeThinkingImmediate = true, true
	streamFakeThinkingText = "开流首帧"
	streamFakeThinkingTexts = productionFakeThinkingTexts
	t.Cleanup(func() {
		streamFakeThinkingEnabled, streamFakeThinkingImmediate = restoreEnabled, restoreImmediate
		streamFakeThinkingText, streamFakeThinkingTexts = restoreFirst, restoreTexts
	})
}

// silentUpstream 返回一个「先保持静默、再输出译文」的假上游：静默时长即
// 「等上游首个内容」的窗口，假思考只能在这个窗口里推进。
func silentUpstream(t *testing.T, silentFor time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_silent"}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(silentFor)
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"译文"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.done","text":"译文"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_silent","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// chatStreamBody 是一次最小的流式 chat 请求体。
const chatStreamBody = `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":true}`

// TestFakeThinkingRhythmOverridesKeepalive 验证按序文案的推进节奏由
// STREAM_FAKE_THINKING_INTERVAL 决定（默认 1s，对齐 CPA 的 keepalive-seconds），
// 而不是全局保活间隔（默认 30s）。
//
// 同一个 400ms 空窗、同样的全局保活间隔 500ms，只改假思考节奏：
//   - 节奏 50ms → 空窗内多次心跳，按序文案全部发出
//   - 节奏 0（跟随全局 500ms）→ 空窗内连第二次心跳都等不到，只有开流首帧
//
// 后者正是「配了多条文案却从未被挑选」的原因。
func TestFakeThinkingRhythmOverridesKeepalive(t *testing.T) {
	restoreGlobal := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 500 * time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = restoreGlobal })

	cases := []struct {
		name          string
		rhythm        time.Duration
		wantSequenced bool
	}{
		{name: "节奏50ms-空窗内多次心跳", rhythm: 50 * time.Millisecond, wantSequenced: true},
		{name: "节奏0-跟随全局500ms", rhythm: 0, wantSequenced: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFakeThinkingForTest(t)
			restoreRhythm := streamFakeThinkingInterval
			streamFakeThinkingInterval = tc.rhythm
			t.Cleanup(func() { streamFakeThinkingInterval = restoreRhythm })

			upstream := silentUpstream(t, 400*time.Millisecond)
			_, router := newStreamLimitTestHandler(t, upstream.URL, 0)
			body := performModelQuotaRequest(router, "/v1/chat/completions", chatStreamBody).Body.String()

			if !strings.Contains(body, "开流首帧") {
				t.Fatalf("首帧假思考缺失: %q", body)
			}
			if !strings.Contains(body, "译文") {
				t.Fatalf("真实译文缺失: %q", body)
			}

			sawSequenced := strings.Contains(body, "再耐心等等") ||
				strings.Contains(body, "这有点超出预计耗时了")
			if sawSequenced != tc.wantSequenced {
				t.Fatalf("按序文案是否发出 = %v, want %v（节奏=%s）: %q",
					sawSequenced, tc.wantSequenced, tc.rhythm, body)
			}
			if tc.wantSequenced {
				for _, want := range []string{"云翻译处于灰测中", "再耐心等等", "这有点超出预计耗时了"} {
					if !strings.Contains(body, want) {
						t.Fatalf("按序文案 %q 未发出: %q", want, body)
					}
				}
			}
		})
	}
}

// TestFakeThinkingDefaultRhythmEmitsAllTexts 用默认节奏（1s）实测线上场景：
// 上游空窗 4.5s，4 条文案（含 1 空项）应全部发出。
//
// 这条直接对应线上配置——CPA 的 keepalive-seconds: 1 就是靠这个节奏在几秒的空窗里
// 把按序文案发完；本网关此前把节奏绑在 30s 的全局保活上，空窗内只发得出开流首帧。
//
// 顺带固定住一个量级关系：**能发出几条 = 空窗时长 ÷ 节奏**（空项也占一个心跳）。
// 想让 4 条文案都出现，空窗至少要有 4 个心跳周期。
func TestFakeThinkingDefaultRhythmEmitsAllTexts(t *testing.T) {
	withFakeThinkingForTest(t)
	restoreRhythm := streamFakeThinkingInterval
	streamFakeThinkingInterval = time.Second
	t.Cleanup(func() { streamFakeThinkingInterval = restoreRhythm })

	upstream := silentUpstream(t, 4500*time.Millisecond)
	_, router := newStreamLimitTestHandler(t, upstream.URL, 0)
	body := performModelQuotaRequest(router, "/v1/chat/completions", chatStreamBody).Body.String()

	if !strings.Contains(body, "开流首帧") {
		t.Fatalf("首帧假思考缺失: %q", body)
	}
	for _, want := range []string{"云翻译处于灰测中", "再耐心等等", "这有点超出预计耗时了"} {
		if !strings.Contains(body, want) {
			t.Fatalf("默认 1s 节奏下按序文案 %q 未发出: %q", want, body)
		}
	}
	if !strings.Contains(body, "译文") {
		t.Fatalf("真实译文缺失: %q", body)
	}
}

// TestFakeThinkingStopsAtFirstUpstreamEvent 验证停止时机与 CPA 一致：上游首个内容事件
// （包括 reasoning）一到就停止注入，后续心跳退回纯注释。
//
// 假思考的语义是「覆盖等上游首个内容的空窗」，上游一旦开始产出就没有空窗可填了。
func TestFakeThinkingStopsAtFirstUpstreamEvent(t *testing.T) {
	withFakeThinkingForTest(t)

	restore := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 50 * time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = restore })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_reason"}}`+"\n\n")
		// 上游开始思考：这已经是「首个内容事件」，假思考窗口到此结束。
		_, _ = io.WriteString(w, `data: {"type":"response.reasoning_summary_text.delta","delta":"上游在思考"}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"译文"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_reason","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	_, router := newStreamLimitTestHandler(t, upstream.URL, 0)
	body := performModelQuotaRequest(router, "/v1/chat/completions", chatStreamBody).Body.String()

	if !strings.Contains(body, "开流首帧") {
		t.Fatalf("首帧假思考缺失: %q", body)
	}
	// reasoning 之后仍有 8 次心跳窗口，但不该再出现任何按序文案。
	for _, unwanted := range []string{"云翻译处于灰测中", "再耐心等等", "这有点超出预计耗时了"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("上游已开始产出后不应再注入按序文案 %q: %q", unwanted, body)
		}
	}
	if !strings.Contains(body, "译文") {
		t.Fatalf("真实译文缺失: %q", body)
	}
}

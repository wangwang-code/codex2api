package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
)

// stallingUpstream 模拟「发几个事件后半开挂死」的上游：不写终态、不关连接，
// 直到代理自己取消请求（r.Context() 结束）或测试收尾放行。
//
// 这正是流预算抓不到的场景：没有事件，预算的三个判据（内容字符 / 原始字节 / 墙钟）
// 都不会被判定，请求只能干等——上线表现就是「首字很快，之后一直不结束」。
func stallingUpstream(t *testing.T, deltas int) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_stall"}}`+"\n\n")
		for i := 0; i < deltas; i++ {
			_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"A"}`+"\n\n")
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server
}

// slowLiveUpstream 模拟「产出慢但一直活着」的上游：每 gap 一个 delta，最后给终态。
// 用来验证停顿看门狗不会误杀慢速但有进展的流。
func slowLiveUpstream(t *testing.T, deltas int, gap time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_slow"}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		for i := 0; i < deltas; i++ {
			select {
			case <-time.After(gap):
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"A"}`+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.done"}`+"\n\n")
		_, _ = io.WriteString(w, fmt.Sprintf(
			`data: {"type":"response.completed","response":{"id":"resp_slow","status":"completed","usage":{"input_tokens":1,"output_tokens":%d}}}`+"\n\n", deltas))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// streamStallUsageLog 取本次请求的用量记录（停顿中止也应留下一条可判读的记录）。
func streamStallUsageLog(t *testing.T, h *Handler) *database.UsageLog {
	t.Helper()
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
	return logs[0]
}

// TestChatCompletionsStreamStallEndsHungNonStreamRequest 验证线上那类「上游发完事件就
// 不说话了」的请求能在空闲超时内收尾：非流式请求必须拿到真实错误码 + 可读原因，
// 而不是把半截译文当 200 返回，更不是让 App 一直挂在 55s 墙钟上限上。
func TestChatCompletionsStreamStallEndsHungNonStreamRequest(t *testing.T) {
	withStreamStallTimeout(t, 80*time.Millisecond)
	upstream := stallingUpstream(t, 2)
	h, router := newStreamLimitTestHandler(t, upstream.URL, 0)

	start := time.Now()
	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	elapsed := time.Since(start)
	body := response.Body.String()
	t.Logf("status=%d elapsed=%s body=%q", response.Code, elapsed, body)

	if elapsed < 80*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("应在停顿超时附近收尾，实际耗时 %s", elapsed)
	}
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502（非流式停顿必须报错，不能把半截正文当成功）: %q", response.Code, body)
	}
	if !strings.Contains(body, ErrorCodeUpstreamStreamBreak) {
		t.Fatalf("非流式停顿应给出可编程识别的错误码: %q", body)
	}

	entry := streamStallUsageLog(t, h)
	t.Logf("logged: status=%d kind=%q message=%q", entry.StatusCode, entry.UpstreamErrorKind, entry.ErrorMessage)
	if entry.UpstreamErrorKind != "stream_stall" {
		t.Fatalf("logged kind = %q, want stream_stall", entry.UpstreamErrorKind)
	}
	if entry.StatusCode != logStatusUpstreamStreamBreak {
		t.Fatalf("logged status = %d, want %d", entry.StatusCode, logStatusUpstreamStreamBreak)
	}
	if !strings.Contains(entry.ErrorMessage, "停顿超时") {
		t.Fatalf("用量记录应保留真实中止原因: %q", entry.ErrorMessage)
	}
}

// TestChatCompletionsStreamStallEmitsStreamBreakAfterContent 验证流式请求在已下发正文后
// 停顿：必须补流内断流事件且不补 [DONE]，让下游能感知失败并重试，而不是收到一个
// 静默 EOF 的截断流（issue #473 的同一诉求）。
func TestChatCompletionsStreamStallEmitsStreamBreakAfterContent(t *testing.T) {
	withStreamStallTimeout(t, 80*time.Millisecond)
	upstream := stallingUpstream(t, 3)
	_, router := newStreamLimitTestHandler(t, upstream.URL, 0)

	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	body := response.Body.String()
	t.Logf("status=%d body=%q", response.Code, body)

	if !strings.Contains(body, `"content":"A"`) {
		t.Fatalf("停顿前的正文应先下发: %q", body)
	}
	if !strings.Contains(body, ErrorCodeUpstreamStreamBreak) {
		t.Fatalf("已写出正文后的停顿必须补流内断流事件: %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("失败的流不应补 [DONE]: %q", body)
	}
}

// TestChatCompletionsStreamStallKeepsSlowButLiveStream 是防误杀回归：事件间隔小于超时
// 的慢流必须完整放行——看门狗管「空闲」，不管「总时长」。
func TestChatCompletionsStreamStallKeepsSlowButLiveStream(t *testing.T) {
	withStreamStallTimeout(t, 200*time.Millisecond)
	upstream := slowLiveUpstream(t, 6, 30*time.Millisecond)
	h, router := newStreamLimitTestHandler(t, upstream.URL, 0)

	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	body := response.Body.String()
	t.Logf("status=%d body=%q", response.Code, body)

	if response.Code != http.StatusOK {
		t.Fatalf("慢但有进展的流被误杀: status=%d body=%q", response.Code, body)
	}
	if !strings.Contains(body, "AAAAAA") {
		t.Fatalf("六个内容帧都应转发: %q", body)
	}
	if isStreamStallUsageLogKind(t, h) {
		t.Fatal("不应留下 stream_stall 记录")
	}
}

// isStreamStallUsageLogKind 报告本次请求是否被记为停顿中止。
func isStreamStallUsageLogKind(t *testing.T, h *Handler) bool {
	t.Helper()
	h.db.FlushUsageLogs()
	logs, err := h.db.ListUsageLogsByFilter(context.Background(), database.UsageLogFilter{
		Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), IncludeCanceled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range logs {
		if entry.UpstreamErrorKind == "stream_stall" {
			return true
		}
	}
	return false
}

// TestChatCompletionsStreamStallDisabledKeepsLegacyBehavior 验证默认关闭时行为不变：
// 同样的上游空窗在未配置时不会被中止，请求照常等到终态。
func TestChatCompletionsStreamStallDisabledKeepsLegacyBehavior(t *testing.T) {
	withStreamStallTimeout(t, 0)
	upstream := slowLiveUpstream(t, 3, 250*time.Millisecond)
	_, router := newStreamLimitTestHandler(t, upstream.URL, 0)

	response := performModelQuotaRequest(router, "/v1/chat/completions",
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	body := response.Body.String()
	t.Logf("status=%d body=%q", response.Code, body)

	if response.Code != http.StatusOK {
		t.Fatalf("关闭时不应改变既有行为: status=%d body=%q", response.Code, body)
	}
	if !strings.Contains(body, "AAA") {
		t.Fatalf("关闭时应完整放行: %q", body)
	}
}

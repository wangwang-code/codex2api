package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// Handles decides whether the reviewer is allowed to short-circuit into this
// process. Getting it wrong in the permissive direction would route an audit
// aimed at some other gateway back into us, so the negatives matter as much as
// the positives.
func TestInternalChatExecutorHandles(t *testing.T) {
	exec := InternalChatExecutor{Handler: &Handler{}, Port: 8317}

	yes := []string{
		"http://127.0.0.1:8317/v1",
		"http://localhost:8317/v1",
		"http://[::1]:8317/v1",
		"http://0.0.0.0:8317/v1",
		"http://127.0.0.2:8317/v1",
		"http://127.0.0.1:8317",
	}
	for _, u := range yes {
		if !exec.Handles(u) {
			t.Fatalf("Handles(%q) = false, want true", u)
		}
	}

	no := []string{
		"http://127.0.0.1:9999/v1",     // 端口不符
		"https://openrouter.ai/api/v1", // 外部主机
		"http://10.0.0.5:8317/v1",      // 非回环
		"",                             // 空
		"not a url",                    // 解析失败
		"http://127.0.0.1/v1",          // 缺端口 → 默认 80，不符
	}
	for _, u := range no {
		if exec.Handles(u) {
			t.Fatalf("Handles(%q) = true, want false", u)
		}
	}

	// 没有 handler 时一律拒绝：宁可走 HTTP，也不能短路到一个空壳。
	if (InternalChatExecutor{Port: 8317}).Handles("http://127.0.0.1:8317/v1") {
		t.Fatal("executor without a handler claimed the endpoint")
	}
}

// 进程内执行器必须回报真实的 (status, body)。
//
// httptest.ResponseRecorder 的默认状态码是 200，而请求上下文超时后
// ChatCompletions 一个字节都不会写。原样把 (200, 空) 交出去，复核方会把空串
// 当 JSON 解析，报成「invalid review model JSON: unexpected end of JSON input」
// —— 一次超时被记成模型返回了坏 JSON，于是既不重试、也不计熔断失败。
func TestInternalChatResultReportsAbortedRequest(t *testing.T) {
	newCtx := func() *gin.Context {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		return c
	}

	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	status, body := internalChatResult(deadlineCtx, httptest.NewRecorder(), newCtx())
	if status != http.StatusGatewayTimeout {
		t.Fatalf("超时应当回报 504，得到 %d", status)
	}
	if !bytes.Contains(body, []byte("internal_request_aborted")) || !bytes.Contains(body, []byte("deadline exceeded")) {
		t.Fatalf("504 必须带可读的错误体，得到 %q", body)
	}

	canceledCtx, cancelCancel := context.WithCancel(context.Background())
	cancelCancel()
	if status, _ := internalChatResult(canceledCtx, httptest.NewRecorder(), newCtx()); status != logStatusClientClosed {
		t.Fatalf("取消应当回报 499，得到 %d", status)
	}

	// 正常写出正文时原样返回，不能改写状态。
	recorder := httptest.NewRecorder()
	recorder.WriteHeader(http.StatusOK)
	recorder.WriteString(`{"choices":[]}`)
	status, body = internalChatResult(context.Background(), recorder, newCtx())
	if status != http.StatusOK || string(body) != `{"choices":[]}` {
		t.Fatalf("正常响应被改写: %d %q", status, body)
	}

	// 访问日志状态（200 已提交后才知道的真实结果）要能顶替默认 200。
	c := newCtx()
	c.Set(AccessLogStatusContextKey, logStatusUpstreamStreamBreak)
	if status, _ := internalChatResult(context.Background(), httptest.NewRecorder(), c); status != logStatusUpstreamStreamBreak {
		t.Fatalf("未采纳访问日志状态，得到 %d", status)
	}
}

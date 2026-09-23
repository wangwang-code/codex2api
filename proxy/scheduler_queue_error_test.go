package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// parkSchedulerWaiterAccount 给「空池」场景放一个停放账号：结构上可服务（凭据齐全、
// 未被停用），但处于冷却中，所以等待者会正常停在调度队列里。
//
// 契约变更说明：池里连一个结构性候选都没有时，调度等待不再空等（会立即失败，避免把
// 客户端挂满超时）。下面这些用例真正要验证的是队列行为——满队拒绝、超时/停止排空、
// 心跳不扰动队列、reconcile 重新入队——因此用一个冷却账号把等待者停在队列里，
// 而不是依赖「空池也排队」。
func parkSchedulerWaiterAccount(t *testing.T, store *auth.Store) *auth.Account {
	t.Helper()
	account := &auth.Account{DBID: 9001, AccessToken: "parking-token", PlanType: "plus"}
	store.AddAccount(account)
	store.MarkCooldown(account, time.Minute, "scheduler-queue-test")
	return account
}

// TestDispatchWaitTimeoutZeroFailsImmediately 验证把等待上限设为 0 后，池里账号都在
// 冷却（结构可服务、但没有任何可立刻派发的账号）时也会立即失败，而不是等满超时。
//
// 这正是「索引调度 + 顺序耗尽」下账号全部限流/耗尽时的期望行为：索引调度默认会排队
// 等容量，而 CPA 没有等待队列、立刻报错。等待上限设 0 即在索引调度下对齐 CPA。
func TestDispatchWaitTimeoutZeroFailsImmediately(t *testing.T) {
	previous := dispatchAccountWaitTimeout
	dispatchAccountWaitTimeout = 0
	t.Cleanup(func() { dispatchAccountWaitTimeout = previous })

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	t.Cleanup(store.Stop)
	parkSchedulerWaiterAccount(t, store)
	h := &Handler{store: store}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	account, _, _, err := h.nextRetryAccountWithGuard(ctx, "", 0, newRetryAccountExclusions(), nil, false, auth.DispatchPolicyStandard)
	elapsed := time.Since(started)

	if account != nil || err != nil {
		t.Fatalf("selection returned account=%v, error=%v", account, err)
	}
	if elapsed > time.Second {
		t.Fatalf("等待上限为 0 时仍等了 %s，应当立即失败", elapsed)
	}
	if got := store.GetSchedulerMetrics().Waiters; got != 0 {
		t.Fatalf("不应进入等待队列，leaked %d queue waiters", got)
	}
}

// TestDispatchWaitTimeoutEnvOverride 验证等待上限可由环境变量配置，且 0 是合法值。
func TestDispatchWaitTimeoutEnvOverride(t *testing.T) {
	t.Setenv("DISPATCH_ACCOUNT_WAIT_TIMEOUT", "")
	if got := dispatchAccountWaitTimeoutFromEnv(); got != defaultDispatchAccountWaitTimeout {
		t.Fatalf("默认 = %s, want %s", got, defaultDispatchAccountWaitTimeout)
	}
	t.Setenv("DISPATCH_ACCOUNT_WAIT_TIMEOUT", "0")
	if got := dispatchAccountWaitTimeoutFromEnv(); got != 0 {
		t.Fatalf("0 应当表示不等待，got %s", got)
	}
	t.Setenv("DISPATCH_ACCOUNT_WAIT_TIMEOUT", "1s")
	if got := dispatchAccountWaitTimeoutFromEnv(); got != time.Second {
		t.Fatalf("1s => %s", got)
	}
	t.Setenv("DISPATCH_ACCOUNT_WAIT_TIMEOUT", "later")
	if got := dispatchAccountWaitTimeoutFromEnv(); got != defaultDispatchAccountWaitTimeout {
		t.Fatalf("非法值应沿用默认，got %s", got)
	}
}

func saturatedSchedulerQueue(t *testing.T) *auth.Store {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	t.Cleanup(store.Stop)
	parkSchedulerWaiterAccount(t, store)
	store.SetSchedulerWaitLimits(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _, _ = store.WaitForDispatchAvailable(ctx, "", time.Minute, 0, nil, nil, false, auth.DispatchPolicyStandard)
	}()
	t.Cleanup(func() { cancel(); <-done })
	until := time.Now().Add(time.Second)
	for store.GetSchedulerMetrics().Waiters != 1 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if store.GetSchedulerMetrics().Waiters != 1 {
		t.Fatal("queue did not fill")
	}
	return store
}

func TestSchedulerQueueOverloadStopsContinuousRetry(t *testing.T) {
	s := saturatedSchedulerQueue(t)
	h := &Handler{store: s}
	exclusions := newRetryAccountExclusions()
	exclusions.MarkTransient(99)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _, err := h.nextRetryAccountWithGuard(ctx, "", 0, exclusions, nil, false, auth.DispatchPolicyStandard)
	if !errors.Is(err, auth.ErrSchedulerQueueFull) || ctx.Err() != nil {
		t.Fatalf("overload was retried: %v (context %v)", err, ctx.Err())
	}
	if !exclusions.ForSelection()[99] {
		t.Fatal("queue rejection reset upstream retry exclusions")
	}
	m := s.GetSchedulerMetrics()
	if m.WaitRejected != 1 || m.WaitRejectedPerKey != 1 || m.Waiters != 1 {
		t.Fatalf("rejection metrics = %+v", m)
	}
}

func TestSchedulerQueueOverloadHTTPRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, path, body string
		invoke           func(*Handler, *gin.Context)
	}{
		{"responses", "/v1/responses", `{"model":"gpt-5.5","input":"hello"}`, (*Handler).Responses},
		{"compact", "/v1/responses/compact", `{"model":"gpt-5.5","input":"hello"}`, (*Handler).ResponsesCompact},
		{"chat", "/v1/chat/completions", `{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`, (*Handler).ChatCompletions},
		{"messages", "/v1/messages", `{"model":"claude-opus-4-6","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`, (*Handler).Messages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := saturatedSchedulerQueue(t)
			h := NewHandler(s, nil, &config.Config{AllowAnonymousV1: true}, nil)
			r := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(r)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			tc.invoke(h, c)
			if r.Code != http.StatusServiceUnavailable || r.Header().Get("Retry-After") != "1" || !strings.Contains(r.Body.String(), schedulerQueueFullMessage) {
				t.Fatalf("overload response = %d, retry-after=%q, %s", r.Code, r.Header().Get("Retry-After"), r.Body.String())
			}
		})
	}
}

func TestSchedulerQueueOverloadCommittedSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		protocol continuousRetryHTTPProtocol
		marker   string
	}{
		{continuousRetryProtocolResponses, `"type":"response.failed"`},
		{continuousRetryProtocolChat, `"error"`},
		{continuousRetryProtocolAnthropic, "event: error"},
	} {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString(": keepalive\n\n")
		c.Writer.Flush()
		if !writeSchedulerQueueError(c, auth.ErrSchedulerQueueFull, tc.protocol) {
			t.Fatal("overload not handled")
		}
		body := r.Body.String()
		if r.Code != http.StatusOK || !strings.HasPrefix(body, ": keepalive\n\n") || !strings.Contains(body, tc.marker) || !strings.Contains(body, schedulerQueueFullMessage) {
			t.Fatalf("committed overload response = %d %s", r.Code, body)
		}
	}
}

func TestSelectionTimeoutHTTPProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		protocol continuousRetryHTTPProtocol
		marker   string
	}{
		{continuousRetryProtocolResponses, `"type":"response.failed"`},
		{continuousRetryProtocolChat, `"error"`},
		{continuousRetryProtocolAnthropic, "event: error"},
	} {
		for _, committed := range []bool{false, true} {
			r := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(r)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			if committed {
				c.Header("Content-Type", "text/event-stream")
				_, _ = c.Writer.WriteString(": keepalive\n\n")
				c.Writer.Flush()
			}
			if !writeSchedulerQueueError(c, context.DeadlineExceeded, tc.protocol) {
				t.Fatal("selection timeout would fall through to another account scan")
			}
			body := r.Body.String()
			if !strings.Contains(body, schedulerSelectionTimeoutMessage) {
				t.Fatalf("missing selection timeout: %s", body)
			}
			if committed {
				if r.Code != http.StatusOK || !strings.HasPrefix(body, ": keepalive\n\n") || !strings.Contains(body, tc.marker) {
					t.Fatalf("committed timeout response = %d %s", r.Code, body)
				}
			} else if r.Code != http.StatusServiceUnavailable || r.Header().Get("Retry-After") != "1" {
				t.Fatalf("timeout response = %d, retry-after=%q", r.Code, r.Header().Get("Retry-After"))
			}
		}
	}
}

func TestSchedulerQueueOverloadResponsesWebSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := saturatedSchedulerQueue(t)
	h := NewHandler(s, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-5.5", "input": "hello"}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil || event["type"] != "error" || !strings.Contains(string(payload), schedulerQueueFullMessage) {
		t.Fatalf("WS overload frame = %s", payload)
	}
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseTryAgainLater) {
		t.Fatalf("WS overload close = %v", err)
	}
}

func TestSchedulerWaitHeartbeatPreservesOneAdmission(t *testing.T) {
	previous := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 5 * time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previous })
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	defer store.Stop()
	parkSchedulerWaiterAccount(t, store)
	h := &Handler{store: store}
	keepalive := &recordingContinuousRetryKeepalive{active: true}
	ctx, cancel := context.WithTimeout(contextWithContinuousRetryKeepalive(keepalive), 35*time.Millisecond)
	defer cancel()
	_, _, _, _ = h.waitForRetryAccountAvailableWithGuard(ctx, "", 0, nil, nil, false, auth.DispatchPolicyStandard)
	m := store.GetSchedulerMetrics()
	if keepalive.writes < 2 || m.WaitStarted != 1 || m.Waiters != 0 || m.SelectionTotal != 1 {
		t.Fatalf("heartbeats disturbed queue: writes=%d metrics=%+v", keepalive.writes, m)
	}
}

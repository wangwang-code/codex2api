package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// newPriorityFallbackHandler 构造「codex 官方号（高优先级）+ 中转号（优先级 -1）」的
// handler，用于实测兜底编排。两个上游各自计数，便于判断实际走了哪一个。
func newPriorityFallbackHandler(t *testing.T, codexUpstream, relayUpstream string) (*Handler, *gin.Engine, *auth.Account, *auth.Account) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previousSettings := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "priority-fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Key: modelQuotaTestKey, Name: "priority-fallback",
	}); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4, MaxRetries: 0, MaxRateLimitRetries: 0})
	t.Cleanup(store.Stop)

	codexAccount := &auth.Account{DBID: 1, AccessToken: "codex-token", PlanType: "pro", Models: []string{"gpt-6-astra"}}
	codexAccount.SetSchedulerPriority(10)
	relayAccount := &auth.Account{
		DBID: 2, UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL: relayUpstream, APIKey: "sk-relay", PlanType: "api",
		Models: []string{"gpt-6-astra"},
	}
	relayAccount.SetSchedulerPriority(-1)
	store.AddAccount(codexAccount)
	store.AddAccount(relayAccount)

	h := NewHandler(store, db, &config.Config{}, nil)
	router := gin.New()
	h.RegisterRoutes(router)
	return h, router, codexAccount, relayAccount
}

// servedAccountID 返回最近一条用量记录实际使用的账号 ID。
// ListUsageLogsByFilter 是 created_at DESC（最新在前），所以取 logs[0]。
func servedAccountID(t *testing.T, h *Handler) int64 {
	t.Helper()
	h.db.FlushUsageLogs()
	logs, err := h.db.ListUsageLogsByFilter(context.Background(), database.UsageLogFilter{
		Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), IncludeCanceled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 {
		t.Fatal("没有用量记录")
	}
	return logs[0].AccountID
}

// TestPriorityFallbackPrefersCodexThenFallsBackToRelay 实测「官方号设正优先级、
// 中转号设 -1」的兜底编排：
//   - 官方号可用时严格优先用它（中转一次都不碰）；
//   - 官方号被打入冷却（等价于 401 后账号退出调度）后，自动落到中转。
//
// 这条不靠读代码推断，而是走真实 handler + 渠道过滤链，并用用量记录里的 account_id
// 作为「实际用了哪个账号」的判据。
func TestPriorityFallbackPrefersCodexThenFallsBackToRelay(t *testing.T) {
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	var codexHits, relayHits atomic.Int32
	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codexHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, modelQuotaSSE)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(codexUpstream.Close)
	relayUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, modelQuotaSSE)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(relayUpstream.Close)

	// codex 官方号的出站统一经 Resin 指向假上游。
	SetResinConfig(&ResinConfig{BaseURL: codexUpstream.URL, PlatformName: "priority-fallback-test"})

	h, router, codexAccount, relayAccount := newPriorityFallbackHandler(t, codexUpstream.URL, relayUpstream.URL)
	body := `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`

	response := performModelQuotaRequest(router, "/v1/chat/completions", body)
	if response.Code != http.StatusOK {
		t.Fatalf("首个请求失败: status=%d body=%q", response.Code, response.Body.String())
	}
	if got := servedAccountID(t, h); got != codexAccount.DBID {
		t.Fatalf("官方号可用时应优先用它，实际 account_id=%d（codex 命中 %d，中转命中 %d）",
			got, codexHits.Load(), relayHits.Load())
	}

	// 把官方号打入冷却：等价于 401 之后账号退出调度的状态。
	h.store.MarkCooldown(codexAccount, time.Hour, "unauthorized")
	codexHitsBefore := codexHits.Load()

	response = performModelQuotaRequest(router, "/v1/chat/completions", body)
	if response.Code != http.StatusOK {
		t.Fatalf("兜底请求失败: status=%d body=%q", response.Code, response.Body.String())
	}
	if got := servedAccountID(t, h); got != relayAccount.DBID {
		t.Fatalf("官方号不可用时应落到中转号 %d，实际 account_id=%d（codex 命中 %d，中转命中 %d）",
			relayAccount.DBID, got, codexHits.Load(), relayHits.Load())
	}
	if codexHits.Load() != codexHitsBefore {
		t.Fatalf("官方号已冷却，不应再被调用（codex 命中从 %d 涨到 %d）", codexHitsBefore, codexHits.Load())
	}
	if relayHits.Load() == 0 {
		t.Fatal("中转上游一次都没被调用，兜底没有真正生效")
	}
}

// TestPriorityFallbackKeepsRelayIdleWhileCodexIsHealthy 是上一条的对照：
// 官方号健康期间，中转号不应被碰——否则「兜底」就变成了「抢流量」。
func TestPriorityFallbackKeepsRelayIdleWhileCodexIsHealthy(t *testing.T) {
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	var relayHits atomic.Int32
	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, modelQuotaSSE)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(codexUpstream.Close)
	relayUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, modelQuotaSSE)
	}))
	t.Cleanup(relayUpstream.Close)
	SetResinConfig(&ResinConfig{BaseURL: codexUpstream.URL, PlatformName: "priority-fallback-test"})

	_, router, codexAccount, _ := newPriorityFallbackHandler(t, codexUpstream.URL, relayUpstream.URL)
	body := `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`

	for i := 0; i < 3; i++ {
		response := performModelQuotaRequest(router, "/v1/chat/completions", body)
		if response.Code != http.StatusOK {
			t.Fatalf("第 %d 个请求失败: status=%d body=%q", i+1, response.Code, response.Body.String())
		}
	}
	if got := relayHits.Load(); got != 0 {
		t.Fatalf("官方号健康期间中转不应被调用，实际 %d 次", got)
	}
	if codexAccount.GetSchedulerPriority() <= 0 {
		t.Fatalf("官方号优先级 = %d，应为正", codexAccount.GetSchedulerPriority())
	}
}

// TestSchedulerPriorityRangePreservesNegativeFallback 验证优先级范围保留负值：
// 归一化不能把 -1 抹成 0，否则「兜底渠道」的编排就失效了。
func TestSchedulerPriorityRangePreservesNegativeFallback(t *testing.T) {
	account := &auth.Account{DBID: 1}
	for _, want := range []int64{-100, -1, 0, 50, 100} {
		account.SetSchedulerPriority(want)
		if got := account.GetSchedulerPriority(); got != want {
			t.Fatalf("SetSchedulerPriority(%d) => %d", want, got)
		}
	}
	// 越界钳制到 [-100, 100]。
	account.SetSchedulerPriority(-500)
	if got := account.GetSchedulerPriority(); got != -100 {
		t.Fatalf("下越界应钳到 -100，实际 %d", got)
	}
	account.SetSchedulerPriority(500)
	if got := account.GetSchedulerPriority(); got != 100 {
		t.Fatalf("上越界应钳到 100，实际 %d", got)
	}
}
